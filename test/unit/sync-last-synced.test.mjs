import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Sync } from "../../dist-test/entry.mjs";
import { FakeApp } from "../obsidian-shim.mjs";

function fakeClient({ head = "serverhead" } = {}) {
	return {
		async index() {
			return { service: "archivist", version: "test", protocol: 2, vault: "personal" };
		},
		async head() {
			return head;
		},
		async changes() {
			return { head, entries: [] };
		},
		async snapshot() {
			return { head, files: {} };
		},
		async missing(hashes) {
			return hashes;
		},
		async putContent() {},
		async push(base, device, changes) {
			return {
				head,
				results: changes.map((c) => ({ path: c.path, status: "applied", hash: c.hash, size: c.size })),
			};
		},
	};
}

async function device() {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), "last-synced-"));
	return new FakeApp(root);
}

test("a successful cycle records when it finished", async () => {
	const app = await device();
	const client = fakeClient();
	const sync = new Sync(app, () => client, () => "mac", () => {});

	const before = Date.now();
	await sync.run();
	const after = Date.now();

	const state = app.loadLocalStorage("archivist.state");
	assert.ok(
		state.lastSyncedAt >= before && state.lastSyncedAt <= after,
		`lastSyncedAt ${state.lastSyncedAt} was not between ${before} and ${after}`,
	);
});

test("a cycle that throws before finishing does not record a time", async () => {
	const app = await device();
	const client = {
		async index() {
			throw new Error("server down");
		},
	};
	const sync = new Sync(app, () => client, () => "mac", () => {});

	await assert.rejects(() => sync.run());

	const state = app.loadLocalStorage("archivist.state");
	assert.equal(state, null, "no state should have been saved at all");
});
