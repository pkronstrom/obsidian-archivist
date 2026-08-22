/**
 * Sync scheduling: wait for typing to stop, but never wait forever.
 *
 * The plugin used a plain 2-second trailing debounce. That already waited for a
 * quiet gap -- the gap was simply far too short. Two seconds is a pause
 * mid-sentence, so a note drafted over an hour pushed every time the writer
 * stopped to think. Measured on the live vault: one 50-minute session produced
 * 250 commits, which makes note_history useless for exactly the note being
 * worked on.
 *
 * Raising the threshold alone introduces the opposite failure: type
 * continuously with no gap longer than the threshold and nothing ever syncs. So
 * there are two clocks. The quiet timer restarts on every edit; the ceiling
 * starts at the FIRST edit of a burst and is never extended. Whichever expires
 * first wins.
 *
 * Timers and the clock are injected so this is testable without waiting in real
 * time, and so the file stays free of Obsidian imports.
 */
export type SyncSchedulerOptions = {
	/** Quiet period after the last edit before syncing. */
	quietMs: number;
	/** Longest a burst may defer a sync, measured from its first edit. */
	maxWaitMs: number;
	/**
	 * Performs the sync. May be async; the scheduler tracks the returned
	 * promise so a caller that must wait for completion -- the quit hook -- can.
	 */
	run: () => Promise<void> | void;
	setTimer: (fn: () => void, ms: number) => unknown;
	clearTimer: (handle: unknown) => void;
};

export type SyncScheduler = {
	/** An edit happened. */
	schedule: () => void;
	/**
	 * Sync now.
	 *
	 * Resolves when the sync started by this call has finished, so the quit
	 * hook can hand the promise to Obsidian rather than letting the work run
	 * detached. With nothing pending it resolves once any in-flight sync ends.
	 *
	 * `force` syncs even with nothing pending. That is what the blur handler
	 * needs: blurring should pull remote changes regardless of whether this
	 * device has edits waiting.
	 */
	flush: (opts?: { force?: boolean }) => Promise<void>;
	/**
	 * Drop pending timers without syncing.
	 *
	 * Does NOT abort a sync already running -- there is no way to, and a
	 * half-applied sync would be worse than a completed one. Used on unload.
	 */
	cancel: () => void;
};

export function createSyncScheduler(opts: SyncSchedulerOptions): SyncScheduler {
	let quiet: unknown = null;
	let ceiling: unknown = null;
	let inFlight: Promise<void> | null = null;

	const clearTimers = () => {
		if (quiet !== null) opts.clearTimer(quiet);
		if (ceiling !== null) opts.clearTimer(ceiling);
		quiet = null;
		ceiling = null;
	};

	const fire = (): Promise<void> => {
		clearTimers();

		// run() is called SYNCHRONOUSLY, not deferred into a microtask. A timer
		// firing should start the sync at that instant; deferring it would also
		// make the scheduler untestable against a fake clock, because the run
		// would not have happened by the time the tick returns.
		let result: Promise<void> | void;
		try {
			result = opts.run();
		} catch {
			// A synchronous throw is a finished (failed) sync, not a hung one.
			result = undefined;
		}

		// A rejection must not surface as an unhandled promise, and must not
		// leave inFlight stuck forever. runSync reports failures to the user
		// itself; this only has to leave the scheduler usable.
		const tracked: Promise<void> = Promise.resolve(result)
			.then(
				() => undefined,
				() => undefined,
			)
			.then(() => {
				if (inFlight === tracked) inFlight = null;
			});
		inFlight = tracked;
		return tracked;
	};

	return {
		schedule() {
			if (quiet !== null) opts.clearTimer(quiet);
			quiet = opts.setTimer(() => void fire(), opts.quietMs);

			// Set once per burst and never extended. Extending it would just
			// make it a second quiet timer, and the point is that it bounds how
			// long an unbroken burst can defer a sync.
			if (ceiling === null) {
				ceiling = opts.setTimer(() => void fire(), opts.maxWaitMs);
			}
		},

		flush(flushOpts) {
			const pending = quiet !== null || ceiling !== null;
			if (!pending && !flushOpts?.force) {
				return inFlight ?? Promise.resolve();
			}
			// Chain onto work already running rather than starting a second
			// cycle beside it. Sync.run() COALESCES a concurrent call and
			// returns at once, so firing during an in-flight run would resolve
			// this flush while the real work was still going -- and a caller
			// that flushes in order to act on a fresh head (pinning) would
			// then act on the stale one.
			const running = inFlight;
			if (running) {
				const chained = running.then(() => fire()).then(() => undefined);
				inFlight = chained;
				return chained;
			}
			return fire();
		},

		cancel() {
			clearTimers();
		},
	};
}
