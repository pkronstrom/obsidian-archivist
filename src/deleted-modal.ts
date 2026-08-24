/**
 * Recover a note that was deleted.
 *
 * Nothing here is new capability: every version has always been in the
 * repository, and reading one has always been possible for anyone who knew the
 * path. This makes it discoverable, which is the half that was missing -- "I
 * deleted something yesterday" is not a question you can answer if you have to
 * remember the filename first.
 *
 * Restoring writes the file back at its ORIGINAL path, unlike the revision
 * browser's materialise-beside. The reason is simply that there is nothing to
 * overwrite: the path is empty, so the copy-beside dance that protects a live
 * note protects nothing here, and a `.local` copy the user then has to rename
 * would be ceremony for its own sake.
 */
import { App, Modal, Notice, TFile } from "obsidian";
import type { Client, DeletedPath } from "./client";

export class DeletedModal extends Modal {
	private listEl?: HTMLElement;
	private items: DeletedPath[] = [];

	constructor(
		app: App,
		private readonly client: () => Client | null,
	) {
		super(app);
	}

	async onOpen() {
		this.titleEl.setText("Restore a deleted note");
		this.contentEl.addClass("archivist-rev-modal");
		this.contentEl.createEl("p", {
			cls: "archivist-rev-hint",
			text:
				"Notes the server still has but this vault no longer shows. Restoring writes the " +
				"file back where it was, and it syncs to your other devices like any new note.",
		});
		this.listEl = this.contentEl.createDiv({ cls: "archivist-rev-list" });
		await this.load();
	}

	private async load() {
		const client = this.client();
		if (!client || !this.listEl) return;

		const box = this.listEl.createDiv({ cls: "archivist-rev-loading" });
		box.createDiv({ cls: "archivist-rev-spinner" });
		box.createSpan({ text: "Looking through history…" });

		try {
			this.items = await client.deleted();
		} catch (e) {
			this.listEl.empty();
			this.listEl.createEl("p", { text: `Could not read history (${String(e)})` });
			return;
		}
		this.render();
	}

	private render() {
		const list = this.listEl;
		if (!list) return;
		list.empty();

		if (this.items.length === 0) {
			list.createEl("p", { text: "Nothing deleted that the server still holds." });
			return;
		}

		for (const item of this.items) {
			const row = list.createDiv({ cls: "archivist-rev-row" });
			const main = row.createDiv({ cls: "archivist-rev-main" });
			main.createSpan({ cls: "archivist-rev-when", text: item.path });

			const meta = row.createDiv({ cls: "archivist-rev-metaline" });
			meta.createSpan({
				cls: "archivist-rev-meta",
				text: new Date(item.when).toLocaleString(undefined, {
					dateStyle: "medium",
					timeStyle: "short",
				}),
			});
			if (item.device) {
				meta.createSpan({ cls: "archivist-rev-device", text: `deleted by ${item.device}` });
			}

			const restore = main.createEl("button", {
				text: "Restore",
				cls: "archivist-rev-restore",
			});
			restore.onclick = () => void this.restore(item, row);
		}
	}

	private async restore(item: DeletedPath, row: HTMLElement) {
		const client = this.client();
		if (!client) return;
		try {
			// The PARENT of the deletion holds the content; the deleting commit
			// is exactly where the path stopped existing.
			const buf = await client.readAt(item.revision, item.path);

			// Never clobber: if something now lives at that path, the file was
			// re-created since and restoring over it would destroy the newer
			// note to recover the older one.
			if (this.app.vault.getAbstractFileByPath(item.path)) {
				new Notice(`archivist: ${item.path} exists again — not overwriting it`);
				return;
			}

			const slash = item.path.lastIndexOf("/");
			if (slash > 0) {
				const dir = item.path.slice(0, slash);
				if (!this.app.vault.getAbstractFileByPath(dir)) {
					await this.app.vault.createFolder(dir).catch(() => undefined);
				}
			}

			const file = await this.app.vault.createBinary(item.path, buf);
			row.remove();
			this.items = this.items.filter((i) => i.path !== item.path);
			new Notice(`archivist: restored ${item.path}`);
			if (file instanceof TFile) {
				await this.app.workspace.getLeaf(true).openFile(file);
			}
		} catch (e) {
			new Notice(`archivist: could not restore (${String(e)})`);
		}
	}
}
