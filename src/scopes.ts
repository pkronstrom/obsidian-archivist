/**
 * Scope names the server understands. Kept here rather than imported so this
 * file stays free of Obsidian and can be unit-tested in the Node harness.
 */
export const SCOPE_READ = "read";
export const SCOPE_WRITE = "write";

/**
 * missingPluginScopes names the verbs a token lacks for the plugin to work.
 *
 * The plugin needs BOTH: it syncs down, and asks the read-scoped POST /v1/have
 * which blobs are missing before it uploads anything. A token holding only one
 * of them syncs partway and then fails at the worst possible moment -- on a
 * save, with the note already edited and nowhere to put it.
 *
 */
export function missingPluginScopes(scopes: string[]): string[] {
	return [SCOPE_READ, SCOPE_WRITE].filter((s) => !scopes.includes(s));
}

/** A sentence for a Notice, or "" when the token is fine. */
export function scopeWarning(scopes: string[], label?: string): string {
	const missing = missingPluginScopes(scopes);
	if (missing.length === 0) return "";
	const who = label ? ` "${label}"` : "";
	return (
		`this token${who} cannot drive the plugin: it is missing ` +
		`${missing.join(" and ")}. Mint one with read and write.`
	);
}

/** One pending local change, before it becomes a wire Change. */
export type Pending = {
	path: string;
	op: "put" | "del" | "move";
	hash?: string;
	from?: string;
	content?: ArrayBuffer;
};

/**
 * pairRenames folds a deletion and an addition of identical content into one
 * move.
 *
 * A rename otherwise arrives as del+put, which needs the delete scope — so an
 * agent token holding only read and write could not rename a note at all. The
 * move op exists to make renaming a write, and this is where the plugin
 * notices one happened.
 *
 * Pairing is by exact hash, and only ever one-to-one: two deletions of the same
 * content are ambiguous, and guessing which one became the addition would move
 * the wrong file. Ambiguous groups are left as del+put, which still works for
 * any token that can delete.
 *
 * The content is dropped from a paired put, because the server already has it —
 * that is what makes a move preserving rather than destructive.
 */
export function pairRenames(changes: Pending[]): Pending[] {
	const deletes = changes.filter((c) => c.op === "del");
	const puts = changes.filter((c) => c.op === "put" && c.hash);
	if (deletes.length === 0 || puts.length === 0) return changes;

	// A hash is only usable as a pairing key when exactly one deletion and one
	// addition carry it. Anything else is ambiguous.
	const byHash = new Map<string, { dels: Pending[]; puts: Pending[] }>();
	for (const d of deletes) {
		if (!d.hash) continue;
		const e = byHash.get(d.hash) ?? { dels: [], puts: [] };
		e.dels.push(d);
		byHash.set(d.hash, e);
	}
	for (const p of puts) {
		const e = byHash.get(p.hash as string);
		if (e) e.puts.push(p);
	}

	const pairedDel = new Set<Pending>();
	const moves = new Map<Pending, Pending>();
	for (const { dels, puts: ps } of byHash.values()) {
		if (dels.length !== 1 || ps.length !== 1) continue;
		pairedDel.add(dels[0]);
		moves.set(ps[0], dels[0]);
	}

	const out: Pending[] = [];
	for (const c of changes) {
		if (pairedDel.has(c)) continue;
		const from = moves.get(c);
		if (from) {
			// hash is what the server must find at the source: If-Match for a
			// rename. Without it a move refused because the source changed
			// remotely is retried against a fresh base, the staleness check no
			// longer fires, and the server's newer bytes land at the new path
			// while this client records its own older snapshot there.
			out.push({ path: c.path, op: "move", from: from.path, hash: from.hash });
			continue;
		}
		out.push(c);
	}
	return out;
}

/** Step-up kinds, mirroring internal/auth's catalog. */
export const STEP_UP_VAULT = "vault";

/**
 * stepUpWarning names the protected vaults this token is gated on for ACCESS.
 *
 * Two inputs, because they are two different facts: which vaults the server
 * protects, and what this token decided about them. Warning on either alone
 * would be wrong -- a posture for a vault the server does not protect is
 * harmless, and a protected vault this token is not gated on is somebody else's
 * problem.
 *
 * Only `vault:` entries warn. A device gated on vault ACCESS is a
 * misconfiguration: the plugin cannot present a code, and it persists the whole
 * vault to disk anyway, so a synced protected vault is plaintext on that device
 * forever. An `ops:` posture is the opposite -- confirming a destructive
 * operation triggered from the plugin is worth having.
 *
 */
export function stepUpWarning(
	protectedVaults: string[],
	posture: string[],
	label?: string,
): string {
	if (!protectedVaults.length || !posture.length) return "";
	const gated = protectedVaults.filter((v) => posture.includes(`${STEP_UP_VAULT}:${v}`));
	if (gated.length === 0) return "";
	const who = label ? ` "${label}"` : "";
	return (
		`this token${who} needs an unlock code for ${gated.join(", ")}, ` +
		`and the plugin cannot supply one. Mint a token without a vault: posture for this device.`
	);
}
