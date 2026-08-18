import { App, Modal, Setting } from "obsidian";
import type { PairingChoice, PairingHazardError } from "./pairing";

/**
 * Asks which of the three recoveries to take.
 *
 * A modal rather than a Notice, and no default action, because every option
 * moves files and there is no safe guess. Dismissing it does nothing at all,
 * which is the correct outcome for someone who wants to think about it.
 *
 * The UI lives here rather than in sync.ts so the sync engine stays free of
 * Obsidian's UI classes and can be bundled and driven from Node.
 */
export class PairingModal extends Modal {
	constructor(
		app: App,
		private readonly hazard: PairingHazardError,
		private readonly onChoice: (choice: PairingChoice) => void,
	) {
		super(app);
	}

	onOpen(): void {
		const { contentEl } = this;
		contentEl.empty();
		contentEl.createEl("h2", { text: "Both this vault and the server have content" });

		contentEl.createEl("p", {
			text:
				`This vault holds ${this.hazard.localFiles} file(s) and has never synced ` +
				`with this server, and the server already has a history of its own. ` +
				`Syncing now would merge two unrelated vaults into both, and every ` +
				`file that exists on both sides under the same name would become a ` +
				`conflict pair.`,
		});
		contentEl.createEl("p", { text: "Nothing has been changed. Choose what should happen:" });

		new Setting(contentEl)
			.setName("Adopt server")
			.setDesc(
				"Move this vault's files into a dated _archivist-rescued-… folder, then " +
					"pull the server's. Nothing is deleted, and the rescued folder syncs " +
					"to your other devices too.",
			)
			.addButton((b) => b.setButtonText("Adopt server").onClick(() => this.choose("adopt")));

		new Setting(contentEl)
			.setName("Publish local")
			.setDesc(
				"Push this vault over the server's. No rescue folder is needed: every " +
					"version of every server file is already in git history. Files only " +
					"the server has are kept and arrive on the next sync.",
			)
			.addButton((b) => b.setButtonText("Publish local").onClick(() => this.choose("publish")));

		new Setting(contentEl)
			.setName("Merge anyway")
			.setDesc(
				"Take the union of both. Files present on both sides under the same " +
					"name become conflict pairs you resolve by hand.",
			)
			.addButton((b) =>
				b
					.setWarning()
					.setButtonText("Merge anyway")
					.onClick(() => this.choose("merge")),
			);
	}

	private choose(choice: PairingChoice): void {
		this.close();
		this.onChoice(choice);
	}

	onClose(): void {
		this.contentEl.empty();
	}
}
