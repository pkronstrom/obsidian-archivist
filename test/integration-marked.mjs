// Drives the real sync engine against a real archivist-server holding an
// ORDINARY vault in which some files are marked .encrypted.
//
// Usage:
//   node test/integration-marked.mjs <serverUrl> <token> <root>
//
// <root> is the server's data root. It must hold an ordinary, empty vault
// "mixed":
//
//   archivist-server vault add -root <root> mixed
//
// The root is read directly, because the property under test is what the
// SERVER ends up holding: ordinary notes readable, marked ones not, anywhere
// in the vault's history.

import { execFileSync } from "node:child_process";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { FakeApp } from "./obsidian-shim.mjs";
import { Sync, Client, VaultCrypt, ENVELOPE, generateKey, loadState } from "../dist-test/entry.mjs";

const [, , SERVER, TOKEN, ROOT] = process.argv;
if (!SERVER || !TOKEN || !ROOT) {
	console.error("usage: node test/integration-marked.mjs <serverUrl> <token> <root>");
	process.exit(2);
}
const VAULT = "mixed";
const DIR = path.join(ROOT, "vaults", VAULT);

let failures = 0;
function check(name, ok, detail = "") {
	console.log(`${ok ? "  OK  " : "  FAIL"} ${name}${!ok && detail ? "  — " + detail : ""}`);
	if (!ok) failures++;
}

/** A device. `crypt` can be set later, which is a device being given the key. */
async function device(name, crypt) {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), `vm-${name}-`));
	const app = new FakeApp(root);
	const d = {
		name,
		root,
		app,
		crypt,
		client: () => new Client(SERVER, TOKEN, VAULT, d.crypt),
		read: (p) => fs.readFile(path.join(root, p), "utf8"),
		write: async (p, content) => {
			await fs.mkdir(path.dirname(path.join(root, p)), { recursive: true });
			await fs.writeFile(path.join(root, p), content);
		},
		rename: async (from, to) => {
			await fs.mkdir(path.dirname(path.join(root, to)), { recursive: true });
			await fs.rename(path.join(root, from), path.join(root, to));
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
	d.sync = new Sync(app, d.client, () => name, () => {});
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

/** Every file in the server's vault directory, as a map of path to bytes. */
async function serverFiles() {
	const out = new Map();
	for (const rel of await fs.readdir(DIR, { recursive: true })) {
		const full = path.join(DIR, String(rel));
		const name = String(rel);
		if (name === ".git" || name.startsWith(".git/") || name.startsWith(".archivist")) continue;
		if ((await fs.stat(full)).isFile()) out.set(name, await fs.readFile(full));
	}
	return out;
}

const sealed = (bytes) => bytes.subarray(0, ENVELOPE.length).toString() === ENVELOPE;

/** Whether text appears anywhere in the vault's history: a name or the content
 *  of any commit, not only what is on disk now. */
function inHistory(text) {
	const git = (...a) => {
		try {
			return execFileSync("git", ["-c", "safe.directory=*", "-C", DIR, ...a], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
		} catch (err) {
			return err.stdout ?? "";
		}
	};
	const revs = git("rev-list", "--all").trim().split("\n").filter(Boolean);
	if (revs.length === 0) return false;
	if (git("log", "--all", "--name-only", "--format=").includes(text)) return true;
	return git("grep", "-a", "-l", "-F", text, ...revs).trim() !== "";
}

const index = () => new Client(SERVER, TOKEN, VAULT).index();

const key = generateKey();
const crypt = await VaultCrypt.fromKey(key, "files");

// --- 1. a device with no key and no marked files is an ordinary device ------
const phone = await device("phone", undefined);
await phone.write("Shopping.md", "milk\n");
let r = await phone.sync.run();
check("a keyless device syncs an ordinary vault", r.pushed === 1 && !r.needsKey, JSON.stringify(r));
check("and the vault has no key yet", (await index()).keyCheck === "" && (await index()).encrypted === false);

// --- 2. the first marked file asks for a key and stays put -------------------
await phone.write("Pin.encrypted.md", "SECRET-PIN 4711\n");
r = await phone.sync.run();
check("a marked file with no key raises the key question", r.needsKey?.hasKey === false, JSON.stringify(r));
check("and is not uploaded", r.pushed === 0 && !(await serverFiles()).has("Pin.encrypted.md"));

// --- 3. the mac creates the key and publishes --------------------------------
const mac = await device("mac", crypt);
await mac.write("Plan.md", "line one\nline two\nline three\n");
await mac.write("Salary.encrypted.md", "SECRET-SALARY 5000\n");
await mac.write("Work/Clients.encrypted/Acme Öy/Contract.md", "SECRET-CONTRACT terms\n");
const blob = Buffer.from(Array.from({ length: 4096 }, (_, i) => i % 251));
await mac.write("Work/Clients.encrypted/scan.bin", blob);
// The server already holds the phone's note, so the mac is asked the pairing
// question first, as any device joining a vault with content is.
let err = await thrown(() => mac.sync.run());
check("joining a vault that has content still asks the pairing question", err?.constructor?.name === "PairingHazardError" && err.comparison.onlyHere.length === 4, String(err));
r = await mac.sync.resolvePairing("merge");
check("the mac publishes its four files", r.pushed === 4 && r.refused.length === 0, JSON.stringify(r));
check("and installs the key check on the ordinary vault", (await index()).keyCheck === crypt.keyCheck && (await index()).encrypted === false);

let held = await serverFiles();
check("ordinary notes are readable on the server", held.get("Plan.md")?.toString() === "line one\nline two\nline three\n" && held.get("Shopping.md")?.toString() === "milk\n");
check("a marked file keeps its name and is ciphertext", held.has("Salary.encrypted.md") && sealed(held.get("Salary.encrypted.md")));
const under = [...held.keys()].filter((p) => p.startsWith("Work/Clients.encrypted/"));
check("beneath a marked folder every name is encrypted", under.length === 2 && under.every((p) => /^Work\/Clients\.encrypted\/[a-z2-7/]+$/.test(p)), under.join(", "));
check("and every file there is ciphertext", under.every((p) => sealed(held.get(p))));
for (const text of ["SECRET-", "Acme", "Contract", "scan.bin"]) {
	check(`"${text}" appears nowhere in the vault's history`, !inHistory(text));
}
check("(the same search does find an ordinary note)", inHistory("line two") && inHistory("Plan.md"));

r = await mac.sync.run();
check("an unchanged vault pushes nothing", r.pushed === 0 && r.pulled === 0, JSON.stringify(r));

// --- 4. the keyless device keeps syncing everything else ---------------------
r = await phone.sync.run();
check("the keyless device pulls the ordinary note only", r.pulled === 1 && (await phone.read("Plan.md")) === "line one\nline two\nline three\n", JSON.stringify(r));
check("it is told the vault now has a key to enter", r.needsKey?.hasKey === true && r.needsKey.keyCheck === crypt.keyCheck);
check("no encrypted file or name reached it", !(await phone.list()).some((p) => p.includes("Clients") || p.startsWith("Salary")), (await phone.list()).join(", "));
await phone.write("Shopping.md", "milk\neggs\n");
r = await phone.sync.run();
check("its ordinary edits still sync", r.pushed === 1);
held = await serverFiles();
check("and nothing it lacks was deleted on the server", held.has("Salary.encrypted.md") && [...held.keys()].filter((p) => p.startsWith("Work/Clients.encrypted/")).length === 2);

// --- 5. the key arrives -------------------------------------------------------
phone.crypt = crypt;
r = await phone.sync.run();
check("with the key, the files it was skipping arrive readable", (await phone.read("Salary.encrypted.md")) === "SECRET-SALARY 5000\n" && (await phone.read("Work/Clients.encrypted/Acme Öy/Contract.md")) === "SECRET-CONTRACT terms\n", JSON.stringify(r));
check("attachments byte for byte", (await fs.readFile(path.join(phone.root, "Work/Clients.encrypted/scan.bin"))).equals(blob));
check("and the marked file it was holding is published, sealed", !r.needsKey && sealed((await serverFiles()).get("Pin.encrypted.md") ?? Buffer.alloc(0)));
check("the device remembers it has adopted them", loadState(phone.app).keyed === true);
r = await mac.sync.run();
check("which reaches the mac", (await mac.read("Pin.encrypted.md")) === "SECRET-PIN 4711\n");
r = await phone.sync.run();
check("and the phone then settles", r.pushed === 0 && r.pulled === 0 && r.conflicts.length === 0, JSON.stringify(r));
check("with no conflict copies made along the way", !(await phone.list()).some((p) => p.includes("conflict")), (await phone.list()).join(", "));

// --- 6. edits, and renames that stay on one side -----------------------------
await mac.write("Salary.encrypted.md", "SECRET-SALARY 6000\n");
await mac.rename("Work/Clients.encrypted/Acme Öy/Contract.md", "Work/Clients.encrypted/Acme Öy/Deal.md");
r = await mac.sync.run();
check("an edit and a rename inside a marked folder push as two changes", r.pushed === 2 && r.refused.length === 0, JSON.stringify(r));
r = await phone.sync.run();
check("both reach the other device", (await phone.read("Salary.encrypted.md")) === "SECRET-SALARY 6000\n" && (await phone.read("Work/Clients.encrypted/Acme Öy/Deal.md")) === "SECRET-CONTRACT terms\n" && !(await phone.exists("Work/Clients.encrypted/Acme Öy/Contract.md")));

// --- 7. renames across the marker ---------------------------------------------
await mac.write("Diary.md", "SECRET-DIARY not yet marked\n");
await mac.sync.run();
check("(a note not yet marked is readable on the server)", (await serverFiles()).get("Diary.md")?.toString() === "SECRET-DIARY not yet marked\n");
await mac.rename("Diary.md", "Diary.encrypted.md");
r = await mac.sync.run();
held = await serverFiles();
check("renaming a note INTO the marker removes the readable file and stores ciphertext", !held.has("Diary.md") && sealed(held.get("Diary.encrypted.md") ?? Buffer.alloc(0)) && r.refused.length === 0, JSON.stringify(r));
check("and is not reported as exposed", !r.exposed);
check("but is reported as marked, so the user can be told its history is still readable", r.marked?.length === 1 && r.marked[0] === "Diary.encrypted.md", JSON.stringify(r));
r = await phone.sync.run();
check("the other device follows the rename", (await phone.read("Diary.encrypted.md")) === "SECRET-DIARY not yet marked\n" && !(await phone.exists("Diary.md")));

await mac.rename("Pin.encrypted.md", "Pin.md");
r = await mac.sync.run();
held = await serverFiles();
check("renaming a note OUT of the marker uploads it readable", held.get("Pin.md")?.toString() === "SECRET-PIN 4711\n" && !held.has("Pin.encrypted.md"));
check("and says so", r.exposed?.length === 1 && r.exposed[0] === "Pin.md" && !r.marked, JSON.stringify(r));
await phone.sync.run();

// A rename into the marker is a delete and a put. If another device edited the
// note in the meantime, the server keeps that edit and refuses the delete, so
// the readable note is still there. That must be said, not passed over.
await mac.write("Memo.md", "SECRET-MEMO first\n");
await mac.sync.run();
await phone.sync.run();
await phone.write("Memo.md", "SECRET-MEMO edited on the phone\n");
await phone.sync.run();
await mac.rename("Memo.md", "Memo.encrypted.md");
r = await mac.sync.run();
const kept = r.refused.find((x) => x.path === "Memo.md");
check("a rename into the marker that loses a race is reported as still readable", kept !== undefined && /^still stored readable/.test(kept.reason ?? "") && kept.reason.includes("Memo.encrypted.md"), JSON.stringify(r.refused));
check("the other device's edit comes back beside the marked copy", (await mac.read("Memo.md")) === "SECRET-MEMO edited on the phone\n" && (await mac.read("Memo.encrypted.md")) === "SECRET-MEMO first\n");
await mac.remove("Memo.md");
await mac.sync.run();
await phone.sync.run();
check("deleting it again then takes", !(await serverFiles()).has("Memo.md") && !(await phone.exists("Memo.md")));

// --- 8. a conflict on a marked note -------------------------------------------
await mac.write("Salary.encrypted.md", "SECRET-SALARY from the mac\n");
await phone.write("Salary.encrypted.md", "SECRET-SALARY from the phone\n");
await mac.sync.run();
r = await phone.sync.run();
check("two devices editing one marked note is a conflict", r.conflicts.length === 1, JSON.stringify(r));
const copies = (await phone.list()).filter((p) => p.includes("conflict"));
check("the copy keeps the marker, so it is an encrypted note too", copies.length === 1 && /^Salary\.conflict-phone-\d{8}T\d{6}-local\.encrypted\.md$/.test(copies[0]), copies.join(", "));
check("both versions survive on the device, readable", (await phone.read("Salary.encrypted.md")) === "SECRET-SALARY from the mac\n" && copies[0] !== undefined && (await phone.read(copies[0])) === "SECRET-SALARY from the phone\n");
r = await phone.sync.run();
held = await serverFiles();
check("the copy is uploaded as ciphertext", r.pushed === 1 && sealed(held.get(copies[0]) ?? Buffer.alloc(0)), JSON.stringify(r));
await mac.sync.run();
check("and reaches the other device", copies[0] !== undefined && (await mac.read(copies[0])) === "SECRET-SALARY from the phone\n");

// --- 9. an ordinary note still merges on the server ----------------------------
await mac.write("Plan.md", "LINE ONE\nline two\nline three\n");
await phone.write("Plan.md", "line one\nline two\nLINE THREE\n");
await mac.sync.run();
r = await phone.sync.run();
check("two devices editing different lines of an ordinary note merge", r.conflicts.length === 0 && (await phone.read("Plan.md")) === "LINE ONE\nline two\nLINE THREE\n", JSON.stringify(r));

// --- 10. history, old versions and deleted files -------------------------------
const reader = mac.client();
const revs = (await reader.history("Salary.encrypted.md")).revisions;
check("a marked note has history", revs.length >= 3, String(revs.length));
const oldest = new TextDecoder().decode(await reader.readAt(revs[revs.length - 1].commit, "Salary.encrypted.md"));
check("an old version reads back decrypted", oldest === "SECRET-SALARY 5000\n", oldest);
const plainOld = new TextDecoder().decode(await reader.readAt((await reader.history("Plan.md")).revisions.at(-1).commit, "Plan.md"));
check("and an ordinary note's old version is read as it is", plainOld === "line one\nline two\nline three\n", plainOld);
check("a marked note cannot be pinned", (await thrown(() => reader.pin("before the raise", "x", "Salary.encrypted.md"))) !== null);
await mac.remove("Work/Clients.encrypted/scan.bin");
await mac.sync.run();
const gone = (await reader.deleted()).map((d) => d.path);
check("deleted files are listed under their real names", gone.includes("Work/Clients.encrypted/scan.bin") && gone.includes("Diary.md"), gone.join(", "));
const keylessView = (await new Client(SERVER, TOKEN, VAULT).deleted()).map((d) => d.path);
check("without the key the same list shows only what the server can name", !keylessView.some((p) => p.includes("scan.bin")), keylessView.join(", "));

// --- 11. re-bootstrap -----------------------------------------------------------
await phone.sync.run();
const before = await phone.list();
r = await phone.sync.forceRebootstrap();
check("re-bootstrapping makes no conflict copies and re-uploads nothing", r.pushed === 0 && (await phone.list()).join() === before.join(), JSON.stringify(r));

// --- 12. a fresh device with the key --------------------------------------------
const laptop = await device("laptop", crypt);
r = await laptop.sync.run();
check("a new device with the key gets the whole vault readable", (await laptop.list()).join() === (await mac.list()).join(), `${(await laptop.list()).join(", ")} vs ${(await mac.list()).join(", ")}`);
check("including what is beneath the marked folder", (await laptop.read("Work/Clients.encrypted/Acme Öy/Deal.md")) === "SECRET-CONTRACT terms\n");

// --- 13. keys that must not be used here -----------------------------------------
const stranger = await device("stranger", await VaultCrypt.fromKey(generateKey(), "files"));
err = await thrown(() => stranger.sync.run());
check("a device with a different key is refused", err?.constructor?.name === "WrongKeyError", String(err));
const confused = await device("confused", await VaultCrypt.fromKey(key, "vault"));
await confused.write("Note.md", "would be sealed under a name the server refuses\n");
err = await thrown(() => confused.sync.run());
check("a whole-vault key is refused on an ordinary vault", err !== null && /encrypted vault/.test(String(err)), String(err));

// --- 14. what the server holds at the end ----------------------------------------
held = await serverFiles();
const marked = [...held.keys()].filter((p) => /\.encrypted(\/|\.[^./]+$)/i.test(p));
check("every marked path on the server is ciphertext", marked.length >= 4 && marked.every((p) => sealed(held.get(p))), marked.join(", "));
for (const text of ["SECRET-SALARY", "SECRET-CONTRACT", "Acme", "Deal.md"]) {
	check(`"${text}" appears nowhere in the vault's history`, !inHistory(text));
}
check("what was readable before it was marked is still in history, as documented", inHistory("SECRET-DIARY"));

console.log(failures === 0 ? "\nall checks passed" : `\n${failures} check(s) FAILED`);
process.exit(failures === 0 ? 0 : 1);
