import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import { FakeApp } from "../obsidian-shim.mjs";
import {
	SwitchRefused,
	beginSwitch,
	finishSwitch,
	loadState,
	loadSwitchBackup,
	planSwitch,
	saveState,
	undoSwitch,
} from "../../dist-test/entry.mjs";

const synced = { base: "abc", vault: "work", files: { "a.md": { hash: "h", mtime: 1, size: 1 } } };
const clean = { pulled: 0, pushed: 0, conflicts: [], refused: [], rebootstrapped: false };

/** A target vault as planSwitch sees it: an index and a head. */
function target({ vault = "work-enc", encrypted = true, head = "", fail } = {}) {
	return {
		async index() {
			if (fail) throw new Error(fail);
			return { service: "archivist", version: "t", protocol: 2, vault, encrypted, keyCheck: "" };
		},
		async head() {
			return head;
		},
	};
}

function plan(over = {}) {
	return planSwitch({
		state: synced,
		current: {},
		target: target(),
		syncCurrent: async () => clean,
		...over,
	});
}

async function refusal(promise) {
	try {
		await promise;
	} catch (err) {
		assert.ok(err instanceof SwitchRefused, `want SwitchRefused, got ${err}`);
		return err;
	}
	assert.fail("the switch was allowed");
}

test("a synced folder may switch to a new, empty vault", async () => {
	const p = await plan();
	assert.deepEqual(p.from, { vault: "work", encrypted: false });
	assert.deepEqual(p.to, { vault: "work-enc", encrypted: true });
	assert.equal(p.exposes, false);
	assert.equal(p.unsynced, undefined);
});

test("an unreachable target is refused before the old vault is touched", async () => {
	let syncs = 0;
	const err = await refusal(
		plan({ target: target({ fail: "401 unauthorized" }), syncCurrent: async () => (syncs++, clean) }),
	);
	assert.match(err.message, /cannot be reached/);
	assert.equal(err.overridable, false);
	assert.equal(syncs, 0);
});

test("a vault that already has content is refused, and cannot be overridden", async () => {
	const err = await refusal(plan({ target: target({ head: "deadbeef" }), skipSync: true }));
	assert.match(err.message, /already has content/);
	assert.equal(err.overridable, false);
});

test("switching to the vault already in use is refused", async () => {
	const err = await refusal(plan({ target: target({ vault: "work" }) }));
	assert.match(err.message, /already syncs/);
});

test("the old vault must sync first, so nothing unpulled is left behind", async () => {
	const err = await refusal(
		plan({
			syncCurrent: async () => {
				throw new Error("server unreachable");
			},
		}),
	);
	assert.match(err.message, /could not be synced first/);
	assert.equal(err.overridable, true);
});

test("a last sync that left conflicts or refusals blocks the switch", async () => {
	const dirty = { ...clean, conflicts: [{ path: "a.md", status: "conflict" }] };
	const err = await refusal(plan({ syncCurrent: async () => dirty }));
	assert.match(err.message, /1 conflict/);
	assert.equal(err.overridable, true);
});

test("the override is recorded in the plan, so the confirmation can say so", async () => {
	let syncs = 0;
	const p = await plan({ skipSync: true, syncCurrent: async () => (syncs++, clean) });
	assert.ok(p.unsynced);
	assert.equal(syncs, 0);
});

test("encrypted to ordinary is flagged as exposing the notes", async () => {
	const p = await plan({
		state: { ...synced, encrypted: true },
		target: target({ vault: "plain", encrypted: false }),
	});
	assert.equal(p.exposes, true);
});

test("a switch can be undone exactly until it is finished", async () => {
	const app = new FakeApp(await fs.mkdtemp(path.join(os.tmpdir(), "sw-")));
	saveState(app, synced);

	beginSwitch(app, { vault: "work", token: "arch_old" }, "work-enc");
	assert.deepEqual(loadState(app).files, {});
	assert.equal(loadState(app).base, "");
	assert.equal(loadSwitchBackup(app).to, "work-enc");

	const backup = undoSwitch(app);
	assert.equal(backup.vault, "work");
	assert.equal(backup.token, "arch_old");
	assert.deepEqual(loadState(app).files, synced.files);
	assert.equal(loadState(app).base, "abc");
	assert.equal(loadSwitchBackup(app), null);
});

test("finishing a switch lets the old state go", async () => {
	const app = new FakeApp(await fs.mkdtemp(path.join(os.tmpdir(), "sw-")));
	saveState(app, synced);
	beginSwitch(app, { vault: "work", token: "arch_old" }, "work-enc");
	finishSwitch(app);
	assert.equal(loadSwitchBackup(app), null);
	assert.equal(undoSwitch(app), null);
	assert.equal(loadState(app).base, "");
});
