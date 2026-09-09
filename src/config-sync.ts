/** Legacy types retained for callers upgrading from config sync. No stored choice
	* can enable live config transfer anymore. */
export const CONFIG_DIR = ".obsidian";
export type ConfigLevel = "files" | "appearance" | "plugins";
export type ConfigSyncSettings = {
	level: ConfigLevel;
	acceptedPlugins: string[];
	acceptAllPlugins: boolean;
};
export const DEFAULT_CONFIG_SYNC: ConfigSyncSettings = {
	level: "files", acceptedPlugins: [], acceptAllPlugins: false,
};
export function configSyncable(_path: string, _settings: ConfigSyncSettings): boolean {
	return false;
}
