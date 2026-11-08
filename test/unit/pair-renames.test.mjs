import { test } from "node:test";
import assert from "node:assert/strict";
import { pairRenames } from "../../dist-test/entry.mjs";

test("a delete and an add of the same content become one move carrying the source hash", () => {
	const out = pairRenames([
		{ path: "a.md", op: "del", hash: "H" },
		{ path: "b.md", op: "put", hash: "H", content: new ArrayBuffer(1) },
		{ path: "c.md", op: "put", hash: "OTHER" },
	]);
	assert.deepEqual(out, [
		{ path: "b.md", op: "move", from: "a.md", hash: "H" },
		{ path: "c.md", op: "put", hash: "OTHER" },
	]);
});

test("two deletes of the same content are ambiguous and left as delete and put", () => {
	const changes = [
		{ path: "a.md", op: "del", hash: "H" },
		{ path: "b.md", op: "del", hash: "H" },
		{ path: "c.md", op: "put", hash: "H" },
	];
	assert.deepEqual(pairRenames(changes), changes);
});

test("two adds of the same content are ambiguous too", () => {
	const changes = [
		{ path: "a.md", op: "del", hash: "H" },
		{ path: "b.md", op: "put", hash: "H" },
		{ path: "c.md", op: "put", hash: "H" },
	];
	assert.deepEqual(pairRenames(changes), changes);
});
