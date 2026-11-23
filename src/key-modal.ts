import { App, Modal, Notice, Setting } from "obsidian";
import { VaultCrypt, decodeRecoveryKey, encodeRecoveryKey, generateKey } from "./crypt";
import type { KeyRequiredError } from "./sync";

/**
 * Sets up the key for an encrypted vault on this device.
 *
 * Two cases, decided by the server. A vault with no key check yet has never
 * been synced: this device creates the key. A vault that has one already has a
 * key, and it must be typed in -- generating another would produce a device
 * that can write notes nobody else can read.
 *
 * Like the pairing modal it has no default action. Closing it changes nothing
 * and the device simply stays paused.
 */
export class KeyModal extends Modal {
	constructor(
		app: App,
		private readonly need: KeyRequiredError,
		private readonly onKey: (key: Uint8Array) => void,
		private readonly onDefer: () => void = () => {},
	) {
		super(app);
	}

	/** Fires on every close; see PairingModal.onClosed. */
	onClosed: () => void = () => {};

	private decided = false;

	/** The key is for the .encrypted files of an ordinary vault. */
	private get files(): boolean {
		return this.need.scope === "files";
	}

	onOpen(): void {
		this.contentEl.addClass("archivist-key");
		if (this.need.hasKey) this.renderEnter();
		else this.renderCreate();
	}

	private renderCreate(): void {
		const { contentEl } = this;
		contentEl.empty();
		contentEl.createEl("h2", { text: `Create the key for "${this.need.vault}"` });
		contentEl.createEl("p", {
			text: this.files
				? `This vault has files marked .encrypted and no key yet. This device will ` +
					`create it. Those files are encrypted here before they are sent, and the ` +
					`server never holds the key. They stay readable on this device, and the ` +
					`rest of the vault syncs as before.`
				: `This is an encrypted vault and nothing has been synced to it yet. ` +
					`This device will create its key. Notes and file names are encrypted ` +
					`here before they are sent, and the server never holds the key.`,
		});
		new Setting(contentEl)
			.addButton((b) =>
				b
					.setCta()
					.setButtonText("Create key")
					.onClick(() => void this.renderSave(generateKey())),
			)
			.addButton((b) => b.setButtonText("Not now").onClick(() => this.close()));
	}

	/**
	 * Shows the recovery key before the key is used for anything.
	 *
	 * The order is the safety property: nothing is encrypted, and the server
	 * is not told a key exists, until the user has said they saved it. So no
	 * ciphertext can exist that only this one device could ever read.
	 */
	private async renderSave(key: Uint8Array): Promise<void> {
		const { contentEl } = this;
		const recovery = await encodeRecoveryKey(key);
		contentEl.empty();
		contentEl.createEl("h2", { text: "Save this recovery key" });
		contentEl.createEl("p", {
			text:
				`Put it in your password manager now. You need it to add another ` +
				`device and to read a backup. If every device that holds it is lost ` +
				`and you do not have this, ${this.files ? "the encrypted files" : "the vault"} cannot be read by anyone.`,
		});
		const box = contentEl.createEl("textarea", { cls: "archivist-key-box" });
		box.value = recovery;
		box.readOnly = true;
		box.rows = 3;

		let saved = false;
		let go: { setDisabled(disabled: boolean): unknown } | undefined;
		new Setting(contentEl)
			.addButton((b) =>
				b.setButtonText("Copy").onClick(async () => {
					await navigator.clipboard.writeText(recovery);
					new Notice("archivist: recovery key copied");
				}),
			);
		new Setting(contentEl)
			.setName("I have saved the recovery key")
			.addToggle((t) =>
				t.setValue(false).onChange((v) => {
					saved = v;
					go?.setDisabled(!saved);
				}),
			);
		new Setting(contentEl)
			.addButton((b) => {
				go = b;
				b.setCta()
					.setButtonText("Start syncing")
					.setDisabled(true)
					.onClick(() => {
						if (!saved) return;
						this.finish(key);
					});
			})
			.addButton((b) => b.setButtonText("Cancel").onClick(() => this.close()));
	}

	private renderEnter(): void {
		const { contentEl } = this;
		contentEl.empty();
		contentEl.createEl("h2", { text: `Enter the key for "${this.need.vault}"` });
		contentEl.createEl("p", {
			text: this.files
				? `This vault has files marked .encrypted, and they already have a key. ` +
					`Enter the recovery key that was shown when it was created. Until then ` +
					`those files are not synced on this device; everything else is.`
				: `This is an encrypted vault that already has a key. Enter the ` +
					`recovery key that was shown when the vault was first set up.`,
		});
		const box = contentEl.createEl("textarea", { cls: "archivist-key-box" });
		box.rows = 3;
		box.placeholder = "ABCDE-FGHIJ-…";
		const problem = contentEl.createEl("p", { cls: "archivist-key-problem" });

		new Setting(contentEl)
			.addButton((b) =>
				b
					.setCta()
					.setButtonText("Unlock")
					.onClick(async () => {
						problem.setText("");
						try {
							const key = await decodeRecoveryKey(box.value);
							// Checked against the server's value here, before the
							// key is stored or anything is read with it.
							if ((await VaultCrypt.fromKey(key)).keyCheck !== this.need.keyCheck) {
								problem.setText("That is a valid recovery key, but it is not the key for this vault.");
								return;
							}
							this.finish(key);
						} catch (err) {
							problem.setText(err instanceof Error ? err.message : String(err));
						}
					}),
			)
			.addButton((b) => b.setButtonText("Not now").onClick(() => this.close()));
	}

	private finish(key: Uint8Array): void {
		this.decided = true;
		this.close();
		this.onKey(key);
	}

	onClose(): void {
		this.contentEl.empty();
		if (!this.decided) this.onDefer();
		this.onClosed();
	}
}
