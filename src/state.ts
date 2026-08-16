import type { App } from "obsidian";

export type FileState = { hash: string; mtime: number; size: number };

export type SyncState = {
	/** The commit we last synced. "" means never synced. */
	base: string;
	/** The vault as of `base`. This is what makes offline diffs possible. */
	files: Record<string, FileState>;
};

const KEY = "archivist.state";

/**
 * State lives in Obsidian's vault-scoped LOCAL storage, deliberately not in
 * saveData().
 *
 * saveData writes .obsidian/plugins/archivist/data.json -- inside the vault. If
 * .obsidian is ever synced, every device would adopt every other device's
 * cursor and snapshot, and each would then reason about local changes from a
 * snapshot it never built. That is silent, baffling data loss. Keeping the
 * state out of the vault makes the mistake impossible rather than forbidden.
 */
export function loadState(app: App): SyncState {
	const raw = app.loadLocalStorage(KEY);
	if (!raw || typeof raw !== "object") return emptyState();
	const s = raw as Partial<SyncState>;
	return { base: typeof s.base === "string" ? s.base : "", files: s.files ?? {} };
}

export function saveState(app: App, state: SyncState): void {
	app.saveLocalStorage(KEY, state);
}

export function emptyState(): SyncState {
	return { base: "", files: {} };
}

/**
 * A client that has never synced must never infer a deletion: it has no idea
 * what the server holds, so an absence tells it nothing. This is the exact bug
 * Self-hosted LiveSync shipped in 1.0.12, and the server refuses such deletes
 * too -- belt and braces.
 */
export function isFirstRun(state: SyncState): boolean {
	return state.base === "" && Object.keys(state.files).length === 0;
}
