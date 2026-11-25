import { App, Modal, Setting } from "obsidian";
import { SwitchRefused, type SwitchPlan } from "./switch";

/**
 * Asks where to switch to, runs the checks, and confirms.
 *
 * Two steps on purpose. The first collects the vault and token and changes
 * nothing; the second shows what was found -- both vaults by name and kind --
 * and only its button switches.
 */
export class SwitchModal extends Modal {
	private vault = "";
	private token = "";

	constructor(
		app: App,
		private readonly currentVault: string,
		/** Runs the checks. skipSync is the user's override of the one
		 *  overridable refusal. */
		private readonly plan: (vault: string, token: string, skipSync: boolean) => Promise<SwitchPlan>,
		private readonly onConfirm: (plan: SwitchPlan, token: string) => void,
	) {
		super(app);
	}

	onOpen(): void {
		this.contentEl.addClass("archivist-key");
		this.renderAsk();
	}

	private renderAsk(problem = "", overridable = false): void {
		const { contentEl } = this;
		contentEl.empty();
		contentEl.createEl("h2", { text: "Switch this folder to another vault" });
		contentEl.createEl("p", {
			text:
				`This folder syncs with "${this.currentVault}". Switching points it at a new, ` +
				`empty vault and publishes every note there. Your notes are not changed, and ` +
				`"${this.currentVault}" stays on the server as it is until you delete it yourself.`,
		});
		new Setting(contentEl)
			.setName("New vault")
			.setDesc("Its name on the server. It must be empty.")
			.addText((t) => t.setValue(this.vault).onChange((v) => (this.vault = v.trim())));
		new Setting(contentEl)
			.setName("Token")
			.setDesc("A token that opens the new vault. Leave empty to keep the current one.")
			.addText((t) => t.setValue(this.token).onChange((v) => (this.token = v.trim())));
		if (problem) contentEl.createEl("p", { cls: "archivist-key-problem", text: problem });

		const row = new Setting(contentEl).addButton((b) =>
			b
				.setCta()
				.setButtonText("Check")
				.onClick(() => void this.check(false)),
		);
		if (overridable) {
			row.addButton((b) =>
				b
					.setWarning()
					.setButtonText("Continue without syncing")
					.onClick(() => void this.check(true)),
			);
		}
		row.addButton((b) => b.setButtonText("Cancel").onClick(() => this.close()));
	}

	private async check(skipSync: boolean): Promise<void> {
		if (!this.vault) {
			this.renderAsk("Name the vault to switch to.");
			return;
		}
		this.contentEl.empty();
		this.contentEl.createEl("p", { text: "Checking both vaults…" });
		try {
			this.renderConfirm(await this.plan(this.vault, this.token, skipSync));
		} catch (err) {
			const msg = err instanceof Error ? err.message : String(err);
			this.renderAsk(msg, err instanceof SwitchRefused && err.overridable);
		}
	}

	private renderConfirm(plan: SwitchPlan): void {
		const { contentEl } = this;
		const kind = (encrypted: boolean) => (encrypted ? "encrypted" : "not encrypted");
		contentEl.empty();
		contentEl.createEl("h2", { text: "Confirm the switch" });
		const list = contentEl.createEl("ul");
		list.createEl("li", { text: `From "${plan.from.vault}" (${kind(plan.from.encrypted)})` });
		list.createEl("li", { text: `To "${plan.to.vault}" (${kind(plan.to.encrypted)}, empty)` });
		if (plan.to.encrypted) {
			contentEl.createEl("p", {
				text: "Next you will create or enter the vault's key. Nothing is uploaded before that.",
			});
		}
		if (plan.unsynced) {
			contentEl.createEl("p", {
				cls: "archivist-key-problem",
				text:
					`"${plan.from.vault}" was not synced first. Anything other devices wrote there ` +
					`that this folder has not pulled will be missing from the new vault.`,
			});
		}
		contentEl.createEl("p", {
			text: "Other devices keep syncing with the old vault until you switch them too.",
		});

		let typed = "";
		let go: { setDisabled(disabled: boolean): unknown } | undefined;
		if (plan.exposes) {
			contentEl.createEl("p", {
				cls: "archivist-key-problem",
				text:
					`"${plan.to.vault}" is not encrypted. Every note in this folder will be uploaded ` +
					`as readable text. Type the vault's name to confirm.`,
			});
			new Setting(contentEl).addText((t) =>
				t.setPlaceholder(plan.to.vault).onChange((v) => {
					typed = v.trim();
					go?.setDisabled(typed !== plan.to.vault);
				}),
			);
		}
		new Setting(contentEl)
			.addButton((b) => {
				go = b;
				b.setCta()
					.setButtonText("Switch")
					.setDisabled(plan.exposes)
					.onClick(() => {
						if (plan.exposes && typed !== plan.to.vault) return;
						this.close();
						this.onConfirm(plan, this.token);
					});
			})
			.addButton((b) => b.setButtonText("Back").onClick(() => this.renderAsk()));
	}

	onClose(): void {
		this.contentEl.empty();
	}
}
