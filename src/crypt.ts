import { aessiv } from "@noble/ciphers/aes.js";

/**
 * Encryption for an encrypted vault.
 *
 * Everything here runs on the device. The server holds no key: it stores what
 * this module produces and checks only that content starts with ENVELOPE and
 * that names are lowercase base32. See
 * docs/2026-11-19-encrypted-vaults-design-note.md.
 *
 * Contents use WebCrypto AES-256-GCM with a random nonce. Names use AES-SIV,
 * which is deterministic: one name always encrypts to one ciphertext, so the
 * server can still move, delete and list history for a path it cannot read.
 * WebCrypto has no SIV mode, which is the one reason for the dependency.
 */

/** The literal prefix of every object in an encrypted vault. Mirrors
 *  protocol.Envelope on the server, which refuses content without it. */
export const ENVELOPE = "archivist-vault/v1\n";

const ENVELOPE_BYTES = new TextEncoder().encode(ENVELOPE);
const NONCE_BYTES = 12;
const KEY_BYTES = 32;
/** Filesystems cap one name at 255 bytes, and that applies to the ENCRYPTED
 *  name, which is what the server writes to disk. */
const MAX_SEGMENT = 255;

/**
 * Marks a file or folder of an ORDINARY vault as encrypted: the server stores
 * it and cannot read it. Mirrors protocol.EncryptedMarker on the server.
 *
 * Fixed here, deliberately not a setting. A rule kept in the vault could be
 * edited by anything that can write a note.
 */
export const ENCRYPTED_MARKER = ".encrypted";

/**
 * Which paths a key is for. "vault" is an encrypted vault: everything.
 * "files" is the marked paths of an ordinary vault.
 *
 * Stored on the device with the key and never taken from the server on a
 * later day. A server that could flip it could talk a device into uploading
 * a whole encrypted vault as plaintext.
 */
export type KeyScope = "vault" | "files";

/**
 * Case is ignored, and that is a safety property rather than a convenience. A
 * marker that fails to match does not stop a file syncing, it sends the file
 * readable: "Clients.Encrypted", from a phone keyboard that capitalised it,
 * would otherwise be published in plaintext.
 */
const endsMarked = (s: string) =>
	s.length > ENCRYPTED_MARKER.length && s.slice(-ENCRYPTED_MARKER.length).toLowerCase() === ENCRYPTED_MARKER;

/**
 * Offset just past the first marked segment, or -1.
 *
 * The grammar is localOnly's, for localOnly's reason. Any segment ending in
 * the marker takes its subtree with it; the last segment is also marked when
 * the marker sits before its extension. A folder has to END in the marker,
 * because "project.encrypted.assets" would match the file spelling by
 * coincidence.
 */
function markedAt(path: string): number {
	let start = 0;
	for (;;) {
		const slash = path.indexOf("/", start);
		const end = slash < 0 ? path.length : slash;
		const seg = path.slice(start, end);
		if (endsMarked(seg)) return end;
		if (slash < 0) {
			const dot = seg.lastIndexOf(".");
			return dot > 0 && dot < seg.length - 1 && endsMarked(seg.slice(0, dot)) ? end : -1;
		}
		start = slash + 1;
	}
}

/**
 * Whether a path of an ordinary vault is marked encrypted. Must stay identical
 * to protocol.EncryptedPath in Go; protocol/testdata/marked-paths.json is the
 * table both are tested against.
 *
 * It gives the same answer for the path on this device and the path on the
 * server: the two are identical up to and including the first marked segment,
 * and that segment decides.
 */
export function encryptedPath(path: string): boolean {
	return markedAt(path) >= 0;
}

/** encryptedPath for a DIRECTORY, without the file grammar: a folder that
 *  happens to be called "Old.encrypted.notes" is an ordinary folder. */
export function encryptedDir(path: string): boolean {
	return path.split("/").some(endsMarked);
}

/** A path segment too long to store once encrypted. The file is not synced. */
export class NameTooLongError extends Error {
	constructor(readonly path: string) {
		super(`"${path}" has a name too long to encrypt; shorten it to sync it`);
	}
}

/** The key does not open this vault: wrong key, or a typo in the recovery key. */
export class WrongKeyError extends Error {}

export class VaultCrypt {
	private encCache = new Map<string, string>();
	private decCache = new Map<string, string>();

	private constructor(
		private content: CryptoKey,
		private names: Uint8Array,
		/** What the server stores so a second device can verify its key. An
		 *  HMAC under its own subkey: it reveals nothing about the others. */
		readonly keyCheck: string,
		readonly scope: KeyScope,
	) {}

	/** Whether this key encrypts the file at `path`, given as this device
	 *  names it. Everything in an encrypted vault; the marked paths of an
	 *  ordinary one. */
	covers(path: string): boolean {
		return this.scope === "vault" || encryptedPath(path);
	}

	static async fromKey(key: Uint8Array, scope: KeyScope = "vault"): Promise<VaultCrypt> {
		if (key.byteLength !== KEY_BYTES) throw new Error("a vault key is 32 bytes");
		const base = await crypto.subtle.importKey("raw", new Uint8Array(key), "HKDF", false, ["deriveBits"]);
		const derive = async (info: string, bytes: number) =>
			new Uint8Array(
				await crypto.subtle.deriveBits(
					{
						name: "HKDF",
						hash: "SHA-256",
						salt: new Uint8Array(32),
						info: new TextEncoder().encode(info),
					},
					base,
					bytes * 8,
				),
			);

		const content = await crypto.subtle.importKey(
			"raw",
			await derive("archivist/v1/content", 32),
			"AES-GCM",
			false,
			["encrypt", "decrypt"],
		);
		// AES-256-SIV takes a double-length key: one half for the synthetic IV,
		// one for the cipher.
		const names = await derive("archivist/v1/names", 64);
		const check = await crypto.subtle.importKey(
			"raw",
			await derive("archivist/v1/keycheck", 32),
			{ name: "HMAC", hash: "SHA-256" },
			false,
			["sign"],
		);
		const mac = await crypto.subtle.sign("HMAC", check, new TextEncoder().encode("archivist-keycheck-v1"));
		return new VaultCrypt(content, names, hex(new Uint8Array(mac)), scope);
	}

	/** Envelope, then nonce, then ciphertext and tag. */
	async seal(plain: ArrayBuffer): Promise<ArrayBuffer> {
		const nonce = crypto.getRandomValues(new Uint8Array(NONCE_BYTES));
		const sealed = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce }, this.content, plain));
		const out = new Uint8Array(ENVELOPE_BYTES.byteLength + NONCE_BYTES + sealed.byteLength);
		out.set(ENVELOPE_BYTES, 0);
		out.set(nonce, ENVELOPE_BYTES.byteLength);
		out.set(sealed, ENVELOPE_BYTES.byteLength + NONCE_BYTES);
		return out.buffer;
	}

	async open(sealed: ArrayBuffer): Promise<ArrayBuffer> {
		const bytes = new Uint8Array(sealed);
		const head = ENVELOPE_BYTES.byteLength;
		if (bytes.byteLength < head + NONCE_BYTES || !ENVELOPE_BYTES.every((b, i) => bytes[i] === b)) {
			throw new Error("content from an encrypted vault has no envelope");
		}
		try {
			return await crypto.subtle.decrypt(
				{ name: "AES-GCM", iv: bytes.subarray(head, head + NONCE_BYTES) },
				this.content,
				bytes.subarray(head + NONCE_BYTES),
			);
		} catch {
			throw new WrongKeyError("content could not be decrypted with this vault's key");
		}
	}

	/**
	 * Segment by segment, so the folder tree survives and the server can still
	 * operate on a path. Normalised to NFC first: the two Unicode spellings of
	 * one name must not become two unrelated ciphertexts.
	 *
	 * In "files" scope a path that is not marked comes back unchanged, and a
	 * marked one keeps everything through its first marked segment: the server
	 * has to be able to read the marker to enforce it.
	 */
	encryptPath(path: string): string {
		const hit = this.encCache.get(path);
		if (hit !== undefined) return hit;
		const keep = this.readablePrefix(path);
		if (keep >= path.length) return path;
		const out = path.slice(0, keep) + path
			.slice(keep)
			.split("/")
			.map((seg) => {
				const sealed = base32(aessiv(this.names).encrypt(new TextEncoder().encode(seg.normalize("NFC"))));
				if (sealed.length > MAX_SEGMENT) throw new NameTooLongError(path);
				return sealed;
			})
			.join("/");
		this.encCache.set(path, out);
		this.decCache.set(out, path.slice(0, keep) + path.slice(keep).normalize("NFC"));
		return out;
	}

	/**
	 * How much of a path is left as it is: nothing in "vault" scope, all of an
	 * unmarked path, and a marked one through the slash after its first marked
	 * segment. Works on a server path as well as a local one.
	 */
	private readablePrefix(path: string): number {
		if (this.scope === "vault") return 0;
		const end = markedAt(path);
		return end < 0 ? path.length : Math.min(end + 1, path.length);
	}

	/** Throws when a segment was not produced under this key. SIV is
	 *  authenticated, so a foreign or tampered name fails rather than decoding
	 *  to garbage. */
	decryptPath(path: string): string {
		const hit = this.decCache.get(path);
		if (hit !== undefined) return hit;
		const keep = this.readablePrefix(path);
		if (keep >= path.length) return path;
		const out = path.slice(0, keep) + path
			.slice(keep)
			.split("/")
			.map((seg) => new TextDecoder("utf-8", { fatal: true }).decode(aessiv(this.names).decrypt(unbase32(seg))))
			.join("/");
		this.decCache.set(path, out);
		return out;
	}
}

export function generateKey(): Uint8Array {
	return crypto.getRandomValues(new Uint8Array(KEY_BYTES));
}

/**
 * The key as something a person can copy into a password manager and type on
 * a second device: base32 in groups of five, with a two-byte checksum so a
 * typo is caught here rather than surfacing as a vault that will not decrypt.
 */
export async function encodeRecoveryKey(key: Uint8Array): Promise<string> {
	const framed = new Uint8Array(KEY_BYTES + 2);
	framed.set(key, 0);
	framed.set(await checksum(key), KEY_BYTES);
	return (base32(framed).toUpperCase().match(/.{1,5}/g) ?? []).join("-");
}

export async function decodeRecoveryKey(text: string): Promise<Uint8Array> {
	let framed: Uint8Array;
	try {
		framed = unbase32(text.replace(/[^A-Za-z2-7]/g, "").toLowerCase());
	} catch {
		throw new WrongKeyError("that is not a recovery key");
	}
	if (framed.byteLength !== KEY_BYTES + 2) throw new WrongKeyError("that recovery key is the wrong length");
	const key = framed.slice(0, KEY_BYTES);
	const sum = await checksum(key);
	if (sum[0] !== framed[KEY_BYTES] || sum[1] !== framed[KEY_BYTES + 1]) {
		throw new WrongKeyError("that recovery key has a typo in it");
	}
	return key;
}

async function checksum(key: Uint8Array): Promise<Uint8Array> {
	return new Uint8Array(await crypto.subtle.digest("SHA-256", new Uint8Array(key))).subarray(0, 2);
}

const ALPHABET = "abcdefghijklmnopqrstuvwxyz234567";

/** RFC 4648 base32, lowercase and unpadded: safe on a case-insensitive
 *  filesystem, never starts with a dot, and is what the server's path
 *  allowlist accepts. */
export function base32(bytes: Uint8Array): string {
	let out = "";
	let acc = 0;
	let bits = 0;
	for (const b of bytes) {
		acc = (acc << 8) | b;
		bits += 8;
		while (bits >= 5) {
			out += ALPHABET[(acc >>> (bits - 5)) & 31];
			bits -= 5;
		}
		acc &= (1 << bits) - 1;
	}
	if (bits > 0) out += ALPHABET[(acc << (5 - bits)) & 31];
	return out;
}

export function unbase32(text: string): Uint8Array {
	const out = new Uint8Array(Math.floor((text.length * 5) / 8));
	let acc = 0;
	let bits = 0;
	let n = 0;
	for (const ch of text) {
		const v = ALPHABET.indexOf(ch);
		if (v < 0) throw new Error("not base32");
		acc = ((acc << 5) | v) & 0xfff;
		bits += 5;
		if (bits >= 8) {
			out[n++] = (acc >>> (bits - 8)) & 0xff;
			bits -= 8;
		}
	}
	return out;
}

export function hex(bytes: Uint8Array): string {
	let out = "";
	for (const b of bytes) out += b.toString(16).padStart(2, "0");
	return out;
}

export function unhex(text: string): Uint8Array {
	if (!/^([0-9a-f]{2})*$/.test(text)) throw new Error("not hex");
	const out = new Uint8Array(text.length / 2);
	for (let i = 0; i < out.length; i++) out[i] = parseInt(text.slice(i * 2, i * 2 + 2), 16);
	return out;
}
