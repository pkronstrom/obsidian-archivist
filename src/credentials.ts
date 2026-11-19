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
 * Vault keys, by server vault name. Device-local for the same reason the token
 * is, and more so: a key that synced would sit on the server next to the
 * ciphertext it opens.
 *
 * Not protected beyond that. The device already holds every note in plaintext
 * in its vault folder, so the key at rest here gives away nothing extra.
 */
const KEYS = "archivist.vaultKeys";

function loadKeys(app: App): Record<string, string> {
	const raw = app.loadLocalStorage(KEYS);
	return raw && typeof raw === "object" ? (raw as Record<string, string>) : {};
}

/** The key for a vault as lowercase hex, or "" when this device has none. */
export function loadVaultKey(app: App, vault: string): string {
	const key = loadKeys(app)[vault];
	return typeof key === "string" ? key : "";
}

export function saveVaultKey(app: App, vault: string, keyHex: string): void {
	app.saveLocalStorage(KEYS, { ...loadKeys(app), [vault]: keyHex });
}
