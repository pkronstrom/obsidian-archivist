import assert from "node:assert/strict";
import { test } from "node:test";
import {
	ENVELOPE,
	NameTooLongError,
	VaultCrypt,
	WrongKeyError,
	base32,
	decodeRecoveryKey,
	encodeRecoveryKey,
	generateKey,
	unbase32,
} from "../../dist-test/entry.mjs";

const text = (buf) => new TextDecoder().decode(buf);
const bytes = (s) => new TextEncoder().encode(s).buffer;

test("base32 round-trips every length and stays in the server's alphabet", () => {
	for (let n = 0; n < 70; n++) {
		const input = Uint8Array.from({ length: n }, (_, i) => (i * 37 + n) & 0xff);
		const encoded = base32(input);
		assert.match(encoded, /^[a-z2-7]*$/);
		assert.deepEqual(unbase32(encoded), input);
	}
	// RFC 4648 test vector, lowercased and unpadded.
	assert.equal(base32(new TextEncoder().encode("foobar")), "mzxw6ytboi");
});

test("content round-trips and carries the envelope the server checks for", async () => {
	const crypt = await VaultCrypt.fromKey(generateKey());
	const sealed = await crypt.seal(bytes("the client list"));
	assert.equal(text(sealed.slice(0, ENVELOPE.length)), ENVELOPE);
	assert.ok(!text(sealed).includes("client list"));
	assert.equal(text(await crypt.open(sealed)), "the client list");
});

test("sealing the same content twice gives different bytes", async () => {
	const crypt = await VaultCrypt.fromKey(generateKey());
	const a = new Uint8Array(await crypt.seal(bytes("same")));
	const b = new Uint8Array(await crypt.seal(bytes("same")));
	assert.notDeepEqual(a, b);
});

test("content sealed under another key is refused, not returned as garbage", async () => {
	const mine = await VaultCrypt.fromKey(generateKey());
	const theirs = await VaultCrypt.fromKey(generateKey());
	await assert.rejects(mine.open(await theirs.seal(bytes("x"))), WrongKeyError);
});

test("tampered content is refused", async () => {
	const crypt = await VaultCrypt.fromKey(generateKey());
	const sealed = new Uint8Array(await crypt.seal(bytes("an honest note")));
	sealed[sealed.length - 1] ^= 1;
	await assert.rejects(crypt.open(sealed.buffer), WrongKeyError);
});

test("names are deterministic, per segment, and unreadable", async () => {
	const key = generateKey();
	const crypt = await VaultCrypt.fromKey(key);
	const again = await VaultCrypt.fromKey(key);

	const sealed = crypt.encryptPath("Clients/Acme/Notes.md");
	assert.equal(sealed, again.encryptPath("Clients/Acme/Notes.md"));
	assert.match(sealed, /^[a-z2-7]+\/[a-z2-7]+\/[a-z2-7]+$/);
	assert.ok(!/acme|clients|notes/i.test(sealed));
	// The folder keeps its ciphertext wherever it appears, so the tree survives.
	assert.equal(crypt.encryptPath("Clients/Other.md").split("/")[0], sealed.split("/")[0]);
	assert.equal(again.decryptPath(sealed), "Clients/Acme/Notes.md");
});

test("the two Unicode spellings of a name are one encrypted name", async () => {
	const crypt = await VaultCrypt.fromKey(generateKey());
	const nfc = "Päivä.md".normalize("NFC");
	const nfd = "Päivä.md".normalize("NFD");
	assert.notEqual(nfc, nfd);
	assert.equal(crypt.encryptPath(nfc), crypt.encryptPath(nfd));
	assert.equal(crypt.decryptPath(crypt.encryptPath(nfd)), nfc);
});

test("a name from another key does not decrypt", async () => {
	const mine = await VaultCrypt.fromKey(generateKey());
	const theirs = await VaultCrypt.fromKey(generateKey());
	assert.throws(() => mine.decryptPath(theirs.encryptPath("Notes.md")));
});

test("a name too long to store encrypted is refused by name", async () => {
	const crypt = await VaultCrypt.fromKey(generateKey());
	assert.ok(crypt.encryptPath("a".repeat(143) + "/b.md"));
	assert.throws(() => crypt.encryptPath("a".repeat(144) + "/b.md"), NameTooLongError);
});

test("the key check depends on the key and nothing else", async () => {
	const key = generateKey();
	const a = await VaultCrypt.fromKey(key);
	const b = await VaultCrypt.fromKey(key);
	const other = await VaultCrypt.fromKey(generateKey());
	assert.match(a.keyCheck, /^[0-9a-f]{64}$/);
	assert.equal(a.keyCheck, b.keyCheck);
	assert.notEqual(a.keyCheck, other.keyCheck);
});

test("a recovery key round-trips however it is typed back", async () => {
	const key = generateKey();
	const shown = await encodeRecoveryKey(key);
	assert.match(shown, /^([A-Z2-7]{5}-)+[A-Z2-7]{1,5}$/);
	assert.deepEqual(await decodeRecoveryKey(shown), key);
	assert.deepEqual(await decodeRecoveryKey(` ${shown.toLowerCase().replaceAll("-", " ")}\n`), key);
});

test("a recovery key with a typo is caught before it is used", async () => {
	const shown = await encodeRecoveryKey(generateKey());
	const swapped = (shown[0] === "A" ? "B" : "A") + shown.slice(1);
	await assert.rejects(decodeRecoveryKey(swapped), WrongKeyError);
	await assert.rejects(decodeRecoveryKey(shown.slice(0, -4)), WrongKeyError);
	await assert.rejects(decodeRecoveryKey("not a key at all!"), WrongKeyError);
});
