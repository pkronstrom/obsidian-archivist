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
