import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";
import {
	FakeApp,
} from "../obsidian-shim.mjs";
import {
	VaultCrypt,
	conflictName,
	encryptedDir,
	encryptedPath,
	generateKey,
	keyDeclined,
	loadVaultKey,
	pairRenames,
	saveVaultKey,
	setKeyDeclined,
} from "../../dist-test/entry.mjs";

// The table the Go rule is tested against, in protocol/marked_test.go. The
// rule exists twice, and a path the two disagree about is one the plugin
// sends readable and the server stores.
const table = JSON.parse(fs.readFileSync(new URL("../../protocol/testdata/marked-paths.json", import.meta.url), "utf8"));

test("the marker rule matches the table shared with the server", () => {
	assert.ok(table.length > 0);
	for (const c of table) assert.equal(encryptedPath(c.path), c.marked, c.path);
});

test("a folder is marked only when its own name ends in the marker", () => {
	assert.equal(encryptedDir("Clients.encrypted"), true);
	assert.equal(encryptedDir("Work/Clients.encrypted/Acme"), true);
	assert.equal(encryptedDir("Clients.ENCRYPTED"), true);
	// The file spelling, on a folder, is a coincidence and not a mark.
	assert.equal(encryptedDir("Old.encrypted.notes"), false);
	assert.equal(encryptedDir("Clients"), false);
});

test("a files key covers marked paths and nothing else", async () => {
	const crypt = await VaultCrypt.fromKey(generateKey(), "files");
	assert.equal(crypt.covers("Salary.encrypted.md"), true);
	assert.equal(crypt.covers("Clients.encrypted/Acme/Contract.md"), true);
	assert.equal(crypt.covers("Notes/Plan.md"), false);
	const whole = await VaultCrypt.fromKey(generateKey());
	assert.equal(whole.scope, "vault");
	assert.equal(whole.covers("Notes/Plan.md"), true);
});

test("a files key leaves unmarked paths and marked names as they are", async () => {
	const crypt = await VaultCrypt.fromKey(generateKey(), "files");
	for (const p of ["Notes/Plan.md", "Salary.encrypted.md", "Work/Salary.encrypted.md"]) {
		assert.equal(crypt.encryptPath(p), p);
		assert.equal(crypt.decryptPath(p), p);
	}
});

test("beneath a marked folder every name is encrypted, and the folder's is kept", async () => {
	const crypt = await VaultCrypt.fromKey(generateKey(), "files");
	const local = "Work/Clients.encrypted/Acme Öy/Contract 2026.md";
	const sent = crypt.encryptPath(local);
	assert.match(sent, /^Work\/Clients\.encrypted\/[a-z2-7]+\/[a-z2-7]+$/);
	assert.ok(!sent.includes("Acme") && !sent.includes("Contract"));
	assert.equal(crypt.decryptPath(sent), local.normalize("NFC"));
	// The server sees the same rule give the same answer on what it stores.
	assert.equal(encryptedPath(sent), true);
	// A second key cannot read the names.
	const other = await VaultCrypt.fromKey(generateKey(), "files");
	assert.throws(() => other.decryptPath(sent));
});

test("what a files key sends is what the server's path check accepts", async () => {
	const crypt = await VaultCrypt.fromKey(generateKey(), "files");
	const valid = (p) => {
		// protocol.ValidMarkedPath, restated: nothing after a marked file,
		// base32 segments after a marked folder.
		const seg = p.split("/");
		const at = seg.findIndex((s) => /.\.encrypted$/i.test(s));
		if (at < 0) return /.\.encrypted\.[^.]+$/i.test(seg[seg.length - 1]);
		return seg.slice(at + 1).every((s) => /^[a-z2-7]+$/.test(s));
	};
	for (const c of table.filter((c) => c.valid)) assert.equal(valid(c.path), true, c.path);
	for (const p of ["A.encrypted/b.md", "X/Y.encrypted/deep/er/note.md", "Z.encrypted/Inner.encrypted/n.md", "Solo.encrypted.md"]) {
		assert.equal(valid(crypt.encryptPath(p)), true, p);
	}
});

test("a conflict copy of an encrypted note is itself an encrypted note", async () => {
	const crypt = await VaultCrypt.fromKey(generateKey(), "files");
	for (const p of [
		"Salary.encrypted.md",
		"Work/Salary.encrypted.md",
		"Salary.Encrypted.md",
		"photo.encrypted.png",
		"notes.encrypted",
		"Clients.encrypted/Acme/Contract.md",
		"Clients.encrypted/README",
	]) {
		const copy = conflictName(p, "my mac");
		assert.notEqual(copy, p);
		assert.equal(crypt.covers(copy), true, `${p} -> ${copy} would be uploaded readable`);
		assert.equal(copy.split("/").length, p.split("/").length, "the copy stays beside the note");
	}
	assert.match(conflictName("Salary.encrypted.md", "mac"), /^Salary\.conflict-mac-\d{8}T\d{6}-local\.encrypted\.md$/);
});

test("a conflict copy of an ordinary note is named as before", () => {
	assert.match(conflictName("Notes/Plan.md", "mac"), /^Notes\/Plan\.conflict-mac-\d{8}T\d{6}-local\.md$/);
	assert.match(conflictName("Makefile", "mac"), /^Makefile\.conflict-mac-\d{8}T\d{6}-local$/);
	assert.equal(encryptedPath(conflictName("Notes/Plan.md", "mac")), false);
});

test("a rename across the marker stays a delete and a put, each naming the other", () => {
	const movable = (from, to) => encryptedPath(from) === encryptedPath(to);
	const crossing = pairRenames(
		[
			{ path: "Salary.md", op: "del", hash: "h1" },
			{ path: "Salary.encrypted.md", op: "put", hash: "h1" },
		],
		movable,
	);
	assert.deepEqual(crossing.map((c) => c.op), ["del", "put"]);
	assert.equal(crossing[0].becomes, "Salary.encrypted.md");
	assert.equal(crossing[1].was, "Salary.md");

	const within = pairRenames(
		[
			{ path: "A.encrypted/x.md", op: "del", hash: "h2" },
			{ path: "B.encrypted/x.md", op: "put", hash: "h2" },
		],
		movable,
	);
	assert.deepEqual(within, [{ path: "B.encrypted/x.md", op: "move", from: "A.encrypted/x.md", hash: "h2" }]);
});

test("a stored key keeps its scope, and a key from before scopes is a vault key", () => {
	const app = new FakeApp("/nowhere");
	assert.equal(loadVaultKey(app, "personal"), null);
	saveVaultKey(app, "personal", "ab".repeat(32), "files");
	saveVaultKey(app, "work", "cd".repeat(32), "vault");
	assert.deepEqual(loadVaultKey(app, "personal"), { key: "ab".repeat(32), scope: "files" });
	assert.deepEqual(loadVaultKey(app, "work"), { key: "cd".repeat(32), scope: "vault" });
	app.saveLocalStorage("archivist.vaultKeys", { old: "ef".repeat(32) });
	assert.deepEqual(loadVaultKey(app, "old"), { key: "ef".repeat(32), scope: "vault" });
});

test("declining the key is remembered per vault and can be undone", () => {
	const app = new FakeApp("/nowhere");
	assert.equal(keyDeclined(app, "personal"), false);
	setKeyDeclined(app, "personal", true);
	setKeyDeclined(app, "personal", true);
	assert.equal(keyDeclined(app, "personal"), true);
	assert.equal(keyDeclined(app, "other"), false);
	setKeyDeclined(app, "personal", false);
	assert.equal(keyDeclined(app, "personal"), false);
});
