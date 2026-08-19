import { test } from "node:test";
import assert from "node:assert/strict";
import { missingPluginScopes, scopeWarning } from "../../dist-test/entry.mjs";

test("a token with both verbs is fine", () => {
	assert.deepEqual(missingPluginScopes(["read", "write"]), []);
	assert.equal(scopeWarning(["read", "write", "delete"]), "");
});

test("a read-only token is named as missing write", () => {
	assert.deepEqual(missingPluginScopes(["read"]), ["write"]);
	assert.match(scopeWarning(["read"], "agent-n8n"), /agent-n8n/);
	assert.match(scopeWarning(["read"]), /missing write/);
});

// The plugin needs read for POST /v1/have, so write alone is not enough. This
// is the case the first version of the warning missed.
test("a write-only token is refused too", () => {
	assert.deepEqual(missingPluginScopes(["write"]), ["read"]);
	assert.match(scopeWarning(["write"]), /missing read/);
});

// An older server predates scopes; absent is not empty.
test("a server that reports no scopes is not treated as holding none", () => {
	assert.deepEqual(missingPluginScopes(undefined), []);
	assert.equal(scopeWarning(undefined), "");
});

test("a token with neither verb names both", () => {
	assert.deepEqual(missingPluginScopes([]), ["read", "write"]);
});
