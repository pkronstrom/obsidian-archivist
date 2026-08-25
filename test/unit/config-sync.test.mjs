import { test } from "node:test";
import assert from "node:assert/strict";
import { configSyncable, DEFAULT_CONFIG_SYNC, acceptedOnlyByDefault } from "../../dist-test/entry.mjs";
import { skip } from "../../dist-test/entry.mjs";

const files = { ...DEFAULT_CONFIG_SYNC, level: "files" };
const appearance = { ...DEFAULT_CONFIG_SYNC, level: "appearance" };
const plugins = { ...DEFAULT_CONFIG_SYNC, level: "plugins" };

test("files only syncs no config at all, which is today's behaviour", () => {
	assert.equal(configSyncable(".obsidian/appearance.json", files), false);
	assert.equal(configSyncable(".obsidian/snippets/dark.css", files), false);
});

test("appearance carries look and feel but not the plugin list", () => {
	for (const p of [
		".obsidian/appearance.json",
		".obsidian/app.json",
		".obsidian/hotkeys.json",
		".obsidian/snippets/dark.css",
		".obsidian/themes/Minimal/theme.css",
	]) {
		assert.equal(configSyncable(p, appearance), true, p);
	}
	assert.equal(configSyncable(".obsidian/community-plugins.json", appearance), false);
	assert.equal(configSyncable(".obsidian/core-plugins.json", appearance), false);
});

test("plugins adds the lists, and everything appearance had", () => {
	assert.equal(configSyncable(".obsidian/community-plugins.json", plugins), true);
	assert.equal(configSyncable(".obsidian/core-plugins.json", plugins), true);
	assert.equal(configSyncable(".obsidian/appearance.json", plugins), true);
});

test("plugin data.json is off until that plugin is opted in", () => {
	assert.equal(configSyncable(".obsidian/plugins/dataview/data.json", plugins), false);
	const opted = { ...plugins, acceptedPlugins: ["dataview"] };
	assert.equal(configSyncable(".obsidian/plugins/dataview/data.json", opted), true);
});

test("archivist's own data.json cannot be opted in", () => {
	const opted = { ...plugins, acceptedPlugins: ["archivist", "obsidian-archivist"] };
	assert.equal(configSyncable(".obsidian/plugins/archivist/data.json", opted), false);
	assert.equal(configSyncable(".obsidian/plugins/obsidian-archivist/data.json", opted), true);
});

test("nothing outside the allowlist syncs at any level", () => {
	const opted = { ...plugins, acceptedPlugins: ["dataview"] };
	for (const p of [
		".obsidian/workspace.json",
		".obsidian/workspace-mobile.json",
		".obsidian/graph.json",
		".obsidian/plugins/dataview/main.js",
		".obsidian/plugins/dataview/manifest.json",
		".obsidian/types.json",
		".smart-env/cache.json",
		".trash/gone.md",
	]) {
		assert.equal(configSyncable(p, opted), false, p);
	}
});

test("skip defaults to today's behaviour, so callers that pass no level are unchanged", () => {
	assert.equal(skip(".obsidian/appearance.json"), true);
	assert.equal(skip("notes/idea.md"), false);
});

test("skip honours the level when given one", () => {
	assert.equal(skip(".obsidian/appearance.json", appearance), false);
	assert.equal(skip(".obsidian/workspace.json", appearance), true);
	assert.equal(skip("notes/idea.md", appearance), false);
});

import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Sync } from "../../dist-test/entry.mjs";
import { FakeApp } from "../obsidian-shim.mjs";

test("turning the level DOWN does not delete the config off the server", async () => {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), "downgrade-"));
	const app = new FakeApp(root);

	// This device synced config at the appearance level, so its snapshot holds
	// the config paths.
	app.saveLocalStorage("archivist.state", {
		base: "serverhead",
		vault: "personal",
		files: {
			"notes/a.md": { hash: "h1", mtime: 1, size: 1 },
			".obsidian/appearance.json": { hash: "h2", mtime: 1, size: 1 },
			".obsidian/snippets/dark.css": { hash: "h3", mtime: 1, size: 1 },
		},
	});
	await fs.mkdir(path.join(root, "notes"), { recursive: true });
	await fs.writeFile(path.join(root, "notes/a.md"), "still here\n");

	// Now it is set back to Files only.
	const level = { ...DEFAULT_CONFIG_SYNC, level: "files" };
	const pushes = [];
	const client = {
		async index() {
			return { service: "archivist", version: "t", protocol: 2, vault: "personal" };
		},
		async head() {
			return "serverhead";
		},
		async changes() {
			return { head: "serverhead", entries: [] };
		},
		async missing(h) {
			return h;
		},
		async putContent() {},
		async push(base, device, changes) {
			pushes.push(...changes);
			return { head: "serverhead", results: [] };
		},
	};
	const sync = new Sync(app, () => client, () => "mac", () => {}, () => level);
	await sync.run();

	const deleted = pushes.filter((c) => c.op === "del").map((c) => c.path);
	assert.deepEqual(
		deleted.filter((p) => p.startsWith(".obsidian/")),
		[],
		"downgrading the level pushed deletes for config, which would wipe it for every device",
	);
});

test("accept-all syncs every plugin's data.json", () => {
	const all = { ...DEFAULT_CONFIG_SYNC, level: "plugins", acceptAllPlugins: true };
	assert.equal(configSyncable(".obsidian/plugins/dataview/data.json", all), true);
	assert.equal(configSyncable(".obsidian/plugins/templater/data.json", all), true);
});

test("accept-all still cannot sync archivist's own data.json", () => {
	const all = {
		...DEFAULT_CONFIG_SYNC,
		level: "plugins",
		acceptAllPlugins: true,
		acceptedPlugins: ["archivist", "obsidian-archivist"],
	};
	assert.equal(configSyncable(".obsidian/plugins/archivist/data.json", all), false);
	assert.equal(configSyncable(".obsidian/plugins/obsidian-archivist/data.json", all), true);
});

test("accept-all does not widen anything beyond plugin data", () => {
	const all = { ...DEFAULT_CONFIG_SYNC, level: "plugins", acceptAllPlugins: true };
	for (const p of [
		".obsidian/workspace.json",
		".obsidian/plugins/dataview/main.js",
		".obsidian/plugins/dataview/manifest.json",
		".obsidian/graph.json",
	]) {
		assert.equal(configSyncable(p, all), false, p);
	}
});

test("accept-all is inert below the plugins level", () => {
	const appearanceAll = { ...DEFAULT_CONFIG_SYNC, level: "appearance", acceptAllPlugins: true };
	assert.equal(configSyncable(".obsidian/plugins/dataview/data.json", appearanceAll), false);
});

// The distinction enforcement rests on: an explicit per-plugin opt-in is a
// decision someone made having read what the scanner found, so it must keep
// working. Accept-all is a blanket default, and a blanket default must not be
// able to push a credential nobody looked at.
test("acceptedOnlyByDefault separates a blanket default from a real decision", () => {
	const dataPath = ".obsidian/plugins/some-plugin/data.json";
	const all = { ...DEFAULT_CONFIG_SYNC, level: "plugins", acceptAllPlugins: true };
	const named = {
		...DEFAULT_CONFIG_SYNC,
		level: "plugins",
		acceptedPlugins: ["some-plugin"],
	};

	assert.equal(acceptedOnlyByDefault(dataPath, all), true, "blanket default: needs scanning");
	assert.equal(
		acceptedOnlyByDefault(dataPath, named),
		false,
		"explicitly enabled: the user already decided",
	);
	assert.equal(
		acceptedOnlyByDefault(dataPath, { ...all, acceptedPlugins: ["some-plugin"] }),
		false,
		"explicit opt-in wins even with accept-all on",
	);
	// Not a plugin data.json at all.
	assert.equal(acceptedOnlyByDefault(".obsidian/appearance.json", all), false);
	assert.equal(acceptedOnlyByDefault("Note.md", all), false);
	// Nested files under a plugin are not its settings.
	assert.equal(acceptedOnlyByDefault(".obsidian/plugins/p/sub/data.json", all), false);
});
