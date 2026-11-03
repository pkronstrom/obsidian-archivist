import { test } from "node:test";
import assert from "node:assert/strict";
import { Watcher } from "../../dist-test/entry.mjs";

// A watcher whose sleep is recorded instead of waited, stopped after `rounds`
// waits so the loop terminates.
function harness({ rounds, cursors, wait }) {
	const sleeps = [];
	let waits = 0;
	let i = 0;
	let w;
	const client = {
		wait: async () => {
			if (++waits >= rounds) w.stop();
			if (wait) return wait(waits);
			return { changed: true };
		},
	};
	w = new Watcher(
		() => client,
		() => cursors[Math.min(i, cursors.length - 1)],
		async () => {
			i++;
		},
		() => {},
		60,
		async (ms) => {
			sleeps.push(ms);
		},
	);
	return { w, sleeps };
}

async function run(h) {
	h.w.start();
	// The loop is async; let it drain.
	for (let n = 0; n < 200 && h.w.active; n++) await new Promise((r) => setImmediate(r));
}

test("a sync that leaves the cursor where it was backs off exponentially", async () => {
	// The live failure: every cycle failed, the saved base never moved, so the
	// server answered "changed" at once and the loop ran at network speed.
	const h = harness({ rounds: 12, cursors: ["base"] });
	await run(h);
	assert.deepEqual(h.sleeps.slice(0, 5), [1000, 2000, 4000, 8000, 16000]);
	assert.equal(Math.max(...h.sleeps), 5 * 60_000, "backoff is capped at five minutes");
});

test("a sync that advances the cursor does not back off", async () => {
	const h = harness({ rounds: 5, cursors: ["a", "b", "c", "d", "e", "f"] });
	await run(h);
	assert.deepEqual(h.sleeps, []);
});

test("progress resets the backoff", async () => {
	// Stuck twice, then a cycle that advances, then stuck again: the third
	// stall starts over at one second.
	const h = harness({ rounds: 5, cursors: ["a", "a", "a", "b", "b", "b"] });
	await run(h);
	assert.deepEqual(h.sleeps, [1000, 2000, 1000]);
});
