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
import { App, Modal, Notice, Platform, TFile, setIcon } from "obsidian";
import type { Client, DeletedPath } from "./client";

export class DeletedModal extends Modal {
	private listEl?: HTMLElement;
	private items: DeletedPath[] = [];
	/** Look without committing: restore as a .local copy, which never syncs.
	 *  Deleting is usually deliberate, so "let me see what this was" should not
	 *  push a note back to every device before the user has decided. */
	private localOnly = false;
	private open2 = new Set<string>();

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
		const opts = this.contentEl.createDiv({ cls: "archivist-del-options" });
		const label = opts.createEl("label", { cls: "archivist-del-check" });
		const box = label.createEl("input", { type: "checkbox" });
		box.checked = this.localOnly;
		label.createSpan({ text: "Open a local-only copy instead of restoring" });
		box.onchange = () => {
			this.localOnly = box.checked;
			this.render();
		};

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

		// Grouped by the folder each note was deleted FROM. A flat list is
		// fine at thirty entries and unreadable at three hundred, and the
		// folder is usually how a person remembers what they lost -- "the
		// thing in Inbox" rather than its filename.
		const byFolder = new Map<string, DeletedPath[]>();
		for (const item of this.items) {
			const slash = item.path.lastIndexOf("/");
			const folder = slash > 0 ? item.path.slice(0, slash) : "/";
			const group = byFolder.get(folder);
			if (group) group.push(item);
			else byFolder.set(folder, [item]);
		}

		for (const [folder, items] of [...byFolder.entries()].sort()) {
			const section = list.createDiv({ cls: "archivist-del-folder" });
			const header = section.createDiv({ cls: "archivist-del-folderhead mod-clickable" });
			const chevron = header.createSpan({ cls: "archivist-del-chevron" });
			const expanded = this.open2.has(folder);
			setIcon(chevron, expanded ? "chevron-down" : "chevron-right");
			header.createSpan({
				cls: "archivist-rev-when",
				text: folder === "/" ? "(vault root)" : folder,
			});
			header.createSpan({ cls: "archivist-rev-meta", text: `${items.length}` });

			const body = section.createDiv({ cls: "archivist-del-items" });
			if (!expanded) body.hide();
			header.onclick = () => {
				if (this.open2.has(folder)) this.open2.delete(folder);
				else this.open2.add(folder);
				this.render();
			};

			for (const item of items) {
				this.drawItem(body, item);
			}
		}
	}

	private drawItem(body: HTMLElement, item: DeletedPath) {
		const row = body.createDiv({ cls: "archivist-rev-row" });
		const main = row.createDiv({ cls: "archivist-rev-main" });
		main.createSpan({
			cls: "archivist-rev-when",
			text: item.path.slice(item.path.lastIndexOf("/") + 1),
		});

		const meta = row.createDiv({ cls: "archivist-rev-metaline" });
		// "21 Aug 2026 at 15:09" is most of a phone's width on its own, and the
		// date is context here rather than the thing being read -- the filename
		// is. Short form on mobile, and the device loses its "deleted by"
		// preamble, which the modal's own title already implies.
		meta.createSpan({
			cls: "archivist-rev-meta",
			text: new Date(item.when).toLocaleString(
				undefined,
				Platform.isMobile
					? { dateStyle: "short", timeStyle: "short" }
					: { dateStyle: "medium", timeStyle: "short" },
			),
		});
		if (item.device) {
			meta.createSpan({
				cls: "archivist-rev-device",
				text: Platform.isMobile ? item.device : `deleted by ${item.device}`,
			});
		}

		// A child of the ROW, not of the text block: the grid places it in the
		// second column, which is what pulls it to the right edge. Created
		// inside main it was just another flex item hugging the filename.
		const action = row.createEl("button", {
			cls: "archivist-rev-restore",
		});
		// An icon on mobile: the word costs a third of the row, and the tap
		// target stays the same size either way.
		if (Platform.isMobile) {
			// rotate-ccw is the refresh glyph and reads as "sync this again".
			// archive-restore is the actual metaphor -- something coming back
			// out of storage. Verified rather than assumed: an icon name the
			// running Obsidian does not ship renders NOTHING, which would
			// leave an invisible button as the modal's only action.
			const label = this.localOnly ? "Open copy" : "Restore";
			for (const name of this.localOnly ? ["eye"] : ["archive-restore", "undo-2", "undo"]) {
				setIcon(action, name);
				if (action.querySelector("svg")) break;
				action.empty();
			}
			if (!action.querySelector("svg")) action.setText(label);
			action.setAttr("aria-label", label);
		} else {
			action.setText(this.localOnly ? "Open copy" : "Restore");
		}
		action.onclick = () => void this.restore(item, row);
	}

	private async restore(item: DeletedPath, row: HTMLElement) {
		const client = this.client();
		if (!client) return;
		try {
			// The PARENT of the deletion holds the content; the deleting commit
			// is exactly where the path stopped existing.
			const buf = await client.readAt(item.revision, item.path);
			const target = this.localOnly ? this.localCopyPath(item) : item.path;

			// Never clobber. For a real restore this means the note was
			// re-created since; for a local copy it means a previous look is
			// still open. Either way the existing file wins.
			if (this.app.vault.getAbstractFileByPath(target)) {
				new Notice(`archivist: ${target} already exists — not overwriting it`);
				return;
			}

			if (!this.localOnly) {
				// A real restore recreates the folders it needs, because the
				// path is where the note belongs.
				const slash = target.lastIndexOf("/");
				if (slash > 0) {
					const dir = target.slice(0, slash);
					if (!this.app.vault.getAbstractFileByPath(dir)) {
						await this.app.vault.createFolder(dir).catch(() => undefined);
					}
				}
			}

			const file = await this.app.vault.createBinary(target, buf);
			if (!this.localOnly) {
				row.remove();
				this.items = this.items.filter((i) => i.path !== item.path);
			}
			new Notice(
				this.localOnly
					? `archivist: opened a local-only copy as ${target}`
					: `archivist: restored ${target}`,
			);
			if (file instanceof TFile) {
				await this.app.workspace.getLeaf(true).openFile(file);
			}
		} catch (e) {
			new Notice(`archivist: could not open that note (${String(e)})`);
		}
	}

	/**
	 * Where a look-only copy goes.
	 *
	 * At the VAULT ROOT, not the original folder: the folder is usually gone
	 * too -- a deleted note is often a deleted directory -- and recreating a
	 * tree just to hold a copy the user may discard would rebuild the very
	 * structure they removed. The original path stays in the filename so it is
	 * still obvious where it came from.
	 */
	private localCopyPath(item: DeletedPath): string {
		const name = item.path.slice(item.path.lastIndexOf("/") + 1);
		const dot = name.lastIndexOf(".");
		const stem = dot > 0 ? name.slice(0, dot) : name;
		const ext = dot > 0 ? name.slice(dot) : ".md";
		return `${stem}.${item.short.slice(0, 7)}.local${ext}`;
	}
}
