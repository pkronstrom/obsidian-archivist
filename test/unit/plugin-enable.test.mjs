import { test } from "node:test";
import assert from "node:assert/strict";
import { installPlugins } from "../../dist-test/entry.mjs";

/**
 * installPlugins writes a freshly-installed plugin's files to disk, then
 * calls Obsidian's undocumented enablePlugin(id). Obsidian's plugin manager
 * only knows about plugins from its last manifest scan, so writing files
 * straight to the adapter does not refresh that cache -- enabling
 * immediately after install silently no-ops unless loadManifests() runs
 * first. This is the exact bug: files land, the plugin never turns on.
 */
function fakeAdapter() {
	const files = new Map();
	const dirs = new Set([""]);
	return {
		files,
		async mkdir(p) {
			dirs.add(p);
		},
		async write(p, data) {
			files.set(p, data);
		},
	};
}

function fakeApp() {
	const calls = [];
	return {
		vault: { adapter: fakeAdapter(), configDir: ".obsidian" },
		plugins: {
			async loadManifests() {
				calls.push("loadManifests");
			},
			async enablePlugin(id) {
				calls.push(`enablePlugin:${id}`);
			},
		},
		calls,
	};
}

function withFetch(handler, fn) {
	const original = global.fetch;
	global.fetch = handler;
	return fn().finally(() => {
		global.fetch = original;
	});
}

test("a freshly installed plugin is enabled only after manifests are reloaded", async () => {
	const app = fakeApp();

	await withFetch(async (url) => {
		const u = String(url);
		if (u.includes("community-plugins.json")) {
			return new Response(JSON.stringify([{ id: "dataview", repo: "blacksmithgu/obsidian-dataview" }]));
		}
		if (u.endsWith("/manifest.json")) {
			return new Response(JSON.stringify({ id: "dataview", isDesktopOnly: false }));
		}
		if (u.endsWith("/main.js")) {
			return new Response("console.log('main')");
		}
		if (u.endsWith("/styles.css")) {
			return new Response("", { status: 404 });
		}
		throw new Error(`unexpected fetch: ${u}`);
	}, async () => {
		const result = await installPlugins(app, ["dataview"], () => {});
		assert.deepEqual(result.installed, ["dataview"]);
		assert.deepEqual(result.failed, []);
	});

	assert.ok(app.vault.adapter.files.has(".obsidian/plugins/dataview/manifest.json"),
		"the manifest was never written");
	assert.ok(app.vault.adapter.files.has(".obsidian/plugins/dataview/main.js"),
		"main.js was never written");

	const loadIdx = app.calls.indexOf("loadManifests");
	const enableIdx = app.calls.indexOf("enablePlugin:dataview");
	assert.ok(loadIdx !== -1, "loadManifests was never called");
	assert.ok(enableIdx !== -1, "enablePlugin was never called");
	assert.ok(loadIdx < enableIdx,
		"loadManifests must run BEFORE enablePlugin, or Obsidian enables against a stale manifest cache");
});
