import type { App } from "obsidian";

export type FileState = { hash: string; mtime: number; size: number };

export type SyncState = {
	/** The commit we last synced. "" means never synced. */
	base: string;
	/** The vault as of `base`. This is what makes offline diffs possible. */
	files: Record<string, FileState>;
	/**
	 * Which server vault this device bootstrapped against, from GET /v1.
	 *
	 * Guards the one misconfiguration that silently destroys data: point this
	 * Obsidian vault's settings at a DIFFERENT vault's server -- easy to do with
	 * two servers and two tokens -- and the client bootstraps from whatever
	 * snapshot it is handed, merging two unrelated vaults into both. Git history
	 * makes it recoverable, but only if somebody notices, and nothing would say.
	 *
	 * Empty on state written by older versions; then the current name is adopted
	 * rather than treated as a mismatch.
	 */
	vault?: string;
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
	return {
		base: typeof s.base === "string" ? s.base : "",
		files: s.files ?? {},
		vault: typeof s.vault === "string" ? s.vault : undefined,
	};
}

export function saveState(app: App, state: SyncState): void {
	app.saveLocalStorage(KEY, state);
}

export function emptyState(): SyncState {
	return { base: "", files: {} };
}

/** Thrown when the server serves a different vault than this device adopted. */
export class WrongVaultError extends Error {
	constructor(readonly expected: string, readonly got: string) {
		super(
			`this device synced vault "${expected}" but the server now serves "${got}". ` +
				`Refusing to sync: continuing would merge two unrelated vaults. ` +
				`Check the server URL in settings, or use "Re-bootstrap from server" ` +
				`if you moved this vault on purpose.`,
		);
		this.name = "WrongVaultError";
	}
}

/**
 * checkVault compares the server's vault name against the one this device
 * adopted, and refuses on a change.
 *
 * Refuse rather than warn. A warning is a Notice somebody dismisses while the
 * merge proceeds; the whole point is that the damage happens on the first sync.
 */
export function checkVault(state: SyncState, serverVault: string): void {
	if (!serverVault) return; // older server: nothing to compare against
	if (!state.vault) return; // older state, or first run: adopt below
	if (state.vault !== serverVault) throw new WrongVaultError(state.vault, serverVault);
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
