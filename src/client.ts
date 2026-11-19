import { requestUrl, type RequestUrlResponse } from "obsidian";
import type { VaultCrypt } from "./crypt";

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

export const PROTOCOL_VERSION = 2;
export type ServerIndex = {
	service: string;
	version: string;
	protocol: number;
	vault: string;
	/** The server stores only what a client encrypted, and holds no key. */
	encrypted: boolean;
	/** What a key is verified against. "" until the first device installs it. */
	keyCheck: string;
};

export function requireCurrentIndex(value: unknown): ServerIndex {
	if (!value || typeof value !== "object") {
		throw new Error("server response is missing current index metadata");
	}
	const index = value as Record<string, unknown>;
	if (index.protocol !== PROTOCOL_VERSION) {
		throw new Error(
			`server protocol ${String(index.protocol)} is incompatible; this plugin requires protocol ${PROTOCOL_VERSION}`,
		);
	}
	if (index.vault === "") throw new Error("server response has no vault identity");
	if (
		index.service !== "archivist" ||
		typeof index.version !== "string" ||
		!index.version ||
		typeof index.vault !== "string" ||
		typeof index.encrypted !== "boolean" ||
		typeof index.keyCheck !== "string"
	) {
		throw new Error("server response is missing current index metadata");
	}
	return index as ServerIndex;
}

function isStringArray(value: unknown): value is string[] {
	return Array.isArray(value) && value.every((item) => typeof item === "string");
}

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
	/** A second attempt to install an encrypted vault's key check. */
	KeyCheckSet: "key_check_set",
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

/** A path that history still holds but the vault no longer shows. */
export type DeletedPath = {
	path: string;
	when: string;
	/** The commit whose tree still HOLDS the content — the parent of the
	 *  deletion. Reading the deleting commit itself would 404, since that is
	 *  precisely where the file stopped existing. */
	revision: string;
	short: string;
	deletedIn: string;
	device?: string;
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
		/**
		 * Set for an encrypted vault. Then this class is the boundary: paths
		 * and downloaded content are plaintext above it and ciphertext below,
		 * so the rest of the plugin never sees an encrypted name.
		 *
		 * Uploads are the exception. putContent takes bytes that are already
		 * sealed, because the caller needs their hash before it can upload.
		 */
		readonly crypt?: VaultCrypt,
	) {}

	private enc(path: string): string {
		return this.crypt ? this.crypt.encryptPath(path) : path;
	}

	/** undefined for a name this key did not produce. Callers drop those: a
	 *  file we cannot name is one we cannot sync, and must not delete. */
	private dec(path: string): string | undefined {
		if (!this.crypt) return path;
		try {
			return this.crypt.decryptPath(path);
		} catch {
			return undefined;
		}
	}

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
		/** Verbs this token holds. */
		scopes: string[];
		/** What the token is called on the server. Never a secret. */
		label?: string;
		/** Vaults this token must unlock with a code. */
		stepUp: string[];
	}> {
		const res = await this.callRoot("GET", "/v1/vaults");
		const json = res.json;
		if (
			!isStringArray(json?.vaults) ||
			!isStringArray(json?.scopes) ||
			typeof json?.canCreate !== "boolean" ||
			(json?.label !== undefined && typeof json.label !== "string") ||
			!isStringArray(json?.stepUp)
		) {
			throw new Error("server response is missing current scoped-token metadata");
		}
		return {
			vaults: json.vaults,
			canCreate: json.canCreate,
			scopes: json.scopes,
			label: json.label,
			stepUp: json.stepUp,
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
		let q = `/v1/history?path=${encodeURIComponent(this.enc(path))}&limit=${limit}`;
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
		const escaped = this.enc(path).split("/").map(encodeURIComponent).join("/");
		const res = await this.call("GET", `/v1/at/${encodeURIComponent(rev)}/${escaped}`);
		return this.crypt ? this.crypt.open(res.arrayBuffer) : res.arrayBuffer;
	}

	/** deleted lists paths history holds that head does not, newest first. */
	async deleted(): Promise<DeletedPath[]> {
		const j = (await this.call("GET", "/v1/deleted")).json;
		const out: DeletedPath[] = [];
		for (const d of (j?.deleted ?? []) as DeletedPath[]) {
			const path = this.dec(d.path);
			if (path !== undefined) out.push({ ...d, path });
		}
		return out;
	}

	/** pins lists named restore points. `path` filters to one file; "*" asks
	 *  for vault-wide pins only; omitted returns every pin. */
	async pins(path?: string): Promise<Pin[]> {
		// The server keeps pin names as plaintext in the vault, so an encrypted
		// vault has no pins.
		if (this.crypt) return [];
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
		if (this.crypt) throw new Error("pins are not available in an encrypted vault");
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
		//
		// errorBody(), not res.json: on a body that is not JSON the getter
		// THROWS, and it throws from the branch whose whole job is to explain a
		// failure. A plain-text 400 from Go's own header validation -- what a
		// token pasted with a stray newline produces -- surfaced to the user as
		// "JSON Parse error: Unable to parse JSON string", naming neither the
		// status nor the route. The status is the diagnosis; never lose it.
		const envelope = errorBody(res);
		const code: string | undefined = envelope?.error?.code;
		const message: string = envelope?.error?.message ?? errorText(res);

		if (code === Code.UnknownBase) throw new UnknownBaseError(message);
		if (code) throw new ServerError(code, message, res.status);

		// No envelope: typically a proxy or gateway response.
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
	async index(): Promise<ServerIndex> {
		const res = await this.call("GET", "/v1");
		return requireCurrentIndex(res.json as ServerIndex);
	}

	async head(): Promise<string> {
		return (await this.call("GET", "/v1/head")).json.head as string;
	}

	async snapshot(): Promise<{ head: string; files: Record<string, Entry> }> {
		const j = (await this.call("GET", "/v1/snapshot")).json;
		const raw = (j.files ?? {}) as Record<string, Entry>;
		if (!this.crypt) return { head: j.head as string, files: raw };
		const files: Record<string, Entry> = {};
		for (const [p, e] of Object.entries(raw)) {
			const path = this.dec(p);
			if (path !== undefined) files[path] = e;
		}
		return { head: j.head as string, files };
	}

	async changes(since: string): Promise<{ head: string; entries: Change[] }> {
		const j = (await this.call("GET", `/v1/changes?since=${encodeURIComponent(since)}`)).json;
		const raw = (j.entries ?? []) as Change[];
		if (!this.crypt) return { head: j.head as string, entries: raw };
		const entries: Change[] = [];
		for (const e of raw) {
			const path = this.dec(e.path);
			const from = e.from === undefined ? undefined : this.dec(e.from);
			if (path === undefined || (e.from !== undefined && from === undefined)) continue;
			entries.push({ ...e, path, from });
		}
		return { head: j.head as string, entries };
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

	/** Plaintext, whatever the vault kind. `hash` names what the server holds,
	 *  which for an encrypted vault is the ciphertext. */
	async getContent(hash: string): Promise<ArrayBuffer> {
		const body = (await this.call("GET", `/v1/content/${hash}`)).arrayBuffer;
		return this.crypt ? this.crypt.open(body) : body;
	}

	/** Install the value later devices verify their key against. The server
	 *  accepts it once; a second device gets Code.KeyCheckSet and must be
	 *  given the existing key instead. */
	async putKeyCheck(check: string): Promise<void> {
		await this.call("PUT", "/v1/keycheck", { check });
	}

	async push(
		base: string,
		device: string,
		changes: Change[],
	): Promise<{ head: string; results: Result[] }> {
		const sent = this.crypt
			? changes.map((c) => ({ ...c, path: this.enc(c.path), from: c.from === undefined ? undefined : this.enc(c.from) }))
			: changes;
		const j = (await this.call("POST", "/v1/push", { base, device, changes: sent })).json;
		const raw = (j.results ?? []) as Result[];
		if (!this.crypt) return { head: j.head as string, results: raw };
		// Every result path is one we just encrypted, so it decrypts from cache.
		const results = raw.map((r) => ({ ...r, path: this.dec(r.path) ?? r.path }));
		return { head: j.head as string, results };
	}
}

/**
 * The error envelope, or undefined when the body is not one.
 *
 * `RequestUrlResponse.json` is a GETTER that parses on access and throws on
 * anything that is not JSON -- a proxy's HTML, an empty body, Go's plain-text
 * 400. Reading it unguarded turns every such response into a parse error.
 */
function errorBody(res: RequestUrlResponse): { error?: { code?: string; message?: string } } | undefined {
	try {
		return res.json as { error?: { code?: string; message?: string } } | undefined;
	} catch {
		return undefined;
	}
}

/** The body as text, for a response with no envelope. Also defensive: an
 *  empty or binary body must not become an exception in the error path. */
function errorText(res: RequestUrlResponse): string {
	try {
		return res.text.slice(0, 200);
	} catch {
		return "";
	}
}
