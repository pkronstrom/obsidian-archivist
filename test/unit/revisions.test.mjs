import { test } from "node:test";
import assert from "node:assert/strict";
import {
	clusterRevisions,
	materialisedName,
	uniqueName,
	localOnly,
} from "../../dist-test/entry.mjs";

const rev = (when, extra = {}) => ({
	commit: `c${when}`,
	short: `c${when}`.slice(0, 8),
	when,
	message: "edit",
	size: 1,
	hash: "sha256:x",
	...extra,
});

// /v1/history returns newest first and the fold runs in that direction.
// Reversed, every session's representative would be the state the note was in
// when the session STARTED -- the one version nobody wants.
test("a burst becomes one session represented by its newest revision", () => {
	const sessions = clusterRevisions([
		rev("2026-08-22T12:20:00Z"),
		rev("2026-08-22T12:10:00Z"),
		rev("2026-08-22T12:00:00Z"),
	]);
	assert.equal(sessions.length, 1);
	assert.equal(sessions[0].revisions.length, 3);
	assert.equal(sessions[0].representative.when, "2026-08-22T12:20:00Z");
	assert.equal(sessions[0].endedAt.toISOString(), "2026-08-22T12:20:00.000Z");
	assert.equal(sessions[0].startedAt.toISOString(), "2026-08-22T12:00:00.000Z");
});

test("a gap over the threshold splits sessions", () => {
	const sessions = clusterRevisions([
		rev("2026-08-22T18:00:00Z"),
		rev("2026-08-22T12:20:00Z"),
		rev("2026-08-22T12:00:00Z"),
	]);
	assert.equal(sessions.length, 2);
	assert.equal(sessions[0].revisions.length, 1);
	assert.equal(sessions[1].revisions.length, 2);
});

// /v1/at cannot serve a path absent from the commit's tree, so a deleted
// representative would make an entire session unopenable even though earlier
// content exists.
test("the representative skips deletion revisions", () => {
	const sessions = clusterRevisions([
		rev("2026-08-22T12:20:00Z", { deleted: true }),
		rev("2026-08-22T12:10:00Z"),
	]);
	assert.equal(sessions.length, 1);
	assert.equal(sessions[0].representative.when, "2026-08-22T12:10:00Z");
});

test("a session with nothing openable has a null representative", () => {
	const sessions = clusterRevisions([rev("2026-08-22T12:20:00Z", { deleted: true })]);
	assert.equal(sessions[0].representative, null);
});

test("no revisions is no sessions, not one empty session", () => {
	assert.deepEqual(clusterRevisions([]), []);
});

// The marker must land second-to-last so the real extension stays final --
// that is what keeps the file a genuine note, and what makes localOnly match.
test("materialised names keep the real extension last and are local-only", () => {
	const name = materialisedName("Notes/Plan.md", "ae56b1c9999");
	assert.equal(name, "Notes/Plan.ae56b1c.local.md");
	assert.equal(localOnly(name), true);

	const png = materialisedName("attachments/diagram.png", "8f1c2ad0000");
	assert.equal(png, "attachments/diagram.8f1c2ad.local.png");
	assert.equal(localOnly(png), true);

	// A file with no extension still needs the marker second-to-last.
	const bare = materialisedName("Inbox/scratch", "1234567aaaa");
	assert.equal(localOnly(bare), true);
});

test("collisions suffix rather than overwrite", () => {
	const taken = new Set(["Plan.ae56b1c.local.md", "Plan.ae56b1c-2.local.md"]);
	assert.equal(
		uniqueName("Plan.ae56b1c.local.md", (p) => taken.has(p)),
		"Plan.ae56b1c-3.local.md",
	);
	assert.equal(
		uniqueName("Fresh.ae56b1c.local.md", (p) => taken.has(p)),
		"Fresh.ae56b1c.local.md",
	);
});

// A suffixed collision must still be excluded from sync, or the second copy
// of a revision would quietly start syncing while the first did not.
test("suffixed collisions stay local-only", () => {
	assert.equal(localOnly("Plan.ae56b1c-3.local.md"), true);
});
