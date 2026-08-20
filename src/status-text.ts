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
 * watchRemote governs INCOMING changes, independently of how this device
 * schedules its own outgoing syncs -- so this clause is appended identically
 * in every branch below, never tied to whether an interval is set.
 */
function remoteClause(watchRemote: boolean): string {
	return watchRemote
		? "Changes from other devices arrive within about a second."
		: "Changes from other devices show up on the next sync.";
}

export function formatSyncSchedule(s: ScheduleSettings): string {
	const { syncOnChange, intervalSeconds, watchRemote } = s;

	if (intervalSeconds > 0 && syncOnChange) {
		return (
			`Syncing right after you edit, and every ${formatDuration(intervalSeconds)} ` +
			`in the background. ${remoteClause(watchRemote)}`
		);
	}
	if (intervalSeconds > 0 && !syncOnChange) {
		return (
			`Syncing every ${formatDuration(intervalSeconds)} in the background — ` +
			`not on every edit. ${remoteClause(watchRemote)}`
		);
	}
	if (intervalSeconds === 0 && syncOnChange) {
		return (
			`Syncing right after you edit — no background interval set. ` +
			`${remoteClause(watchRemote)}`
		);
	}
	// intervalSeconds === 0 && !syncOnChange: main.ts still syncs on window
	// blur/focus and via the sync-now command/ribbon icon regardless of
	// these two settings, so "not syncing automatically" would be false.
	return (
		`Syncing when Obsidian gains or loses focus, and via the sync command ` +
		`— no automatic interval or on-edit sync set. ${remoteClause(watchRemote)}`
	);
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

/**
 * intervalSeconds below 10 is almost always a typo (e.g. "1" for "10"); 0
 * means "disabled" and stays 0.
 */
export function clampInterval(seconds: number): number {
	if (seconds <= 0) return 0;
	return Math.max(10, Math.floor(seconds));
}
