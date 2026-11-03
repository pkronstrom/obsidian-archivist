import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Sync, PairingHazardError, compareVaults } from "../../dist-test/entry.mjs";
import { FakeApp } from "../obsidian-shim.mjs";

/** A Client stub: only the calls the detection path makes. */
function fakeClient({ head = "", vault = "personal", files = {} } = {}) {
	return {
		calls: [],
		async index() {
			this.calls.push("index");
			return { service: "archivist", version: "test", protocol: 2, vault };
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
			return { head, files };
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

// Every Obsidian vault has a .obsidian/ directory. Counting it as local content
// would make a brand-new empty vault with config sync enabled refuse to
// onboard -- and the setting to turn config sync off lives behind the sync that
// just refused.
test("config files alone do not trigger the pairing hazard", async () => {
	const { root, app } = await device();
	await fs.mkdir(path.join(root, ".obsidian"), { recursive: true });
	await fs.writeFile(path.join(root, ".obsidian/appearance.json"), '{"theme":"minimal"}\n');

	const client = fakeClient({ head: "serverhead" });
	const sync = new Sync(app, () => client, () => "mac", () => {}, () => ({
		level: "appearance",
		acceptedPlugins: [],
	}));

	await sync.run(); // must not throw
	assert.ok(client.calls.includes("changes"), "an all-config vault was blocked from onboarding");
});

// Re-bootstrap EMPTIES the sync state, which is exactly what the guard fires
// on. Without an exemption the documented escape hatch walks into the prompt
// it exists to bypass -- reported from a real Mac.
test("re-bootstrap does not trip the pairing guard", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "mine.md"), "local\n");
	app.saveLocalStorage("archivist.state", {
		base: "oldhead",
		files: { "mine.md": { hash: "x", mtime: 1, size: 1 } },
		vault: "personal",
	});

	const client = fakeClient({ head: "serverhead" });
	const sync = new Sync(app, () => client, () => "mac", () => {});

	await sync.forceRebootstrap(); // must not throw
	assert.ok(client.calls.includes("snapshot") || client.calls.includes("changes"),
		"re-bootstrap never reached the server");
});

/** The git blob hash, which is what the server reports for each path. */
function blob(text) {
	const body = Buffer.from(text);
	return createHash("sha1").update(`blob ${body.length}\0`).update(body).digest("hex");
}

test("the hazard reports how the two sides compare, without writing", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "same.md"), "same\n");
	await fs.writeFile(path.join(root, "edited.md"), "mine\n");
	await fs.writeFile(path.join(root, "new.md"), "only here\n");

	const client = fakeClient({
		head: "serverhead",
		files: {
			"same.md": { hash: blob("same\n"), size: 5 },
			"edited.md": { hash: blob("theirs\n"), size: 7 },
			"remote.md": { hash: blob("only there\n"), size: 11 },
		},
	});
	const sync = new Sync(app, () => client, () => "mac", () => {});

	await assert.rejects(() => sync.run(), (err) => {
		assert.ok(err instanceof PairingHazardError, `got ${err}`);
		assert.deepEqual(err.comparison, {
			identical: 1,
			onlyHere: ["new.md"],
			onlyServer: ["remote.md"],
			differ: ["edited.md"],
		});
		return true;
	});
	assert.ok(!client.calls.includes("push"), "it pushed before refusing");
	assert.ok(!client.calls.includes("putContent"), "it uploaded before refusing");
});

test("paths compare in NFC, so a decomposed Mac name matches the server's", () => {
	const nfd = "Päivä.md".normalize("NFD");
	const c = compareVaults({ [nfd]: "h" }, { ["Päivä.md".normalize("NFC")]: "h" });
	assert.deepEqual(c, { identical: 1, onlyHere: [], onlyServer: [], differ: [] });
});

// Reported from a real Mac: re-bootstrap's first sync failed (a quarantined
// path refused the push), the empty state it had saved survived, and after a
// restart the device asked the first-connect question about its own vault.
test("a re-bootstrap whose sync fails does not leave the device looking never-synced", async () => {
	const { root, app } = await device();
	await fs.writeFile(path.join(root, "same.md"), "same\n");
	await fs.writeFile(path.join(root, "new.md"), "only here\n");
	app.saveLocalStorage("archivist.state", {
		base: "oldhead",
		files: { "same.md": { hash: "x", mtime: 1, size: 1 } },
		vault: "personal",
	});

	const client = fakeClient({
		head: "serverhead",
		files: { "same.md": { hash: blob("same\n"), size: 5 } },
	});
	client.push = async () => {
		throw new Error("path is quarantined after repeated writes");
	};
	await assert.rejects(() => new Sync(app, () => client, () => "mac", () => {}).forceRebootstrap());

	// A fresh Sync is what a restart gives you: no in-memory decisions.
	const restarted = new Sync(app, () => client, () => "mac", () => {});
	await assert.rejects(() => restarted.run(), (err) => {
		assert.ok(!(err instanceof PairingHazardError), "a restart asked the pairing question");
		return true;
	});
});
