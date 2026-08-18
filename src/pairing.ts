/**
 * The first-connect hazard, and the three ways out of it.
 *
 * Every other pairing combination is already safe: an empty device pulls, an
 * empty server is pushed to, and a device that has already synced is protected
 * by the vault-identity guard in state.ts. The one gap is a device that has
 * content and has NEVER synced meeting a server that also has content. On that
 * cycle `state.base` is "", so `changes("")` returns the server's entire
 * contents as puts, the device writes them all, and then pushes everything it
 * had. Both ends end up with the union and same-path collisions become conflict
 * files. Nothing warns, and warning afterwards would be useless: the damage
 * lands in the same cycle that would raise it.
 *
 * The identity guard cannot catch this. It compares an ADOPTED vault name, and
 * on first connect there is not one yet.
 *
 * This module is deliberately free of any Obsidian import, so the sync engine
 * that uses it can be bundled and driven from Node.
 */

/** What the user chose when told about the hazard. */
export type PairingChoice = "adopt" | "publish" | "merge";

/**
 * Thrown before anything is read or written, so both vaults are untouched when
 * it surfaces. Carries the numbers the user needs to choose: how much is here,
 * and that the server has a history at all.
 */
export class PairingHazardError extends Error {
	constructor(
		readonly localFiles: number,
		readonly serverHead: string,
	) {
		super(
			`this vault holds ${localFiles} file(s) and has never synced, but the ` +
				`server already has content. Syncing now would merge two unrelated ` +
				`vaults and turn every same-path collision into a conflict file. ` +
				`Choose one: "Adopt server" (move local files aside, then pull), ` +
				`"Publish local" (this vault wins every collision), or ` +
				`"Merge anyway" (the union, with conflict files).`,
		);
		this.name = "PairingHazardError";
	}
}

/**
 * The folder local files are moved into by "Adopt server".
 *
 * Dated, so adopting twice does not collide with the first rescue. NOT
 * dot-prefixed, deliberately: the folder lands inside the vault and therefore
 * syncs, so the rescued files reach the other devices too. Deleting them is not
 * an option for a tool whose pitch is that git makes nothing unrecoverable.
 *
 * The date alone is not enough. An adoption interrupted halfway, or a second
 * one on the same day, would reuse the same destination paths and rename
 * straight over the first rescue -- losing exactly the files the folder exists
 * to preserve. The caller probes for the first free suffix.
 */
const RESCUE_PREFIX = "_archivist-rescued-";

/**
 * Whether a path already lives inside a rescue folder.
 *
 * A second adoption must leave the first rescue where it is. Without this it
 * sweeps up the earlier rescue as ordinary local content and nests it one level
 * deeper -- `_archivist-rescued-A/_archivist-rescued-B/note.md` -- and it does
 * that again on every adoption after. Nothing is destroyed, but the folder
 * whose entire job is being findable stops being findable.
 */
export function isRescuePath(path: string): boolean {
	return path.split("/")[0].startsWith(RESCUE_PREFIX);
}

export function rescueFolder(now: Date, suffix = 0): string {
	const p = (n: number) => String(n).padStart(2, "0");
	const date = `${now.getUTCFullYear()}${p(now.getUTCMonth() + 1)}${p(now.getUTCDate())}`;
	return suffix === 0 ? `${RESCUE_PREFIX}${date}` : `${RESCUE_PREFIX}${date}-${suffix}`;
}
