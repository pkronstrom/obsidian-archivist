import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

/**
 * Source-level guard, in the spirit of the tzdata import tests under cmd/.
 *
 * Every test in sync-schedule.test.mjs exercises the scheduler directly through
 * the harness bundle. All of them pass while main.ts still uses the old
 * `debounce(fn, 2000, true)` and the scheduler is dead code -- which is exactly
 * the failure that would reintroduce 250-commit drafting sessions while the
 * suite stayed green.
 *
 * These assertions are about wiring, so they read the source rather than the
 * behaviour.
 */
const main = readFileSync(new URL("../../src/main.ts", import.meta.url), "utf8");

test("main.ts drives sync through the scheduler", () => {
	assert.match(
		main,
		/createSyncScheduler\(/,
		"main.ts does not construct a sync scheduler; the scheduler is dead code",
	);
	assert.match(
		main,
		/scheduleSync\s*=\s*\(\)[^\n]*syncScheduler\.schedule\(\)/,
		"scheduleSync no longer delegates to the scheduler",
	);
});

test("the old fixed debounce is gone", () => {
	assert.doesNotMatch(
		main,
		/debounce\(\s*\(\)\s*=>\s*void this\.runSync\(\)/,
		"main.ts still debounces runSync directly, so the ceiling never applies",
	);
});

test("blur flushes rather than syncing a second time", () => {
	// The blur handler used to call runSync unconditionally. Doing both queues
	// two complete sync cycles, since Sync.run coalesces rather than drops.
	const blur = main.slice(main.indexOf('"blur"'), main.indexOf('"focus"'));
	assert.match(blur, /syncScheduler\.flush\(\{\s*force:\s*true\s*\}\)/, "blur does not force a flush");
	assert.doesNotMatch(blur, /this\.runSync\(\)/, "blur still calls runSync as well as flushing");
});

test("the quit hook returns the flush promise", () => {
	// Obsidian awaits what the task returns. Not returning it lets the sync run
	// detached, which is the same as not having the hook.
	assert.match(
		main,
		/tasks\.add\(\(\)\s*=>\s*this\.syncScheduler\.flush\(\)\)/,
		"the quit task does not return the flush promise, so Obsidian will not wait for it",
	);
});

test("unload cancels pending timers", () => {
	const unload = main.slice(main.indexOf("onunload()"));
	assert.match(unload, /syncScheduler\.cancel\(\)/, "onunload leaves timers pending");
});
