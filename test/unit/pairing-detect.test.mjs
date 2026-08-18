import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Sync, PairingHazardError } from "../../dist-test/entry.mjs";
import { FakeApp } from "../obsidian-shim.mjs";

/** A Client stub: only the calls the detection path makes. */
function fakeClient({ head = "", vault = "personal" } = {}) {
	return {
		calls: [],
		async index() {
			this.calls.push("index");
			return { service: "archivist", version: "test", protocol: 1, vault };
		},
		async head() {
			this.calls.push("head");
			return head;
		},
		async changes() {
			this.calls.push("changes");
			return { head, entries: [] };
		},
		async snapshot() {
			this.calls.push("snapshot");
			return { head, files: {} };
		},
		async missing(h) {
			return h;
		},
		async putContent() {
			this.calls.push("putContent");
		},
		async push() {
			this.calls.push("push");
			return { head, results: [] };
		},
	};
}

async function device() {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), "pair-"));
	return { root, app: new FakeApp(root) };
}

test("populated device meeting a populated server refuses, touching nothing", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "mine.md"), "local only\n");

	const client = fakeClient({ head: "serverhead" });
	const sync = new Sync(app, () => client, () => "mac", () => {});

	await assert.rejects(() => sync.run(), (err) => {
		assert.ok(err instanceof PairingHazardError, `got ${err}`);
		assert.equal(err.localFiles, 1);
		assert.equal(err.serverHead, "serverhead");
		return true;
	});

	assert.ok(!client.calls.includes("changes"), "it asked for changes before refusing");
	assert.ok(!client.calls.includes("push"), "it pushed before refusing");
	assert.equal(await fs.readFile(path.join(root, "mine.md"), "utf8"), "local only\n");
});

test("an empty device pulls from a populated server without refusing", async () => {
	const { app } = await device();
	const client = fakeClient({ head: "serverhead" });
	const sync = new Sync(app, () => client, () => "mac", () => {});
	const report = await sync.run();
	assert.equal(report.pulled, 0);
	assert.ok(client.calls.includes("changes"), "it never pulled");
});

test("a populated device bootstrapping an empty server does not refuse", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "mine.md"), "local only\n");
	const client = fakeClient({ head: "" });
	const sync = new Sync(app, () => client, () => "mac", () => {});
	await sync.run();
	assert.ok(client.calls.includes("push"), "the bootstrap push never happened");
});

test("an already-synced device is not asked again", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "mine.md"), "local only\n");
	app.saveLocalStorage("archivist.state", {
		base: "oldhead",
		files: { "mine.md": { hash: "x", mtime: 1, size: 1 } },
		vault: "personal",
	});
	const client = fakeClient({ head: "serverhead" });
	const sync = new Sync(app, () => client, () => "mac", () => {});
	await sync.run();
	assert.ok(client.calls.includes("changes"), "an established device was blocked");
});
