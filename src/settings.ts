import { App, PluginSettingTab, Setting, Notice } from "obsidian";
import { Client } from "./client";
import { loadToken, saveToken } from "./credentials";
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
}
