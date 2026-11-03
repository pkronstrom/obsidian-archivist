import { App, Modal, Setting } from "obsidian";
import type { PairingChoice, PairingHazardError, VaultComparison } from "./pairing";

/** A first connect against an unrelated vault can differ in thousands of
 *  files; past this the list stops being readable anyway. */
const MAX_LISTED = 200;

/**
 * Asks which of the three recoveries to take.
 *
 * A modal rather than a Notice, and no default action: dismissing it does
 * nothing at all, which is the correct outcome for someone who wants to think
 * about it. It does show what each side holds, and marks Merge recommended
 * when no file differs -- then merging can only copy missing files across.
 *
 * The UI lives here rather than in sync.ts so the sync engine stays free of
 * Obsidian's UI classes and can be bundled and driven from Node.
 */
export class PairingModal extends Modal {
	constructor(
		app: App,
		private readonly hazard: PairingHazardError,
		private readonly onChoice: (choice: PairingChoice) => void,
		private readonly onDefer: () => void = () => {},
	) {
		super(app);
	}

	/**
	 * Fires on every close -- a choice, "Not now", the X and Escape alike.
	 *
	 * The owner uses it to forget the open modal. Without it a dismissal by X
	 * leaves the owner believing a modal is still up and it can never reopen.
	 */
	onClosed: () => void = () => {};

	onOpen(): void {
		const { contentEl } = this;
		const c = this.hazard.comparison;
		const n = this.hazard.localFiles;
		contentEl.empty();
		contentEl.addClass("archivist-pairing");
		contentEl.createEl("h2", { text: "This vault has not synced with this server yet" });

		contentEl.createEl("p", {
			text:
				`Both sides already hold files, so Archivist compared them before ` +
				`doing anything. Nothing has been changed yet.`,
		});

		const list = contentEl.createEl("ul", { cls: "archivist-pairing-summary" });
		list.createEl("li", {
			text: `${c.identical} of this vault's ${plural(n, "file")} match the server exactly. Every choice leaves them alone.`,
		});
		this.group(list, c.differ, "differ from the server's version");
		this.group(list, c.onlyHere, "only on this device");
		this.group(list, c.onlyServer, "only on the server");

		contentEl.createEl("p", { text: mergeOutcome(c) });

		const safe = c.differ.length === 0;
		new Setting(contentEl)
			.setName(safe ? "Merge (recommended)" : "Merge")
			.setDesc(
				"Keep everything from both sides. Files only on one side are copied " +
					"to the other. " +
					(safe
						? "No file differs, so nothing is overwritten or duplicated."
						: "Each file that differs keeps the server's version at its path " +
							"and this device's in a .conflict file beside it."),
			)
			.addButton((b) => {
				b.setButtonText("Merge").onClick(() => this.choose("merge"));
				if (safe) b.setCta();
			});

		new Setting(contentEl)
			.setName("Use the server's vault")
			.setDesc(
				`Move all ${plural(n, "file")} on this device into a dated ` +
					"_archivist-rescued-… folder, then download the server's. Nothing is " +
					"deleted, and the rescued folder syncs to your other devices too.",
			)
			.addButton((b) => b.setButtonText("Adopt server").onClick(() => this.choose("adopt")));

		new Setting(contentEl)
			.setName("Use this device's vault")
			.setDesc(
				(c.differ.length > 0
					? `This device's version wins for the ${plural(c.differ.length, "file")} that differ. `
					: "") +
					"The server's older versions stay in its history, and files only " +
					"the server has are kept and arrive on the next sync.",
			)
			.addButton((b) => b.setButtonText("Publish local").onClick(() => this.choose("publish")));

		// An explicit way out. Without one the only exit is dismissing the
		// modal, and because runSync fires on an interval, on focus, on blur
		// and on every file change, the dialog reopens within seconds -- so
		// "not now" was not actually available.
		new Setting(contentEl)
			.setName("Decide later")
			.setDesc(
				"Stop asking until you come back to it. This device will not sync " +
					"meanwhile — reopen the choice from Archivist's settings.",
			)
			.addButton((b) => b.setButtonText("Not now").onClick(() => this.dismiss()));
	}

	/** One summary line, with the paths behind a disclosure. Empty groups are
	 *  left out: "0 files only on the server" is noise. */
	private group(list: HTMLElement, paths: string[], what: string): void {
		if (paths.length === 0) return;
		const details = list.createEl("li").createEl("details");
		details.createEl("summary", { text: `${plural(paths.length, "file")} ${what}` });
		const ul = details.createEl("ul", { cls: "archivist-pairing-paths" });
		for (const p of paths.slice(0, MAX_LISTED)) ul.createEl("li", { text: p });
		if (paths.length > MAX_LISTED) {
			ul.createEl("li", { text: `…and ${paths.length - MAX_LISTED} more` });
		}
	}

	private dismiss(): void {
		this.close();
		this.onDefer();
	}

	onClose(): void {
		this.contentEl.empty();
		this.onClosed();
	}

	private choose(choice: PairingChoice): void {
		this.close();
		this.onChoice(choice);
	}
}

function plural(n: number, word: string): string {
	return `${n} ${word}${n === 1 ? "" : "s"}`;
}

/** What Merge would do, in one sentence, from the comparison alone. */
export function mergeOutcome(c: VaultComparison): string {
	if (c.differ.length === 0 && c.onlyHere.length === 0 && c.onlyServer.length === 0) {
		return "This device already matches the server. Merging changes nothing.";
	}
	if (c.differ.length === 0) {
		return "No file differs, so merging only copies the missing files across. Nothing is lost.";
	}
	return (
		`Merging keeps both versions of the ${plural(c.differ.length, "file")} that differ, ` +
		"so nothing is lost either way."
	);
}
