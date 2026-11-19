// Drives the real sync engine against a real archivist-server holding an
// ENCRYPTED vault, with two devices sharing one key.
//
// Usage:
//   node test/integration-encrypted.mjs <serverUrl> <token> <root>
//
// <root> is the server's data root. It must hold an encrypted vault "work"
// and an ordinary vault "personal", both empty:
//
//   archivist-server vault add -root <root> -encrypted work
//   archivist-server vault add -root <root> personal
//
// The root is read directly, because the property under test is what the
// SERVER ends up holding: no readable name and no readable content.

import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { FakeApp } from "./obsidian-shim.mjs";
import {
	Sync,
	Client,
	VaultCrypt,
	KeyRequiredError,
	WrongKeyError,
	PairingHazardError,
	ENVELOPE,
	generateKey,
} from "../dist-test/entry.mjs";

const [, , SERVER, TOKEN, ROOT] = process.argv;
if (!SERVER || !TOKEN || !ROOT) {
	console.error("usage: node test/integration-encrypted.mjs <serverUrl> <token> <root>");
	process.exit(2);
}

let failures = 0;
function check(name, ok, detail = "") {
	console.log(`${ok ? "  OK  " : "  FAIL"} ${name}${detail ? "  — " + detail : ""}`);
	if (!ok) failures++;
}

async function device(name, crypt, vault = "work") {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), `ve-${name}-`));
	const app = new FakeApp(root);
	const sync = new Sync(app, () => new Client(SERVER, TOKEN, vault, crypt), () => name, () => {});
	return {
		name,
		root,
		app,
		sync,
		read: (p) => fs.readFile(path.join(root, p), "utf8"),
		write: async (p, content) => {
			await fs.mkdir(path.dirname(path.join(root, p)), { recursive: true });
			await fs.writeFile(path.join(root, p), content);
		},
		exists: (p) => fs.access(path.join(root, p)).then(() => true, () => false),
		list: async () => (await fs.readdir(root, { recursive: true })).map(String).sort(),
	};
}

async function thrown(fn) {
	try {
		await fn();
	} catch (err) {
		return err;
	}
	return null;
}

/** Every file under the server's vault directory, as [relative path, bytes]. */
async function serverFiles() {
	const dir = path.join(ROOT, "vaults", "work");
	const out = [];
	for (const rel of await fs.readdir(dir, { recursive: true })) {
		const full = path.join(dir, String(rel));
		if ((await fs.stat(full)).isFile() && !String(rel).startsWith(".git")) {
			out.push([String(rel), await fs.readFile(full)]);
		}
	}
	return out;
}

const key = generateKey();
const crypt = await VaultCrypt.fromKey(key);

// --- 1. no key, no sync -----------------------------------------------------
const keyless = await device("keyless", undefined);
await keyless.write("Secret.md", "must not leave this device\n");
let err = await thrown(() => keyless.sync.run());
check("a device with no key is asked for one", err instanceof KeyRequiredError && err.hasKey === false, String(err));
check("and nothing reached the server", (await serverFiles()).length === 0);

// --- 2. first device installs the key and publishes -------------------------
const mac = await device("mac", crypt);
await mac.write("Clients/Acme/Notes.md", "the client list\n");
await mac.write("Daily.md", "one\ntwo\nthree\n");
const blob = Buffer.from(Array.from({ length: 4096 }, (_, i) => i % 251));
await mac.write("att/img.bin", blob);
let r = await mac.sync.run();
check("mac publishes three files", r.pushed === 3 && r.refused.length === 0, JSON.stringify(r));

let held = await serverFiles();
check("the server holds three files", held.length === 3, held.map((f) => f[0]).join(", "));
check(
	"no name on the server is readable",
	held.every(([rel]) => /^[a-z2-7/]+$/.test(rel)) && !held.some(([rel]) => /acme|clients|daily|notes|img/i.test(rel)),
	held.map((f) => f[0]).join(", "),
);
check(
	"every file on the server starts with the envelope",
	held.every(([, bytes]) => bytes.subarray(0, ENVELOPE.length).toString() === ENVELOPE),
);
check(
	"no content on the server is readable",
	!held.some(([, bytes]) => bytes.includes("client list") || bytes.includes("two\nthree")),
);

r = await mac.sync.run();
check("an unchanged vault pushes nothing", r.pushed === 0 && r.pulled === 0, JSON.stringify(r));

// --- 3. the key gates a second device ---------------------------------------
err = await thrown(() => keyless.sync.run());
check("a keyless device is now told the vault has a key", err instanceof KeyRequiredError && err.hasKey === true, String(err));

const stranger = await device("stranger", await VaultCrypt.fromKey(generateKey()));
err = await thrown(() => stranger.sync.run());
check("a device with the wrong key is refused", err instanceof WrongKeyError, String(err));
check("and received nothing", (await stranger.list()).length === 0);

const phone = await device("phone", crypt);
r = await phone.sync.run();
check("phone pulls with the right key", r.pulled === 3, JSON.stringify(r));
check("nested note matches", (await phone.read("Clients/Acme/Notes.md")) === "the client list\n");
check("binary round-trips byte-identically", Buffer.compare(blob, await fs.readFile(path.join(phone.root, "att/img.bin"))) === 0);

// --- 4. edits, renames and deletes travel -----------------------------------
await mac.write("Daily.md", "ONE\ntwo\nthree\n");
r = await mac.sync.run();
check("mac pushes an edit", r.pushed === 1, JSON.stringify(r));
r = await phone.sync.run();
check("phone pulls the edit", (await phone.read("Daily.md")) === "ONE\ntwo\nthree\n", JSON.stringify(r));
r = await phone.sync.run();
check("a pulled file is not pushed back", r.pushed === 0, JSON.stringify(r));

await fs.rename(path.join(mac.root, "Daily.md"), path.join(mac.root, "Journal.md"));
r = await mac.sync.run();
check("a rename is one change", r.pushed === 1 && r.refused.length === 0, JSON.stringify(r));
await phone.sync.run();
check(
	"phone sees the rename",
	!(await phone.exists("Daily.md")) && (await phone.read("Journal.md")) === "ONE\ntwo\nthree\n",
);

await fs.rm(path.join(mac.root, "att/img.bin"));
await mac.sync.run();
await phone.sync.run();
check("a delete travels", !(await phone.exists("att/img.bin")));

// --- 5. both edit one note: the plugin keeps both ---------------------------
await mac.write("Journal.md", "mac's version\n");
await phone.write("Journal.md", "phone's version\n");
await mac.sync.run();
r = await phone.sync.run();
check("phone's push is a conflict", r.conflicts.length === 1, JSON.stringify(r));
check("the note holds what the server kept", (await phone.read("Journal.md")) === "mac's version\n");
let copies = (await phone.list()).filter((p) => p.includes(".conflict-"));
check("phone's version survives in a copy with a readable name", copies.length === 1 && copies[0].startsWith("Journal.conflict-phone-"), copies.join(", "));
check("the copy holds phone's version", copies.length === 1 && (await phone.read(copies[0])) === "phone's version\n");

r = await phone.sync.run();
check("the copy is uploaded on the next cycle", r.pushed === 1, JSON.stringify(r));
await mac.sync.run();
check("and reaches the other device", copies.length === 1 && (await mac.read(copies[0])) === "phone's version\n");
held = await serverFiles();
check("the copy's name is encrypted on the server too", held.every(([rel]) => /^[a-z2-7/]+$/.test(rel)), held.map((f) => f[0]).join(", "));

// --- 6. history is readable on the device -----------------------------------
const client = new Client(SERVER, TOKEN, "work", crypt);
const { revisions } = await client.history("Journal.md");
check("history lists revisions of a note by its real name", revisions.length >= 1, JSON.stringify(revisions.length));
const old = revisions.length ? new TextDecoder().decode(await client.readAt(revisions[0].commit, "Journal.md")) : "";
check("an old revision decrypts", old === "mac's version\n", JSON.stringify(old));
const gone = await client.deleted();
check("deleted files are listed by their real names", gone.some((d) => d.path === "att/img.bin"), JSON.stringify(gone.map((d) => d.path)));

// --- 7. re-bootstrap must not duplicate the vault ---------------------------
const before = await phone.list();
r = await phone.sync.forceRebootstrap();
check("re-bootstrap makes no conflict copies of identical files", JSON.stringify(await phone.list()) === JSON.stringify(before), (await phone.list()).join(", "));
check("and pushes nothing", r.pushed === 0, JSON.stringify(r));

// --- 8. a second device that already has the notes --------------------------
const laptop = await device("laptop", crypt);
for (const p of await mac.list()) {
	if ((await fs.stat(path.join(mac.root, p))).isFile()) await laptop.write(p, await fs.readFile(path.join(mac.root, p)));
}
await laptop.write("Journal.md", "laptop drifted\n");
err = await thrown(() => laptop.sync.run());
check("pairing stops and asks", err instanceof PairingHazardError, String(err));
check(
	"and compares on plaintext",
	err instanceof PairingHazardError && err.comparison.differ.length === 1 && err.comparison.differ[0] === "Journal.md" && err.comparison.identical === 2,
	err instanceof PairingHazardError ? JSON.stringify(err.comparison) : "",
);

r = await laptop.sync.resolvePairing("merge");
copies = (await laptop.list()).filter((p) => p.includes(".conflict-laptop-"));
check("merging copies only the note that really differs", copies.length === 1 && copies[0].startsWith("Journal.conflict-laptop-"), copies.join(", "));
check("and reports one conflict, not one per file", r.conflicts.length === 1, JSON.stringify(r.conflicts.map((c) => c.path)));
check("the differing note holds the server's version", (await laptop.read("Journal.md")) === "mac's version\n");
r = await laptop.sync.run();
r = await laptop.sync.run();
check("the merged device then settles", r.pushed === 0 && r.pulled === 0, JSON.stringify(r));

// --- 9. a key must never be used against an ordinary vault ------------------
const fooled = await device("fooled", crypt, "personal");
await fooled.write("Secret.md", "must not be uploaded in plaintext\n");
err = await thrown(() => fooled.sync.run());
check("a device holding a key refuses a vault that claims to be ordinary", err instanceof Error && /Refusing to sync/.test(err.message), String(err));
const leaked = await fs.readdir(path.join(ROOT, "vaults", "personal"));
check("and nothing was uploaded", leaked.filter((f) => !f.startsWith(".")).length === 0, leaked.join(", "));

console.log(failures === 0 ? "\nall checks passed" : `\n${failures} check(s) FAILED`);
process.exit(failures === 0 ? 0 : 1);
