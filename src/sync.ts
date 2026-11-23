import type { App, DataAdapter } from "obsidian";
import { inventoryPathAllowed, inventoryDirectory } from "./plugin-inventory";
import {
	Client,
	Code,
	ServerError,
	UnknownBaseError,
	requireCurrentIndex,
	type Change,
	type Result,
	type ServerIndex,
} from "./client";
import { pairRenames, type Pending } from "./scopes";
import { gitHash } from "./hash";
import { NameTooLongError, WrongKeyError, encryptedDir, encryptedPath, type KeyScope, type VaultCrypt } from "./crypt";
import {
	checkVault,
	isFirstRun,
	loadState,
	localHash,
	saveState,
	type FileState,
	type SyncState,
} from "./state";
import {
	PairingHazardError,
	compareVaults,
	isRescuePath,
	rescueFolder,
	type PairingChoice,
	type VaultComparison,
} from "./pairing";

export type SyncReport = {
	pulled: number;
	pushed: number;
	conflicts: Result[];
	refused: Result[];
	rebootstrapped: boolean;
	/**
	 * Ordinary vaults only: this device has no key, and there are files marked
	 * .encrypted it therefore left alone -- on the server, here, or both. The
	 * rest of the vault synced. A question to raise, not a failure.
	 */
	needsKey?: { hasKey: boolean; keyCheck: string };
	/** Files that were renamed out of an .encrypted name and so were uploaded
	 *  as readable text. Said out loud, because it is the one direction in
	 *  which a rename publishes a note. */
	exposed?: string[];
	/** The opposite: notes renamed INTO an .encrypted name. They are
	 *  ciphertext from here on, but every version from before the rename is
	 *  still readable in the server's history, and the user should know. */
	marked?: string[];
};

export type Logger = (msg: string, ...rest: unknown[]) => void;

/**
 * An encrypted vault, and this device has no key for it. Like the pairing
 * hazard this is a question rather than a failure: nothing was read or
 * written, and the device does not sync until it is answered.
 */
export class KeyRequiredError extends Error {
	constructor(
		readonly vault: string,
		/** The vault already has a key, so it must be entered. Otherwise this is
		 *  the first device and it generates one. */
		readonly hasKey: boolean,
		/** The server's key check, for verifying an entered key before it is
		 *  stored. "" when the vault has no key yet. */
		readonly keyCheck: string = "",
		/** What the key is for. "files" is never thrown: an ordinary vault
		 *  syncs without its key and reports needsKey instead. The plugin
		 *  builds one of these to ask the same question in the same dialog. */
		readonly scope: KeyScope = "vault",
	) {
		super(
			scope === "files"
				? `"${vault}" has files marked .encrypted. ${hasKey ? "Enter" : "Create"} the vault's key to sync them.`
				: hasKey
					? `"${vault}" is an encrypted vault. Enter its recovery key to sync it.`
					: `"${vault}" is a new encrypted vault. Create its key to start syncing.`,
		);
		this.name = "KeyRequiredError";
	}
}

/**
 * What happened to one incoming remote change.
 *
 * "deferred" is the load-bearing one: we chose not to apply it because the file
 * is modified locally too, which means we have NOT caught up and the sync
 * cursor must not advance past it.
 */
type RemoteOutcome = "applied" | "skipped" | "deferred";

/**
 * Paths excluded from sync entirely.
 *
 * Dotfiles at every level: .obsidian is device-specific workspace state, .trash
 * is Obsidian's own, and the server refuses them anyway. Obsidian config sync,
 * if it ever happens, needs its own scan with different rules -- it is not a
 * matter of loosening this.
 */
/**
 * conflictName mirrors the server's naming so a locally-created conflict looks
 * the same as a server-created one. Content is not hashed here: the server
 * disambiguates by content, and this path only ever produces one copy.
 */
/**
 * conflictName names the local copy set aside during a re-bootstrap.
 *
 * It keeps a timestamp where the SERVER's equivalent dropped one, and the
 * asymmetry is deliberate rather than an oversight. The server names a conflict
 * from the content it is holding, so a hash fragment gives uniqueness and
 * identical content collapses onto one file. Here there is no content in hand —
 * this runs before anything is read — so the timestamp is the only thing
 * keeping two rescues of the same path apart. Removing it would let the second
 * overwrite the first, which is the one thing a rescue must not do.
 */
export function conflictName(path: string, device: string): string {
	const stamp = new Date().toISOString().replace(/[-:]/g, "").replace(/\.\d+Z$/, "");
	const safe = (device || "device").replace(/[^A-Za-z0-9-]/g, "-");
	const slash = path.lastIndexOf("/");
	let dot = path.lastIndexOf(".");
	const suffix = `.conflict-${safe}-${stamp}-local`;
	if (dot <= slash) return path + suffix;
	// A copy of an encrypted note must itself be an encrypted note. Inserted
	// before the extension, the suffix would sit between the marker and the
	// extension and unmark it: Salary.encrypted.conflict-….md is an ordinary
	// name, and the copy would be uploaded as readable text. So it goes before
	// the marker instead. Under a marked folder nothing needs doing; the
	// folder covers whatever the file is called.
	const marker = path.slice(0, dot).toLowerCase().lastIndexOf(".encrypted");
	if (marker > slash && marker + ".encrypted".length === dot) dot = marker;
	return path.slice(0, dot) + suffix + path.slice(dot);
}

/** Whether this client encrypts the file at `path`. False without a key. */
function covered(client: { crypt?: VaultCrypt }, path: string): boolean {
	return client.crypt?.covers(path) ?? false;
}

/**
 * localOnly reports whether a path is in the user's never-sync namespace.
 *
 * Must stay identical in meaning to vault.LocalOnly in Go. One rule, two
 * shapes: Scratch.local.md (a file, marked before its extension) and
 * Scratch.local/ (a folder -- nothing inside it ever syncs). A folder has to
 * END in .local rather than use the file spelling, because a directory called
 * "project.local.assets" would match the file grammar by coincidence and
 * strand every note inside a tree nobody meant to hide.
 *
 * The revision browser materialises Note.<hash>.local.md and lands here by
 * construction; renaming the marker away is what "restores" it.
 */
export function localOnly(path: string): boolean {
	const seg = path.split("/");
	// Any ancestor marked .local takes its whole subtree with it.
	if (seg.some((s) => s.endsWith(".local") && s.length > ".local".length)) return true;
	const base = seg[seg.length - 1].split(".");
	return base.length >= 3 && base[base.length - 2] === "local";
}

/**
 * localOnlyDir is localOnly for a DIRECTORY, without the file grammar.
 *
 * Traversal uses this: an ordinary folder that happens to match the file
 * spelling must still be descended into, or a naming coincidence silently
 * stops a whole tree from syncing.
 */
export function localOnlyDir(path: string): boolean {
	return path.split("/").some((s) => s.endsWith(".local") && s.length > ".local".length);
}

export function skip(path: string): boolean {
	// Before the dotfile rules, and never widened by the inventory allowance: the
	// user picked this name to stop the file syncing, so it outranks every
	// other rule including the allowlist that lets some config paths through.
	if (localOnly(path)) return true;
	return skipDir(path);
}

/**
 * skipDir is skip WITHOUT the .local rule, for deciding whether to descend
 * into a directory.
 *
 * A folder named "project.local.assets" satisfies the .local grammar, and
 * treating it as excluded would silently stop every note inside it from
 * syncing -- a whole tree lost to a naming coincidence, reported nowhere.
 * .local marks FILES the user wants kept local; it was never meant to hide
 * directories.
 */
export function skipDir(path: string): boolean {
	// A folder deliberately marked .local is skipped whole -- that IS the
	// feature. Only the accidental file-grammar match is exempted here.
	if (localOnlyDir(path)) return true;
	if (!path.split("/").some((seg) => seg.startsWith("."))) return false;
	return !inventoryPathAllowed(path);
}

/**
 * The sync engine.
 *
 * Every cycle diffs the vault against the snapshot rather than replaying an
 * event log. That is what makes an arbitrary offline period and a mid-sync
 * process kill both survivable: the computation is identical either way, and
 * nothing depends on having observed the edits as they happened.
 *
 * Vault events are a latency hint only. They say "sync soon", never "here is
 * what changed".
 */
export class Sync {
	private running = false;
	private queued = false;

	/**
	 * Set once the user has answered the pairing question. It is never reset:
	 * within a session, having chosen is permanent, and after one successful
	 * cycle `state.base` is non-empty so the check cannot fire again anyway.
	 */
	private pairingResolved = false;

	/**
	 * Set for a cycle on an ordinary vault with no key on this device. Marked
	 * paths are then left alone in both directions, exactly like excluded
	 * ones: not downloaded, not uploaded, and never deleted on the server for
	 * being absent here.
	 */
	private keyless = false;
	/** A marked file was seen on this device during a keyless cycle. */
	private held = false;

	constructor(
		private app: App,
		private client: () => Client,
		private device: () => string,
		private log: Logger = () => {},
		private prepareLocal: () => Promise<void> = async () => {},
	) {}

	private get adapter(): DataAdapter {
		return this.app.vault.adapter;
	}

	/** run coalesces concurrent calls: two cycles would diff against the same
	 *  snapshot and push the same work twice. */
	async run(): Promise<SyncReport | null> {
		if (this.running) {
			this.queued = true;
			return null;
		}
		this.running = true;
		try {
			const report = await this.cycle();
			return report;
		} finally {
			this.running = false;
			if (this.queued) {
				this.queued = false;
				void this.run();
			}
		}
	}

	private async cycle(): Promise<SyncReport> {
		const client = this.client();
		let state = loadState(this.app);
		const report: SyncReport = { pulled: 0, pushed: 0, conflicts: [], refused: [], rebootstrapped: false };

		// Identity first, BEFORE anything is read or written.
		//
		// If the settings point at a different vault's server than this device
		// adopted, syncing would merge two unrelated vaults into both -- and the
		// damage lands on the very first cycle, so a check anywhere later is too
		// late. Throwing here surfaces it as a Notice and leaves both vaults
		// untouched.
		const idx = requireCurrentIndex(await client.index());
		checkVault(state, idx.vault);
		// Also before anything is read or written: an encrypted vault with the
		// wrong key, or no key, must not see one byte from this device.
		await this.checkKind(client, state, idx);
		this.keyless = !client.crypt;
		this.held = false;

		// Then the first-connect hazard, which the identity check cannot see:
		// it compares an ADOPTED vault name, and there is not one yet.
		//
		// Deliberately before the vault name is adopted and saved, so a refusal
		// leaves NOTHING persisted -- not one byte on disk and not one field in
		// local storage. See src/pairing.ts for what goes wrong without it.
		if (!this.pairingResolved && isFirstRun(state)) {
			const serverHead = await client.head();
			if (serverHead !== "") {
				// Inventory metadata alone must not make an empty vault look
				// populated. Pairing guards protect the user's notes.
				const localFiles = (await this.listAll("")).filter(
					(p) => !inventoryPathAllowed(p),
				);
				if (localFiles.length > 0) {
					throw new PairingHazardError(
						localFiles.length,
						serverHead,
						await this.compareWithServer(client, localFiles),
					);
				}
			}
		}

		if ((!state.vault && idx.vault) || (idx.encrypted && !state.encrypted)) {
			// First run, or state from a version that did not record it. Adopt.
			state.vault = idx.vault;
			if (idx.encrypted) state.encrypted = true;
			saveState(this.app, state);
		}

		// --- pull ------------------------------------------------------------
		// Always before push, so the push is computed against the freshest base
		// we can get.
		try {
			const { head, entries } = await client.changes(state.base);
			let deferred = 0;
			for (const e of entries) {
				if (this.skips(e.path)) continue;
				const outcome = await this.applyRemote(e, state);
				if (outcome === "applied") report.pulled++;
				if (outcome === "deferred") deferred++;
			}
			// Advance the cursor ONLY if we absorbed everything the server had.
			//
			// If we declined a remote change because the file is modified here
			// too, then we have NOT caught up on that path, and moving the
			// cursor to head would tell the server we had -- so the push would
			// look like a clean fast-forward and silently overwrite the other
			// device's edit instead of merging with it.
			//
			// Keeping the old base is precisely the information the server needs
			// to three-way merge. Verified: without this, a disjoint edit from a
			// second device is lost rather than merged.
			if (deferred === 0) state.base = head;
		} catch (err) {
			if (!(err instanceof UnknownBaseError)) throw err;
			// Our cursor predates a history rewrite, or belongs to another
			// server. There is no valid diff to ask for; adopt server state.
			this.log("base not recognised; re-bootstrapping from /snapshot");
			state = await this.rebootstrap(state);
			report.rebootstrapped = true;
		}

		// A device that has just been given the key for the vault's encrypted
		// files has been skipping them, and its cursor is already past the
		// commits that made them: nothing would ever pull them. Once, pick
		// them up from the snapshot. The cursor is not touched, so ordinary
		// changes are still pulled in order.
		if (client.crypt?.scope === "files" && !state.keyed) {
			await this.adoptCovered(client, state);
			state.keyed = true;
		}

		// --- push ------------------------------------------------------------
		await this.prepareLocal();
		const local = await this.seal(client, state, await this.localChanges(state), report);
		if (local.length > 0) {
			await this.upload(local);
			const { head, results } = await client.push(state.base, this.device(), local.map(toChange));
			state.base = head;
			for (const r of results) {
				if (r.status === "conflict") report.conflicts.push(r);
				if (r.status === "refused") report.refused.push(r);
			}
			this.reportBoundary(local, results, report);
			report.pushed = local.length - results.filter((r) => r.status === "refused").length;

			// Adopt into the snapshot ONLY what the server actually took.
			//
			// "applied" is the single case where our bytes are now the server's
			// bytes. Every other status means the server holds something we do
			// not have on disk:
			//
			//   merged   the server wrote the three-way merge -- NOT what we sent
			//   conflict the server kept its own version, ours is in a side file
			//   refused  the server declined, e.g. a delete of a path it changed
			//
			// Recording our own hash for any of those is a lie the next cycle
			// believes: the diff sees no local change, the cursor is already
			// past the commit, and the device sits permanently diverged showing
			// stale content with no error anywhere. Re-fetch instead.
			const byPath = new Map(results.map((r) => [r.path, r]));
			const needsRefetch: { path: string; hash: string; size: number }[] = [];
			for (const c of local) {
				const r = byPath.get(c.path);
				if (r?.status === "applied") {
					if (c.op === "del") delete state.files[c.path];
					else if (c.op === "move") {
						// The bytes did not change, only where they live. Carry
						// the old entry across rather than re-hashing, and drop
						// the source so the next diff does not read it as a
						// fresh deletion and delete it on the server.
						const prev = state.files[c.from!];
						delete state.files[c.from!];
						if (prev) state.files[c.path] = prev;
					} else {
						state.files[c.path] = { hash: c.hash!, mtime: c.mtime!, size: c.size! };
						if (c.plain) state.files[c.path].plain = c.plain;
					}
					continue;
				}
				// An encrypted vault's server cannot make a conflict copy: it has
				// no key to produce a name this device could read. It kept its
				// own version and wrote nothing, so the copy is made here, BEFORE
				// the refetch below replaces the file with the server's version.
				// Left out of the snapshot, so the next cycle uploads it.
				if (r?.status === "conflict" && r.hash && covered(client, c.path) && c.op === "put") {
					const remote = await client.getContent(r.hash, c.path);
					if ((await gitHash(remote)) === c.plain) {
						// The same note, sealed twice. Two devices that already
						// hold identical files meet this on every one of them
						// when they first pair, and a copy of each would double
						// the vault for nothing.
						state.files[c.path] = { hash: r.hash, plain: c.plain, mtime: c.mtime!, size: c.size! };
						report.conflicts = report.conflicts.filter((x) => x.path !== c.path);
						continue;
					}
					const aside = conflictName(c.path, this.device());
					await this.adapter.writeBinary(aside, await this.adapter.readBinary(c.path));
					this.log(`${c.path} changed on both sides; kept ours as ${aside}`);
					state.files[c.path] = await this.place(c.path, r.hash, remote, r.size ?? 0);
					continue;
				}
				// Anything else means the server holds bytes we do not have. It
				// tells us their hash, so one content fetch per path repairs
				// this -- previously it cost a whole-snapshot download to find
				// out what to fetch.
				if (r?.hash) needsRefetch.push({ path: c.path, hash: r.hash, size: r.size ?? 0 });
			}
			for (const r of results) {
				if (r.conflictPath && r.conflictHash) {
					needsRefetch.push({ path: r.conflictPath, hash: r.conflictHash, size: 0 });
				}
			}
			for (const f of needsRefetch) {
				if (this.skips(f.path)) continue;
				state.files[f.path] = await this.materialise(f.path, f.hash, f.size);
			}

			// A refused delete or move is the server keeping something this
			// device dropped, usually because another device edited it since our
			// base. The cursor has just moved past that edit, so nothing will
			// ever pull it: without this the next cycle resends the delete, which
			// now lands and takes the edit out of head, and a refused move is
			// resent and refused for ever while this device never sees the edit.
			//
			// So every path the op touched is made to match the server again.
			// That undoes a refused rename here, which loses nothing: a move is
			// only ever paired from content identical to the last synced
			// version, and history holds that.
			for (const c of local) {
				if (byPath.get(c.path)?.status !== "refused") continue;
				if (c.op === "del") await this.matchServer(c.path, head, state, null);
				if (c.op === "move") {
					await this.matchServer(c.from!, head, state, null);
					await this.matchServer(c.path, head, state, c.plain ?? c.hash!);
				}
			}
		}

		if (this.keyless && !idx.encrypted && (idx.keyCheck !== "" || this.held)) {
			report.needsKey = { hasKey: idx.keyCheck !== "", keyCheck: idx.keyCheck };
		}
		state.lastSyncedAt = Date.now();
		saveState(this.app, state);
		return report;
	}

	/** skip, plus the marked paths a keyless device leaves alone. */
	private skips(path: string): boolean {
		return skip(path) || (this.keyless && encryptedPath(path));
	}

	/**
	 * Says what a rename across the .encrypted marker did. Such a rename is
	 * sent as a delete and a put, never a move, so its two halves can fare
	 * differently and neither result explains itself.
	 */
	private reportBoundary(local: PendingChange[], results: Result[], report: SyncReport): void {
		const byPath = new Map(results.map((r) => [r.path, r]));
		for (const c of local) {
			const r = byPath.get(c.path);
			if (c.op === "del" && c.becomes && r?.status === "refused") {
				// The readable copy is still on the server, which is what the
				// user renamed it to end.
				r.reason = `still stored readable: it could not be removed after being renamed to ${c.becomes} (${r.reason ?? "refused"})`;
			}
			if (c.op === "put" && c.was && r?.status === "applied") {
				// `plain` is set on a put that was sealed.
				if (c.plain) (report.marked ??= []).push(c.path);
				else (report.exposed ??= []).push(c.path);
			}
		}
	}

	/**
	 * applyRemote writes a remote change locally, but only when the local file
	 * still matches the snapshot. If it does not, the user edited it here too,
	 * and overwriting would destroy that edit silently -- the push step will
	 * raise it as a conflict instead, where both versions survive.
	 */
	private async applyRemote(e: Change, state: SyncState): Promise<RemoteOutcome> {
		const known = state.files[e.path];

		if (e.op === "del") {
			if (!(await this.adapter.exists(e.path))) {
				delete state.files[e.path];
				return "skipped";
			}
			const cur = await this.readState(e.path);
			if (!known || !cur || cur.hash !== localHash(known)) {
				this.log(`keeping locally modified ${e.path} despite a remote delete`);
				return "deferred";
			}
			await this.adapter.remove(e.path);
			delete state.files[e.path];
			return "applied";
		}

		const exists = await this.adapter.exists(e.path);

		// The path is in our snapshot but gone from disk: we deleted it locally
		// while the server modified it. Writing the remote version here would
		// silently discard the deletion -- the diff would then see the file
		// present and matching, so the delete would never be sent at all.
		// Defer instead, and let the push resolve it against the old base.
		if (!exists && known) {
			this.log(`${e.path} deleted locally but modified remotely; deferring to the push`);
			return "deferred";
		}

		if (exists) {
			const cur = await this.readState(e.path);
			if (cur && cur.hash === e.hash) {
				// Already identical -- record it and do no I/O.
				state.files[e.path] = cur;
				return "skipped";
			}
			if (cur && (!known || cur.hash !== localHash(known))) {
				this.log(`keeping locally modified ${e.path}; push will resolve it`);
				return "deferred";
			}
		}

		state.files[e.path] = await this.materialise(e.path, e.hash!, e.size ?? 0);
		return "applied";
	}

	/**
	 * materialise writes server content to a path and returns the snapshot entry
	 * for it. Three call sites needed exactly this -- pulling a remote change,
	 * bootstrapping, and adopting after a push -- and each had its own copy of
	 * the mkdir/write/stat/record dance to get subtly wrong.
	 */
	private async materialise(path: string, hash: string, fallbackSize: number): Promise<FileState> {
		return this.place(path, hash, await this.client().getContent(hash, path), fallbackSize);
	}

	/** place writes content already in hand and returns its snapshot entry.
	 *  `hash` is the server's; in an encrypted vault the plaintext's is
	 *  recorded beside it. */
	private async place(path: string, hash: string, content: ArrayBuffer, fallbackSize: number): Promise<FileState> {
		await this.mkdirs(path);
		await this.adapter.writeBinary(path, content);
		const st = await this.adapter.stat(path);
		const entry: FileState = {
			hash,
			mtime: st?.mtime ?? Date.now(),
			size: st?.size ?? fallbackSize ?? content.byteLength,
		};
		if (covered(this.client(), path)) entry.plain = await gitHash(content);
		return entry;
	}

	/**
	 * matchServer makes one path hold what the server has at rev, or nothing if
	 * the server has nothing there.
	 *
	 * expect is what the path must hold locally for this to touch it: null for
	 * absent, otherwise a hash. Anything else means the user changed it after
	 * the push was computed, and that edit wins; the next cycle sends it.
	 */
	private async matchServer(path: string, rev: string, state: SyncState, expect: string | null): Promise<void> {
		const cur = await this.readState(path);
		if ((cur?.hash ?? null) !== expect) {
			this.log(`leaving ${path} as it is: it changed here after the push`);
			return;
		}
		let content: ArrayBuffer;
		try {
			content = await this.client().readAt(rev, path);
		} catch (err) {
			if (!(err instanceof ServerError && err.code === Code.NotFound)) throw err;
			if (cur) await this.adapter.remove(path);
			delete state.files[path];
			return;
		}
		await this.mkdirs(path);
		await this.adapter.writeBinary(path, content);
		const st = await this.adapter.stat(path);
		const hash = await gitHash(content);
		state.files[path] = {
			hash,
			mtime: st?.mtime ?? Date.now(),
			size: st?.size ?? content.byteLength,
		};
		if (covered(this.client(), path)) {
			// readAt returned plaintext, so the server's own hash for this path
			// was never seen. "" is safe: only a move sends it, as an If-Match
			// the server skips when empty.
			state.files[path].hash = "";
			state.files[path].plain = hash;
		}
		this.log(`the server refused a change to ${path}; restored its version`);
	}

	/**
	 * rebootstrap adopts the server's state wholesale.
	 *
	 * It deliberately DELETES NOTHING. We have no valid base, so a local file
	 * absent from the server might be one we created offline -- the next push
	 * will send it. Worst case is a duplicate; the alternative risks loss.
	 */
	private async rebootstrap(prev?: SyncState): Promise<SyncState> {
		const client = this.client();
		// Also reached outside a cycle, from forceRebootstrap.
		this.keyless = !client.crypt;
		const { head, files } = await client.snapshot();
		const state: SyncState = { base: head, files: {} };
		if (prev?.vault) state.vault = prev.vault;
		if (prev?.encrypted) state.encrypted = true;
		if (prev?.keyed || client.crypt?.scope === "files") state.keyed = true;
		for (const [path, entry] of Object.entries(files)) {
			if (this.skips(path)) continue;
			state.files[path] = await this.adoptPath(client, path, entry, prev);
		}
		return state;
	}

	/**
	 * adoptCovered takes up the encrypted files this device was skipping while
	 * it had no key. Only paths the snapshot has and the state does not: a
	 * path already tracked is the ordinary cycle's business.
	 */
	private async adoptCovered(client: Client, state: SyncState): Promise<void> {
		const { files } = await client.snapshot();
		for (const [path, entry] of Object.entries(files)) {
			if (state.files[path] || this.skips(path) || !covered(client, path)) continue;
			state.files[path] = await this.adoptPath(client, path, entry);
		}
	}

	/**
	 * adoptPath makes one path hold what the server has, with no common
	 * ancestor to reason from, and returns its snapshot entry. A local file
	 * that differs is kept beside it under a conflict name.
	 */
	private async adoptPath(client: Client, path: string, entry: { hash: string; size: number }, prev?: SyncState): Promise<FileState> {
		const cur = await this.readState(path);
		if (!cur) return this.materialise(path, entry.hash, entry.size);
		if (!covered(client, path)) {
			if (cur.hash === entry.hash) return cur;
			// Differs locally, and we have NO common ancestor -- that is what
			// re-bootstrapping means. Leaving it out of state.files was a
			// silent data-loss bug: the next diff saw an untracked file,
			// pushed it as a new put against the freshly adopted head, and
			// because the path had not moved since that base the server
			// applied our bytes over its own without merging.
			//
			// Keep both instead. Ours moves aside under a conflict name, the
			// server's version lands at the real path, and nothing is lost.
			const aside = conflictName(path, this.device());
			await this.adapter.writeBinary(aside, await this.adapter.readBinary(path));
			// The aside copy is deliberately NOT recorded in state.files.
			// Recorded with its own hash, the next diff saw it as unchanged
			// and it never uploaded -- rescued content stayed on this device
			// only, invisible to every other device and lost with this one.
			// Left untracked, the next push sees a new file and uploads it.
			// That is safe for the ASIDE path in a way it was not for the
			// real path (the bug the comment above describes): the aside
			// name is new and device-suffixed, so its put creates a path on
			// the server rather than overwriting anything.
			this.log(`no common ancestor for ${path}; kept ours as ${aside}`);
			return this.materialise(path, entry.hash, entry.size);
		}
		// The server's hash is of ciphertext and never equals a local one, so
		// "unchanged" has to be decided on plaintext. Without this every
		// encrypted file would be set aside as a conflict copy. The old
		// snapshot answers it for free when it still describes both sides;
		// otherwise download and compare.
		const old = prev?.files[path];
		if (old && old.hash === entry.hash && localHash(old) === cur.hash) {
			return { ...cur, hash: entry.hash, plain: cur.hash };
		}
		const remote = await client.getContent(entry.hash, path);
		if ((await gitHash(remote)) === cur.hash) {
			return { ...cur, hash: entry.hash, plain: cur.hash };
		}
		const aside = conflictName(path, this.device());
		await this.adapter.writeBinary(aside, await this.adapter.readBinary(path));
		this.log(`no common ancestor for ${path}; kept ours as ${aside}`);
		return this.place(path, entry.hash, remote, entry.size);
	}

	/**
	 * localChanges diffs the vault against the snapshot.
	 *
	 * mtime AND size are the prefilter, and hashing happens only when one of
	 * them differs. On a large vault, hashing every file on every tick is the
	 * difference between instant and unusable. Size alone misses same-length
	 * edits; mtime alone misses a same-size edit inside one clock tick.
	 */
	private async localChanges(state: SyncState): Promise<PendingChange[]> {
		const out: PendingChange[] = [];
		const seen = new Set<string>();

		for (const path of await this.listAll("")) {
			seen.add(path);
			const st = await this.adapter.stat(path);
			if (!st || st.type !== "file") continue;

			const known = state.files[path];
			if (known && known.mtime === st.mtime && known.size === st.size) continue;

			const content = await this.adapter.readBinary(path);
			const hash = await gitHash(content);
			if (known && localHash(known) === hash) {
				// Touched but unchanged: refresh the metadata so we stop
				// re-hashing it every cycle.
				state.files[path] = { ...known, mtime: st.mtime, size: st.size };
				continue;
			}
			out.push({ path, op: "put", hash, mtime: st.mtime, size: st.size, content });
		}

		// A path in the snapshot and absent from disk is a deletion -- and this
		// is the only way we can know that, which is precisely why a client
		// with no snapshot must never delete.
		//
		// EXCEPT for paths sync now excludes: config recorded by the retired
		// config sync, or a file the user renamed into .local. listAll no
		// longer returns them, so without this guard they would look exactly
		// like local deletions and be pushed as such, wiping them off the
		// server and out of every other device. Stopping syncing a path must
		// never thereby delete it.
		if (!isFirstRun(state)) {
			for (const path of Object.keys(state.files)) {
				if (seen.has(path)) continue;
				if (this.skips(path)) {
					// Retired config is forgotten locally, never deleted remotely.
					delete state.files[path];
					continue;
				}
				// The hash comes along so pairRenames can recognise this as
				// half of a rename. It is never sent -- toChange drops it.
				// The LOCAL hash, since it is matched against a new file's.
				out.push({ path, op: "del", hash: localHash(state.files[path]) });
			}
		}
		// A deletion plus an addition of identical content is a rename. Folding
		// them into one move is what lets a token with write but not delete
		// rename a note; anything ambiguous is left as del+put.
		//
		// Not across the .encrypted marker. A move carries bytes as they are,
		// so across it the server would be asked to put plaintext at a marked
		// path or ciphertext at a readable one, and refuses. Those stay a
		// delete and a put, and the put is sealed or not as its path requires.
		const client = this.client();
		return pairRenames(out as Pending[], (from, to) => covered(client, from) === covered(client, to)) as PendingChange[];
	}

	/**
	 * Read-only: hashes the local files and lists the server's, so the pairing
	 * question can say what is actually different instead of only that both
	 * sides hold something. Server paths this device would not sync are left
	 * out, or they would read as "only on the server" for ever.
	 */
	private async compareWithServer(client: Client, paths: string[]): Promise<VaultComparison> {
		const local: Record<string, string> = {};
		for (const p of paths) {
			// An unreadable file has no hash; "" never matches, so it lands in
			// differ rather than being silently counted as identical.
			local[p] = (await this.readState(p))?.hash ?? "";
		}
		const server: Record<string, string> = {};
		for (const [p, e] of Object.entries((await client.snapshot()).files)) {
			if (this.skips(p) || inventoryPathAllowed(p)) continue;
			// Ciphertext hashes never match local ones, so an encrypted file
			// is compared on plaintext: one download per such file, once.
			server[p] = covered(client, p) ? await gitHash(await client.getContent(e.hash, p)) : e.hash;
		}
		return compareVaults(local, server);
	}

	/**
	 * checkKind settles whether this is an encrypted vault and whether this
	 * device may sync it, and installs the key check on the first device.
	 *
	 * The refusals are the point. A device holding a key that is told the vault
	 * is ordinary would upload every note in plaintext, so that is refused even
	 * though it can only happen through a misconfigured or hostile server.
	 */
	private async checkKind(client: Client, state: SyncState, idx: ServerIndex): Promise<void> {
		const crypt = client.crypt;
		if (!idx.encrypted) {
			if (state.encrypted || crypt?.scope === "vault") {
				throw new Error(
					`Refusing to sync: this device holds "${idx.vault}" as an encrypted vault, but the ` +
						`server says it is not one. Syncing would upload every note in plaintext.`,
				);
			}
			// An ordinary vault syncs without a key: its .encrypted files are
			// left alone until this device is given one. With a key, it is
			// verified exactly as an encrypted vault's is.
			if (crypt) await this.verifyKey(client, crypt, idx);
			return;
		}
		if (!crypt) throw new KeyRequiredError(idx.vault, idx.keyCheck !== "", idx.keyCheck);
		if (crypt.scope !== "vault") {
			// The key on this device only covers marked files. Used here it
			// would encrypt nothing, and the server would refuse every note.
			throw new Error(
				`Refusing to sync: the server says "${idx.vault}" is an encrypted vault, but the key on ` +
					`this device was made for the encrypted files of an ordinary one.`,
			);
		}
		await this.verifyKey(client, crypt, idx);
	}

	/** Installs the key check on the first device, and verifies the key
	 *  against it on every other. */
	private async verifyKey(client: Client, crypt: VaultCrypt, idx: ServerIndex): Promise<void> {
		let check = idx.keyCheck;
		if (check === "") {
			try {
				await client.putKeyCheck(crypt.keyCheck);
				return;
			} catch (err) {
				if (!(err instanceof ServerError && err.code === Code.KeyCheckSet)) throw err;
				// Another device installed a key between our reading the index
				// and writing. Theirs stands; ours is compared against it.
				check = (await client.index()).keyCheck;
			}
		}
		if (check !== crypt.keyCheck) {
			throw new WrongKeyError(`the key on this device does not open "${idx.vault}"`);
		}
	}

	/**
	 * seal turns pending changes into what the server takes for an encrypted
	 * path: sealed content under the hash of the sealed bytes. Changes to
	 * paths the key does not cover pass through untouched.
	 *
	 * After pairRenames, deliberately. Renames are recognised on plaintext
	 * hashes -- sealed hashes never repeat -- and a file that turns out to be a
	 * move is never sealed or uploaded at all.
	 */
	private async seal(
		client: Client,
		state: SyncState,
		changes: PendingChange[],
		report: SyncReport,
	): Promise<PendingChange[]> {
		const crypt = client.crypt;
		if (!crypt) return changes;
		const out: PendingChange[] = [];
		for (const c of changes) {
			if (!crypt.covers(c.path)) {
				out.push(c);
				continue;
			}
			try {
				crypt.encryptPath(c.path);
			} catch (err) {
				if (!(err instanceof NameTooLongError)) throw err;
				// Not sent, and said so. Its snapshot entry is left alone, so
				// nothing is deleted on the server because of it.
				report.refused.push({ path: c.path, status: "refused", reason: "name too long to encrypt" });
				continue;
			}
			if (c.op === "put") {
				const sealed = await crypt.seal(c.content!);
				out.push({ ...c, plain: c.hash, hash: await gitHash(sealed), content: sealed });
			} else if (c.op === "move") {
				// pairRenames carried the source's local hash; the server wants
				// its own for the If-Match.
				out.push({ ...c, plain: c.hash, hash: state.files[c.from!]?.hash });
			} else out.push(c);
		}
		return out;
	}

	private async upload(changes: PendingChange[]): Promise<void> {
		const puts = changes.filter((c) => c.op === "put");
		if (puts.length === 0) return;
		const missing = new Set(await this.client().missing(puts.map((c) => c.hash!)));
		for (const c of puts) {
			if (!missing.has(c.hash!)) continue; // the server already has it
			await this.client().putContent(c.hash!, c.content!);
		}
	}

	private async readState(path: string): Promise<FileState | null> {
		try {
			const st = await this.adapter.stat(path);
			if (!st || st.type !== "file") return null;
			const content = await this.adapter.readBinary(path);
			return { hash: await gitHash(content), mtime: st.mtime, size: st.size };
		} catch {
			return null;
		}
	}

	/**
	 * mkdirs creates every missing ancestor of a path.
	 *
	 * Segment by segment, because Obsidian's DataAdapter.mkdir is not
	 * documented as recursive. The old one-level version happened to work for
	 * pulled files only because their parents usually already existed, and the
	 * test shim's mkdir IS recursive -- so the harness could never have caught
	 * it. The rescue folder nests every vault path one level deeper, which is
	 * where a one-level mkdir stops working.
	 */
	private async mkdirs(path: string): Promise<void> {
		const segments = path.split("/").slice(0, -1);
		let built = "";
		for (const seg of segments) {
			if (!seg) continue;
			built = built ? `${built}/${seg}` : seg;
			if (await this.adapter.exists(built)) continue;
			await this.adapter.mkdir(built);
		}
	}

	/** listAll walks the vault via the adapter, which sees everything -- unlike
	 *  vault.getFiles(), which excludes the config directory. */
	private async listAll(dir: string): Promise<string[]> {
		const out: string[] = [];
		const listing = await this.adapter.list(dir);
		const listed = new Set(listing.files);
		for (let f of listing.files) {
			// The server composes every incoming path (NFC) and answers with
			// that spelling, so a decomposed name -- what macOS hands out for a
			// file made outside Obsidian -- never matched its own push result:
			// it was re-pushed every cycle, and once the composed spelling came
			// back from another device the next diff paired the two into a move
			// onto itself, which fails the whole push.
			//
			// Only when the composed spelling opens the same file, as it does on
			// APFS. On a filesystem that keeps the spellings apart, using it
			// would make the file unreadable, and if both exist they are two
			// files.
			const nfc = f.normalize("NFC");
			if (nfc !== f && !listed.has(nfc) && (await this.adapter.exists(nfc))) f = nfc;
			if (skip(f)) continue;
			if (this.keyless && encryptedPath(f)) {
				this.held = true;
				continue;
			}
			out.push(f);
		}
		for (const d of listing.folders) {
			if (this.keyless && encryptedDir(d) && !skipDir(d)) {
				// Not descended into: without the key nothing inside can be
				// sent. Noted, so the user is asked for the key.
				this.held = true;
				continue;
			}
			// A directory is worth descending into if anything under it could
			// sync. skip() answers that for a FILE path; for a directory the
			// inventory ancestors are excluded as files but contain allowed JSON.
			if (skipDir(d) && !inventoryDirectory(d)) continue;
			out.push(...(await this.listAll(d)));
		}
		return out;
	}

	/**
	 * Carry out the choice the user made when told about the pairing hazard,
	 * then sync.
	 *
	 * The asymmetry between "adopt" and "publish" is the point. A push cannot
	 * destroy server content: every version of every server file is already in
	 * git history, so publishing needs no rescue folder. The local vault has no
	 * such safety net, so adopting does. Only the side without a repository
	 * needs rescuing.
	 */
	async resolvePairing(choice: PairingChoice): Promise<SyncReport | null> {
		const state = loadState(this.app);
		// Already resolved, or already synced. Nothing to do but sync.
		if (!isFirstRun(state)) return this.run();

		switch (choice) {
			case "merge":
				// The union, with conflict files: exactly what would have
				// happened without the guard. Keeping base "" is what makes the
				// server three-way merge each collision rather than overwrite.
				break;

			case "publish": {
				// Adopt the server's head as our base WITHOUT pulling. Then the
				// server sees base == head, so no path counts as moved, and every
				// local file applies cleanly over whatever was there.
				//
				// With base "" instead, the server would treat every one of its
				// own paths as moved-since-base and produce a conflict file for
				// each collision -- which is "merge anyway", not "publish".
				//
				// No deletes are synthesised for server-only files. Those stay,
				// and arrive here on a later cycle. Publishing means local wins
				// every COLLISION, not that the server is emptied; a push has no
				// business deleting content it did not put there.
				const head = await this.client().head();
				saveState(this.app, { ...state, base: head, files: {} });
				break;
			}

			case "adopt":
				await this.rescueLocalFiles();
				break;
		}

		this.pairingResolved = true;
		return this.run();
	}

	/**
	 * Move every synced local file into a dated rescue folder, and return its
	 * name.
	 *
	 * Inside the vault, deliberately: the folder therefore syncs, so the rescued
	 * files reach the other devices rather than existing only on the machine
	 * that happened to adopt. Reversible, visible, and it needs no trust in a
	 * backup.
	 *
	 * `listAll` is snapshotted before the first move, so files written into the
	 * rescue folder are never re-read as sources. It also skips dotfiles, which
	 * is why .obsidian is left where it is -- config is not part of this and
	 * moving it would reset the editor.
	 */
	private async rescueLocalFiles(): Promise<string> {
		// Probe for a folder that does not exist yet. A retry after an
		// interrupted adoption, or a second adoption on the same day, would
		// otherwise rename straight over the first rescue and destroy the files
		// this whole mechanism exists to preserve.
		let folder = rescueFolder(new Date());
		for (let n = 1; await this.adapter.exists(folder); n++) {
			if (n > 100) throw new Error(`archivist: cannot find a free rescue folder beside ${folder}`);
			folder = rescueFolder(new Date(), n);
		}

		// Everything except an EARLIER rescue. Those are already rescued, and
		// sweeping them up again would nest them one level deeper on every
		// adoption, burying the thing the folder exists to make findable.
		const paths = (await this.listAll("")).filter((p) => !isRescuePath(p) && !inventoryPathAllowed(p));
		for (const p of paths) {
			const dest = `${folder}/${p}`;
			await this.mkdirs(dest);
			// rename, not read-then-write: it moves the bytes without holding a
			// whole attachment in memory, which matters on a phone.
			await this.adapter.rename(p, dest);
			this.log(`rescued ${p} -> ${dest}`);
		}

		// Remove the directories the move emptied, deepest first.
		//
		// Not cosmetic. The server may hold a FILE at a path that is a
		// DIRECTORY here -- local `notes/a.md` rescued away leaves an empty
		// `notes/`, and if the server has a file called `notes` the pull then
		// calls writeBinary("notes", ...) against a directory and fails. That
		// would break adoption for a perfectly ordinary pairing.
		const dirs = new Set<string>();
		for (const p of paths) {
			const parts = p.split("/");
			for (let i = 1; i < parts.length; i++) dirs.add(parts.slice(0, i).join("/"));
		}
		for (const dir of [...dirs].sort((a, b) => b.length - a.length)) {
			try {
				const listing = await this.adapter.list(dir);
				if (listing.files.length === 0 && listing.folders.length === 0) {
					await this.adapter.rmdir(dir, false);
				}
			} catch {
				// An adapter without rmdir, or a directory already gone. Leaving
				// one behind is untidy, never destructive.
			}
		}

		return folder;
	}

	/**
	 * Discard local state and start over from the server.
	 *
	 * This EMPTIES the sync state, which is exactly the condition the pairing
	 * guard fires on -- never synced, local content, server content. So the
	 * guard is marked resolved first: the user pressing this button has already
	 * made the decision the guard exists to ask about, and asking again turns
	 * the documented escape hatch into a dead end.
	 *
	 * The semantics are safe on their own terms. rebootstrap() deletes nothing:
	 * a local file that differs from the server is moved aside under a conflict
	 * name and the server's version lands at the real path, and a local-only
	 * file is simply pushed on the next cycle.
	 *
	 * The adopted state is saved BEFORE the sync. Saving an empty state and
	 * leaving the rebuild to run() meant a failed first cycle left the device
	 * looking never-synced, and pairingResolved lives only in memory -- so
	 * after a restart the device asked the first-connect question about its
	 * own vault. The vault name is dropped so the cycle adopts the server's:
	 * this is also the documented way to repoint a device on purpose.
	 */
	async forceRebootstrap(): Promise<SyncReport | null> {
		// A cycle in flight would finish by saving the snapshot it loaded,
		// over the one rebuilt here: the disk would hold the server's files
		// while the state still named the old hashes, and the next diff would
		// push every touched file back. Holding `running` also makes any cycle
		// asked for meanwhile wait for the run() below.
		if (this.running) throw new Error("a sync is running; try again in a moment");
		this.running = true;
		try {
			this.pairingResolved = true;
			saveState(this.app, await this.rebootstrap(loadState(this.app)));
		} finally {
			this.running = false;
		}
		return this.run();
	}
}

type PendingChange = {
	path: string;
	op: "put" | "del" | "move";
	/** What the server will hold: for a put, the hash of `content`. */
	hash?: string;
	/** Encrypted paths only: the hash of the plaintext on this device. */
	plain?: string;
	/** Half of a rename across the .encrypted marker: where the file went
	 *  (on the delete) and where it came from (on the put). */
	becomes?: string;
	was?: string;
	mtime?: number;
	size?: number;
	content?: ArrayBuffer;
	/** move only: the path being renamed. */
	from?: string;
};

function toChange(c: PendingChange): Change {
	if (c.op === "del") return { path: c.path, op: "del" };
	if (c.op === "move") return { path: c.path, op: "move", from: c.from, hash: c.hash || undefined };
	// The size on the wire is of what is stored, which for a sealed put is not
	// the size of the file on this device.
	return { path: c.path, op: "put", hash: c.hash, size: c.plain ? c.content!.byteLength : c.size };
}
