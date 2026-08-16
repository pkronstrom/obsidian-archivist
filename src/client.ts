import { requestUrl, type RequestUrlResponse } from "obsidian";

export type Entry = { hash: string; size: number };
export type Change = { path: string; op: "put" | "del"; hash?: string; size?: number };
export type Result = {
	path: string;
	status: "applied" | "merged" | "conflict" | "refused";
	conflictPath?: string;
	reason?: string;
};

/** Thrown when the server does not recognise our cursor. The only recovery is
 *  to re-bootstrap from /snapshot -- never a retry. */
export class UnknownBaseError extends Error {
	constructor() {
		super("server does not recognise our base; re-bootstrap required");
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
		if (res.status === 401) throw new Error("unauthorized: check the token");
		if (res.status === 409) throw new UnknownBaseError();
		if (res.status >= 400) {
			throw new Error(`${method} ${path} -> ${res.status}: ${res.text.slice(0, 200)}`);
		}
		return res;
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
