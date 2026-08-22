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

// Clustering each page and concatenating splits a session that straddles a
// page boundary: one long burst would read as two sessions purely because of
// the page size. Re-clustering the accumulated list is what fixes it.
test("a session straddling a page boundary stays one session", () => {
	const all = [];
	// 8 revisions, 5 minutes apart -- one continuous session.
	for (let i = 7; i >= 0; i--) {
		all.push(rev(new Date(Date.UTC(2026, 7, 22, 12, i * 5)).toISOString()));
	}
	const whole = clusterRevisions(all);
	assert.equal(whole.length, 1);

	// Paged 4 at a time, clustered per page and concatenated: the bug.
	const naive = clusterRevisions(all.slice(0, 4)).concat(clusterRevisions(all.slice(4)));
	assert.equal(naive.length, 2, "per-page clustering does split it");

	// Accumulating raw revisions and re-clustering does not.
	const accumulated = [...all.slice(0, 4), ...all.slice(4)];
	assert.equal(clusterRevisions(accumulated).length, 1);
});

// The setting's floor: 0 means "show every sync", so it must not group even
// revisions that share a timestamp -- those are zero apart, and a plain
// "gap > threshold" test would merge exactly the pair a user asking for full
// detail most wants to see.
test("a zero gap never groups, even for same-second revisions", () => {
	const same = "2026-08-22T12:00:00Z";
	const sessions = clusterRevisions(
		[
			{ ...rev(same), commit: "a" },
			{ ...rev(same), commit: "b" },
			{ ...rev("2026-08-22T11:59:00Z"), commit: "c" },
		],
		0,
	);
	assert.equal(sessions.length, 3, "every revision should stand alone at gap 0");
});

// And a large gap groups a whole day into one entry, which is the other end of
// the same dial.
test("a large gap groups distant edits", () => {
	const sessions = clusterRevisions(
		[rev("2026-08-22T18:00:00Z"), rev("2026-08-22T09:00:00Z")],
		12 * 60 * 60 * 1000,
	);
	assert.equal(sessions.length, 1);
	assert.equal(sessions[0].revisions.length, 2);
});
