import { requestUrl, type RequestUrlResponse } from "obsidian";

export type Entry = { hash: string; size: number };
export type Change = {
	path: string;
	/**
	 * move renames a path and carries `from` instead of a hash: the content is
	 * already on the server. It needs only the write scope, where the del+put a
	 * rename used to take needed delete as well.
	 */
	op: "put" | "del" | "move";
	hash?: string;
	size?: number;
	from?: string;
};
export type Result = {
	path: string;
	status: "applied" | "merged" | "conflict" | "refused";
	/** What the server now holds at `path`. For "merged" this is the merged
	 *  content, which is NOT what we sent. */
	hash?: string;
	size?: number;
	conflictPath?: string;
	conflictHash?: string;
	reason?: string;
};

/** Stable error codes from the server. Branch on these, never on the message:
 *  messages are for humans and get reworded. */
export const Code = {
	Unauthorized: "unauthorized",
	UnknownBase: "unknown_base",
	InvalidPath: "invalid_path",
	MissingContent: "missing_content",
	HashMismatch: "hash_mismatch",
	TooLarge: "too_large",
	Malformed: "malformed",
	NotFound: "not_found",
	DuplicatePath: "duplicate_path",
	Internal: "internal",
	/** A precondition failure: the head named is no longer the tip. */
	StaleHead: "stale_head",
} as const;

/** One point in a path's history, as /v1/history returns it. */
export type Revision = {
	commit: string;
	short: string;
	when: string;
	message: string;
	size: number;
	hash: string;
	/** The path was absent at this commit. Cannot be opened through /v1/at,
	 *  so a session's representative must never be one of these. */
	deleted?: boolean;
	/** Who made the change: a device name, or "server" for a vault-side edit.
	 *  Absent on commits predating the convention. */
	device?: string;
	/** The intermediary, when there was one — the relay, an agent. */
	via?: string;
	/** Lines against the previous revision. Both absent for a binary or
	 *  oversized file; see `created`, which distinguishes "nothing to compare"
	 *  from "nothing changed". */
	added?: number;
	removed?: number;
	/** This revision introduced the file. */
	created?: boolean;
};

/** A named restore point. */
export type Pin = {
	id: string;
	name: string;
	/** Absent on a vault-wide pin. */
	path?: string;
	created: string;
	/** The commit that introduced this pin: the tree it names. */
	commit: string;
	/** False when the snapshot no longer holds the path -- a prune can drop
	 *  the blobs of a deleted path while the pin entry survives. Shown, but
	 *  not openable. */
	available: boolean;
	/** The id appears more than once in the pin file, so which revision it
	 *  names is ambiguous. Shown, never opened. */
	duplicate?: boolean;
};

/** An error carrying the server's stable code. */
export class ServerError extends Error {
	constructor(
		readonly code: string,
		message: string,
		readonly status: number,
	) {
		super(message);
	}
}

/** Thrown when the server does not recognise our cursor. The only recovery is
 *  to re-bootstrap from /snapshot -- never a retry. */
export class UnknownBaseError extends ServerError {
	constructor(message = "server does not recognise our base; re-bootstrap required") {
		super(Code.UnknownBase, message, 409);
	}
}

export class Client {
	constructor(
		private baseUrl: string,
		private token: string,
		/**
		 * Which vault on that server. A SEPARATE field from the URL, not baked
		 * into it: a picker needs GET /v1/vaults at the server root, and it
		 * cannot reach that if the vault is part of the base URL.
		 */
		private vault: string,
	) {}

	/**
	 * The ONE place a request URL is built, called only by call(). That is what
	 * makes switching to path-qualified addressing a one-line change here --
	 * and what makes switching vault on a device one settings field.
	 */
	private url(p: string): string {
		const base = this.baseUrl.replace(/\/+$/, "");
		if (!this.vault) return base + p;
		// A vault called "My Own Vault" is legal and must be encoded.
		return base + "/" + encodeURIComponent(this.vault) + p;
	}

	/** call() with no vault prefix, for the server-root routes. */
	private async callRoot(method: string, path: string): Promise<RequestUrlResponse> {
		const saved = this.vault;
		this.vault = "";
		try {
			return await this.call(method, path);
		} finally {
			this.vault = saved;
		}
	}

	/**
	 * What this token opens. A server-root route, so it is reachable before a
	 * vault has been chosen -- which is the whole reason the vault is a
	 * separate settings field.
	 */
	async listVaults(): Promise<{
		vaults: string[];
		canCreate: boolean;
		/** Verbs this token holds. Absent on servers older than scoped tokens. */
		scopes?: string[];
		/** What the token is called on the server. Never a secret. */
		label?: string;
		/** Vaults the SERVER protects with step-up. Absent on older servers. */
		protectedVaults?: string[];
		/** What THIS token decided about them. Absent on older servers. */
		requiresStepUpAuth?: string[];
	}> {
		const res = await this.callRoot("GET", "/v1/vaults");
		return res.json as {
			vaults: string[];
			canCreate: boolean;
			scopes?: string[];
			label?: string;
			protectedVaults?: string[];
			requiresStepUpAuth?: string[];
		};
	}

	/**
	 * history lists revisions that touched a path, newest first.
	 *
	 * `before` is an exclusive cursor from a previous page's `next`. Paging is
	 * by commit rather than offset because history grows at the newest end: an
	 * offset would shift between pages and skip or repeat a revision.
	 */
	async history(
		path: string,
		limit = 50,
		before?: string,
	): Promise<{ revisions: Revision[]; hasMore: boolean; next?: string }> {
		let q = `/v1/history?path=${encodeURIComponent(path)}&limit=${limit}`;
		if (before) q += `&before=${encodeURIComponent(before)}`;
		const j = (await this.call("GET", q)).json;
		return {
			revisions: (j?.revisions ?? []) as Revision[],
			hasMore: Boolean(j?.hasMore),
			next: j?.next as string | undefined,
		};
	}

	/**
	 * readAt fetches a path as it was at a revision. Never touches the working
	 * tree -- inspecting an old version is not a restore.
	 *
	 * The path is escaped SEGMENT BY SEGMENT: it sits in path position, where
	 * encodeURIComponent would turn every separator into %2F, and a raw
	 * interpolation would break on the "#" and "?" that ordinary note titles
	 * contain.
	 */
	async readAt(rev: string, path: string): Promise<ArrayBuffer> {
		const escaped = path.split("/").map(encodeURIComponent).join("/");
		const res = await this.call("GET", `/v1/at/${encodeURIComponent(rev)}/${escaped}`);
		return res.arrayBuffer;
	}

	/** pins lists named restore points. `path` filters to one file; "*" asks
	 *  for vault-wide pins only; omitted returns every pin. */
	async pins(path?: string): Promise<Pin[]> {
		const q = path ? `/v1/pins?path=${encodeURIComponent(path)}` : "/v1/pins";
		const j = (await this.call("GET", q)).json;
		return (j?.pins ?? []) as Pin[];
	}

	/**
	 * pin names a restore point at `expectedHead`.
	 *
	 * expectedHead is required by the server: a pin is a claim about a
	 * specific tree, so pinning against a head that has moved would silently
	 * name an older state than the note on screen. A StaleHead error means
	 * flush and retry once.
	 */
	async pin(name: string, expectedHead: string, path?: string): Promise<{ id: string; head: string }> {
		const j = (await this.call("POST", "/v1/pin", { name, path, expectedHead })).json;
		return { id: j?.id as string, head: j?.head as string };
	}

	private async call(
		method: string,
		path: string,
		body?: unknown,
		binary?: ArrayBuffer,
	): Promise<RequestUrlResponse> {
		// requestUrl rather than fetch: the Obsidian API documents it as HTTP
		// "without any CORS restrictions", and it works on mobile. throw:false
		// because 409 is a protocol signal we branch on, not a failure.
		const res = await requestUrl({
			url: this.url(path),
			method,
			headers: {
				Authorization: `Bearer ${this.token}`,
				...(body !== undefined ? { "Content-Type": "application/json" } : {}),
			},
			body: binary ?? (body !== undefined ? JSON.stringify(body) : undefined),
			throw: false,
		});
		if (res.status < 400) return res;

		// Prefer the server's code over the status. A 409 used to be assumed to
		// mean unknown-base, which was only ever true because there was exactly
		// one thing a 409 could mean -- a fragile assumption to leave in a
		// client that outlives the server it was written against.
		const code: string | undefined = res.json?.error?.code;
		const message: string = res.json?.error?.message ?? res.text.slice(0, 200);

		if (code === Code.UnknownBase) throw new UnknownBaseError(message);
		if (code) throw new ServerError(code, message, res.status);

		// No envelope: an older server, or something in front of it.
		if (res.status === 409) throw new UnknownBaseError(message);
		throw new ServerError(
			res.status === 401 ? Code.Unauthorized : Code.Internal,
			`${method} ${path} -> ${res.status}: ${message}`,
			res.status,
		);
	}

	// wait long-polls: the server holds the request until the head moves past
	// `since`, or `timeoutSeconds` elapses.
	//
	// This is how the plugin learns about remote changes without polling every
	// few minutes. It is long-polling rather than the server's SSE stream
	// because requestUrl returns a complete response rather than a stream, and
	// requestUrl is the only transport that works on both desktop and iOS --
	// native fetch would need CORS, and the preflight for an Authorization
	// header is an unauthenticated OPTIONS the server answers with 401.
	//
	// `changed` is advisory. The caller still asks what actually moved, so a
	// spurious wake costs one wasted sync and a missed one is caught by the
	// interval.
	async wait(since: string, timeoutSeconds: number): Promise<{ head: string; changed: boolean }> {
		const res = await this.call(
			"GET",
			`/v1/wait?since=${encodeURIComponent(since)}&timeout=${timeoutSeconds}`,
		);
		return res.json as { head: string; changed: boolean };
	}

	// index is GET /v1: what the server is, and WHICH VAULT it serves.
	async index(): Promise<{ service: string; version: string; protocol: number; vault: string }> {
		const res = await this.call("GET", "/v1");
		return res.json as { service: string; version: string; protocol: number; vault: string };
	}

	async head(): Promise<string> {
		return (await this.call("GET", "/v1/head")).json.head as string;
	}

	async snapshot(): Promise<{ head: string; files: Record<string, Entry> }> {
		const j = (await this.call("GET", "/v1/snapshot")).json;
		return { head: j.head as string, files: (j.files ?? {}) as Record<string, Entry> };
	}

	async changes(since: string): Promise<{ head: string; entries: Change[] }> {
		const j = (await this.call("GET", `/v1/changes?since=${encodeURIComponent(since)}`)).json;
		return { head: j.head as string, entries: (j.entries ?? []) as Change[] };
	}

	/** Returns the subset the server does not have, so we upload only those. */
	async missing(hashes: string[]): Promise<string[]> {
		if (hashes.length === 0) return [];
		const j = (await this.call("POST", "/v1/have", { hashes })).json;
		return (j.missing ?? []) as string[];
	}

	async putContent(hash: string, content: ArrayBuffer): Promise<void> {
		await this.call("PUT", `/v1/content/${hash}`, undefined, content);
	}

	async getContent(hash: string): Promise<ArrayBuffer> {
		return (await this.call("GET", `/v1/content/${hash}`)).arrayBuffer;
	}

	async push(
		base: string,
		device: string,
		changes: Change[],
	): Promise<{ head: string; results: Result[] }> {
		const j = (await this.call("POST", "/v1/push", { base, device, changes })).json;
		return { head: j.head as string, results: (j.results ?? []) as Result[] };
	}
}
