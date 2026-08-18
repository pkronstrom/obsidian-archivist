// A minimal stand-in for the Obsidian runtime, so the sync engine can be
// driven against a REAL archivist-server server from Node.
//
// This is not a mock of the logic under test -- client.ts, sync.ts and hash.ts
// run unmodified. Only the two things Obsidian provides are substituted:
// requestUrl (an HTTP call) and DataAdapter (the filesystem). Everything the
// integration test exercises is production code.

import fs from "node:fs/promises";
import path from "node:path";

/** requestUrl: Obsidian's CORS-free HTTP. fetch is a faithful stand-in here. */
export async function requestUrl(opts) {
	const res = await fetch(opts.url, {
		method: opts.method ?? "GET",
		headers: opts.headers ?? {},
		body: opts.body,
	});
	const buf = await res.arrayBuffer();
	const text = new TextDecoder().decode(buf);
	let json;
	try {
		json = JSON.parse(text);
	} catch {
		json = undefined;
	}
	if (opts.throw !== false && res.status >= 400) {
		throw new Error(`${res.status}: ${text.slice(0, 200)}`);
	}
	return { status: res.status, headers: {}, arrayBuffer: buf, text, json };
}

export function debounce(fn) {
	return fn;
}

export class Plugin {}
export class PluginSettingTab {}
export class Setting {}
export class Notice {}
export class TAbstractFile {}

/** A DataAdapter backed by a real directory. */
export class FsAdapter {
	constructor(root) {
		this.root = root;
	}
	abs(p) {
		return path.join(this.root, p);
	}
	async exists(p) {
		try {
			await fs.stat(this.abs(p));
			return true;
		} catch {
			return false;
		}
	}
	async stat(p) {
		try {
			const st = await fs.stat(this.abs(p));
			return {
				type: st.isDirectory() ? "folder" : "file",
				ctime: Math.floor(st.ctimeMs),
				mtime: Math.floor(st.mtimeMs),
				size: st.size,
			};
		} catch {
			return null;
		}
	}
	async list(dir) {
		const abs = dir === "" ? this.root : this.abs(dir);
		const out = { files: [], folders: [] };
		let entries;
		try {
			entries = await fs.readdir(abs, { withFileTypes: true });
		} catch {
			return out;
		}
		for (const e of entries) {
			const rel = dir === "" ? e.name : `${dir}/${e.name}`;
			if (e.isDirectory()) out.folders.push(rel);
			else out.files.push(rel);
		}
		return out;
	}
	async read(p) {
		return fs.readFile(this.abs(p), "utf8");
	}
	async readBinary(p) {
		const b = await fs.readFile(this.abs(p));
		return b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength);
	}
	async write(p, data) {
		await fs.mkdir(path.dirname(this.abs(p)), { recursive: true });
		await fs.writeFile(this.abs(p), data);
	}
	async writeBinary(p, data) {
		await fs.mkdir(path.dirname(this.abs(p)), { recursive: true });
		await fs.writeFile(this.abs(p), Buffer.from(data));
	}
	async mkdir(p) {
		await fs.mkdir(this.abs(p), { recursive: true });
	}
	async remove(p) {
		await fs.rm(this.abs(p), { force: true });
	}
	async rmdir(p, recursive) {
		// fs.rm with recursive:false throws EISDIR on a directory, so the
		// non-recursive case has to be fs.rmdir -- which is also what Obsidian's
		// own rmdir does: remove this directory, and only if it is empty.
		if (recursive) await fs.rm(this.abs(p), { recursive: true, force: true });
		else await fs.rmdir(this.abs(p));
	}
	async rename(from, to) {
		await fs.mkdir(path.dirname(this.abs(to)), { recursive: true });
		await fs.rename(this.abs(from), this.abs(to));
	}
}

/** An App with vault-scoped local storage held in memory, matching
 *  loadLocalStorage/saveLocalStorage semantics. */
export class FakeApp {
	constructor(root) {
		this.vault = { adapter: new FsAdapter(root) };
		this.store = new Map();
	}
	loadLocalStorage(key) {
		return this.store.get(key) ?? null;
	}
	saveLocalStorage(key, data) {
		if (data === null) this.store.delete(key);
		else this.store.set(key, JSON.parse(JSON.stringify(data)));
	}
}
