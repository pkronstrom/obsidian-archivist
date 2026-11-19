import type { App } from "obsidian";
import type { Client } from "./client";
import { emptyState, loadState, saveState, type SyncState } from "./state";
import type { SyncReport } from "./sync";

/**
 * Moving an already-synced folder to another vault.
 *
 * Normally refused outright (state.ts, checkVault): pointing a synced folder
 * at a different vault merges two unrelated vaults into both. This is the one
 * deliberate way past that refusal, and it is only as safe as the checks here.
 * It exists so an existing vault can be moved to an encrypted one, which
 * cannot be done in place -- the old vault's history already holds plaintext.
 *
 * The switch clears what the plugin remembers about the last sync. It never
 * touches a note, and it never touches the old vault on the server: deleting
 * that stays a manual step, taken after the new one has been checked.
 */

export type VaultKind = { vault: string; encrypted: boolean };

export type SwitchPlan = {
	from: VaultKind;
	to: VaultKind;
	/** Encrypted to ordinary uploads every note as plaintext, so the caller
	 *  must make the user type the vault name before going ahead. */
	exposes: boolean;
	/** The last sync with the old vault did not complete, and the user chose
	 *  to go on regardless. Anything not yet pulled is not carried over. */
	unsynced?: string;
};

/** A switch that must not happen, with the reason in words. */
export class SwitchRefused extends Error {
	constructor(
		message: string,
		/** The only refusal the user may override: the old vault could not be
		 *  synced first. Everything else protects data and is final. */
		readonly overridable = false,
	) {
		super(message);
		this.name = "SwitchRefused";
	}
}

/**
 * planSwitch runs every check and changes nothing.
 *
 * Order matters. The target is examined first, so a typo in the vault name or
 * token is reported before the old vault is touched at all.
 */
export async function planSwitch(opts: {
	state: SyncState;
	/** The vault this folder syncs now, or null when it cannot be reached. */
	current: Client | null;
	target: Client;
	/** Runs one ordinary sync cycle against the current vault. */
	syncCurrent: () => Promise<SyncReport | null>;
	/** Go ahead although the current vault could not be synced first. */
	skipSync?: boolean;
}): Promise<SwitchPlan> {
	const { state, target } = opts;

	// The target must answer with this token before anything is cleared.
	let to: VaultKind;
	let targetHead: string;
	try {
		const idx = await target.index();
		to = { vault: idx.vault, encrypted: idx.encrypted };
		targetHead = await target.head();
	} catch (err) {
		throw new SwitchRefused(`the new vault cannot be reached: ${message(err)}`);
	}
	if (state.vault && to.vault === state.vault) {
		throw new SwitchRefused(`this folder already syncs with "${to.vault}"`);
	}
	// Empty means no history at all, not merely no files. Then the first sync
	// can only upload: there is nothing on the other side to merge with or to
	// overwrite anything here.
	if (targetHead !== "") {
		throw new SwitchRefused(
			`"${to.vault}" already has content. A folder can only be switched to a new, empty vault.`,
		);
	}

	const from: VaultKind = { vault: state.vault ?? "", encrypted: state.encrypted === true };
	const plan: SwitchPlan = { from, to, exposes: from.encrypted && !to.encrypted };

	// The folder must be caught up with the vault it is leaving. An edit made
	// on another device and not yet pulled here would simply be missing from
	// the new vault, and nothing would ever say so.
	if (opts.skipSync) {
		plan.unsynced = "the old vault was not synced first";
		return plan;
	}
	let report: SyncReport | null;
	try {
		if (!opts.current) throw new Error("it is not configured");
		report = await opts.syncCurrent();
	} catch (err) {
		throw new SwitchRefused(`"${from.vault}" could not be synced first: ${message(err)}`, true);
	}
	if (report === null) {
		throw new SwitchRefused("a sync is already running; try again in a moment", true);
	}
	if (report.conflicts.length > 0 || report.refused.length > 0) {
		throw new SwitchRefused(
			`the last sync with "${from.vault}" left ${report.conflicts.length} conflict(s) and ` +
				`${report.refused.length} refused change(s). Resolve those first.`,
			true,
		);
	}
	return plan;
}

/**
 * What a switch replaced, kept until the first sync with the new vault
 * succeeds. Until then the switch can be undone exactly.
 */
export type SwitchBackup = {
	vault: string;
	token: string;
	state: SyncState;
	/** Where the folder was switched to, for the messages. */
	to: string;
};

const BACKUP = "archivist.switchBackup";

export function loadSwitchBackup(app: App): SwitchBackup | null {
	const raw = app.loadLocalStorage(BACKUP);
	return raw && typeof raw === "object" ? (raw as SwitchBackup) : null;
}

/**
 * beginSwitch forgets the old sync and remembers how to get it back.
 *
 * The backup is written BEFORE the state is cleared. A crash between the two
 * leaves a backup beside the old state, which undo restores to itself.
 */
export function beginSwitch(app: App, old: { vault: string; token: string }, to: string): void {
	const backup: SwitchBackup = { vault: old.vault, token: old.token, state: loadState(app), to };
	app.saveLocalStorage(BACKUP, backup);
	saveState(app, emptyState());
}

/** The first sync with the new vault succeeded; the old state is let go. */
export function finishSwitch(app: App): void {
	app.saveLocalStorage(BACKUP, null);
}

/** Put the old sync state back. The caller restores the vault and token. */
export function undoSwitch(app: App): SwitchBackup | null {
	const backup = loadSwitchBackup(app);
	if (!backup) return null;
	saveState(app, backup.state);
	app.saveLocalStorage(BACKUP, null);
	return backup;
}

function message(err: unknown): string {
	return err instanceof Error ? err.message : String(err);
}
