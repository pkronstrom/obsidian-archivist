import type { App } from "obsidian";
import type { KeyScope } from "./crypt";

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

/** A key as lowercase hex, and which paths it is for. */
export type StoredKey = { key: string; scope: KeyScope };

function loadKeys(app: App): Record<string, unknown> {
	const raw = app.loadLocalStorage(KEYS);
	return raw && typeof raw === "object" ? (raw as Record<string, unknown>) : {};
}

/**
 * The key for a vault, or null when this device has none.
 *
 * The scope is read from here and from nowhere else. What kind of vault the
 * server says it is serving today must never change what a stored key is
 * taken to cover. A bare string is a key stored before scopes existed, which
 * could only have been for an encrypted vault.
 */
export function loadVaultKey(app: App, vault: string): StoredKey | null {
	const stored = loadKeys(app)[vault];
	if (typeof stored === "string") return stored ? { key: stored, scope: "vault" } : null;
	if (!stored || typeof stored !== "object") return null;
	const { key, scope } = stored as Partial<StoredKey>;
	if (typeof key !== "string" || !key) return null;
	return { key, scope: scope === "files" ? "files" : "vault" };
}

export function saveVaultKey(app: App, vault: string, key: string, scope: KeyScope): void {
	app.saveLocalStorage(KEYS, { ...loadKeys(app), [vault]: { key, scope } });
}

/**
 * Vaults whose encrypted files this device was offered the key for and said
 * "not now". Remembered so a device kept without the key on purpose is not
 * asked again at every start; settings still offer it.
 */
const DECLINED = "archivist.keyDeclined";

export function keyDeclined(app: App, vault: string): boolean {
	const raw = app.loadLocalStorage(DECLINED);
	return Array.isArray(raw) && raw.includes(vault);
}

export function setKeyDeclined(app: App, vault: string, declined: boolean): void {
	const raw = app.loadLocalStorage(DECLINED);
	const rest = (Array.isArray(raw) ? raw : []).filter((v) => v !== vault);
	app.saveLocalStorage(DECLINED, declined ? [...rest, vault] : rest);
}
