import { test } from "node:test";
import assert from "node:assert/strict";
import { createSyncScheduler } from "../../dist-test/entry.mjs";

// A fake clock, so these assert behaviour rather than wait for it.
function harness({ quietMs, maxWaitMs, run } = {}) {
	let now = 0;
	const timers = [];
	const runs = [];
	const scheduler = createSyncScheduler({
		quietMs: quietMs ?? 20_000,
		maxWaitMs: maxWaitMs ?? 180_000,
		run:
			run ??
			(() => {
				runs.push(now);
			}),
		setTimer: (fn, ms) => {
			const t = { fn, at: now + ms };
			timers.push(t);
			return t;
		},
		clearTimer: (t) => {
			const i = timers.indexOf(t);
			if (i >= 0) timers.splice(i, 1);
		},
	});
	return {
		runs,
		scheduler,
		get pendingTimers() {
			return timers.length;
		},
		advance(ms) {
			now += ms;
			for (const t of [...timers]) {
				if (t.at <= now) {
					const i = timers.indexOf(t);
					if (i >= 0) timers.splice(i, 1);
					t.fn();
				}
			}
		},
	};
}

test("waits for a quiet gap before syncing", () => {
	const h = harness({ quietMs: 20_000 });
	h.scheduler.schedule();
	h.advance(19_000);
	assert.equal(h.runs.length, 0, "synced before the quiet period elapsed");
	h.advance(2_000);
	assert.equal(h.runs.length, 1);
});

test("a later edit restarts the quiet timer", () => {
	// The defining property of a trailing debounce. Without it this is just a
	// fixed delay, and the whole design collapses.
	const h = harness({ quietMs: 20_000 });
	h.scheduler.schedule();
	h.advance(15_000);
	h.scheduler.schedule();
	h.advance(15_000); // 30s since the first edit, only 15s since the last
	assert.equal(h.runs.length, 0, "the quiet timer did not restart on the second edit");
	h.advance(6_000);
	assert.equal(h.runs.length, 1);
});

test("continuous typing still syncs, at the ceiling", () => {
	// The failure this exists to prevent: keep typing forever, never sync.
	// Edits land closer together than the quiet threshold, so only the ceiling
	// can fire.
	const h = harness({ quietMs: 20_000, maxWaitMs: 60_000 });
	for (let i = 0; i < 10; i++) {
		h.scheduler.schedule();
		h.advance(10_000);
	}
	assert.ok(h.runs.length >= 1, "continuous typing never synced");
	assert.ok(
		h.runs[0] <= 60_000,
		`first sync at ${h.runs[0]}ms, want at or before the 60000ms ceiling`,
	);
});

test("the ceiling is measured from the first edit of a burst, not the last", () => {
	// Edits every 10s against a 20s quiet threshold, so the quiet timer never
	// expires and the ceiling is the only thing that can fire. If the ceiling
	// were extended per edit it would never fire either.
	const h = harness({ quietMs: 20_000, maxWaitMs: 60_000 });
	for (let elapsed = 0; elapsed < 60_000; elapsed += 10_000) {
		h.scheduler.schedule();
		h.advance(10_000);
	}
	assert.equal(h.runs.length, 1, "the ceiling did not fire 60s after the burst began");
	assert.equal(h.runs[0], 60_000);
});

test("flush syncs immediately and clears pending timers", async () => {
	const h = harness();
	h.scheduler.schedule();
	await h.scheduler.flush();
	assert.equal(h.runs.length, 1);
	assert.equal(h.pendingTimers, 0, "flush left timers behind");
	h.advance(200_000);
	assert.equal(h.runs.length, 1, "a flushed sync fired again from its timers");
});

test("flush with nothing pending does nothing", async () => {
	const h = harness();
	await h.scheduler.flush();
	assert.equal(h.runs.length, 0, "flush synced with no edits pending");
});

test("a forced flush syncs even with nothing pending", async () => {
	// What blur needs: leaving the window should pull remote changes whether or
	// not this device has edits waiting.
	const h = harness();
	await h.scheduler.flush({ force: true });
	assert.equal(h.runs.length, 1);
});

test("flush resolves only after the sync finishes", async () => {
	let release;
	let finished = false;
	const gate = new Promise((r) => {
		release = r;
	});
	const h = harness({
		run: () => gate.then(() => {
			finished = true;
		}),
	});

	h.scheduler.schedule();
	const flushed = h.scheduler.flush();
	assert.equal(finished, false, "the sync finished before it was released");
	release();
	await flushed;
	assert.equal(finished, true, "flush resolved before the sync completed");
});

test("a failing sync does not wedge the scheduler", async () => {
	let calls = 0;
	const h = harness({
		run: () => {
			calls++;
			return Promise.reject(new Error("network down"));
		},
	});
	h.scheduler.schedule();
	await h.scheduler.flush();
	// A rejection must be swallowed here -- runSync reports failures itself --
	// and must leave the scheduler usable.
	h.scheduler.schedule();
	await h.scheduler.flush();
	assert.equal(calls, 2, "the scheduler stopped syncing after a failure");
});

test("cancel drops pending work", () => {
	const h = harness();
	h.scheduler.schedule();
	h.scheduler.cancel();
	h.advance(200_000);
	assert.equal(h.runs.length, 0);
});

test("a new burst after a sync starts a fresh ceiling", () => {
	const h = harness({ quietMs: 20_000, maxWaitMs: 60_000 });
	h.scheduler.schedule();
	h.advance(21_000);
	assert.equal(h.runs.length, 1);
	h.scheduler.schedule();
	h.advance(21_000);
	assert.equal(h.runs.length, 2);
});
