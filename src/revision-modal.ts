/**
 * The revision browser: one note's history as sessions, with its pins.
 *
 * Nothing in here can destroy anything. Opening a revision writes a NEW file
 * beside the original -- there is no overwrite path in this file, deliberately,
 * because "restore an old version" is the operation people fear and the one
 * that would otherwise need a confirmation dance. Restoring is renaming the
 * materialised file, which is an ordinary Obsidian gesture the user already
 * knows.
 */
import { App, Modal, Notice, TFile, setIcon } from "obsidian";
import { Code, ServerError } from "./client";
import type { Client, Pin, Revision } from "./client";
import { clusterRevisions, materialisedName, uniqueName, type Session } from "./revisions";

const PAGE = 100;

export class RevisionModal extends Modal {
	/**
	 * Every revision fetched so far, newest first.
	 *
	 * Kept raw and RE-CLUSTERED after each page rather than clustering each
	 * page and concatenating: a session that straddles a page boundary would
	 * otherwise be split in two, so one long editing burst of 101 revisions
	 * would show as two sessions purely because of the page size.
	 */
	private revisions: Revision[] = [];
	private sessions: Session[] = [];
	private pins: Pin[] = [];
	private next?: string;
	private hasMore = false;
	private loading = false;

	constructor(
		app: App,
		private readonly path: string,
		private readonly client: () => Client | null,
		private readonly head: () => string,
		private readonly flush: () => Promise<void>,
	) {
		super(app);
	}

	async onOpen() {
		this.titleEl.setText(this.path.split("/").pop() ?? this.path);
		this.contentEl.createEl("p", {
			cls: "archivist-rev-hint",
			text: "Opening a version writes a copy beside the note. Copies stay on this device until you rename them.",
		});
		// "Pin this version" names the CURRENT state, not a historical one:
		// pins mark now. Preserving an old version is materialise-then-pin,
		// which keeps every pin's snapshot a tree that actually exists.
		const pin = this.contentEl.createEl("button", {
			text: "Pin this version…",
			cls: "archivist-rev-pinbtn",
		});
		pin.onclick = () => {
			this.close();
			new PinModal(this.app, this.path, this.client, this.head, this.flush).open();
		};
		await this.load();
	}

	private async load() {
		const client = this.client();
		if (!client) {
			this.contentEl.createEl("p", { text: "Not configured." });
			return;
		}
		this.loading = true;
		try {
			const [page, pins] = await Promise.all([
				client.history(this.path, PAGE, this.next),
				// Pins are a separate call by necessity: a pin's commit does
				// not touch the note, so it can never appear in the note's own
				// history. Fetched once, then merged by time.
				this.next ? Promise.resolve(this.pins) : client.pins(this.path),
			]);
			this.pins = pins;
			this.hasMore = page.hasMore;
			this.next = page.next;
			// Deduplicate by commit: a cursor page can repeat its boundary if
			// the history moved between requests.
			const known = new Set(this.revisions.map((r) => r.commit));
			for (const rev of page.revisions) {
				if (!known.has(rev.commit)) this.revisions.push(rev);
			}
			this.sessions = clusterRevisions(this.revisions);
		} catch (e) {
			new Notice(`Archivist: could not load history (${String(e)})`);
		} finally {
			this.loading = false;
		}
		this.render();
	}

	private render() {
		const body = this.contentEl.querySelector(".archivist-rev-list");
		if (body) body.remove();
		const list = this.contentEl.createDiv({ cls: "archivist-rev-list" });

		if (this.sessions.length === 0) {
			list.createEl("p", { text: "No history for this note yet." });
			return;
		}

		// Pins and sessions interleave by time, newest first: both are things
		// that happened to this note, and splitting them into two lists would
		// make the user reconcile two chronologies by hand.
		type Row = { at: number; draw: (el: HTMLElement) => void };
		const rows: Row[] = [];

		for (const s of this.sessions) {
			rows.push({ at: s.endedAt.getTime(), draw: (el) => this.drawSession(el, s) });
		}
		for (const p of this.pins) {
			rows.push({ at: new Date(p.created).getTime(), draw: (el) => this.drawPin(el, p) });
		}
		rows.sort((a, b) => b.at - a.at);
		for (const r of rows) r.draw(list);

		if (this.hasMore) {
			const more = list.createEl("button", { text: "Load older", cls: "archivist-rev-more" });
			more.onclick = () => {
				if (!this.loading) void this.load();
			};
		}
	}

	private drawSession(list: HTMLElement, s: Session) {
		const row = list.createDiv({ cls: "archivist-rev-row" });
		const when = s.endedAt;
		const count = s.revisions.length;
		row.createSpan({
			cls: "archivist-rev-when",
			text: when.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" }),
		});
		row.createSpan({
			cls: "archivist-rev-meta",
			text: count === 1 ? "1 change" : `${count} changes`,
		});

		if (!s.representative) {
			// A session whose only entry is a deletion: /v1/at cannot serve a
			// path absent from the tree, so this is an event, not something to
			// open.
			row.addClass("archivist-rev-unavailable");
			row.createSpan({ cls: "archivist-rev-meta", text: "deleted" });
			return;
		}
		row.addClass("mod-clickable");
		row.onclick = () => void this.materialise(s.representative as Revision);
	}

	private drawPin(list: HTMLElement, p: Pin) {
		const row = list.createDiv({ cls: "archivist-rev-row archivist-rev-pin" });
		const icon = row.createSpan({ cls: "archivist-rev-pinicon" });
		setIcon(icon, "pin");
		row.createSpan({ cls: "archivist-rev-when", text: p.name });
		row.createSpan({
			cls: "archivist-rev-meta",
			text: new Date(p.created).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" }),
		});
		if (!p.available) {
			// The entry outlived its content: a prune can drop the blobs of a
			// deleted path. Saying so beats a 404 when they click.
			row.addClass("archivist-rev-unavailable");
			row.createSpan({ cls: "archivist-rev-meta", text: "content no longer stored" });
			return;
		}
		row.addClass("mod-clickable");
		row.onclick = () =>
			void this.materialiseAt(p.commit, `pin "${p.name}"`);
	}

	private materialise(rev: Revision) {
		return this.materialiseAt(rev.commit, "revision");
	}

	private async materialiseAt(commit: string, what: string) {
		const client = this.client();
		if (!client) return;
		try {
			const buf = await client.readAt(commit, this.path);
			const target = uniqueName(materialisedName(this.path, commit), (p) =>
				this.app.vault.getAbstractFileByPath(p) !== null,
			);
			const file = await this.app.vault.createBinary(target, buf);
			new Notice(`Archivist: opened ${what} as ${target.split("/").pop()}`);
			this.close();
			if (file instanceof TFile) {
				await this.app.workspace.getLeaf(true).openFile(file);
			}
		} catch (e) {
			new Notice(`Archivist: could not open that version (${String(e)})`);
		}
	}
}

/** Prompt for a pin name, then create it against the current head. */
export class PinModal extends Modal {
	private value = "";

	constructor(
		app: App,
		private readonly path: string | undefined,
		private readonly client: () => Client | null,
		private readonly head: () => string,
		private readonly flush: () => Promise<void>,
	) {
		super(app);
	}

	onOpen() {
		this.titleEl.setText(this.path ? "Pin this version" : "Pin the whole vault");
		const input = this.contentEl.createEl("input", {
			type: "text",
			cls: "archivist-pin-name",
			attr: { placeholder: "draft sent to Anna" },
		});
		input.oninput = () => (this.value = input.value);
		input.onkeydown = (e) => {
			if (e.key === "Enter") void this.save();
		};
		const save = this.contentEl.createEl("button", { text: "Pin", cls: "mod-cta" });
		save.onclick = () => void this.save();
		window.setTimeout(() => input.focus(), 0);
	}

	private async save() {
		const client = this.client();
		if (!client || !this.value.trim()) return;
		try {
			// Flush first: with the sync debounce, the server's head can be
			// older than the note on screen, and pinning it would name a tree
			// the user never saw.
			await this.flush();
			try {
				await client.pin(this.value.trim(), this.head(), this.path);
			} catch (e) {
				// One retry on a stale head -- the flush may have landed a
				// commit of its own, which is the common case rather than an
				// error.
				if (!(e instanceof ServerError) || e.code !== Code.StaleHead) throw e;
				await this.flush();
				await client.pin(this.value.trim(), this.head(), this.path);
			}
			new Notice(`Archivist: pinned "${this.value.trim()}"`);
			this.close();
		} catch (e) {
			new Notice(`Archivist: could not pin (${String(e)})`);
		}
	}
}
