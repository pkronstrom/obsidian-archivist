// Single entry point for the integration harness.
//
// sync.ts and client.ts must be bundled TOGETHER. Bundled separately, each gets
// its own copy of client.ts, so `err instanceof UnknownBaseError` compares
// against two different class objects and the 409 recovery path never runs.
// The plugin itself is one bundle, so this only ever bit the harness -- but it
// bit it silently, as an unhandled rejection three tests later.
export { Sync, skip, conflictName, localOnly } from "../src/sync";
export { Client, UnknownBaseError } from "../src/client";

export { PairingHazardError, rescueFolder } from "../src/pairing";

export { isRescuePath } from "../src/pairing";

export { loadToken, saveToken, migrateToken } from "../src/credentials";

export { configSyncable, DEFAULT_CONFIG_SYNC, CONFIG_DIR } from "../src/config-sync";

export { scanForSecrets } from "../src/secrets";

export { plannedInstalls, installPlugins } from "../src/plugin-install";
export { missingPluginScopes, scopeWarning, stepUpWarning } from "../src/scopes";
export { createSyncScheduler } from "../src/sync-schedule";
export { pairRenames } from "../src/scopes";

export {
	deriveSyncMode,
	applySyncMode,
	formatRelativeTime,
	formatBytes,
	formatVaultStats,
	formatPermissions,
	suggestDeviceName,
} from "../src/status-text";
