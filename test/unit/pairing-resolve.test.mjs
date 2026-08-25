import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Sync } from "../../dist-test/entry.mjs";
import { FakeApp } from "../obsidian-shim.mjs";

/**
 * `entries` is what the server reports as changed since the client's base. It
 * matters for "merge anyway": the pull only leaves the cursor at "" when
 * something DEFERS, and a deferral is what a same-path collision produces. A
 * stub with no entries models a server that is populated and yet collides with
 * nothing, which is not the situation the guard fires in.
 */
function fakeClient({ head = "serverhead", entries = [] } = {}) {
	return {
		pushes: [],
		async index() {
			return { service: "archivist", version: "test", protocol: 2, vault: "personal" };
		},
		async head() {
			return head;
		},
		async changes() {
			return { head, entries };
		},
		async snapshot() {
			return { head, files: {} };
		},
		async missing(hashes) {
			return hashes;
		},
		async putContent() {},
		async push(base, device, changes) {
			this.pushes.push({ base, device, changes });
			return {
				head,
				results: changes.map((c) => ({
					path: c.path,
					status: "applied",
					hash: c.hash,
					size: c.size,
				})),
			};
		},
	};
}

async function device() {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), "resolve-"));
	return { root, app: new FakeApp(root) };
}

test("merge anyway keeps the empty base on a collision, so the server three-way merges", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "mine.md"), "local\n");

	// The server holds a DIFFERENT mine.md. The pull defers it -- the file is
	// modified here too -- which is what keeps the cursor at "" and hands the
	// server the information it needs to merge rather than overwrite.
	const client = fakeClient({
		entries: [{ path: "mine.md", op: "put", hash: "serverhash", size: 7 }],
	});
	const sync = new Sync(app, () => client, () => "mac", () => {});

	await sync.resolvePairing("merge");

	assert.equal(client.pushes.length, 1);
	assert.equal(
		client.pushes[0].base,
		"",
		"merge must keep the empty base so the server three-way merges the collision",
	);
	assert.equal(client.pushes[0].changes.length, 1);
});

test("merge anyway advances the cursor when nothing collides", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "mine.md"), "local\n");
	// Server populated, but nothing it holds clashes with what is here.
	const client = fakeClient({ entries: [] });
	const sync = new Sync(app, () => client, () => "mac", () => {});

	await sync.resolvePairing("merge");

	assert.equal(
		client.pushes[0].base,
		"serverhead",
		"with nothing deferred the pull caught us up, so the push is an ordinary fast-forward",
	);
});

test("publish local pushes against the server head, so local wins every collision", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "mine.md"), "local\n");
	const client = fakeClient({ head: "serverhead" });
	const sync = new Sync(app, () => client, () => "mac", () => {});

	await sync.resolvePairing("publish");

	assert.equal(client.pushes.length, 1);
	assert.equal(
		client.pushes[0].base,
		"serverhead",
		"publish must adopt the server head as base, or the server treats every path as moved and writes conflict files",
	);
});

test("publish local sends no deletes for server-only files", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "mine.md"), "local\n");
	const client = fakeClient();
	const sync = new Sync(app, () => client, () => "mac", () => {});

	await sync.resolvePairing("publish");

	const dels = client.pushes[0].changes.filter((c) => c.op === "del");
	assert.equal(dels.length, 0, "publish must never delete server content");
});

test("resolving is one-shot: a second cycle does not refuse again", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "mine.md"), "local\n");
	const client = fakeClient();
	const sync = new Sync(app, () => client, () => "mac", () => {});

	await sync.resolvePairing("merge");
	await sync.run(); // must not throw
});

test("adopt server moves local files into a dated folder inside the vault", async () => {
	const { root, app } = await device();
	await fs.mkdir(path.join(root, "notes"), { recursive: true });
	await fs.writeFile(path.join(root, "notes/a.md"), "mine\n");
	await fs.writeFile(path.join(root, "top.md"), "also mine\n");

	const client = fakeClient();
	const sync = new Sync(app, () => client, () => "mac", () => {});

	await sync.resolvePairing("adopt");

	const entries = await fs.readdir(root);
	const rescue = entries.find((e) => e.startsWith("_archivist-rescued-"));
	assert.ok(rescue, `no rescue folder in ${JSON.stringify(entries)}`);

	assert.equal(await fs.readFile(path.join(root, rescue, "notes/a.md"), "utf8"), "mine\n");
	assert.equal(await fs.readFile(path.join(root, rescue, "top.md"), "utf8"), "also mine\n");

	// The originals are gone from their old paths, so the server's versions can
	// land there without colliding.
	await assert.rejects(() => fs.readFile(path.join(root, "notes/a.md")));
	await assert.rejects(() => fs.readFile(path.join(root, "top.md")));
});

test("adopt server leaves dotfiles alone", async () => {
	const { root, app } = await device();
	await fs.mkdir(path.join(root, ".obsidian"), { recursive: true });
	await fs.writeFile(path.join(root, ".obsidian/app.json"), "{}\n");
	await fs.writeFile(path.join(root, "note.md"), "mine\n");

	const client = fakeClient();
	const sync = new Sync(app, () => client, () => "mac", () => {});

	await sync.resolvePairing("adopt");

	assert.equal(await fs.readFile(path.join(root, ".obsidian/app.json"), "utf8"), "{}\n");
});

test("adopt server clears the directories it emptied", async () => {
	const { root, app } = await device();
	await fs.mkdir(path.join(root, "notes"), { recursive: true });
	await fs.writeFile(path.join(root, "notes/a.md"), "mine\n");

	const client = fakeClient();
	const sync = new Sync(app, () => client, () => "mac", () => {});
	await sync.resolvePairing("adopt");

	// An empty `notes/` left behind would make the server's own FILE called
	// `notes` unwritable on the following pull.
	await assert.rejects(() => fs.stat(path.join(root, "notes")),
		"the emptied source directory was left behind");
});

test("adopt server does not overwrite an earlier rescue on the same day", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "note.md"), "second adoption\n");
	// A rescue folder from an earlier adoption today, already holding a file.
	const stamp = new Date();
	const p = (n) => String(n).padStart(2, "0");
	const first = `_archivist-rescued-${stamp.getUTCFullYear()}${p(stamp.getUTCMonth() + 1)}${p(stamp.getUTCDate())}`;
	await fs.mkdir(path.join(root, first), { recursive: true });
	await fs.writeFile(path.join(root, first, "note.md"), "first adoption\n");

	const client = fakeClient();
	const sync = new Sync(app, () => client, () => "mac", () => {});
	await sync.resolvePairing("adopt");

	assert.equal(
		await fs.readFile(path.join(root, first, "note.md"), "utf8"),
		"first adoption\n",
		"the second adoption renamed over the first rescue",
	);
	const entries = await fs.readdir(root);
	assert.ok(
		entries.some((e) => e.startsWith(first + "-")),
		`expected a suffixed second rescue folder in ${JSON.stringify(entries)}`,
	);
});

test("adopt server pushes the rescued files, so other devices get them too", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "note.md"), "mine\n");

	const client = fakeClient();
	const sync = new Sync(app, () => client, () => "mac", () => {});

	await sync.resolvePairing("adopt");

	const pushed = client.pushes.flatMap((p) => p.changes.map((c) => c.path));
	assert.ok(
		pushed.some((p) => p.startsWith("_archivist-rescued-")),
		`rescued files were never pushed: ${JSON.stringify(pushed)}`,
	);
});
