/**
 * Git object hashing.
 *
 * The server addresses content by git object hash, so the plugin has to compute
 * the same thing to name an upload. The upside of matching git exactly is that
 * any value here can be checked against the real tool:
 *
 *   printf 'hello\n' | git hash-object --stdin
 */

/**
 * gitHash returns the git blob object id for content:
 *
 *   sha1("blob " + <byte length> + "\0" + content)
 *
 * The header carries the BYTE length, not the character count. Getting that
 * wrong only diverges on non-ASCII content, so the tests include an emoji.
 */
export async function gitHash(content: ArrayBuffer | Uint8Array): Promise<string> {
	const body = content instanceof Uint8Array ? content : new Uint8Array(content);
	const header = new TextEncoder().encode(`blob ${body.byteLength}\0`);

	const framed = new Uint8Array(header.byteLength + body.byteLength);
	framed.set(header, 0);
	framed.set(body, header.byteLength);

	const digest = await crypto.subtle.digest("SHA-1", framed);
	return hex(new Uint8Array(digest));
}

function hex(bytes: Uint8Array): string {
	let out = "";
	for (const b of bytes) out += b.toString(16).padStart(2, "0");
	return out;
}
