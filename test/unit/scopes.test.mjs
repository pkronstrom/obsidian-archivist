import { test } from "node:test";
import assert from "node:assert/strict";
import { stepUpWarning, syncableVaults } from "../../dist-test/entry.mjs";

test("a token gated on every vault it opens warns, because the plugin cannot present a code", () => {
	const w = stepUpWarning(["work"], ["work"], "agent");
	assert.match(w, /work/);
	assert.match(w, /cannot/);
});

test("a token that gates one vault can still sync the others", () => {
	assert.equal(stepUpWarning(["personal", "work"], ["work"], "agent"), "");
	assert.deepEqual(syncableVaults(["personal", "work"], ["work"]), ["personal"]);
});

test("no step-up, no warning", () => {
	assert.equal(stepUpWarning(["work"], [], "phone"), "");
	assert.deepEqual(syncableVaults(["work"], []), ["work"]);
});

test("a token that opens nothing is not a step-up problem", () => {
	assert.equal(stepUpWarning([], ["work"], "agent"), "");
});
