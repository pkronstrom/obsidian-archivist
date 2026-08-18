import type { App } from "obsidian";

/**
 * Which Obsidian configuration files this device syncs.
 *
 * An ALLOWLIST, mirrored from the server's internal/vault/config.go. Enforced in
 * both places on purpose: an older or buggy plugin cannot push data.json into
 * history, and a server that has not been updated cannot be talked into
 * accepting something this version would not send.
 *
 * The categories are Obsidian Sync's own vocabulary -- Main settings,
 * Appearance, Themes and snippets, Hotkeys, the core plugin list -- because that
 * is the vocabulary already on the settings screen.
 */

/** Obsidian's default configuration directory. */
export const CONFIG_DIR = ".obsidian";

/**
 * Three states, chosen per device and NEVER synced.
 *
 * Obsidian keeps its own sync settings per device and never syncs them, for the
 * same reason: a synced setting fights across devices, and the device that
 * wanted less config gets more of it every cycle.
 */
export type ConfigLevel = "files" | "appearance" | "plugins";

export type ConfigSyncSettings = {
	level: ConfigLevel;
	/**
	 * Plugin ids whose data.json the user has explicitly accepted, having been
	 * told what the scanner saw. Default empty: plugin data is opt-in, per
	 * plugin, because a plugin's data.json is the most likely place in the whole
	 * vault to find a live credential.
	 */
	acceptedPlugins: string[];
};

export const DEFAULT_CONFIG_SYNC: ConfigSyncSettings = {
	level: "files",
	acceptedPlugins: [],
};

/** Plugin ids this plugin has shipped under. Never syncable, at any level. */
const ARCHIVIST_IDS = new Set(["archivist", "obsidian-archivist"]);

const APPEARANCE_FILES = new Set(["app.json", "appearance.json", "hotkeys.json"]);
const PLUGIN_LIST_FILES = new Set(["community-plugins.json", "core-plugins.json"]);

/**
 * configSyncable reports whether a config path may sync at this device's level.
 *
 * Returns false for every path outside CONFIG_DIR: this decides config only, and
 * ordinary notes are the caller's business.
 */
export function configSyncable(path: string, settings: ConfigSyncSettings): boolean {
	if (settings.level === "files") return false;
	if (!path.startsWith(CONFIG_DIR + "/")) return false;

	const rest = path.slice(CONFIG_DIR.length + 1);
	if (!rest) return false;
	// No traversal, no nested dotfiles.
	const segments = rest.split("/");
	if (segments.some((s) => s === "" || s === "." || s === ".." || s.startsWith("."))) return false;

	if (APPEARANCE_FILES.has(rest)) return true;
	if (segments[0] === "snippets") return segments.length === 2 && rest.endsWith(".css");
	if (segments[0] === "themes") return segments.length >= 3;

	if (settings.level !== "plugins") return false;

	if (PLUGIN_LIST_FILES.has(rest)) return true;

	if (segments[0] === "plugins") {
		// Only data.json, only for a plugin explicitly accepted, and never ours.
		//
		// Plugin CODE is deliberately absent. A realistic 15-plugin set is
		// several MB, and every plugin update rewrites main.js wholesale as a
		// fresh blob -- roughly 50 MB a year, unrecoverable except by prune.
		// The list travels instead, and the receiving device installs from the
		// store. See plugin-install.ts.
		if (segments.length !== 3 || segments[2] !== "data.json") return false;
		const id = segments[1];
		if (ARCHIVIST_IDS.has(id.toLowerCase())) return false;
		return settings.acceptedPlugins.includes(id);
	}

	return false;
}

const KEY = "archivist.configsync";

/**
 * Config sync settings live in device-local storage, never in the vault and
 * never in data.json.
 *
 * Per device and never synced, exactly as Obsidian keeps its own sync settings:
 * a synced setting fights across devices, and the phone that wanted files only
 * would get the laptop's choice back every cycle.
 */
export function loadConfigSync(app: App): ConfigSyncSettings {
	const raw = app.loadLocalStorage(KEY);
	if (!raw || typeof raw !== "object") return { ...DEFAULT_CONFIG_SYNC };
	const s = raw as Partial<ConfigSyncSettings>;
	const level: ConfigLevel = s.level === "appearance" || s.level === "plugins" ? s.level : "files";
	return {
		level,
		acceptedPlugins: Array.isArray(s.acceptedPlugins)
			? s.acceptedPlugins.filter((v): v is string => typeof v === "string")
			: [],
	};
}

export function saveConfigSync(app: App, settings: ConfigSyncSettings): void {
	app.saveLocalStorage(KEY, settings);
}
