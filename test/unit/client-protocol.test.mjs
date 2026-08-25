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
