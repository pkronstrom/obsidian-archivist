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

	// Runs are serialised, but an idle scheduler still starts SYNCHRONOUSLY.
	//
	// Both properties are load-bearing. Synchronous start: a timer firing
	// should begin the sync at that instant, and deferring it into a microtask
	// makes the scheduler untestable against a fake clock, because the run has
	// not happened by the time the tick returns.
	//
	// Serialisation: Sync.run() COALESCES a concurrent call -- it sets a
	// queued flag, returns at once, and lets the active cycle launch the
	// follow-up itself. That follow-up is invisible here, so a caller awaiting
	// a flush would be told "done" while its edits were still unsent, which is
	// exactly when pinning reads the head and names the wrong tree. Queueing
	// the second call ourselves means run() is never called concurrently and
	// its coalescing path is never taken.
	let busy = false;
	let queued: { promise: Promise<void>; resolve: () => void } | null = null;

	const start = (): Promise<void> => {
		busy = true;

		let result: Promise<void> | void;
		try {
			result = opts.run();
		} catch {
			// A synchronous throw is a finished (failed) sync, not a hung one.
			result = undefined;
		}

		// A run that returned no promise is ALREADY done. Clearing busy in a
		// microtask instead would leave the scheduler looking occupied for the
		// rest of the tick, so the next timer in the same tick would queue
		// behind a run that had already finished -- and against a fake clock,
		// where nothing awaits between ticks, it would never run at all.
		if (typeof (result as Promise<void> | undefined)?.then !== "function") {
			busy = false;
			const immediate = queued;
			queued = null;
			if (immediate) {
				start().then(immediate.resolve, immediate.resolve);
			}
			inFlight = null;
			return Promise.resolve();
		}

		// A rejection must not surface as an unhandled promise, and must not
		// leave the scheduler wedged as permanently busy.
		const done: Promise<void> = Promise.resolve(result)
			.then(
				() => undefined,
				() => undefined,
			)
			.then(() => {
				busy = false;
				const next = queued;
				queued = null;
				if (next) {
					// Hand the queued waiters the result of the run they
					// actually asked for, not of the one that just finished.
					start().then(next.resolve, next.resolve);
					return;
				}
				if (inFlight === done) inFlight = null;
			});
		inFlight = done;
		return done;
	};

	const fire = (): Promise<void> => {
		clearTimers();
		if (!busy) return start();

		// One follow-up is enough: several edits arriving mid-sync all want
		// the same thing, which is "a cycle that starts after this one ends".
		if (!queued) {
			let resolve!: () => void;
			const promise = new Promise<void>((r) => (resolve = r));
			queued = { promise, resolve };
		}
		inFlight = queued.promise;
		return queued.promise;
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
				// Nothing waiting: the honest answer is whatever is already
				// running, which may be nothing at all.
				return inFlight ?? Promise.resolve();
			}
			// fire() queues behind any running cycle, so the returned promise
			// always covers a sync that STARTED after this call.
			return fire();
		},

		cancel() {
			clearTimers();
		},
	};
}
