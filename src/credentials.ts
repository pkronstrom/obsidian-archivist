import type { App } from "obsidian";

/**
 * The bearer token, in device-local storage rather than in data.json.
 *
 * data.json lives at .obsidian/plugins/archivist/data.json -- inside the vault.
 * Config sync makes that directory reachable, and although this plugin's own
 * data.json is hard-excluded on the server with no override, a credential that
 * is not in the file cannot be leaked by any future mistake in that exclusion.
 * A sync tool must never be able to sync its own credentials, and the surest
 * way to guarantee that is for the credential not to be in a syncable file.
 *
 * The same reasoning already put the sync cursor here; see state.ts.
 */
const KEY = "archivist.token";

export function loadToken(app: App): string {
	const raw = app.loadLocalStorage(KEY);
	return typeof raw === "string" ? raw : "";
}

export function saveToken(app: App, token: string): void {
	app.saveLocalStorage(KEY, token);
}

/**
 * migrateToken lifts a token written by an older version out of data.json.
 *
 * Returns true when data.json changed and must be saved back. The old value is
 * blanked rather than deleted, so the field keeps its shape for anything
 * reading the file, and the secret is gone from it.
 *
 * No history cleanup is needed: .obsidian was never syncable before this
 * release, so the old data.json has never been committed anywhere.
 */
export function migrateToken(app: App, data: { token?: string }): boolean {
	const inFile = typeof data.token === "string" ? data.token : "";
	if (!inFile) return false;
	if (!loadToken(app)) saveToken(app, inFile);
	data.token = "";
	return true;
}
