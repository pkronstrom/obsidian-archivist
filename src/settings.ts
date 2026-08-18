import { App, PluginSettingTab, Setting, Notice } from "obsidian";
import { Client } from "./client";
import { loadToken, saveToken } from "./credentials";
import {
	CONFIG_DIR,
	loadConfigSync,
	saveConfigSync,
	type ConfigLevel,
	type ConfigSyncSettings,
} from "./config-sync";
import { scanForSecrets } from "./secrets";
import type ArchivistPlugin from "./main";

export type Settings = {
	serverUrl: string;
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
	token: "",
	device: "",
	intervalSeconds: 300,
	syncOnChange: true,
	watchRemote: true,
};

export class ArchivistSettingTab extends PluginSettingTab {
	constructor(app: App, private plugin: ArchivistPlugin) {
		super(app, plugin);
	}

	display(): void {
		const { containerEl } = this;
		containerEl.empty();

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
			.addText((t) =>
				t.setPlaceholder("mac / iphone")
					.setValue(this.plugin.settings.device)
					.onChange(async (v) => {
						this.plugin.settings.device = v.trim();
						await this.plugin.saveSettings();
					}),
			);

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
			.setDesc("Background sync period. 0 disables it; syncing on focus and on change still happens.")
			.addText((t) =>
				t.setValue(String(this.plugin.settings.intervalSeconds)).onChange(async (v) => {
					const n = Number(v);
					if (!Number.isFinite(n) || n < 0) return;
					this.plugin.settings.intervalSeconds = Math.floor(n);
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
						const client = new Client(serverUrl, token);
						const idx = await client.index();
						const files = Object.keys((await client.snapshot()).files).length;
						const name = idx.vault || "unnamed (older server)";
						new Notice(
							`archivist: connected to vault "${name}" — ${files} file(s), ` +
								`server ${idx.version}`,
							10000,
						);
					} catch (err) {
						new Notice(`archivist: ${err instanceof Error ? err.message : String(err)}`, 8000);
					}
				}),
			);

		this.renderConfigSync(containerEl);

		new Setting(containerEl)
			.setName("Re-bootstrap from server")
			.setDesc("Discards local sync state and adopts the server's. Deletes nothing; local-only files are pushed on the next sync.")
			.addButton((b) =>
				b.setWarning().setButtonText("Re-bootstrap").onClick(async () => {
					await this.plugin.sync.forceRebootstrap();
					new Notice("archivist: re-bootstrapped");
				}),
			);
	}

	/** The config-sync section: level, then per-plugin opt-ins. */
	private renderConfigSync(containerEl: HTMLElement): void {
		containerEl.createEl("h3", { text: "Obsidian config" });

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
