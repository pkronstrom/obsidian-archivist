/**
 * Session clustering for the revision browser.
 *
 * A vault commits on every edit, so one drafting session leaves 15-25
 * revisions of the same note and a raw list is unreadable for exactly the note
 * being worked on. Grouping by editing session is what makes the list mean
 * something: "yesterday evening" rather than forty timestamps.
 *
 * This is deliberately client-side and at render time. The server-side
 * alternative (a History-Group projection) was designed and shelved -- this
 * delivers the same thing for the one surface that needs it, for one file at a
 * time, with no server change at all.
 */
import type { Revision } from "./client";

/** A burst of edits with no long gap inside it. */
export type Session = {
	/** Newest first, as the server returned them. */
	revisions: Revision[];
	/**
	 * The revision to open for this session: the NEWEST non-deleted one, i.e.
	 * the state the note was left in when the session ended.
	 *
	 * Null when the session contains nothing openable -- a session whose only
	 * entry is a deletion. Callers must render those as events rather than as
	 * something to click, because /v1/at cannot serve a path that is absent
	 * from the commit's tree.
	 */
	representative: Revision | null;
	/** When the session ended (newest revision) and began (oldest). */
	endedAt: Date;
	startedAt: Date;
	/** Every device that contributed, in first-seen order. Usually one; two
	 *  means the note was edited from two places inside one window, which is
	 *  worth seeing rather than averaging away. */
	devices: string[];
	/** Lines added and removed across the whole session. */
	added: number;
	removed: number;
	/** The session created the file. */
	created: boolean;
};

/** Default gap that separates two sessions. */
export const SESSION_GAP_MS = 30 * 60 * 1000;

/**
 * clusterRevisions folds a newest-first revision list into sessions.
 *
 * The input order matters and is not re-sorted: /v1/history returns newest
 * first, and the fold runs in that direction. Getting this backwards would
 * make every session's representative its OLDEST revision -- the state the
 * note was in when the session started, which is the one version nobody wants.
 */
export function clusterRevisions(revisions: Revision[], gapMs = SESSION_GAP_MS): Session[] {
	const out: Session[] = [];
	let current: Revision[] = [];

	const flush = () => {
		if (current.length === 0) return;
		const times = current.map((r) => new Date(r.when).getTime());
		const devices: string[] = [];
		let added = 0;
		let removed = 0;
		let created = false;
		for (const r of current) {
			if (r.device && !devices.includes(r.device)) devices.push(r.device);
			added += r.added ?? 0;
			removed += r.removed ?? 0;
			created = created || Boolean(r.created);
		}
		out.push({
			revisions: current,
			devices,
			added,
			removed,
			created,
			// First non-deleted in a newest-first list is the newest openable
			// one. A deleted representative would make the whole session
			// unopenable even though earlier content exists.
			representative: current.find((r) => !r.deleted) ?? null,
			endedAt: new Date(Math.max(...times)),
			startedAt: new Date(Math.min(...times)),
		});
		current = [];
	};

	for (const rev of revisions) {
		if (current.length === 0) {
			current.push(rev);
			continue;
		}
		const prev = new Date(current[current.length - 1].when).getTime();
		const here = new Date(rev.when).getTime();
		// Walking newest to oldest, so the gap is prev - here.
		//
		// A gap of zero means "never group", checked explicitly rather than
		// left to the comparison: two commits inside the same second are zero
		// apart, which is not greater than zero, so they would group -- and a
		// setting that says "list every sync" must not quietly merge the two
		// closest ones.
		if (gapMs <= 0 || prev - here > gapMs) flush();
		current.push(rev);
	}
	flush();
	return out;
}

/**
 * materialisedName builds the local-only filename for a revision.
 *
 * The marker sits immediately BEFORE the real extension so the file stays a
 * genuine note: Obsidian opens, renders and links Note.ae56b1c.local.md, and
 * an attachment keeps its own type the same way. Renaming the marker away is
 * what "restores" the revision -- it stops matching the never-sync rule and
 * syncs as an ordinary new note.
 */
export function materialisedName(path: string, commit: string): string {
	const short = commit.slice(0, 7);
	const slash = path.lastIndexOf("/");
	const dot = path.lastIndexOf(".");
	if (dot > slash) return `${path.slice(0, dot)}.${short}.local${path.slice(dot)}`;
	// No extension: the marker still has to be second-to-last, so the file
	// needs a final segment to sit before.
	return `${path}.${short}.local.md`;
}

/**
 * uniqueName resolves a collision by suffixing, never overwriting.
 *
 * `exists` reports whether a path is taken. Callers should skip this entirely
 * when the existing file's content is already identical -- re-materialising
 * the same revision twice should reuse the file rather than litter.
 */
export function uniqueName(name: string, exists: (p: string) => boolean): string {
	if (!exists(name)) return name;
	// The suffix goes on the segment BEFORE the marker, never after it.
	// Suffixing the tail would produce Plan.ae56b1c.local-2.md, whose
	// second-to-last segment is "local-2" -- so it would fall out of the
	// never-sync namespace and the second copy of a revision would quietly
	// start syncing while the first did not.
	const marker = name.lastIndexOf(".local.");
	if (marker < 0) return name;
	const stem = name.slice(0, marker);
	const tail = name.slice(marker);
	for (let n = 2; n < 1000; n++) {
		const candidate = `${stem}-${n}${tail}`;
		if (!exists(candidate)) return candidate;
	}
	return `${stem}-${Date.now()}${tail}`;
}
