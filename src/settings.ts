import { App, PluginSettingTab, Setting, Notice } from "obsidian";
import { Client } from "./client";
import type VaultsyncPlugin from "./main";

export type Settings = {
	serverUrl: string;
	token: string;
	device: string;
	intervalSeconds: number;
	syncOnChange: boolean;
};

export const DEFAULT_SETTINGS: Settings = {
	serverUrl: "",
	token: "",
	device: "",
	intervalSeconds: 300,
	syncOnChange: true,
};

export class VaultsyncSettingTab extends PluginSettingTab {
	constructor(app: App, private plugin: VaultsyncPlugin) {
		super(app, plugin);
	}

	display(): void {
		const { containerEl } = this;
		containerEl.empty();

		new Setting(containerEl)
			.setName("Server URL")
			.setDesc("Base URL of your vaultsync server, e.g. https://vault.example.net")
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
			.setDesc("Bearer token the server expects.")
			.addText((t) => {
				t.inputEl.type = "password";
				t.setValue(this.plugin.settings.token).onChange(async (v) => {
					this.plugin.settings.token = v.trim();
					await this.plugin.saveSettings();
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

		// Most setup failures are a wrong URL or a wrong token. A button that
		// says which beats asking someone to read a console on a phone.
		new Setting(containerEl)
			.setName("Test connection")
			.setDesc("Calls /v1/head and reports what came back.")
			.addButton((b) =>
				b.setButtonText("Test").onClick(async () => {
					const { serverUrl, token } = this.plugin.settings;
					if (!serverUrl || !token) {
						new Notice("vaultsync: set the server URL and token first");
						return;
					}
					try {
						const head = await new Client(serverUrl, token).head();
						new Notice(head ? `vaultsync: connected, head ${head.slice(0, 8)}` : "vaultsync: connected, empty vault");
					} catch (err) {
						new Notice(`vaultsync: ${err instanceof Error ? err.message : String(err)}`, 8000);
					}
				}),
			);

		new Setting(containerEl)
			.setName("Re-bootstrap from server")
			.setDesc("Discards local sync state and adopts the server's. Deletes nothing; local-only files are pushed on the next sync.")
			.addButton((b) =>
				b.setWarning().setButtonText("Re-bootstrap").onClick(async () => {
					await this.plugin.sync.forceRebootstrap();
					new Notice("vaultsync: re-bootstrapped");
				}),
			);
	}
}
