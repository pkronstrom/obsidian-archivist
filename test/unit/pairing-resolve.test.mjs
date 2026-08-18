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
			return { service: "archivist", version: "test", protocol: 1, vault: "personal" };
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
