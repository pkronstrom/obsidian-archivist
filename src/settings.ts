import { App, Notice, Platform, PluginSettingTab, Setting, setIcon } from "obsidian";
import { hostname as osHostname } from "os";
import { Client } from "./client";
import { DeletedModal } from "./deleted-modal";
import { scopeWarning, stepUpWarning } from "./scopes";
import { isFirstRun, loadState } from "./state";
import { loadToken, saveToken } from "./credentials";
import { collectInventory, readInventories } from "./plugin-inventory";
import { renderInventoryView } from "./plugin-inventory-view";
import { VaultPickerModal } from "./vault-picker";
import type ArchivistPlugin from "./main";
import {
	applySyncMode,
	deriveSyncMode,
	formatPermissions,
	formatRelativeTime,
	formatVaultStats,
	suggestDeviceName,
	type SyncMode,
} from "./status-text";

export type Settings = {
	serverUrl: string;
	/**
	 * Which vault on that server. Separate from serverUrl on purpose: the
	 * picker asks the SERVER ROOT what this token opens, which it cannot do if
	 * the vault is part of the base URL.
	 */
	vault: string;
	device: string;
	intervalSeconds: number;
	syncOnChange: boolean;
	/** Hold a long-poll open so remote changes arrive in about a second. */
	watchRemote: boolean;
	/**
	 * How long typing must stop before a sync, in seconds.
	 *
	 * Configurable because how long you pause mid-sentence is personal, and it
	 * is the number worth tuning after living with it. Too low and a drafting
	 * session becomes hundreds of commits, which makes note_history useless for
	 * the note you are working on; too high and a device you walk away from is
	 * stale for longer. The ceiling in main.ts bounds the latter regardless.
	 */
	syncQuietSeconds: number;
	/** Gap that separates two editing sessions in the revision browser, in
	 *  minutes. 0 lists every commit separately. */
	revisionGapMinutes: number;
};

export const DEFAULT_SETTINGS: Settings = {
	serverUrl: "",
	vault: "",
	device: "",
	intervalSeconds: 300,
	syncOnChange: true,
	watchRemote: true,
	syncQuietSeconds: 15,
	revisionGapMinutes: 30,
};

export class ArchivistSettingTab extends PluginSettingTab {
	/** Filled in by "Test connection"; shown in the Status card once known. */
	private vaultStats?: { files: number; bytes: number };

	/** Bumped on every display(). An async render compares it before touching
	 *  the DOM, so a scan that finishes after a re-render throws its results
	 *  away instead of appending them to a pane it no longer owns. */
	private renderGeneration = 0;

	/** Sections the user has opened, surviving the re-renders that saving a
	 *  setting triggers. Not persisted: a fresh visit should show the pane in
	 *  its considered default state rather than however it was left months ago. */
	private opened = new Set<string>();

	/**
	 * A collapsible section, returning the element its rows go in.
	 *
	 * Collapsed by default for everything a settled user does not need, which
	 * is most of the pane: on a phone the flat version was several screens of
	 * scrolling to reach a control someone visits twice a year. `openWhen`
	 * overrides that for a section that currently NEEDS attention -- an
	 * unconfigured server is the case that matters, since a collapsed Server
	 * block on first run would hide the only rows that do anything.
	 */
	private section(containerEl: HTMLElement, title: string, openWhen = false): HTMLElement {
		const open = openWhen || this.opened.has(title);
		const head = new Setting(containerEl).setName(title).setHeading();
		head.settingEl.addClass("archivist-section-head");
		head.settingEl.addClass("mod-clickable");

		const chevron = createSpan({ cls: "archivist-section-chevron" });
		setIcon(chevron, open ? "chevron-down" : "chevron-right");
		head.settingEl.prepend(chevron);

		const body = containerEl.createDiv({ cls: "archivist-section-body" });
		if (!open) body.hide();

		// Toggled IN PLACE rather than by re-rendering the tab. display()
		// rebuilds every section, which re-derives the Status card (so its
		// Access row visibly blinks) and re-runs the plugin data.json scan --
		// a lot of work, and a flash of changing text, to hide four rows.
		head.settingEl.onClickEvent(() => {
			const nowOpen = !this.opened.has(title);
			if (nowOpen) this.opened.add(title);
			else this.opened.delete(title);
			body.toggle(nowOpen);
			if (nowOpen) body.dispatchEvent(new Event("archivist-section-open"));
			setIcon(chevron, nowOpen ? "chevron-down" : "chevron-right");
		});
		return body;
	}

	constructor(app: App, private plugin: ArchivistPlugin) {
		super(app, plugin);
	}

	display(): void {
		const { containerEl } = this;
		containerEl.empty();

		containerEl.createEl("h2", { text: "Archivist" });

		// A control-less Setting rather than a bare <p>: the description class
		// only gets its muted styling inside a .setting-item, and themes align
		// and pad setting rows -- loose paragraphs sit off-grid beside them.
		new Setting(containerEl).setDesc(
			createFragment((f) => {
				// What it is, what that buys, and the one property that is
				// actually unusual. Three short sentences: someone opening
				// settings on a phone will read the first, maybe the second.
				f.appendText(
					"Self-hosted sync for this vault. Your notes stay plain Markdown " +
						"files on a server you own, so nothing is locked in a proprietary " +
						"format and no third party holds them. Every sync is a commit, so " +
						"nothing is overwritten and any earlier version can be read back. ",
				);
				f.createEl("a", {
					href: "https://github.com/pkronstrom/obsidian-archivist",
					text: "github.com/pkronstrom/obsidian-archivist",
				});
			}),
		);

		// Ordered for the SETTLED user, not the first-run one: setup happens
		// once, while "is it working", sync tuning and recovery are what
		// someone comes back for. First-run stays linear anyway, because an
		// unconfigured Status card points straight at Server.
		// Above the sections, deliberately. It is a rule about the VAULT rather
		// than a sync setting, and it is the only place anyone learns why a
		// file stopped syncing -- so it must not be reachable only by expanding
		// something. Everything below can collapse; this cannot.
		new Setting(containerEl)
			.setName("Files that never sync")
			.setDesc(
				"Files with .local before the extension stay on this device, as do " +
					"folders ending in .local: Scratch.local.md, Journal.local/. Remove " +
					"the .local to start syncing one. A folder like project.local.assets/ " +
					"is unaffected.",
			);

		this.renderStatus(containerEl);

		// Server opens itself while there is nothing to connect with: a
		// collapsed block on first run would hide the only rows that do
		// anything. Everything else collapses, sync included -- its defaults
		// are sane, so an open block was scrolling on every visit for a
		// setting most people never touch.
		const unconfigured = !this.plugin.settings.serverUrl || !loadToken(this.app);
		this.renderServer(this.section(containerEl, "Server", unconfigured));
		this.renderSyncBehavior(this.section(containerEl, "Sync behavior"));
		this.renderPlugins(this.section(containerEl, "Plugins on your devices"));
		this.renderHistoryAndRecovery(this.section(containerEl, "History and recovery"));
		this.renderTroubleshooting(this.section(containerEl, "Troubleshooting"));
	}

	/**
	 * One compact card answering "is this device syncing, with what, right
	 * now?" -- aligned label/value rows in debugging order (what it points at,
	 * is it reachable, when it last worked, what the token allows), not a
	 * Setting row per fact. Populated from the same calls "Test connection"
	 * already makes, no new endpoints.
	 */
	private renderStatus(containerEl: HTMLElement): void {
		new Setting(containerEl).setName("Status").setHeading();

		const card = containerEl.createDiv("archivist-status");
		const { serverUrl, vault } = this.plugin.settings;
		const token = loadToken(this.app);

		if (!serverUrl || !token) {
			card.createDiv({ text: "Not configured. Set the server URL and token below." });
			this.renderPairingHazard(containerEl);
			return;
		}

		const row = (label: string): HTMLElement => {
			const r = card.createDiv("archivist-status-row");
			r.createSpan({ cls: "archivist-status-label", text: label });
			return r.createSpan("archivist-status-value");
		};

		const vaultEl = row("Vault");
		vaultEl.addClass("is-vault");
		vaultEl.setText(vault || "not chosen yet");

		const serverEl = row("Server");
		serverEl.setText(serverUrl);
		const dot = serverEl.createSpan({ cls: "archivist-status-dot is-checking", text: "●" });
		// The reason line for a failed check. Created eagerly so it sits under
		// the server row; CSS hides it while empty.
		const detailEl = card.createDiv("archivist-status-detail");

		const state = loadState(this.app);
		row("Last sync").setText(formatRelativeTime(state.lastSyncedAt, Date.now()));

		const accessEl = row("Access");
		accessEl.setText("checking…");

		if (this.vaultStats) {
			row("Size").setText(formatVaultStats(this.vaultStats.files, this.vaultStats.bytes));
		}

		row("Plugin").setText(this.plugin.manifest.version);

		void this.refreshStatus(dot, detailEl, accessEl);

		this.renderPairingHazard(containerEl);
	}

	/** The live checks: index() for reachability, listVaults() for scopes.
	 *  Both calls exist elsewhere in this tab already; this fires them on
	 *  open and writes the answers into the card. */
	private async refreshStatus(
		dot: HTMLElement,
		detailEl: HTMLElement,
		accessEl: HTMLElement,
	): Promise<void> {
		const { serverUrl, vault } = this.plugin.settings;
		const token = loadToken(this.app);

		try {
			const idx = await new Client(serverUrl, token, vault).index();
			if (idx.vault && vault && idx.vault !== vault) {
				dot.className = "archivist-status-dot is-error";
				detailEl.setText(`server reports vault "${idx.vault}", not "${vault}"`);
			} else {
				dot.className = "archivist-status-dot is-ok";
			}
		} catch (err) {
			dot.className = "archivist-status-dot is-error";
			detailEl.setText(`unreachable: ${err instanceof Error ? err.message : String(err)}`);
		}

		try {
			const { scopes, label } = await new Client(serverUrl, token, "").listVaults();
			accessEl.setText(formatPermissions(scopes, label));
		} catch {
			accessEl.setText("could not check");
		}
	}

	/** The deferred pairing question is otherwise unreachable: the modal only
	 *  reopens from a sync, and a deferred one stops syncing on purpose. */
	private renderPairingHazard(containerEl: HTMLElement): void {
		if (!this.plugin.pendingPairing) return;
		const hazard = this.plugin.pendingPairing;
		new Setting(containerEl)
			.setName("This device is not syncing")
			.setDesc(
				`It holds ${hazard.localFiles} file(s) and has never synced, and the ` +
					`server already has content. Nothing has been changed. Until you ` +
					`choose, this device stays paused.`,
			)
			.addButton((b) =>
				b
					.setCta()
					.setButtonText("Choose what happens")
					.onClick(() => {
						this.plugin.openPairingModal();
					}),
			);
	}

	/** How this device is wired up: URL, token, name -- touched at setup,
	 *  then left alone. */
	private renderServer(containerEl: HTMLElement): void {

		new Setting(containerEl)
			.setName("Server URL")
			.setDesc("Base URL of your Archivist server, e.g. https://vault.example.net")
			.addText((t) =>
				t.setPlaceholder("https://vault.example.net")
					.setValue(this.plugin.settings.serverUrl)
					.onChange(async (v) => {
						this.plugin.settings.serverUrl = v.trim();
						await this.plugin.saveSettings();
					}),
			);

		new Setting(containerEl)
			.setName("Token")
			.setDesc(
				"Bearer token the server expects. Stored on this device only, " +
					"never in the vault, so it cannot reach git history.",
			)
			.addText((t) => {
				t.inputEl.type = "password";
				t.setValue(loadToken(this.app)).onChange(async (v) => {
					saveToken(this.app, v.trim());
				});
			});

		new Setting(containerEl)
			.setName("Device name")
			.setDesc("Names this device in commit messages and conflict filenames.")
			.addText((t) => {
				if (!this.plugin.settings.device) {
					const suggestion = suggestDeviceName(Platform, this.desktopHostname());
					this.plugin.settings.device = suggestion;
					void this.plugin.saveSettings();
					t.setValue(suggestion);
				} else {
					t.setValue(this.plugin.settings.device);
				}
				t.setPlaceholder("mac / iphone").onChange(async (v) => {
					this.plugin.settings.device = v.trim();
					await this.plugin.saveSettings();
				});
			});

		// Choosing a vault is a SETUP action: after the first successful sync
		// this device has adopted a vault and repointing it is no longer a
		// settings tweak -- the identity guard would refuse to sync anyway.
		// The deliberate escape hatch is Re-bootstrap, under Troubleshooting.
		if (isFirstRun(loadState(this.app))) {
			this.renderVaultChooser(containerEl);
		}

		this.renderTestConnection(containerEl);
	}

	/** os.hostname() only exists in the Electron/Node desktop runtime -- guarded
	 *  behind Platform.isDesktopApp so the mobile bundle never reaches it. */
	private desktopHostname(): string | null {
		if (!Platform.isDesktopApp) return null;
		try {
			return osHostname();
		} catch {
			return null;
		}
	}

	private async chooseVault(vault: string): Promise<void> {
		this.plugin.settings.vault = vault;
		await this.plugin.saveSettings();
		new Notice(`archivist: syncing vault "${vault}"`);
		this.display();
	}

	/** The last row of Server, beside the URL and token it checks. It used to
	 *  sit at the bottom of Maintenance, a whole pane away from the two fields
	 *  it exists to validate. */
	private renderTestConnection(containerEl: HTMLElement): void {
		new Setting(containerEl)
			.setName("Test connection")
			.setDesc("Check the URL and token, and show which vault they reach")
			.addButton((b) =>
				b.setButtonText("Test").onClick(async () => {
					const { serverUrl } = this.plugin.settings;
					const token = loadToken(this.app);
					if (!serverUrl || !token) {
						new Notice("archivist: set the server URL and token first");
						return;
					}
					try {
						const client = new Client(serverUrl, token, this.plugin.settings.vault);
						const idx = await client.index();
						const { files } = await client.snapshot();
						const entries = Object.values(files);
						this.vaultStats = {
							files: entries.length,
							bytes: entries.reduce((sum, e) => sum + e.size, 0),
						};
						const name = idx.vault;
						new Notice(
							`archivist: connected to vault "${name}": ${entries.length} file(s), ` +
								`server ${idx.version}`,
							10000,
						);
						// Re-render so the Status card above picks up vaultStats.
						this.display();
					} catch (err) {
						new Notice(`archivist: ${err instanceof Error ? err.message : String(err)}`, 8000);
					}
				}),
			);
	}

	/** Shown only before the first successful sync -- see renderServer. */
	private renderVaultChooser(containerEl: HTMLElement): void {
		const vaultSetting = new Setting(containerEl)
			.setName("Vault")
			.setDesc(
				"Which vault on that server this device should sync. Fixed after " +
					"the first sync; a fresh vault name is created on first push if " +
					"the token allows it.",
			)
			.addText((t) =>
				t.setPlaceholder("personal")
					.setValue(this.plugin.settings.vault)
					.onChange(async (v) => {
						this.plugin.settings.vault = v.trim();
						await this.plugin.saveSettings();
					}),
			);

		// A token that opens one vault has one answer. Resolve it on open
		// rather than making someone press a button to be told the only option.
		//
		// Re-render only when that call actually FILLED IN a vault. It returns
		// true for "a vault is set", not "I set one", so re-rendering on every
		// true is an unbounded loop: display -> resolve -> display, which pins
		// a renderer at 100% and never reaches the first sync that would retire
		// this chooser.
		if (!this.plugin.settings.vault) {
			void this.plugin.resolveVaultIfUnambiguous().then((ok) => {
				if (ok) this.display();
			});
		}

		vaultSetting.addButton((b) =>
			b.setButtonText("Choose").onClick(async () => {
				const { serverUrl } = this.plugin.settings;
				const token = loadToken(this.app);
				if (!serverUrl || !token) {
					new Notice("archivist: set the server URL and token first");
					return;
				}
				try {
					const { vaults, scopes, label, protectedVaults, requiresStepUpAuth } =
						await new Client(serverUrl, token, "").listVaults();
					// Refuse here rather than at the first save. A token without
					// both verbs syncs down happily and then fails on a write,
					// with the note already edited -- the worst moment to learn
					// the credential was the wrong one.
					const warning = scopeWarning(scopes, label);
					if (warning) {
						new Notice(`archivist: ${warning}`, 12000);
						return;
					}
					// A token gated on vault ACCESS can never drive this plugin:
					// there is nowhere here to type a code, and the vault lands
					// on disk in plaintext anyway. Say so now rather than at the
					// first sync.
					const gated = stepUpWarning(protectedVaults, requiresStepUpAuth, label);
					if (gated) {
						new Notice(`archivist: ${gated}`, 12000);
						return;
					}
					if (vaults.length === 0) {
						new Notice("archivist: this token opens no vaults", 8000);
						return;
					}
					// One vault is not a choice. Adopt it rather than making
					// someone pick from a list of one.
					if (vaults.length === 1) {
						await this.chooseVault(vaults[0]);
						return;
					}
					new VaultPickerModal(this.app, vaults, (v) => void this.chooseVault(v)).open();
				} catch (err) {
					new Notice(`archivist: ${err instanceof Error ? err.message : String(err)}`, 8000);
				}
			}),
		);
	}

	/**
	 * One intent, not three switches. The underlying settings still exist in
	 * storage (a device upgrading from the toggle era derives its mode from
	 * what it already had); the dropdown writes presets back onto them.
	 */
	private renderSyncBehavior(containerEl: HTMLElement): void {

		const s = this.plugin.settings;
		const mode = deriveSyncMode(s);

		new Setting(containerEl)
			.setName("Mode")
			.setDesc(
				createFragment((f) => {
					const list = f.createDiv("archivist-modes");
					const row = (name: string, text: string) => {
						const r = list.createDiv("archivist-mode-row");
						r.createSpan({ cls: "archivist-mode-name", text: name });
						r.createSpan({ text });
					};
					row(
						"Automatic",
						"syncs shortly after you stop editing; changes from other devices arrive within about a second.",
					);
					row("Periodic", "syncs on a fixed interval; no connection held open.");
					row(
						"Manual",
						"syncs only on demand: the ribbon icon, the sync command, or when the app gains focus.",
					);
				}),
			)
			.addDropdown((d) =>
				d
					.addOption("automatic", "Automatic")
					.addOption("periodic", "Periodic")
					.addOption("manual", "Manual")
					.setValue(mode)
					.onChange(async (v) => {
						const next = applySyncMode(v as SyncMode, s);
						s.syncOnChange = next.syncOnChange;
						s.watchRemote = next.watchRemote;
						s.intervalSeconds = next.intervalSeconds;
						await this.plugin.saveSettings();
						this.plugin.restartTimer();
						this.plugin.restartWatcher();
						// Re-render: the interval field only exists in periodic.
						this.display();
					}),
			);

		if (s.syncOnChange) {
			new Setting(containerEl)
				.setName("Wait after typing (seconds)")
				.setDesc(
					"How long editing must pause before syncing. Lower syncs sooner; higher " +
						"keeps history readable, since every sync is a commit. A continuous " +
						"burst still syncs every 30 seconds.",
				)
				.addText((t) =>
					t.setValue(String(s.syncQuietSeconds)).onChange(async (v) => {
						const n = Number(v);
						// Rejected rather than clamped, so a typo leaves the old
						// value visible instead of silently becoming something
						// else. The ceiling is 30s; a quiet period at or above
						// it would make the quiet timer dead code.
						if (!Number.isFinite(n) || n < 1 || n >= 30) return;
						s.syncQuietSeconds = Math.floor(n);
						await this.plugin.saveSettings();
						this.plugin.restartSyncScheduler();
					}),
				);
		}

		if (mode === "periodic") {
			new Setting(containerEl).setName("Sync interval (minutes)").addText((t) =>
				t.setValue(String(Math.max(1, Math.round(s.intervalSeconds / 60)))).onChange(async (v) => {
					const n = Number(v);
					if (!Number.isFinite(n) || n < 1) return;
					s.intervalSeconds = Math.floor(n) * 60;
					await this.plugin.saveSettings();
					this.plugin.restartTimer();
				}),
			);
		}
	}

	private renderHistoryAndRecovery(containerEl: HTMLElement): void {

		// Same audience as re-bootstrap and vault restore points: rare,
		// deliberate, whole-vault. Not in the note's revision modal, because
		// the note you are looking at is by definition not the one you deleted.
		new Setting(containerEl)
			.setName("Deleted notes")
			.setDesc(
				"Notes the server still holds but this vault no longer shows. Restoring " +
					"puts one back where it was and syncs it.",
			)
			.addButton((b) =>
				b.setButtonText("Browse deleted").onClick(() => {
					const { serverUrl, vault } = this.plugin.settings;
					const token = loadToken(this.app);
					if (!serverUrl || !token) {
						new Notice("archivist: set the server URL and token first");
						return;
					}
					new DeletedModal(this.app, () => new Client(serverUrl, token, vault)).open();
				}),
			);

		new Setting(containerEl)
			.setName("Group revisions within (minutes)")
			.setDesc(
				"The revision browser groups edits closer together than this into one " +
					"session. Lower shows finer detail; 0 lists every sync. Affects the " +
					"list only.",
			)
			.addText((t) =>
				t
					.setValue(String(this.plugin.settings.revisionGapMinutes))
					.onChange(async (v) => {
						const n = Number(v);
						// Rejected rather than clamped: a typo should not silently
						// become a number the user did not choose.
						if (!Number.isFinite(n) || n < 0 || n > 1440) return;
						this.plugin.settings.revisionGapMinutes = Math.floor(n);
						await this.plugin.saveSettings();
					}),
			);

		// Most setup failures are a wrong URL or a wrong token. A button that
		// says which beats asking someone to read a console on a phone.
	}

	/** The only action here discards state, so it gets its own section rather
	 *  than sitting one row below "restore a deleted note" -- which is exactly
	 *  the kind of neighbouring that gets something clicked by mistake. */
	private renderTroubleshooting(containerEl: HTMLElement): void {

		new Setting(containerEl)
			.setName("Re-bootstrap")
			.setDesc(
				"Discards this device's sync state and rebuilds it from the server. " +
					"Deletes nothing and never touches .local files, but a note that " +
					"differs from the server's is renamed aside as a conflict copy, and " +
					"many files may be re-sent. Use it to repair sync state or to point " +
					"this device at a different vault.",
			)
			.addButton((b) =>
				b.setWarning().setButtonText("Re-bootstrap").onClick(async () => {
					await this.plugin.sync.forceRebootstrap();
					new Notice("archivist: re-bootstrapped");
				}),
			);
	}

	private inventoryDevice = "";

	private renderPlugins(container: HTMLElement): void {
		const generation = ++this.renderGeneration;
		container.createEl("p", {text: "Reading plugin inventories…"});
		const refresh = async (): Promise<void> => {
			const request = ++this.inventoryRequest;
			try {
				const [local, remote] = await Promise.all([
					collectInventory(this.app, this.plugin.settings.device, Platform.isMobile ? "mobile" : "desktop"),
					readInventories(this.app.vault.adapter),
				]);
				if (generation !== this.renderGeneration || request !== this.inventoryRequest) return;
				if (!remote.inventories.some(i => i.installationId === this.inventoryDevice)) this.inventoryDevice = "";
				renderInventoryView(container, {local, ...remote, problem:this.plugin.inventoryProblem}, this.inventoryDevice,
					id => { this.inventoryDevice = id; void refresh(); }, () => void refresh());
			} catch (error) {
				if (generation !== this.renderGeneration || request !== this.inventoryRequest) return;
				container.empty();
				container.createEl("p", {text:error instanceof Error ? error.message : "Plugin inventories could not be read."});
				container.createEl("button", {text:"Retry"}).addEventListener("click", () => void refresh());
			}
		};
		// Refresh again on opening the collapsed section, including after a local install.
		container.addEventListener("archivist-section-open", () => void refresh());
		void refresh();
	}
	private inventoryRequest = 0;

	hide(): void {
		this.renderGeneration++;
	}
}
