import type { App, DataAdapter } from "obsidian";
import { Client, UnknownBaseError, type Change, type Result } from "./client";
import { pairRenames, type Pending } from "./scopes";
import { gitHash } from "./hash";
import {
	checkVault,
	emptyState,
	isFirstRun,
	loadState,
	saveState,
	type FileState,
	type SyncState,
} from "./state";
import { PairingHazardError, isRescuePath, rescueFolder, type PairingChoice } from "./pairing";
import {
	CONFIG_DIR,
	configSyncable,
	DEFAULT_CONFIG_SYNC,
	type ConfigSyncSettings,
} from "./config-sync";

export type SyncReport = {
	pulled: number;
	pushed: number;
	conflicts: Result[];
	rebootstrapped: boolean;
};

export type Logger = (msg: string, ...rest: unknown[]) => void;

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
	const dot = path.lastIndexOf(".");
	const suffix = `.conflict-${safe}-${stamp}-local`;
	return dot > slash ? path.slice(0, dot) + suffix + path.slice(dot) : path + suffix;
}

export function skip(
	path: string,
	config: ConfigSyncSettings = DEFAULT_CONFIG_SYNC,
): boolean {
	if (!path.split("/").some((seg) => seg.startsWith("."))) return false;
	return !configSyncable(path, config);
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

	constructor(
		private app: App,
		private client: () => Client,
		private device: () => string,
		private log: Logger = () => {},
		/**
		 * Read fresh on every call, not captured: the user can change the level
		 * mid-session, and a captured value would keep syncing at the old one
		 * until Obsidian restarted.
		 */
		private config: () => ConfigSyncSettings = () => DEFAULT_CONFIG_SYNC,
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
		const report: SyncReport = { pulled: 0, pushed: 0, conflicts: [], rebootstrapped: false };

		// Identity first, BEFORE anything is read or written.
		//
		// If the settings point at a different vault's server than this device
		// adopted, syncing would merge two unrelated vaults into both -- and the
		// damage lands on the very first cycle, so a check anywhere later is too
		// late. Throwing here surfaces it as a Notice and leaves both vaults
		// untouched.
		const idx = await client.index();
		checkVault(state, idx.vault);

		// Then the first-connect hazard, which the identity check cannot see:
		// it compares an ADOPTED vault name, and there is not one yet.
		//
		// Deliberately before the vault name is adopted and saved, so a refusal
		// leaves NOTHING persisted -- not one byte on disk and not one field in
		// local storage. See src/pairing.ts for what goes wrong without it.
		if (!this.pairingResolved && isFirstRun(state)) {
			const serverHead = await client.head();
			if (serverHead !== "") {
				// NOTES only, never config. Every Obsidian vault has a
				// .obsidian/ directory, so counting config here would make a
				// brand-new empty vault with config sync enabled refuse to
				// onboard -- and there would be no way to turn it off, because
				// the setting lives behind the sync that just refused.
				//
				// Config colliding is not the hazard either: allowlisted JSON
				// is merged by key and everything else in there is refused, so
				// there is no silent union to prevent. The hazard is two
				// unrelated sets of NOTES becoming one.
				const localFiles = (await this.listAll("")).filter(
					(p) => !p.startsWith(CONFIG_DIR + "/"),
				).length;
				if (localFiles > 0) throw new PairingHazardError(localFiles, serverHead);
			}
		}

		if (!state.vault && idx.vault) {
			// First run, or state from a version that did not record it. Adopt.
			state.vault = idx.vault;
			saveState(this.app, state);
		}

		// --- pull ------------------------------------------------------------
		// Always before push, so the push is computed against the freshest base
		// we can get.
		try {
			const { head, entries } = await client.changes(state.base);
			let deferred = 0;
			for (const e of entries) {
				if (skip(e.path, this.config())) continue;
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
			state = await this.rebootstrap();
			report.rebootstrapped = true;
		}

		// --- push ------------------------------------------------------------
		const local = await this.localChanges(state);
		if (local.length > 0) {
			await this.upload(local);
			const { head, results } = await client.push(state.base, this.device(), local.map(toChange));
			state.base = head;
			for (const r of results) {
				if (r.status === "conflict") report.conflicts.push(r);
			}
			report.pushed = local.length;

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
					} else state.files[c.path] = { hash: c.hash!, mtime: c.mtime!, size: c.size! };
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
				state.files[f.path] = await this.materialise(f.path, f.hash, f.size);
			}
		}

		saveState(this.app, state);
		return report;
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
			if (!known || !cur || cur.hash !== known.hash) {
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
			if (cur && (!known || cur.hash !== known.hash)) {
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
		const content = await this.client().getContent(hash);
		await this.mkdirs(path);
		await this.adapter.writeBinary(path, content);
		const st = await this.adapter.stat(path);
		return {
			hash,
			mtime: st?.mtime ?? Date.now(),
			size: st?.size ?? fallbackSize ?? content.byteLength,
		};
	}

	/**
	 * rebootstrap adopts the server's state wholesale.
	 *
	 * It deliberately DELETES NOTHING. We have no valid base, so a local file
	 * absent from the server might be one we created offline -- the next push
	 * will send it. Worst case is a duplicate; the alternative risks loss.
	 */
	private async rebootstrap(): Promise<SyncState> {
		const { head, files } = await this.client().snapshot();
		const state: SyncState = { base: head, files: {} };
		for (const [path, entry] of Object.entries(files)) {
			if (skip(path, this.config())) continue;
			const cur = await this.readState(path);
			if (cur && cur.hash === entry.hash) {
				state.files[path] = cur;
				continue;
			}
			if (cur) {
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
				const asideStat = await this.adapter.stat(aside);
				state.files[aside] = {
					hash: cur.hash,
					mtime: asideStat?.mtime ?? Date.now(),
					size: asideStat?.size ?? cur.size,
				};
				this.log(`no common ancestor for ${path}; kept ours as ${aside}`);
				state.files[path] = await this.materialise(path, entry.hash, entry.size);
				continue;
			}
			state.files[path] = await this.materialise(path, entry.hash, entry.size);
		}
		return state;
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
			if (known && known.hash === hash) {
				// Touched but unchanged: refresh the metadata so we stop
				// re-hashing it every cycle.
				state.files[path] = { hash, mtime: st.mtime, size: st.size };
				continue;
			}
			out.push({ path, op: "put", hash, mtime: st.mtime, size: st.size, content });
		}

		// A path in the snapshot and absent from disk is a deletion -- and this
		// is the only way we can know that, which is precisely why a client
		// with no snapshot must never delete.
		//
		// EXCEPT for config paths this device no longer syncs. Turning the level
		// down from "plugins" to "files", or switching one plugin's opt-in off,
		// removes those paths from listAll -- and without this guard they would
		// look exactly like local deletions and be pushed as such, wiping the
		// shared config off the server and out of every other device. A device
		// choosing to stop RECEIVING config must never thereby delete it.
		if (!isFirstRun(state)) {
			for (const path of Object.keys(state.files)) {
				if (seen.has(path)) continue;
				if (skip(path, this.config())) {
					// No longer ours to track. Forget it without telling the
					// server anything; if the level is turned back up, the next
					// pull re-materialises it from the server's copy.
					delete state.files[path];
					continue;
				}
				// The hash comes along so pairRenames can recognise this as
				// half of a rename. It is never sent -- toChange drops it.
				out.push({ path, op: "del", hash: state.files[path]?.hash });
			}
		}
		// A deletion plus an addition of identical content is a rename. Folding
		// them into one move is what lets a token with write but not delete
		// rename a note; anything ambiguous is left as del+put.
		return pairRenames(out as Pending[]) as PendingChange[];
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

	/** Whether a directory is the config directory or lives inside it. */
	private mayHoldConfig(dir: string): boolean {
		if (this.config().level === "files") return false;
		return dir === CONFIG_DIR || dir.startsWith(CONFIG_DIR + "/");
	}

	/** listAll walks the vault via the adapter, which sees everything -- unlike
	 *  vault.getFiles(), which excludes the config directory. */
	private async listAll(dir: string): Promise<string[]> {
		const out: string[] = [];
		const listing = await this.adapter.list(dir);
		for (const f of listing.files) {
			if (!skip(f, this.config())) out.push(f);
		}
		for (const d of listing.folders) {
			// A directory is worth descending into if anything under it could
			// sync. skip() answers that for a FILE path; for a directory the
			// config directory is the one case where the directory itself is
			// excluded and its contents are not.
			if (skip(d, this.config()) && !this.mayHoldConfig(d)) continue;
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
		const paths = (await this.listAll("")).filter((p) => !isRescuePath(p));
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
	 */
	async forceRebootstrap(): Promise<void> {
		saveState(this.app, emptyState());
		this.pairingResolved = true;
		await this.run();
	}
}

type PendingChange = {
	path: string;
	op: "put" | "del" | "move";
	hash?: string;
	mtime?: number;
	size?: number;
	content?: ArrayBuffer;
	/** move only: the path being renamed. */
	from?: string;
};

function toChange(c: PendingChange): Change {
	if (c.op === "del") return { path: c.path, op: "del" };
	if (c.op === "move") return { path: c.path, op: "move", from: c.from };
	return { path: c.path, op: "put", hash: c.hash, size: c.size };
}
