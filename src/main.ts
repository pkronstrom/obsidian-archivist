import { Notice, Plugin, TAbstractFile, setIcon } from "obsidian";
import { Client } from "./client";
import { Sync, skip } from "./sync";
import { DEFAULT_SETTINGS, ArchivistSettingTab, type Settings } from "./settings";
import { Watcher } from "./watch";
import { loadState } from "./state";
import { stepUpWarning } from "./scopes";
import { createSyncScheduler } from "./sync-schedule";
import { loadToken, migrateToken } from "./credentials";
import { loadConfigSync } from "./config-sync";
import {
	installPlugins,
	isMobile,
	plannedInstalls,
	PluginInstallModal,
	reportInstalls,
} from "./plugin-install";
import { PairingHazardError, type PairingChoice } from "./pairing";
import { PairingModal } from "./pairing-modal";

export default class ArchivistPlugin extends Plugin {
	settings: Settings = { ...DEFAULT_SETTINGS };
	sync!: Sync;

	private status?: HTMLElement;
	private statusText?: HTMLElement;
	private lastCounts = "↓– ↑–";
	/**
	 * The pending pairing question, once the user has said "not now".
	 *
	 * Kept so the settings tab can reopen exactly the same choice, and so the
	 * modal stops reappearing: runSync fires on an interval, on focus, on blur
	 * and on every file change, so without this a dismissed dialog came back
	 * within seconds and there was no way to defer at all.
	 */
	pendingPairing?: PairingHazardError;
	private pairingDeferred = false;
	private timer?: number;
	private watcher?: Watcher;
	/**
	 * How long typing must stop before a sync.
	 *
	 * The old value was 2 seconds, which fires mid-sentence: one 50-minute
	 * drafting session produced 250 commits on the live vault. Long enough here
	 * to cover thinking pauses, short enough that stepping away syncs promptly.
	 */
	private static readonly SYNC_QUIET_MS = 20_000;

	/**
	 * The longest an unbroken burst may defer a sync, from its first edit.
	 *
	 * Obsidian saves to local disk on its own schedule regardless, so nothing is
	 * lost to a crash -- the exposure is only that the server has not seen it
	 * yet, and that losing the device loses at most this much. It is also the
	 * only real guarantee: blur can be skipped and quit is explicitly
	 * best-effort.
	 */
	private static readonly SYNC_MAX_WAIT_MS = 180_000;

	private syncScheduler = createSyncScheduler({
		quietMs: ArchivistPlugin.SYNC_QUIET_MS,
		maxWaitMs: ArchivistPlugin.SYNC_MAX_WAIT_MS,
		run: () => this.runSync(),
		setTimer: (fn, ms) => window.setTimeout(fn, ms),
		clearTimer: (h) => window.clearTimeout(h as number),
	});

	private scheduleSync = (): void => this.syncScheduler.schedule();

	async onload(): Promise<void> {
		await this.loadSettings();

		this.sync = new Sync(
			this.app,
			() => new Client(this.settings.serverUrl, loadToken(this.app), this.settings.vault),
			() => this.settings.device || "device",
			(msg, ...rest) => console.log("[archivist]", msg, ...rest),
			() => loadConfigSync(this.app),
		);

		// Long-polls the server so remote changes land in about a second rather
		// than waiting up to a full interval. See watch.ts for why this is
		// long-polling and not the server's SSE stream.
		this.watcher = new Watcher(
			() =>
				this.configured()
					? new Client(this.settings.serverUrl, loadToken(this.app), this.settings.vault)
					: null,
			() => loadState(this.app).base,
			() => this.runSync(),
			(msg, ...rest) => console.log("[archivist:watch]", msg, ...rest),
		);

		// Icon plus the last sync counts. The counts are the ONLY thing the
		// text ever shows -- transient states ("syncing…", "error") go to the
		// tooltip and the icon color instead, so the item never changes width
		// (the old full-text version visibly blinked when a click triggered a
		// focus sync). Dashes, not zeros, before the first cycle: "↓0 ↑0"
		// would claim a completed zero-change sync that has not happened.
		this.status = this.addStatusBarItem();
		this.status.addClasses(["mod-clickable", "archivist-status-item"]);
		setIcon(this.status, "refresh-cw");
		this.statusText = this.status.createSpan({
			cls: "archivist-status-counts",
			text: this.lastCounts,
		});
		this.registerDomEvent(this.status, "click", () => this.openSettings());
		this.setStatus("idle");

		this.addSettingTab(new ArchivistSettingTab(this.app, this));

		this.addCommand({
			id: "sync-now",
			name: "Sync now",
			callback: () => void this.runSync(),
		});
		this.addCommand({
			id: "rebootstrap",
			name: "Re-bootstrap from server",
			callback: async () => {
				await this.sync.forceRebootstrap();
				new Notice("archivist: re-bootstrapped");
			},
		});

		// Title case, because this string is a UI LABEL rather than a toast
		// prefix. It shows up in the ribbon tooltip and, on mobile, in the
		// quick-action menu beside "Open command palette" and "Create new base"
		// -- so it has to read like those, not like the "archivist: …" prefix
		// the Notices use. Obsidian titles the real command itself from the
		// manifest name; this one is ours to get right.
		this.addRibbonIcon("refresh-cw", "Archivist: Sync now", () => void this.runSync());

		// Vault events fire for EVERY existing file when the vault loads, which
		// the API documents and recommends handling by registering inside
		// onLayoutReady. Outside it, the plugin would treat the whole vault as
		// newly created on every launch.
		//
		// These events only schedule a sync. What actually changed is decided by
		// diffing against the snapshot, so a missed or spurious event costs
		// nothing.
		this.app.workspace.onLayoutReady(() => {
			const touched = (f: TAbstractFile) => {
				if (!this.settings.syncOnChange) return;
				// With the level, so a snippet or a theme edit schedules a sync
				// the same way a note does. Without it this keeps the Files-only
				// default and config only moves on the interval.
				if (skip(f.path, loadConfigSync(this.app))) return;
				this.scheduleSync();
			};
			this.registerEvent(this.app.vault.on("create", touched));
			this.registerEvent(this.app.vault.on("modify", touched));
			this.registerEvent(this.app.vault.on("delete", touched));
			// rename gives the old path too, and fires once for a folder rather
			// than once per contained file.
			this.registerEvent(this.app.vault.on("rename", (f) => touched(f)));

			void this.runSync();
		});

		// The mobile lifecycle. iOS suspends the app aggressively, so a push
		// can be cut short: flush when backgrounding, and pull on focus so the
		// other device is current the moment it is picked up -- which is the
		// whole edit-on-phone-then-switch-to-laptop pattern.
		//
		// The watcher stops on blur and starts on focus for the same reason. A
		// held connection does not survive the phone locking, and leaving one
		// pending would keep the radio busy for an answer that can never arrive.
		// Syncing on focus is what actually makes the phone current; the watcher
		// keeps it current while you are looking at it.
		this.registerDomEvent(window, "blur", () => {
			this.watcher?.stop();
			// A forced flush rather than runSync: it syncs pending edits
			// immediately AND still pulls when nothing local is waiting, which
			// is what the old unconditional runSync did. Calling both would
			// queue two full cycles.
			//
			// This is also what actually protects the phone. Backgrounding
			// fires blur; the quit event never fires there, because iOS
			// suspends apps rather than quitting them.
			void this.syncScheduler.flush({ force: true });
		});
		this.registerDomEvent(window, "focus", () => {
			void this.runSync();
			this.startWatching();
		});

		// Obsidian awaits work registered here, so returning the flush promise
		// means a clean quit finishes the sync rather than abandoning it
		// mid-flight. Its own API says this is "not guaranteed to actually
		// run" -- a force quit, an OS kill or a crash skips it -- so it
		// improves the common case and guarantees nothing. SYNC_MAX_WAIT_MS is
		// the actual bound.
		this.registerEvent(
			this.app.workspace.on("quit", (tasks) => {
				tasks.add(() => this.syncScheduler.flush());
			}),
		);

		this.restartTimer();
		this.startWatching();
	}

	onunload(): void {
		if (this.timer !== undefined) window.clearInterval(this.timer);
		this.watcher?.stop();
		// Drops pending timers. A sync already running is left to finish --
		// there is no way to abort one, and a half-applied sync would be worse
		// than a completed one.
		this.syncScheduler.cancel();
	}

	/**
	 * The watcher is the low-latency path; the interval stays on as a backstop
	 * for whatever the long-poll misses -- a dropped connection, a suspended
	 * phone, a proxy that closed the request early.
	 */
	startWatching(): void {
		if (!this.settings.watchRemote) return;
		if (!this.configured()) return;
		this.watcher?.start();
	}

	restartWatcher(): void {
		this.watcher?.stop();
		this.startWatching();
	}

	restartTimer(): void {
		if (this.timer !== undefined) window.clearInterval(this.timer);
		const secs = this.settings.intervalSeconds;
		if (secs > 0) {
			this.timer = window.setInterval(() => void this.runSync(), secs * 1000);
			this.registerInterval(this.timer);
		}
	}

	/**
	 * Fill in the vault when the token leaves no choice.
	 *
	 * A token minted for one vault has exactly one answer, so asking is
	 * ceremony. This is also the whole migration for a device coming from
	 * 0.4.x: it already has the URL and token, so it resolves itself and needs
	 * no setting at all. With more than one vault it stays empty and the
	 * settings tab's Choose button does the picking, because then there IS a
	 * choice and guessing it would be worse than asking.
	 *
	 * Returns true when the vault is now known.
	 */
	async resolveVaultIfUnambiguous(): Promise<boolean> {
		if (this.settings.vault) return true;
		const token = loadToken(this.app);
		if (!this.settings.serverUrl || !token) return false;
		try {
			const { vaults, protectedVaults, requiresStepUpAuth, label } = await new Client(
				this.settings.serverUrl,
				token,
				"",
			).listVaults();
			if (vaults.length !== 1) return false;
			// One vault is not a reason to skip the check the Choose button
			// makes. A token gated on vault ACCESS cannot drive this plugin --
			// there is nowhere here to present a code -- so adopting it silently
			// would start a sync that can only fail.
			const gated = stepUpWarning(protectedVaults, requiresStepUpAuth, label);
			if (gated) {
				console.log("[archivist] not adopting the only vault:", gated);
				return false;
			}
			this.settings.vault = vaults[0];
			await this.saveSettings();
			console.log("[archivist] token opens one vault; adopted", vaults[0]);
			return true;
		} catch {
			// Server down, wrong token, older server: none of those are worth a
			// Notice here. The sync that follows reports the real failure.
			return false;
		}
	}

	private async runSync(): Promise<void> {
		if (!this.settings.vault && (await this.resolveVaultIfUnambiguous())) {
			// Resolved; fall through and sync.
		}
		if (!this.configured()) {
			this.setStatus("not configured");
			return;
		}
		this.setStatus("syncing…");
		try {
			const report = await this.sync.run();
			if (report === null) return; // coalesced into an in-flight cycle
			if (report.conflicts.length > 0) {
				// Conflicts are the one outcome worth interrupting for: a file
				// now exists that the user has to resolve by hand.
				new Notice(
					`archivist: ${report.conflicts.length} conflict(s); see ${report.conflicts[0].conflictPath}`,
					8000,
				);
			}
			this.setStatus(
				report.rebootstrapped
					? "re-bootstrapped"
					: `↓${report.pulled} ↑${report.pushed}`,
			);
			if (report.pulled > 0) await this.installArrivedPlugins();
		} catch (err) {
			if (err instanceof PairingHazardError) {
				// Not an error to report and move past: it is a question, and
				// until it is answered this device does not sync at all.
				this.pendingPairing = err;
				if (this.pairingDeferred) {
					// Already asked and deferred. Say so and stay quiet.
					this.setStatus("paused — pairing undecided");
					return;
				}
				this.setStatus("needs a decision");
				this.openPairingModal();
				return;
			}
			const msg = err instanceof Error ? err.message : String(err);
			console.error("[archivist] sync failed", err);
			this.setStatus("error");
			new Notice(`archivist: ${msg}`, 8000);
		}
	}

	/** Open the pairing choice. Also called from the settings tab. */
	openPairingModal(): void {
		const hazard = this.pendingPairing;
		if (!hazard) return;
		new PairingModal(
			this.app,
			hazard,
			(choice) => void this.resolvePairing(choice),
			() => {
				this.pairingDeferred = true;
				this.setStatus("paused — pairing undecided");
				new Notice(
					"archivist: not syncing until you choose. Reopen it in " +
						"Archivist's settings when you are ready.",
					8000,
				);
			},
		).open();
	}

	/**
	 * Opens this plugin's own settings tab. `app.setting` is undocumented --
	 * there is no public API for it -- but it is the standard way plugins do
	 * this, and the same cast pattern already used below for `plugins.manifests`.
	 */
	private openSettings(): void {
		const setting = (
			this.app as unknown as {
				setting: { open: () => void; openTabById: (id: string) => void };
			}
		).setting;
		setting.open();
		setting.openTabById(this.manifest.id);
	}

	private async resolvePairing(choice: PairingChoice): Promise<void> {
		this.setStatus("syncing…");
		try {
			await this.sync.resolvePairing(choice);
			this.pendingPairing = undefined;
			this.pairingDeferred = false;
			new Notice(
				choice === "adopt"
					? "archivist: local files moved to _archivist-rescued-…, server adopted"
					: choice === "publish"
						? "archivist: this vault published to the server"
						: "archivist: merged; check for conflict files",
				10000,
			);
			// No second runSync here. Sync.resolvePairing ends by calling run()
			// itself, so calling it again would push a fresh cycle straight
			// after the one that just finished.
			this.setStatus("idle");
		} catch (err) {
			const msg = err instanceof Error ? err.message : String(err);
			console.error("[archivist] pairing failed", err);
			this.setStatus("error");
			new Notice(`archivist: ${msg}`, 8000);
		}
	}

	/**
	 * Install plugins that arrived in the synced list but are not here yet.
	 *
	 * Behind a confirmation every time, and never automatic: this downloads code
	 * from the internet at the direction of another device, and anyone with
	 * write access to the vault could add an id to that list. Declining leaves
	 * the list synced and the code absent, which is exactly what Obsidian Sync
	 * does.
	 */
	private async installArrivedPlugins(): Promise<void> {
		if (loadConfigSync(this.app).level !== "plugins") return;

		const listPath = `${this.app.vault.configDir}/community-plugins.json`;
		if (!(await this.app.vault.adapter.exists(listPath))) return;

		let wanted: string[];
		try {
			wanted = JSON.parse(await this.app.vault.adapter.read(listPath));
			if (!Array.isArray(wanted)) return;
		} catch {
			return;
		}

		const registry = (
			this.app as unknown as {
				plugins?: { manifests?: Record<string, { id: string; isDesktopOnly?: boolean }> };
			}
		).plugins;
		const manifests = registry?.manifests ?? {};
		const plan = plannedInstalls(wanted, Object.keys(manifests), manifests, isMobile());
		if (plan.install.length === 0) {
			if (plan.skipped.length > 0) {
				reportInstalls({ installed: [], failed: [], skippedOnMobile: [] }, plan.skipped);
			}
			return;
		}

		new PluginInstallModal(this.app, plan, async () => {
			const result = await installPlugins(this.app, plan.install, (msg, ...rest) =>
				console.log("[archivist:plugins]", msg, ...rest),
			);
			reportInstalls(result, plan.skipped);
		}).open();
	}

	/** All three of URL, token and vault are needed before anything can sync. */
	private configured(): boolean {
		return Boolean(this.settings.serverUrl && loadToken(this.app) && this.settings.vault);
	}

	private setStatus(text: string): void {
		if (!this.status) return;

		if (text.startsWith("↓")) {
			this.lastCounts = text;
			this.statusText?.setText(text);
		}

		// Severity, not one bucket: red is a real failure, orange is a state
		// waiting on the user, and not-configured stays uncolored -- setup is
		// not a fault. Syncing gets a static accent tint (visible only when a
		// sync runs long enough to notice), never an animation.
		this.status.toggleClass("archivist-syncing", text === "syncing…");
		this.status.toggleClass("archivist-error", text === "error");
		this.status.toggleClass(
			"archivist-paused",
			text === "needs a decision" || text.startsWith("paused"),
		);

		// The refresh icon reads as "sync now", so the tooltip says what a
		// click actually does.
		const state = text.startsWith("↓") ? `last sync ${text}` : `${text} (last ${this.lastCounts})`;
		this.status.setAttribute("aria-label", `archivist: ${state}. Click to open settings.`);
		this.status.setAttribute("data-tooltip-position", "top");
	}

	async loadSettings(): Promise<void> {
		// Settings live in data.json, which is fine: they are configuration and
		// losing them is an inconvenience. The sync cursor, the snapshot and the
		// TOKEN are the dangerous state, and those live in device-local storage
		// instead -- see state.ts and credentials.ts.
		const data = (await this.loadData()) ?? {};
		if (migrateToken(this.app, data)) await this.saveData(data);
		this.settings = Object.assign({}, DEFAULT_SETTINGS, data, { token: "" });
	}

	async saveSettings(): Promise<void> {
		await this.saveData(this.settings);
	}
}
