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
 * An older server omits `scopes` entirely. That is not "holds no scopes": it
 * predates them, when every token could do everything, so an absent list means
 * nothing is missing. Treating undefined as empty would lock every device out
 * of a server that is working perfectly well.
 */
export function missingPluginScopes(scopes: string[] | undefined): string[] {
	if (scopes === undefined) return [];
	return [SCOPE_READ, SCOPE_WRITE].filter((s) => !scopes.includes(s));
}

/** A sentence for a Notice, or "" when the token is fine. */
export function scopeWarning(scopes: string[] | undefined, label?: string): string {
	const missing = missingPluginScopes(scopes);
	if (missing.length === 0) return "";
	const who = label ? ` "${label}"` : "";
	return (
		`this token${who} cannot drive the plugin: it is missing ` +
		`${missing.join(" and ")}. Mint one with read and write.`
	);
}
