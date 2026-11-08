import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Sync } from "../../dist-test/entry.mjs";
import { FakeApp } from "../obsidian-shim.mjs";
import { FakeServer } from "../fake-server.mjs";

async function device(srv) {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), "refused-"));
	const sync = new Sync(new FakeApp(root), () => srv.client(), () => "mac", () => {});
	const read = (p) => fs.readFile(path.join(root, p), "utf8").catch(() => undefined);
	return { root, sync, read };
}

async function seeded(srv, file, text) {
	const mac = await device(srv);
	await fs.mkdir(path.join(mac.root, "notes"));
	await fs.writeFile(path.join(mac.root, file), text);
	await mac.sync.run();
	assert.equal(srv.text(file), text);
	return mac;
}

test("a delete refused because another device edited the note restores the edit and never lands", async () => {
	const srv = new FakeServer();
	const mac = await seeded(srv, "notes/a.md", "v1\n");

	srv.edit("notes/a.md", "v2 from the phone\n");
	await fs.rm(path.join(mac.root, "notes/a.md"));

	const first = await mac.sync.run();
	assert.equal(first.refused.length, 1, "the delete is refused, as designed");
	assert.equal(await mac.read("notes/a.md"), "v2 from the phone\n", "and the phone's edit comes back here");

	// The bug: the cursor had moved past the edit, so this cycle resent the
	// delete against a base where nothing had changed, and it landed.
	const second = await mac.sync.run();
	assert.deepEqual([second.pushed, second.refused.length], [0, 0], JSON.stringify(second));
	assert.equal(srv.text("notes/a.md"), "v2 from the phone\n", "the edit is still on the server");
});

test("a rename refused because another device edited the note is undone here, not retried for ever", async () => {
	const srv = new FakeServer();
	const mac = await seeded(srv, "notes/m.md", "m1\n");

	srv.edit("notes/m.md", "m2 from the phone\n");
	await fs.rename(path.join(mac.root, "notes/m.md"), path.join(mac.root, "notes/n.md"));

	const first = await mac.sync.run();
	assert.equal(first.refused.length, 1, "the move is refused, as designed");
	assert.equal(await mac.read("notes/m.md"), "m2 from the phone\n", "the phone's edit arrives at the old name");
	assert.equal(await mac.read("notes/n.md"), undefined, "the renamed copy held only the old version, which history keeps");

	const second = await mac.sync.run();
	assert.deepEqual([second.pushed, second.refused.length], [0, 0], JSON.stringify(second));
	assert.deepEqual(Object.keys(srv.files()), ["notes/m.md"]);
});

test("a renamed file edited after the push is left alone and sent as a new note", async () => {
	const srv = new FakeServer();
	const mac = await seeded(srv, "notes/m.md", "m1\n");

	srv.edit("notes/m.md", "m2 from the phone\n");
	await fs.rename(path.join(mac.root, "notes/m.md"), path.join(mac.root, "notes/n.md"));

	// Edit the renamed file while the push is in flight.
	const client = srv.client();
	const push = client.push;
	client.push = async (...args) => {
		await fs.writeFile(path.join(mac.root, "notes/n.md"), "edited after the rename\n");
		return push(...args);
	};
	const racing = new Sync(mac.sync.app, () => client, () => "mac", () => {});
	await racing.run();

	assert.equal(await mac.read("notes/n.md"), "edited after the rename\n", "an edit made here is never discarded");
	await mac.sync.run();
	assert.equal(srv.text("notes/n.md"), "edited after the rename\n");
	assert.equal(srv.text("notes/m.md"), "m2 from the phone\n");
});
