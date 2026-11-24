// Drives the current sync engine against an OLDER archivist-server: one from
// before encryption (0.20), whose index lists no features.
//
// Usage:
//   node test/integration-old-server.mjs <serverUrl> <token> <root> [vault]
//
// The server must be a real old build, which is the point. The 0.20.0 binaries
// are attached to that release, or build one from the tag:
//
//   git worktree add ../archivist-0.20 v0.20.0
//   (cd ../archivist-0.20 && go build -o archivist-server-0.20 ./cmd/archivist-server)
//
// <root> is its data root, holding an ordinary, empty vault (default
// "personal"): an empty directory <root>/vaults/personal is enough.
//
// What is checked: a plugin that updated ahead of its server keeps syncing an
// ordinary vault exactly as before, and the encryption it cannot use there
// fails closed. Nothing marked .encrypted reaches a server that would store
// it readable.

import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { FakeApp } from "./obsidian-shim.mjs";
import { Sync, Client, VaultCrypt, generateKey } from "../dist-test/entry.mjs";

const [, , SERVER, TOKEN, ROOT, VAULT_ARG] = process.argv;
if (!SERVER || !TOKEN || !ROOT) {
	console.error("usage: node test/integration-old-server.mjs <serverUrl> <token> <root> [vault]");
	process.exit(2);
}
const VAULT = VAULT_ARG ?? "personal";
const DIR = path.join(ROOT, "vaults", VAULT);

let failures = 0;
function check(name, ok, detail = "") {
	console.log(`${ok ? "  OK  " : "  FAIL"} ${name}${!ok && detail ? "  — " + detail : ""}`);
	if (!ok) failures++;
}

async function device(name, crypt) {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), `vo-${name}-`));
	const app = new FakeApp(root);
	const d = {
		root,
		read: (p) => fs.readFile(path.join(root, p), "utf8"),
		write: async (p, content) => {
			await fs.mkdir(path.dirname(path.join(root, p)), { recursive: true });
			await fs.writeFile(path.join(root, p), content);
		},
		remove: (p) => fs.rm(path.join(root, p)),
		exists: (p) => fs.access(path.join(root, p)).then(() => true, () => false),
		list: async () => {
			const out = [];
			for (const rel of await fs.readdir(root, { recursive: true })) {
				if ((await fs.stat(path.join(root, String(rel)))).isFile()) out.push(String(rel));
			}
			return out.sort();
		},
	};
	d.sync = new Sync(app, () => new Client(SERVER, TOKEN, VAULT, crypt), () => name, () => {});
	return d;
}

async function thrown(fn) {
	try {
		await fn();
	} catch (err) {
		return err;
	}
	return null;
}

/** Every file in the server's vault directory. */
async function serverFiles() {
	const out = [];
	for (const rel of await fs.readdir(DIR, { recursive: true })) {
		const name = String(rel);
		if (name === ".git" || name.startsWith(".git/") || name.startsWith(".archivist")) continue;
		if ((await fs.stat(path.join(DIR, name))).isFile()) out.push(name);
	}
	return out.sort();
}

// --- 0. this really is an old server ------------------------------------------
const raw = await fetch(`${SERVER}/${encodeURIComponent(VAULT)}/v1`, {
	headers: { Authorization: `Bearer ${TOKEN}` },
}).then((r) => r.json());
if (Array.isArray(raw.features)) {
	console.error(`this server (${raw.version}) lists features; the test needs one from before 0.21`);
	process.exit(2);
}
const idx = await new Client(SERVER, TOKEN, VAULT).index();
check("the old server's index is accepted, with no features", idx.features.length === 0 && !idx.encrypted && idx.keyCheck === "", JSON.stringify(idx));

// --- 1. an ordinary vault syncs as it always did --------------------------------
const mac = await device("mac");
await mac.write("Plan.md", "line one\n");
await mac.write("Notes/Idea.md", "an idea\n");
let r = await mac.sync.run();
check("a device pushes to the old server", r.pushed === 2 && r.refused.length === 0, JSON.stringify(r));
check("and is told nothing about keys or old servers", !r.needsKey && !r.serverTooOld);

const phone = await device("phone");
r = await phone.sync.run();
check("a second device pulls", r.pulled === 2 && (await phone.read("Plan.md")) === "line one\n", JSON.stringify(r));

await phone.write("Plan.md", "line one\nline two\n");
await phone.remove("Notes/Idea.md");
r = await phone.sync.run();
check("it pushes an edit and a delete", r.pushed === 2 && r.refused.length === 0, JSON.stringify(r));
r = await mac.sync.run();
check(
	"and the first device receives both",
	(await mac.read("Plan.md")) === "line one\nline two\n" && !(await mac.exists("Notes/Idea.md")),
	JSON.stringify(r),
);

// --- 2. marked files stay on the device ------------------------------------------
await mac.write("Salary.encrypted.md", "SECRET-SALARY 5000\n");
await mac.write("Clients.encrypted/Acme.md", "SECRET-CONTRACT\n");
await mac.write("Todo.md", "buy milk\n");
r = await mac.sync.run();
check("the report names the old server's version", r.serverTooOld === raw.version, JSON.stringify(r));
check("and offers no key, which that server could not record", !r.needsKey);
check("the ordinary note beside them still synced", r.pushed === 1 && (await serverFiles()).includes("Todo.md"), JSON.stringify(r));
let held = await serverFiles();
check("nothing marked reached the server", !held.some((p) => /\.encrypted/i.test(p)), held.join(", "));
check(
	"and the marked files are untouched on the device",
	(await mac.read("Salary.encrypted.md")) === "SECRET-SALARY 5000\n" && (await mac.read("Clients.encrypted/Acme.md")) === "SECRET-CONTRACT\n",
);
r = await mac.sync.run();
check("the next cycle says the same and pushes nothing", r.serverTooOld === raw.version && r.pushed === 0, JSON.stringify(r));

r = await phone.sync.run();
check("another device gets the ordinary note and no marked file", r.pulled === 1 && !(await phone.list()).some((p) => /\.encrypted/i.test(p)), JSON.stringify(r));
check("and with no marked files of its own is told nothing", !r.serverTooOld && !r.needsKey);

// --- 3. a device that holds a key does not sync at all -----------------------------
// It got that key from a newer server. Against this one it could neither
// check the key nor rely on the server to refuse a readable upload.
const keyed = await device("keyed", await VaultCrypt.fromKey(generateKey(), "files"));
await keyed.write("Diary.encrypted.md", "SECRET-DIARY\n");
await keyed.write("Loose.md", "not sent either\n");
const before = await serverFiles();
const err = await thrown(() => keyed.sync.run());
check("a device with a files key refuses, naming the server", /too old/.test(err?.message ?? "") && err.message.includes(raw.version), err?.message);
check("having sent nothing", JSON.stringify(await serverFiles()) === JSON.stringify(before));
check("and received nothing", JSON.stringify(await keyed.list()) === JSON.stringify(["Diary.encrypted.md", "Loose.md"]), (await keyed.list()).join(", "));

const whole = await device("whole", await VaultCrypt.fromKey(generateKey(), "vault"));
const err2 = await thrown(() => whole.sync.run());
check("a device with a whole-vault key refuses too", /Refusing to sync/.test(err2?.message ?? ""), err2?.message);

console.log(failures === 0 ? "\nall checks passed" : `\n${failures} check(s) FAILED`);
process.exit(failures === 0 ? 0 : 1);
