import type { App, DataAdapter } from "obsidian";
import { Client, UnknownBaseError, type Change, type Result } from "./client";
import { gitHash } from "./hash";
import { emptyState, isFirstRun, loadState, saveState, type FileState, type SyncState } from "./state";

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
export function skip(path: string): boolean {
	return path.split("/").some((seg) => seg.startsWith("."));
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

	constructor(
		private app: App,
		private client: () => Client,
		private device: () => string,
		private log: Logger = () => {},
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

		// --- pull ------------------------------------------------------------
		// Always before push, so the push is computed against the freshest base
		// we can get.
		try {
			const { head, entries } = await client.changes(state.base);
			let deferred = 0;
			for (const e of entries) {
				if (skip(e.path)) continue;
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
			// A "conflict" result means the server kept its own version and
			// parked ours beside it. Recording our hash as synced would be a
			// lie: the next diff would see no local change, we would never pull
			// the server's version, and this device would sit permanently
			// diverged. Verified -- that is exactly what happened before.
			const byPath = new Map(results.map((r) => [r.path, r.status]));
			for (const c of local) {
				const status = byPath.get(c.path);
				if (status === "conflict" || status === "refused") continue;
				if (c.op === "del") delete state.files[c.path];
				else state.files[c.path] = { hash: c.hash!, mtime: c.mtime!, size: c.size! };
			}

			if (report.conflicts.length > 0) {
				await this.absorbConflicts(state, report.conflicts);
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

		if (await this.adapter.exists(e.path)) {
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

		const content = await this.client().getContent(e.hash!);
		await this.mkdirs(e.path);
		await this.adapter.writeBinary(e.path, content);
		const st = await this.adapter.stat(e.path);
		state.files[e.path] = {
			hash: e.hash!,
			mtime: st?.mtime ?? Date.now(),
			size: st?.size ?? content.byteLength,
		};
		return "applied";
	}

	/**
	 * absorbConflicts brings this device in line after the server refused our
	 * version of a path.
	 *
	 * Two files matter: the path itself, where the server's version now lives,
	 * and the conflict file holding ours. Both are fetched here rather than
	 * waiting for the next pull, because the push already advanced our cursor
	 * past the commit that created them -- so an ordinary pull would never
	 * mention them again.
	 *
	 * Our content is not lost: it is in the conflict file, which is an ordinary
	 * note the user can resolve by editing and deleting.
	 */
	private async absorbConflicts(state: SyncState, conflicts: Result[]): Promise<void> {
		const { files } = await this.client().snapshot();
		const wanted = new Set<string>();
		for (const c of conflicts) {
			wanted.add(c.path);
			if (c.conflictPath) wanted.add(c.conflictPath);
		}
		for (const path of wanted) {
			const entry = files[path];
			if (!entry) {
				// The server has no such path -- it deleted it, or the conflict
				// file was named differently. Drop our record and let the next
				// cycle work it out from a clean diff.
				delete state.files[path];
				continue;
			}
			const cur = await this.readState(path);
			if (cur && cur.hash === entry.hash) {
				state.files[path] = cur;
				continue;
			}
			const content = await this.client().getContent(entry.hash);
			await this.mkdirs(path);
			await this.adapter.writeBinary(path, content);
			const st = await this.adapter.stat(path);
			state.files[path] = {
				hash: entry.hash,
				mtime: st?.mtime ?? Date.now(),
				size: st?.size ?? entry.size,
			};
		}
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
			if (skip(path)) continue;
			const cur = await this.readState(path);
			if (cur && cur.hash === entry.hash) {
				state.files[path] = cur;
				continue;
			}
			if (cur) {
				// Differs locally. Leave it alone and let the push resolve it
				// with a real merge rather than clobbering it here.
				continue;
			}
			const content = await this.client().getContent(entry.hash);
			await this.mkdirs(path);
			await this.adapter.writeBinary(path, content);
			const st = await this.adapter.stat(path);
			state.files[path] = {
				hash: entry.hash,
				mtime: st?.mtime ?? Date.now(),
				size: st?.size ?? entry.size,
			};
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
		if (!isFirstRun(state)) {
			for (const path of Object.keys(state.files)) {
				if (!seen.has(path)) out.push({ path, op: "del" });
			}
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

	private async mkdirs(path: string): Promise<void> {
		const dir = path.split("/").slice(0, -1).join("/");
		if (dir && !(await this.adapter.exists(dir))) {
			await this.adapter.mkdir(dir);
		}
	}

	/** listAll walks the vault via the adapter, which sees everything -- unlike
	 *  vault.getFiles(), which excludes the config directory. */
	private async listAll(dir: string): Promise<string[]> {
		const out: string[] = [];
		const listing = await this.adapter.list(dir);
		for (const f of listing.files) {
			if (!skip(f)) out.push(f);
		}
		for (const d of listing.folders) {
			if (skip(d)) continue;
			out.push(...(await this.listAll(d)));
		}
		return out;
	}

	/** Discard local state and start over from the server. */
	async forceRebootstrap(): Promise<void> {
		saveState(this.app, emptyState());
		await this.run();
	}
}

type PendingChange = {
	path: string;
	op: "put" | "del";
	hash?: string;
	mtime?: number;
	size?: number;
	content?: ArrayBuffer;
};

function toChange(c: PendingChange): Change {
	return c.op === "del"
		? { path: c.path, op: "del" }
		: { path: c.path, op: "put", hash: c.hash, size: c.size };
}
