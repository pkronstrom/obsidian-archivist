import { App, Notice, Platform, PluginSettingTab, Setting } from "obsidian";
import { hostname as osHostname } from "os";
import { Client } from "./client";
import { PinModal } from "./revision-modal";
import { scopeWarning, stepUpWarning } from "./scopes";
import { isFirstRun, loadState } from "./state";
import { loadToken, saveToken } from "./credentials";
import {
	CONFIG_DIR,
	loadConfigSync,
	saveConfigSync,
	type ConfigLevel,
	type ConfigSyncSettings,
} from "./config-sync";
import { scanForSecrets } from "./secrets";
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
	/**
	 * Kept for migration only. The live token is in device-local storage; see
	 * credentials.ts. Anything reading the token must call loadToken.
	 */
	token: string;
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
};

export const DEFAULT_SETTINGS: Settings = {
	serverUrl: "",
	vault: "",
	token: "",
	device: "",
	intervalSeconds: 300,
	syncOnChange: true,
	watchRemote: true,
	syncQuietSeconds: 15,
};

export class ArchivistSettingTab extends PluginSettingTab {
	/** Filled in by "Test connection"; shown in the Status card once known. */
	private vaultStats?: { files: number; bytes: number };

	constructor(app: App, private plugin: ArchivistPlugin) {
		super(app, plugin);
	}

	display(): void {
		const { containerEl } = this;
		containerEl.empty();

		containerEl.createEl("h2", { text: "Archivist – self-hosted Obsidian sync" });

		// A control-less Setting rather than a bare <p>: the description class
		// only gets its muted styling inside a .setting-item, and themes align
		// and pad setting rows -- loose paragraphs sit off-grid beside them.
		new Setting(containerEl).setDesc(
			createFragment((f) => {
				f.appendText(
					"Self-hosted sync: your vault stays plain Markdown files on a " +
						"server you own, with full history. This plugin connects to " +
						"your own Archivist server. Setup instructions: ",
				);
				f.createEl("a", {
					href: "https://github.com/pkronstrom/obsidian-archivist",
					text: "github.com/pkronstrom/obsidian-archivist",
				});
			}),
		);

		this.renderStatus(containerEl);
		this.renderServer(containerEl);
		this.renderSyncBehavior(containerEl);
		this.renderMaintenance(containerEl);
		this.renderConfigSync(containerEl);
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
			card.createDiv({ text: "Not configured — set the server URL and token below." });
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
		new Setting(containerEl).setName("Server").setHeading();

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
				"Bearer token the server expects. Stored on this device only — " +
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
		// The deliberate escape hatch is Re-bootstrap, under Maintenance.
		if (isFirstRun(loadState(this.app))) {
			this.renderVaultChooser(containerEl);
		}
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
		void this.plugin.resolveVaultIfUnambiguous().then((ok) => {
			if (ok) this.display();
		});

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
		new Setting(containerEl).setName("Sync behavior").setHeading();

		// This is the ONLY place a user learns why a file stopped syncing, so
		// it is a visible setting row rather than a tooltip. A file that
		// silently does not sync is only safe when the person chose the name
		// that stopped it -- and that bargain depends entirely on the rule
		// being written somewhere they will read it.
		new Setting(containerEl)
			.setName("Files that never sync")
			.setDesc(
				"Anything marked .local stays on this device and is never uploaded: a " +
					"file like Scratch.local.md, or a whole folder like Journal.local/. " +
					"Rename it without the .local part to start syncing it. A folder must " +
					"END in .local to count, so an ordinary folder such as " +
					"project.local.assets keeps syncing normally. Revisions you open from " +
					"the history browser are saved this way, so browsing an old version " +
					"never touches your other devices.",
			);

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
				.setName("Wait after typing stops (seconds)")
				.setDesc(
					"How long editing must pause before syncing. Lower syncs sooner; higher " +
						"keeps note history readable, because every sync is a commit. " +
						"A long unbroken burst still syncs at least every 30 seconds.",
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

	private renderMaintenance(containerEl: HTMLElement): void {
		new Setting(containerEl).setName("Maintenance").setHeading();

		// Vault-wide pins live HERE and not in the note's revision modal. They
		// are not about any one note, and interleaving them into a file's
		// history would make the user filter someone else's chronology out of
		// the one they asked for. Same audience as re-bootstrap: rare,
		// deliberate, whole-vault.
		new Setting(containerEl)
			.setName("Vault restore points")
			.setDesc(
				"Name the vault's current state before a big reorganisation. A restore " +
					"point is a line in pins.jsonl, so it syncs and survives history " +
					"cleanups like any note. It always marks now \u2014 to keep an older " +
					"state, open it from a note's history first.",
			)
			.addButton((b) =>
				b.setButtonText("Pin the vault now").onClick(() => {
					const { serverUrl, vault } = this.plugin.settings;
					const token = loadToken(this.app);
					if (!serverUrl || !token) {
						new Notice("archivist: set the server URL and token first");
						return;
					}
					new PinModal(
						this.app,
						undefined,
						() => new Client(serverUrl, token, vault),
						() => loadState(this.app).base,
						() => this.plugin.flushSync(),
					).open();
				}),
			);

		// Most setup failures are a wrong URL or a wrong token. A button that
		// says which beats asking someone to read a console on a phone.
		new Setting(containerEl)
			.setName("Test connection")
			.setDesc(
				"Reports WHICH VAULT the server serves, so you can confirm you " +
					"pointed this Obsidian vault at the right one. With more than one " +
					"vault, the URL and token are the only things distinguishing them, " +
					"and the local folder name tells you nothing: you create it empty " +
					"before the plugin can connect.",
			)
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
						const name = idx.vault || "unnamed (older server)";
						new Notice(
							`archivist: connected to vault "${name}" — ${entries.length} file(s), ` +
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

		new Setting(containerEl)
			.setName("Re-bootstrap from server")
			.setDesc(
				"Use this if this device's sync history seems wrong — e.g. after " +
					"restoring this vault from a backup, editing files outside of sync, or " +
					"a sync that got stuck. It makes the plugin forget what it last saw " +
					"here and re-derive that from what the server has now. Deletes " +
					"nothing — files only this device has are pushed on the next sync (except "
					+ "anything marked .local, which never syncs by design). It is also " +
					"the escape hatch for pointing this device at a different vault on " +
					"purpose. It's marked red because discarding correct history " +
					"unnecessarily can cause a burst of re-push/re-pull churn — only use " +
					"it when something actually looks wrong.",
			)
			.addButton((b) =>
				b.setWarning().setButtonText("Re-bootstrap").onClick(async () => {
					await this.plugin.sync.forceRebootstrap();
					new Notice("archivist: re-bootstrapped");
				}),
			);
	}

	/** The config-sync section: level, then per-plugin opt-ins. */
	private renderConfigSync(containerEl: HTMLElement): void {
		new Setting(containerEl).setName("Obsidian config").setHeading();

		// Obsidian lets the config directory be renamed. This plugin does not
		// follow that: the whole point of syncing config in-vault is that files
		// land where Obsidian looks with no translation step, and a rename
		// would reintroduce exactly that step. Say so rather than syncing the
		// wrong paths.
		if (this.app.vault.configDir !== CONFIG_DIR) {
			new Setting(containerEl).setDesc(
				`Config sync is unavailable: this vault's configuration directory is ` +
					`"${this.app.vault.configDir}" rather than "${CONFIG_DIR}", and the ` +
					`server's allowlist names the default. Notes sync normally.`,
			);
			return;
		}

		const config = loadConfigSync(this.app);

		new Setting(containerEl)
			.setName("What to sync")
			.setDesc(
				"Chosen per device and never synced itself, so a phone can stay on " +
					"Files only while a laptop syncs everything. Workspace layout, the " +
					"graph view and plugin caches never sync at any level.",
			)
			.addDropdown((d) =>
				d
					.addOption("files", "Files only")
					.addOption("appearance", "Files + appearance")
					.addOption("plugins", "Files + appearance + plugins")
					.setValue(config.level)
					.onChange(async (v) => {
						config.level = v as ConfigLevel;
						saveConfigSync(this.app, config);
						this.display();
					}),
			);

		if (config.level !== "plugins") return;

		new Setting(containerEl).setName("Plugin settings").setHeading();

		new Setting(containerEl).setDesc(
			"Settings (data.json) are off for every plugin until you turn one on " +
				"below. Each is scanned first, and a plugin whose settings look like " +
				"they hold a credential is refused with a reason. Nothing is stripped " +
				"or rewritten, so you never get a settings file with a hole in it.",
		);

		void this.renderPluginOptIns(containerEl, config);
	}

	/**
	 * One row per installed plugin that has a data.json, with what the scanner
	 * saw. Archivist's own is absent by construction: it is excluded on the
	 * server with no override, so offering the switch would be a lie.
	 */
	private async renderPluginOptIns(
		containerEl: HTMLElement,
		config: ConfigSyncSettings,
	): Promise<void> {
		const adapter = this.app.vault.adapter;
		const dir = `${CONFIG_DIR}/plugins`;
		if (!(await adapter.exists(dir))) return;

		const { folders } = await adapter.list(dir);
		for (const folder of folders.sort()) {
			const id = folder.slice(dir.length + 1);
			if (id.toLowerCase() === "archivist" || id.toLowerCase() === "obsidian-archivist") continue;

			const dataPath = `${folder}/data.json`;
			if (!(await adapter.exists(dataPath))) continue;

			let suspicions: { path: string; why: string }[] = [];
			let unreadable = false;
			try {
				suspicions = scanForSecrets(JSON.parse(await adapter.read(dataPath)));
			} catch {
				unreadable = true;
			}

			const desc = unreadable
				? "Its data.json could not be parsed, so it cannot be checked. Not offered."
				: suspicions.length === 0
					? "Nothing recognised as a credential. That is not a guarantee — the scanner reports what it recognises, and it cannot recognise everything."
					: `Refused: ${suspicions.map((x) => `${x.path} — ${x.why}`).join("; ")}`;

			const setting = new Setting(containerEl).setName(id).setDesc(desc);
			if (unreadable) continue;

			setting.addToggle((t) =>
				t.setValue(config.acceptedPlugins.includes(id)).onChange(async (on) => {
					if (on && suspicions.length > 0) {
						// The override exists, and it states what is being
						// accepted. It is not a dismissal.
						new Notice(
							`archivist: "${id}" is being synced despite ${suspicions.length} ` +
								`suspected credential(s). Those values will be in git history ` +
								`permanently. Turn it off and rotate them if that was not intended.`,
							15000,
						);
					}
					config.acceptedPlugins = on
						? [...new Set([...config.acceptedPlugins, id])]
						: config.acceptedPlugins.filter((p) => p !== id);
					saveConfigSync(this.app, config);
				}),
			);
		}
	}
}
