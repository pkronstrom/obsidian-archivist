import { test } from "node:test";
import assert from "node:assert/strict";
import { requireCurrentIndex } from "../../dist-test/entry.mjs";

test("the plugin refuses a different or incomplete protocol", () => {
	assert.throws(
		() => requireCurrentIndex({ protocol: 1, version: "old", vault: "personal" }),
		/protocol 2/,
	);
	assert.throws(
		() => requireCurrentIndex({ protocol: 2, version: "test", vault: "" }),
		/vault identity/,
	);
	assert.throws(
		() => requireCurrentIndex({ service: "archivist", protocol: 2, version: "test", vault: 7 }),
		/current index metadata/,
	);
	assert.throws(
		() => requireCurrentIndex({ protocol: 2, version: "test", vault: "personal" }),
		/current index metadata/,
	);
});

const base = { service: "archivist", protocol: 2, version: "v0.20.0", vault: "personal" };

test("a server from before encryption reads as one without those features", () => {
	// What 0.20 sends: no features, no encrypted, no keyCheck. It is older,
	// not broken, and an ordinary vault must keep syncing with it.
	const idx = requireCurrentIndex(base);
	assert.deepEqual(idx.features, []);
	assert.equal(idx.encrypted, false);
	assert.equal(idx.keyCheck, "");
	// The sync engine checks an index it was already handed.
	assert.deepEqual(requireCurrentIndex(idx), idx);
});

test("a current server's index is taken as sent", () => {
	const sent = { ...base, features: ["encrypted-vaults", "encrypted-files"], encrypted: true, keyCheck: "ab" };
	assert.deepEqual(requireCurrentIndex(sent), sent);
	// A feature list that is not one is no feature list.
	assert.deepEqual(requireCurrentIndex({ ...base, features: "all" }).features, []);
});
