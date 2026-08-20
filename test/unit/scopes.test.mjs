import { test } from "node:test";
import assert from "node:assert/strict";
import { stepUpWarning } from "../../dist-test/entry.mjs";

test("a vault posture warns, because the plugin cannot present a code", () => {
	const w = stepUpWarning(["work"], ["vault:work"], "agent");
	assert.match(w, /work/);
	assert.match(w, /cannot/);
});

test("an ops posture does not warn", () => {
	// Triggering a destructive operation from the plugin and confirming it with
	// a code is a feature, not a misconfiguration.
	assert.equal(stepUpWarning(["work"], ["ops:work"], "mac"), "");
});

test("a posture on a vault the server does not protect does not warn", () => {
	assert.equal(stepUpWarning([], ["vault:work"], "agent"), "");
});

test("no posture, no warning, even on a protected vault", () => {
	assert.equal(stepUpWarning(["work"], [], "phone"), "");
});

test("an older server sends neither field", () => {
	// Absent is not 'gated on everything', which would refuse every device on a
	// server that predates step-up.
	assert.equal(stepUpWarning(undefined, undefined, "phone"), "");
});
