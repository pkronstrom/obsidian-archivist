import { test } from "node:test";
import assert from "node:assert/strict";
import { localOnly, skip } from "../../dist-test/entry.mjs";
import { DEFAULT_CONFIG_SYNC } from "../../dist-test/entry.mjs";

const plugins = { ...DEFAULT_CONFIG_SYNC, level: "plugins" };

// This table is the same one as internal/vault/localonly_test.go. The two
// predicates must agree: if they drift, one side pushes a file the other
// refuses per-path, and the client rediscovers the same put every cycle.
test("localOnly matches the documented grammar", () => {
	for (const [path, want] of [
		["Scratch.local.md", true],
		["Notes/Machine.local.md", true],
		["Note.ae56b1c.local.md", true],
		["attachments/diagram.8f1c2ad.local.png", true],
		["local.md", false],
		["Note.local", false],
		["Notes/plan.local", false],
		["notes.local/inside.md", false],
		["Note.Local.md", false],
		["Report.LOCAL.md", false],
		["Daily/2026-08-22.md", false],
		["Note.md", false],
	]) {
		assert.equal(localOnly(path), want, path);
	}
});

test("skip applies .local before the dotfile and config rules", () => {
	assert.equal(skip("Scratch.local.md"), true);
	// An allowlisted config path in .local form must still be refused, at the
	// most permissive level -- otherwise the promise has a hole exactly where
	// nobody would look for it.
	assert.equal(skip(".obsidian/appearance.local.json", plugins), true);
	assert.equal(skip(".obsidian/appearance.json", plugins), false);
	assert.equal(skip("Note.md"), false);
});
