import { test } from "node:test";
import assert from "node:assert/strict";
import { PairingHazardError, rescueFolder } from "../../dist-test/entry.mjs";

test("rescueFolder is dated", () => {
	const name = rescueFolder(new Date(Date.UTC(2026, 7, 18, 14, 8, 0)));
	assert.equal(name, "_archivist-rescued-20260818");
});

test("rescueFolder takes a suffix, so a same-day retry cannot overwrite the first rescue", () => {
	const day = new Date(Date.UTC(2026, 7, 18));
	assert.equal(rescueFolder(day, 1), "_archivist-rescued-20260818-1");
	assert.notEqual(rescueFolder(day, 1), rescueFolder(day, 0));
});

test("rescueFolder does not start with a dot, so the rescued files sync", () => {
	const name = rescueFolder(new Date(Date.UTC(2026, 7, 18)));
	assert.ok(!name.startsWith("."), "a dot-prefixed folder would be excluded from sync");
});

test("the hazard error names both sides", () => {
	const err = new PairingHazardError(42, "abc123");
	assert.equal(err.name, "PairingHazardError");
	assert.equal(err.localFiles, 42);
	assert.equal(err.serverHead, "abc123");
	assert.match(err.message, /42/);
	assert.match(err.message, /Adopt server/);
	assert.match(err.message, /Publish local/);
	assert.match(err.message, /Merge anyway/);
});
