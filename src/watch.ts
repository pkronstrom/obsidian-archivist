import type { Client } from "./client";

/**
 * Watcher keeps a long-poll open so remote changes arrive in about a second
 * instead of waiting for the next interval tick.
 *
 * Why long-polling and not the server's SSE stream: Obsidian's `requestUrl` is
 * the only HTTP transport that works on both desktop and iOS, and it returns a
 * complete response rather than a stream. Native fetch would need CORS, and the
 * preflight for an Authorization header is an unauthenticated OPTIONS that the
 * server answers with 401. So the request would fail before it began.
 *
 * The cost is the same either way: one idle connection, no traffic while
 * nothing happens.
 *
 * The loop is deliberately NOT the source of truth. It only decides *when* to
 * sync; what actually changed is worked out by the sync itself. A spurious wake
 * costs one wasted cycle and a missed one is caught by the interval, so the
 * watcher can be stopped and started at will.
 */
export class Watcher {
	private running = false;
	private generation = 0;
	private backoff = 1000;

	constructor(
		private client: () => Client | null,
		private cursor: () => string,
		private onChange: () => Promise<void>,
		private log: (msg: string, ...rest: unknown[]) => void,
		private timeoutSeconds = 60,
	) {}

	get active(): boolean {
		return this.running;
	}

	start(): void {
		if (this.running) return;
		this.running = true;
		const mine = ++this.generation;
		void this.loop(mine);
	}

	/**
	 * Stop waiting. The in-flight request is abandoned rather than cancelled:
	 * requestUrl exposes no abort, so the loop checks its generation on return
	 * and discards a late answer. The server drops the handler when the
	 * connection goes, and it times out on its own regardless.
	 */
	stop(): void {
		this.running = false;
		this.generation++;
	}

	private async loop(mine: number): Promise<void> {
		while (this.running && mine === this.generation) {
			const client = this.client();
			if (!client) {
				this.running = false;
				return;
			}

			try {
				const res = await client.wait(this.cursor(), this.timeoutSeconds);
				if (mine !== this.generation) return; // stopped while we waited
				this.backoff = 1000;
				if (res.changed) {
					this.log("remote change, syncing");
					await this.onChange();
				}
			} catch (err) {
				if (mine !== this.generation) return;
				// A dropped long-poll is ordinary: phones change network, proxies
				// recycle idle connections. Back off so a server that is down
				// does not turn into a hot loop, and let the interval carry sync
				// in the meantime.
				this.log("wait failed, backing off", this.backoff, err);
				await sleep(this.backoff);
				this.backoff = Math.min(this.backoff * 2, 60000);
			}
		}
	}
}

function sleep(ms: number): Promise<void> {
	return new Promise((r) => setTimeout(r, ms));
}
