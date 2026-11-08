import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Sync } from "../../dist-test/entry.mjs";
import { FakeApp } from "../obsidian-shim.mjs";
import { FakeServer } from "../fake-server.mjs";

test("re-bootstrap refuses while a cycle is running, so the cycle cannot save over it", async () => {
	const srv = new FakeServer();
	const client = srv.client();
	let release;
	const gate = new Promise((r) => (release = r));
	const index = client.index;
	client.index = async () => {
		await gate;
		return index();
	};
	const root = await fs.mkdtemp(path.join(os.tmpdir(), "rebootstrap-"));
	const sync = new Sync(new FakeApp(root), () => client, () => "mac", () => {});

	const cycle = sync.run();
	await assert.rejects(() => sync.forceRebootstrap(), /a sync is running/);
	release();
	await cycle;

	await sync.forceRebootstrap();
});
