import { Notice, Plugin, TAbstractFile, debounce } from "obsidian";
import { Client } from "./client";
import { Sync, skip } from "./sync";
import { DEFAULT_SETTINGS, ArchivistSettingTab, type Settings } from "./settings";
import { Watcher } from "./watch";
import { loadState } from "./state";
import { loadToken, migrateToken } from "./credentials";
import { PairingHazardError, type PairingChoice } from "./pairing";
import { PairingModal } from "./pairing-modal";

export default class ArchivistPlugin extends Plugin {
	settings: Settings = { ...DEFAULT_SETTINGS };
	sync!: Sync;

	private status?: HTMLElement;
	private timer?: number;
	private watcher?: Watcher;
	private scheduleSync = debounce(() => void this.runSync(), 2000, true);

	async onload(): Promise<void> {
		await this.loadSettings();

		this.sync = new Sync(
			this.app,
			() => new Client(this.settings.serverUrl, loadToken(this.app)),
			() => this.settings.device || "device",
			(msg, ...rest) => console.log("[archivist]", msg, ...rest),
		);

		// Long-polls the server so remote changes land in about a second rather
		// than waiting up to a full interval. See watch.ts for why this is
		// long-polling and not the server's SSE stream.
		this.watcher = new Watcher(
			() =>
				this.settings.serverUrl && loadToken(this.app)
					? new Client(this.settings.serverUrl, loadToken(this.app))
					: null,
			() => loadState(this.app).base,
			() => this.runSync(),
			(msg, ...rest) => console.log("[archivist:watch]", msg, ...rest),
		);

		this.status = this.addStatusBarItem();
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

		this.addRibbonIcon("refresh-cw", "archivist: sync now", () => void this.runSync());

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
				if (skip(f.path)) return;
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
			void this.runSync();
		});
		this.registerDomEvent(window, "focus", () => {
			void this.runSync();
			this.startWatching();
		});

		this.restartTimer();
		this.startWatching();
	}

	onunload(): void {
		if (this.timer !== undefined) window.clearInterval(this.timer);
		this.watcher?.stop();
	}

	/**
	 * The watcher is the low-latency path; the interval stays on as a backstop
	 * for whatever the long-poll misses -- a dropped connection, a suspended
	 * phone, a proxy that closed the request early.
	 */
	startWatching(): void {
		if (!this.settings.watchRemote) return;
		if (!this.settings.serverUrl || !loadToken(this.app)) return;
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

	private async runSync(): Promise<void> {
		if (!this.settings.serverUrl || !loadToken(this.app)) {
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
		} catch (err) {
			if (err instanceof PairingHazardError) {
				// Not an error to report and move past: it is a question, and
				// until it is answered this device does not sync at all.
				this.setStatus("needs a decision");
				new PairingModal(this.app, err, (choice) => void this.resolvePairing(choice)).open();
				return;
			}
			const msg = err instanceof Error ? err.message : String(err);
			console.error("[archivist] sync failed", err);
			this.setStatus("error");
			new Notice(`archivist: ${msg}`, 8000);
		}
	}

	private async resolvePairing(choice: PairingChoice): Promise<void> {
		this.setStatus("syncing…");
		try {
			await this.sync.resolvePairing(choice);
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

	private setStatus(text: string): void {
		this.status?.setText(`archivist: ${text}`);
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
