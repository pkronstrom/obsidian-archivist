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
	/**
	 * Accept EVERY plugin's data.json, including ones the scanner flagged.
	 *
	 * Per-plugin opt-in is the default because a data.json is the likeliest
	 * place in a vault to hold a live credential, and the scanner cannot be
	 * right on arbitrary JSON. This is the same override the per-plugin switch
	 * offers, applied to all of them at once -- a scale change, not a different
	 * policy, and it states what is being accepted before it takes effect.
	 *
	 * Archivist's own data.json is STILL excluded. Syncing the sync tool's own
	 * control settings would create a cross-device feedback loop.
	 */
	acceptAllPlugins: boolean;
};

export const DEFAULT_CONFIG_SYNC: ConfigSyncSettings = {
	level: "files",
	acceptedPlugins: [],
	acceptAllPlugins: false,
};

/** This plugin's current id. Never syncable, at any level. */
const ARCHIVIST_ID = "archivist";

const APPEARANCE_FILES = new Set(["app.json", "appearance.json", "hotkeys.json"]);
const PLUGIN_LIST_FILES = new Set(["community-plugins.json", "core-plugins.json"]);

/**
 * configSyncable reports whether a config path may sync at this device's level.
 *
 * Returns false for every path outside CONFIG_DIR: this decides config only, and
 * ordinary notes are the caller's business.
 */
/**
 * The plugin id for a path that is a plugin's data.json, or null.
 *
 * Used to decide whether a file needs scanning before it is pushed: only these
 * paths can carry a plugin's credentials.
 */
export function pluginDataId(path: string): string | null {
	if (!path.startsWith(CONFIG_DIR + "/plugins/")) return null;
	const rest = path.slice(CONFIG_DIR.length + "/plugins/".length).split("/");
	if (rest.length !== 2 || rest[1] !== "data.json") return null;
	return rest[0] || null;
}

/**
 * True when this data.json is syncing ONLY because "sync all plugins" is on,
 * rather than because the user enabled that specific plugin.
 *
 * That distinction is the whole basis of enforcement: an explicit per-plugin
 * opt-in is a decision someone made having read what the scanner found, and it
 * must keep working. Accept-all is a blanket default, and a blanket default
 * must not be able to push a credential nobody looked at.
 */
export function acceptedOnlyByDefault(path: string, settings: ConfigSyncSettings): boolean {
	const id = pluginDataId(path);
	if (!id) return false;
	if (settings.acceptedPlugins.includes(id)) return false;
	return settings.acceptAllPlugins;
}

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
		// Before the accept-all check, deliberately: syncing this plugin's own
		// control settings would create a cross-device feedback loop.
		if (id.toLowerCase() === ARCHIVIST_ID) return false;
		return settings.acceptAllPlugins || settings.acceptedPlugins.includes(id);
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
		acceptAllPlugins: s.acceptAllPlugins === true,
	};
}

export function saveConfigSync(app: App, settings: ConfigSyncSettings): void {
	app.saveLocalStorage(KEY, settings);
}
