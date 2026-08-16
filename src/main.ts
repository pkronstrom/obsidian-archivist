import { Notice, Plugin, TAbstractFile, debounce } from "obsidian";
import { Client } from "./client";
import { Sync, skip } from "./sync";
import { DEFAULT_SETTINGS, VaultsyncSettingTab, type Settings } from "./settings";

export default class VaultsyncPlugin extends Plugin {
	settings: Settings = { ...DEFAULT_SETTINGS };
	sync!: Sync;

	private status?: HTMLElement;
	private timer?: number;
	private scheduleSync = debounce(() => void this.runSync(), 2000, true);

	async onload(): Promise<void> {
		await this.loadSettings();

		this.sync = new Sync(
			this.app,
			() => new Client(this.settings.serverUrl, this.settings.token),
			() => this.settings.device || "device",
			(msg, ...rest) => console.log("[vaultsync]", msg, ...rest),
		);

		this.status = this.addStatusBarItem();
		this.setStatus("idle");

		this.addSettingTab(new VaultsyncSettingTab(this.app, this));

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
				new Notice("vaultsync: re-bootstrapped");
			},
		});

		this.addRibbonIcon("refresh-cw", "vaultsync: sync now", () => void this.runSync());

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
		this.registerDomEvent(window, "blur", () => void this.runSync());
		this.registerDomEvent(window, "focus", () => void this.runSync());

		this.restartTimer();
	}

	onunload(): void {
		if (this.timer !== undefined) window.clearInterval(this.timer);
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
		if (!this.settings.serverUrl || !this.settings.token) {
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
					`vaultsync: ${report.conflicts.length} conflict(s); see ${report.conflicts[0].conflictPath}`,
					8000,
				);
			}
			this.setStatus(
				report.rebootstrapped
					? "re-bootstrapped"
					: `↓${report.pulled} ↑${report.pushed}`,
			);
		} catch (err) {
			const msg = err instanceof Error ? err.message : String(err);
			console.error("[vaultsync] sync failed", err);
			this.setStatus("error");
			new Notice(`vaultsync: ${msg}`, 8000);
		}
	}

	private setStatus(text: string): void {
		this.status?.setText(`vaultsync: ${text}`);
	}

	async loadSettings(): Promise<void> {
		// Settings live in data.json, which is fine: they are configuration and
		// losing them is an inconvenience. The sync cursor and snapshot are the
		// dangerous state, and those live in device-local storage instead --
		// see state.ts.
		this.settings = Object.assign({}, DEFAULT_SETTINGS, await this.loadData());
	}

	async saveSettings(): Promise<void> {
		await this.saveData(this.settings);
	}
}
