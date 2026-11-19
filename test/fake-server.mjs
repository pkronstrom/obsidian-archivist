// A small stand-in for archivist-server, for driving the sync engine in unit
// tests without a real server. See the class comment for which rules it keeps.

import { createHash } from "node:crypto";
import { ServerError } from "../dist-test/entry.mjs";

function gitHash(buf) {
	const b = Buffer.from(buf);
	return createHash("sha1").update(`blob ${b.length}\0`).update(b).digest("hex");
}

export function bytes(text) {
	const b = Buffer.from(text);
	return b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength);
}

// A small stand-in for archivist-server that keeps the reconciler's rules for
// del and move: a path changed since the client's base is refused, a move needs
// the source to hold the hash it names and the target to be free. Puts always
// apply, because nothing here needs a merge.
export class FakeServer {
	/** nfc: compose incoming paths, as ARCHIVIST_NORMALIZE_NFC does. */
	constructor({ nfc = false } = {}) {
		this.nfc = nfc;
		this.commits = [{ id: "", files: {} }];
		this.blobs = new Map();
	}
	get head() {
		return this.commits[this.commits.length - 1].id;
	}
	files(rev = this.head) {
		return structuredClone(this.commits.find((c) => c.id === rev)?.files ?? {});
	}
	commit(files) {
		const id = "c" + this.commits.length;
		this.commits.push({ id, files });
		return id;
	}
	changedSince(base) {
		const b = this.files(base);
		const h = this.files();
		const out = [];
		for (const p of new Set([...Object.keys(b), ...Object.keys(h)])) {
			if (!h[p]) out.push({ path: p, op: "del" });
			else if (!b[p] || b[p].hash !== h[p].hash) out.push({ path: p, op: "put", hash: h[p].hash, size: h[p].size });
		}
		return out;
	}
	/** Another device edits a note. */
	edit(p, text) {
		const content = bytes(text);
		const hash = gitHash(content);
		this.blobs.set(hash, content);
		const files = this.files();
		files[p] = { hash, size: content.byteLength };
		this.commit(files);
	}
	text(p) {
		const f = this.files()[p];
		return f ? Buffer.from(this.blobs.get(f.hash)).toString() : undefined;
	}
	client() {
		const srv = this;
		return {
			async index() {
				return { service: "archivist", version: "test", protocol: 2, vault: "personal", encrypted: false, keyCheck: "" };
			},
			async head() {
				return srv.head;
			},
			async changes(since) {
				return { head: srv.head, entries: since === srv.head ? [] : srv.changedSince(since) };
			},
			async snapshot() {
				return { head: srv.head, files: srv.files() };
			},
			async missing(hashes) {
				return hashes.filter((h) => !srv.blobs.has(h));
			},
			async putContent(hash, content) {
				srv.blobs.set(hash, content);
			},
			async getContent(hash) {
				return srv.blobs.get(hash);
			},
			async readAt(rev, p) {
				const f = srv.files(rev)[p];
				if (!f) throw new ServerError("not_found", "no such path at that revision", 404);
				return srv.blobs.get(f.hash);
			},
			async push(base, _device, changes) {
				if (srv.nfc) {
					changes = changes.map((c) => ({ ...c, path: c.path.normalize("NFC"), from: c.from?.normalize("NFC") }));
				}
				const moved = new Set(srv.changedSince(base).map((c) => c.path));
				const files = srv.files();
				const results = [];
				const refuse = (p, reason) => results.push({ path: p, status: "refused", reason });
				for (const c of changes) {
					if (c.op === "put") {
						files[c.path] = { hash: c.hash, size: c.size };
						results.push({ path: c.path, status: "applied", hash: c.hash, size: c.size });
					} else if (c.op === "del") {
						if (moved.has(c.path)) refuse(c.path, "changed on the server since base");
						else {
							delete files[c.path];
							results.push({ path: c.path, status: "applied" });
						}
					} else if (moved.has(c.from) || moved.has(c.path)) refuse(c.path, "changed on the server since base");
					else if (!files[c.from]) refuse(c.path, "no such path: " + c.from);
					else if (c.hash && files[c.from].hash !== c.hash) refuse(c.path, "the source changed since it was read");
					else if (files[c.path]) refuse(c.path, "target already exists: " + c.path);
					else {
						files[c.path] = files[c.from];
						delete files[c.from];
						results.push({ path: c.path, status: "applied", hash: files[c.path].hash, size: files[c.path].size });
					}
				}
				return { head: srv.commit(files), results };
			},
		};
	}
}
