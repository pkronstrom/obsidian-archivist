import { test } from "node:test";
import assert from "node:assert/strict";
import { plannedInstalls } from "../../dist-test/entry.mjs";

const manifests = {
	dataview: { id: "dataview", name: "Dataview", isDesktopOnly: false },
	shellcommands: { id: "shellcommands", name: "Shell commands", isDesktopOnly: true },
};

test("only ids that are not installed are planned", () => {
	const plan = plannedInstalls(["dataview", "templater"], ["dataview"], manifests, false);
	assert.deepEqual(plan.install, ["templater"]);
});

test("nothing is planned when everything is present", () => {
	const plan = plannedInstalls(["dataview"], ["dataview"], manifests, false);
	assert.deepEqual(plan.install, []);
	assert.deepEqual(plan.skipped, []);
});

test("desktop-only plugins are skipped on mobile, with a reason", () => {
	const plan = plannedInstalls(["shellcommands"], [], manifests, true);
	assert.deepEqual(plan.install, []);
	assert.equal(plan.skipped.length, 1);
	assert.equal(plan.skipped[0].id, "shellcommands");
	assert.match(plan.skipped[0].why, /desktop/i);
});

test("desktop-only plugins install on desktop", () => {
	const plan = plannedInstalls(["shellcommands"], [], manifests, false);
	assert.deepEqual(plan.install, ["shellcommands"]);
});

// The realistic case, and the one production actually hits: a plugin about to
// be installed is BY DEFINITION not installed, so app.plugins.manifests has
// nothing for it. Planning cannot know it is desktop-only, so the filter has to
// happen again after its manifest is fetched.
test("an unknown manifest is planned, because mobile support is unknowable until fetched", () => {
	const plan = plannedInstalls(["mystery"], [], manifests, true);
	assert.deepEqual(plan.install, ["mystery"]);
	assert.equal(plan.deferredMobileCheck, true,
		"the plan must say the mobile decision is still outstanding");
});

test("on desktop nothing is deferred", () => {
	const plan = plannedInstalls(["mystery"], [], manifests, false);
	assert.equal(plan.deferredMobileCheck, false);
});

test("archivist is never installed by the list, whatever the list says", () => {
	const plan = plannedInstalls(["archivist", "obsidian-archivist"], [], manifests, false);
	assert.deepEqual(plan.install, ["obsidian-archivist"]);
});
