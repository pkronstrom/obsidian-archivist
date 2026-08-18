import { test } from "node:test";
import assert from "node:assert/strict";
import { Sync } from "../../dist-test/entry.mjs";

/** An adapter whose mkdir is NON-recursive, like Obsidian's own. */
function strictAdapter() {
	const dirs = new Set([""]);
	const files = new Map();
	return {
		dirs,
		files,
		async mkdir(p) {
			const parent = p.split("/").slice(0, -1).join("/");
			if (!dirs.has(parent)) throw new Error(`ENOENT: parent of ${p} does not exist`);
			dirs.add(p);
		},
		async exists(p) {
			return dirs.has(p) || files.has(p);
		},
		async writeBinary(p, data) {
			files.set(p, data);
		},
		async stat(p) {
			if (files.has(p)) return { type: "file", mtime: 1, size: 1 };
			if (dirs.has(p)) return { type: "folder", mtime: 1, size: 0 };
			return null;
		},
		async readBinary(p) {
			return files.get(p);
		},
		async list() {
			return { files: [], folders: [] };
		},
		async remove(p) {
			files.delete(p);
		},
		async rename(from, to) {
			files.set(to, files.get(from));
			files.delete(from);
		},
	};
}

function appWith(adapter) {
	return {
		vault: { adapter },
		loadLocalStorage: () => null,
		saveLocalStorage: () => {},
	};
}

test("mkdirs creates every missing ancestor, not just the last one", async () => {
	const adapter = strictAdapter();
	const sync = new Sync(appWith(adapter), () => null, () => "test", () => {});

	// mkdirs is private in TypeScript; at runtime it is an ordinary method, and
	// exercising it directly is the point of this test.
	await sync.mkdirs("a/b/c/note.md");

	assert.ok(adapter.dirs.has("a"), "a was not created");
	assert.ok(adapter.dirs.has("a/b"), "a/b was not created");
	assert.ok(adapter.dirs.has("a/b/c"), "a/b/c was not created");
});

test("mkdirs is a no-op for a path at the vault root", async () => {
	const adapter = strictAdapter();
	const sync = new Sync(appWith(adapter), () => null, () => "test", () => {});
	await sync.mkdirs("note.md");
	assert.equal(adapter.dirs.size, 1, "nothing should have been created");
});
