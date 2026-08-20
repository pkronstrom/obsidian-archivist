/**
 * Pure text-formatting helpers for the settings tab and status bar. Kept
 * free of Obsidian and DOM so they can be unit-tested directly, the same
 * way scopes.ts is.
 */

export function formatDuration(seconds: number): string {
	if (seconds < 60) return `${seconds} second${seconds === 1 ? "" : "s"}`;
	const minutes = Math.round((seconds / 60) * 10) / 10;
	return `${minutes} minute${minutes === 1 ? "" : "s"}`;
}

export type ScheduleSettings = {
	syncOnChange: boolean;
	intervalSeconds: number;
	watchRemote: boolean;
};

/**
 * The three underlying switches produce combinations nobody wants (edits
 * held local while remote changes stream in, say), so the UI offers three
 * INTENTS instead and maps them onto the switches. The switches stay in
 * storage untouched -- a device upgrading from the toggle era derives its
 * mode from what it already had.
 */
export type SyncMode = "automatic" | "periodic" | "manual";

/** The backstop interval automatic mode runs behind the scenes: it covers a
 *  missed watcher wake, never drives the experience, and is not worth a
 *  visible knob. */
export const AUTOMATIC_BACKSTOP_SECONDS = 300;

export function deriveSyncMode(s: ScheduleSettings): SyncMode {
	// Either live trigger means the user wanted immediacy; the interval is
	// then just the backstop, whatever its value.
	if (s.syncOnChange || s.watchRemote) return "automatic";
	if (s.intervalSeconds > 0) return "periodic";
	return "manual";
}

export function applySyncMode(mode: SyncMode, current: ScheduleSettings): ScheduleSettings {
	switch (mode) {
		case "automatic":
			return {
				syncOnChange: true,
				watchRemote: true,
				intervalSeconds: AUTOMATIC_BACKSTOP_SECONDS,
			};
		case "periodic":
			return {
				syncOnChange: false,
				watchRemote: false,
				intervalSeconds:
					current.intervalSeconds > 0 ? current.intervalSeconds : AUTOMATIC_BACKSTOP_SECONDS,
			};
		case "manual":
			return { syncOnChange: false, watchRemote: false, intervalSeconds: 0 };
	}
}

export function describeSyncMode(mode: SyncMode, intervalSeconds: number): string {
	switch (mode) {
		case "automatic":
			return (
				"Syncing right after you edit; changes from other devices arrive " +
				"within about a second. A background pass every few minutes covers " +
				"anything missed."
			);
		case "periodic":
			return (
				`Syncing every ${formatDuration(intervalSeconds)}; edits wait for ` +
				`the next pass.`
			);
		case "manual":
			// main.ts syncs on window focus regardless of these settings, so
			// "only when you ask" would undersell what actually happens.
			return (
				"Syncing only on demand — the ribbon icon, the sync command, or " +
				"when the app gains focus."
			);
	}
}

export function formatRelativeTime(atMs: number | undefined, nowMs: number): string {
	if (atMs === undefined) return "Never";
	const diffSec = Math.max(0, Math.round((nowMs - atMs) / 1000));
	if (diffSec < 10) return "just now";
	if (diffSec < 60) return `${diffSec} seconds ago`;
	const diffMin = Math.round(diffSec / 60);
	if (diffMin < 60) return `${diffMin} minute${diffMin === 1 ? "" : "s"} ago`;
	const diffHour = Math.round(diffMin / 60);
	if (diffHour < 24) return `${diffHour} hour${diffHour === 1 ? "" : "s"} ago`;
	const diffDay = Math.round(diffHour / 24);
	return `${diffDay} day${diffDay === 1 ? "" : "s"} ago`;
}

export function formatBytes(bytes: number): string {
	if (bytes < 1024) return `${bytes} B`;
	const units = ["KB", "MB", "GB", "TB"];
	let value = bytes / 1024;
	let i = 0;
	while (value >= 1024 && i < units.length - 1) {
		value /= 1024;
		i++;
	}
	return `${value.toFixed(1)} ${units[i]}`;
}

export function formatVaultStats(fileCount: number, totalBytes: number): string {
	return `${fileCount} file${fileCount === 1 ? "" : "s"}, ${formatBytes(totalBytes)}`;
}

export function formatPermissions(scopes: string[] | undefined, label?: string): string {
	const who = label ? ` ("${label}")` : "";
	if (scopes === undefined) {
		return `Read + write, assumed${who} — this server doesn't report scopes`;
	}
	const hasRead = scopes.includes("read");
	const hasWrite = scopes.includes("write");
	if (hasRead && hasWrite) return `Read + write${who}`;
	if (hasRead) return `Read only${who} — writes will fail`;
	if (hasWrite) return `Write only${who} — pulls will fail`;
	return `No read or write${who} — this token cannot sync`;
}

export type DevicePlatform = {
	isDesktopApp: boolean;
	isIosApp: boolean;
	isAndroidApp: boolean;
	isTablet: boolean;
};

export function suggestDeviceName(platform: DevicePlatform, hostname: string | null): string {
	if (platform.isDesktopApp && hostname) return hostname;
	if (platform.isIosApp) return platform.isTablet ? "iPad" : "iPhone";
	if (platform.isAndroidApp) return platform.isTablet ? "Android tablet" : "Android phone";
	return "This device";
}

