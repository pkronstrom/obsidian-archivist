import { requestUrl, type RequestUrlResponse } from "obsidian";

export type Entry = { hash: string; size: number };
export type Change = { path: string; op: "put" | "del"; hash?: string; size?: number };
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
} as const;

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
	) {}

	private url(p: string): string {
		return this.baseUrl.replace(/\/+$/, "") + p;
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
