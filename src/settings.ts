import { App, Notice, Platform, PluginSettingTab, Setting } from "obsidian";
import { hostname as osHostname } from "os";
import { Client } from "./client";
import { scopeWarning } from "./scopes";
import { loadState } from "./state";
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
	clampInterval,
	formatPermissions,
	formatRelativeTime,
	formatSyncSchedule,
	formatVaultStats,
	suggestDeviceName,
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
};

export const DEFAULT_SETTINGS: Settings = {
	serverUrl: "",
	vault: "",
	token: "",
	device: "",
	intervalSeconds: 300,
	syncOnChange: true,
	watchRemote: true,
};

export class ArchivistSettingTab extends PluginSettingTab {
	/** Filled in by "Test connection"; shown in the Status section once known. */
	private vaultStats?: { files: number; bytes: number };

	constructor(app: App, private plugin: ArchivistPlugin) {
		super(app, plugin);
	}

	display(): void {
		const { containerEl } = this;
		containerEl.empty();

		this.renderStatus(containerEl);
		this.renderConnection(containerEl);
		this.renderVaultAndDevice(containerEl);
		this.renderSyncBehavior(containerEl);
		this.renderMaintenance(containerEl);
		this.renderConfigSync(containerEl);
	}

	/** Health at a glance: what's configured, when it last synced, what the
	 *  token can do, and how big the vault is -- populated from the same
	 *  calls "Test connection" already makes, not new endpoints. */
	private renderStatus(containerEl: HTMLElement): void {
		new Setting(containerEl).setName("Status").setHeading();

		new Setting(containerEl)
			.setName("Sync schedule")
			.setDesc(
				formatSyncSchedule({
					syncOnChange: this.plugin.settings.syncOnChange,
					intervalSeconds: this.plugin.settings.intervalSeconds,
					watchRemote: this.plugin.settings.watchRemote,
				}),
			);

		const state = loadState(this.app);
		new Setting(containerEl)
			.setName("Last synced")
			.setDesc(formatRelativeTime(state.lastSyncedAt, Date.now()));

		const connectionSetting = new Setting(containerEl).setName("Connection").setDesc("Checking…");
		const permissionsSetting = new Setting(containerEl).setName("Permissions").setDesc("Checking…");

		if (this.vaultStats) {
			new Setting(containerEl)
				.setName("Vault size")
				.setDesc(formatVaultStats(this.vaultStats.files, this.vaultStats.bytes));
		}

		void this.refreshStatus(connectionSetting, permissionsSetting);

		// A deferred pairing question is otherwise unreachable: the modal only
		// reopens from a sync, and a deferred one stops syncing on purpose.
		if (this.plugin.pendingPairing) {
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
	}

	/** New automatic network calls on every tab open: index() for reachability,
	 *  listVaults() for scopes. Both already exist -- Test connection and the
	 *  vault picker call them too -- this just also fires them here. */
	private async refreshStatus(connectionSetting: Setting, permissionsSetting: Setting): Promise<void> {
		const { serverUrl, vault } = this.plugin.settings;
		const token = loadToken(this.app);

		if (!serverUrl || !token || !vault) {
			connectionSetting.setDesc("Not configured");
			permissionsSetting.setDesc("Not configured");
			return;
		}

		try {
			const idx = await new Client(serverUrl, token, vault).index();
			connectionSetting.setDesc(`Connected to ${serverUrl} (vault "${idx.vault || vault}")`);
		} catch (err) {
			connectionSetting.setDesc(
				`Server unreachable: ${err instanceof Error ? err.message : String(err)}`,
			);
		}

		try {
			const { scopes, label } = await new Client(serverUrl, token, "").listVaults();
			permissionsSetting.setDesc(formatPermissions(scopes, label));
		} catch (err) {
			permissionsSetting.setDesc(
				`Could not check: ${err instanceof Error ? err.message : String(err)}`,
			);
		}
	}

	private renderConnection(containerEl: HTMLElement): void {
		new Setting(containerEl).setName("Connection").setHeading();

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
	}

	private renderVaultAndDevice(containerEl: HTMLElement): void {
		new Setting(containerEl).setName("Vault & device").setHeading();

		this.renderVault(containerEl);

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

	/** Once a vault is resolved, it is shown as a fixed label rather than a
	 *  free-text field: the vault is tied to the token, not something to
	 *  retype. */
	private renderVault(containerEl: HTMLElement): void {
		const vault = this.plugin.settings.vault;

		if (!vault) {
			this.renderVaultChooser(containerEl);
			return;
		}

		const vaultSetting = new Setting(containerEl)
			.setName("Vault")
			.setDesc(
				`Which vault on that server. One server serves several; this device is ` +
					`synced with "${vault}".`,
			);

		void this.maybeShowChangeVaultButton(vaultSetting);
	}

	/** Only offered when the token can reach more than one vault -- changing to
	 *  the only vault it opens is not a choice. */
	private async maybeShowChangeVaultButton(vaultSetting: Setting): Promise<void> {
		const { serverUrl } = this.plugin.settings;
		const token = loadToken(this.app);
		if (!serverUrl || !token) return;

		try {
			const { vaults, scopes, label } = await new Client(serverUrl, token, "").listVaults();
			if (scopeWarning(scopes, label) || vaults.length <= 1) return;

			vaultSetting.addButton((b) =>
				b
					.setWarning()
					.setButtonText("Change vault")
					.onClick(() => {
						new VaultPickerModal(this.app, vaults, (v) => void this.chooseVault(v)).open();
					}),
			);
		} catch {
			// Server unreachable or token invalid: the Status section above
			// already says so. Nothing to add here.
		}
	}

	private async chooseVault(vault: string): Promise<void> {
		this.plugin.settings.vault = vault;
		await this.plugin.saveSettings();
		new Notice(`archivist: syncing vault "${vault}"`);
		this.display();
	}

	/** Fallback for an unresolved vault: no token yet, or a token open to more
	 *  than one vault with no prior choice. */
	private renderVaultChooser(containerEl: HTMLElement): void {
		const vaultSetting = new Setting(containerEl)
			.setName("Vault")
			.setDesc(
				"Which vault on that server. One server serves several; the URL and " +
					"this field together say which one. Changing it points this Obsidian " +
					"vault at different content, and the plugin refuses to sync if it does " +
					"not match what this device already adopted.",
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
					const { vaults, scopes, label } = await new Client(serverUrl, token, "").listVaults();
					// Refuse here rather than at the first save. A token without
					// both verbs syncs down happily and then fails on a write,
					// with the note already edited -- the worst moment to learn
					// the credential was the wrong one.
					const warning = scopeWarning(scopes, label);
					if (warning) {
						new Notice(`archivist: ${warning}`, 12000);
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

	private renderSyncBehavior(containerEl: HTMLElement): void {
		new Setting(containerEl).setName("Sync behavior").setHeading();

		new Setting(containerEl)
			.setName("Sync on change")
			.setDesc("Sync shortly after edits, in addition to the interval below.")
			.addToggle((t) =>
				t.setValue(this.plugin.settings.syncOnChange).onChange(async (v) => {
					this.plugin.settings.syncOnChange = v;
					await this.plugin.saveSettings();
				}),
			);

		new Setting(containerEl)
			.setName("Sync interval (seconds)")
			.setDesc(
				"Background sync period. 0 disables it; syncing on focus and on change " +
					"still happens. Values below 10 are rounded up to 10, to guard " +
					"against a typo hammering the server.",
			)
			.addText((t) =>
				t.setValue(String(this.plugin.settings.intervalSeconds)).onChange(async (v) => {
					const n = Number(v);
					if (!Number.isFinite(n) || n < 0) return;
					this.plugin.settings.intervalSeconds = clampInterval(n);
					await this.plugin.saveSettings();
					this.plugin.restartTimer();
				}),
			);

		new Setting(containerEl)
			.setName("Watch for remote changes")
			.setDesc(
				"Hold a connection open so changes from other devices arrive in about a " +
					"second instead of waiting for the interval. Costs one idle connection " +
					"and no traffic while nothing changes. Turn off to rely on the interval alone.",
			)
			.addToggle((t) =>
				t.setValue(this.plugin.settings.watchRemote).onChange(async (v) => {
					this.plugin.settings.watchRemote = v;
					await this.plugin.saveSettings();
					this.plugin.restartWatcher();
				}),
			);
	}

	private renderMaintenance(containerEl: HTMLElement): void {
		new Setting(containerEl).setName("Maintenance").setHeading();

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
						// Re-render so the Status section above picks up vaultStats.
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
					"nothing — local-only files are pushed on the next sync. It's marked " +
					"red because discarding correct history unnecessarily can cause a " +
					"burst of re-push/re-pull churn — only use it when something actually " +
					"looks wrong.",
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
			containerEl.createEl("p", {
				text:
					`Config sync is unavailable: this vault's configuration directory is ` +
					`"${this.app.vault.configDir}" rather than "${CONFIG_DIR}", and the ` +
					`server's allowlist names the default. Notes sync normally.`,
			});
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

		containerEl.createEl("p", {
			text:
				"Plugin settings (data.json) are off for every plugin until you turn " +
				"one on below. Each is scanned first, and a plugin whose settings look " +
				"like they hold a credential is refused with a reason — nothing is " +
				"stripped or rewritten, so you never get a settings file with a hole in it.",
		});

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
