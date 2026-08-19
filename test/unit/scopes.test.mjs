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

import { pairRenames } from "../../dist-test/entry.mjs";

test("a delete and an add of identical content become one move", () => {
	const out = pairRenames(
		[
			{ path: "old.md", op: "del", hash: "h1" },
			{ path: "new.md", op: "put", hash: "h1", content: new ArrayBuffer(1) },
		],
		2,
	);
	assert.deepEqual(out, [{ path: "new.md", op: "move", from: "old.md", hash: "h1" }]);
});

test("unrelated changes pass through untouched", () => {
	const input = [
		{ path: "a.md", op: "put", hash: "h1" },
		{ path: "b.md", op: "del", hash: "h2" },
	];
	assert.deepEqual(pairRenames(input, 2), input);
});

// Guessing which of two identical deletions became the addition would move the
// wrong file, so an ambiguous group stays as del+put.
test("two deletions of the same content are left alone", () => {
	const input = [
		{ path: "x.md", op: "del", hash: "h1" },
		{ path: "y.md", op: "del", hash: "h1" },
		{ path: "z.md", op: "put", hash: "h1" },
	];
	assert.deepEqual(pairRenames(input, 2), input);
});

test("a delete with no matching add stays a delete", () => {
	const input = [{ path: "gone.md", op: "del", hash: "h1" }];
	assert.deepEqual(pairRenames(input, 2), input);
});

test("a move carries no content, because the server already has it", () => {
	const [m] = pairRenames(
		[
			{ path: "old.md", op: "del", hash: "h1" },
			{ path: "new.md", op: "put", hash: "h1", content: new ArrayBuffer(8) },
		],
		2,
	);
	assert.equal(m.content, undefined);
	// hash IS carried: it is If-Match for the rename.
	assert.equal(m.hash, "h1");
});

// A server that predates move rejects the unknown op and fails the WHOLE push,
// so a rename there must stay del+put rather than take the cycle down.
test("an older server gets del+put, not move", () => {
	const input = [
		{ path: "old.md", op: "del", hash: "h1" },
		{ path: "new.md", op: "put", hash: "h1" },
	];
	assert.deepEqual(pairRenames(input, 1), input);
	assert.deepEqual(pairRenames(input, 0), input);
	assert.equal(pairRenames(input, 2).length, 1);
});
