import { test } from "node:test";
import assert from "node:assert/strict";
import {
	formatDuration,
	formatSyncSchedule,
	formatRelativeTime,
	formatBytes,
	formatVaultStats,
	formatPermissions,
	suggestDeviceName,
	clampInterval,
} from "../../dist-test/entry.mjs";

test("formatDuration: seconds below a minute stay in seconds", () => {
	assert.equal(formatDuration(10), "10 seconds");
	assert.equal(formatDuration(1), "1 second");
	assert.equal(formatDuration(59), "59 seconds");
});

test("formatDuration: 60 and above switch to minutes", () => {
	assert.equal(formatDuration(60), "1 minute");
	assert.equal(formatDuration(300), "5 minutes");
	assert.equal(formatDuration(90), "1.5 minutes");
});

test("formatSyncSchedule: on-change + interval + watch on", () => {
	const s = formatSyncSchedule({ syncOnChange: true, intervalSeconds: 300, watchRemote: true });
	assert.match(s, /right after you edit/);
	assert.match(s, /every 5 minutes/);
	assert.match(s, /arrive within about a second/);
});

test("formatSyncSchedule: on-change + interval + watch off", () => {
	const s = formatSyncSchedule({ syncOnChange: true, intervalSeconds: 300, watchRemote: false });
	assert.match(s, /show up on the next sync/);
});

test("formatSyncSchedule: interval only, no sync on change", () => {
	const s = formatSyncSchedule({ syncOnChange: false, intervalSeconds: 60, watchRemote: true });
	assert.match(s, /every 1 minute/);
	assert.match(s, /not on every edit/);
});

test("formatSyncSchedule: sync on change, no interval", () => {
	const s = formatSyncSchedule({ syncOnChange: true, intervalSeconds: 0, watchRemote: true });
	assert.match(s, /right after you edit/);
	assert.match(s, /no background interval set/);
});

test("formatSyncSchedule: everything off still mentions focus/blur and the command", () => {
	const s = formatSyncSchedule({ syncOnChange: false, intervalSeconds: 0, watchRemote: false });
	assert.match(s, /gains or loses focus/);
	assert.match(s, /sync command/);
	assert.doesNotMatch(s, /not syncing automatically/);
});

test("formatRelativeTime: undefined is Never", () => {
	assert.equal(formatRelativeTime(undefined, Date.now()), "Never");
});

test("formatRelativeTime: buckets", () => {
	const now = 1_000_000_000;
	assert.equal(formatRelativeTime(now - 3_000, now), "just now");
	assert.equal(formatRelativeTime(now - 45_000, now), "45 seconds ago");
	assert.equal(formatRelativeTime(now - 5 * 60_000, now), "5 minutes ago");
	assert.equal(formatRelativeTime(now - 3 * 3_600_000, now), "3 hours ago");
	assert.equal(formatRelativeTime(now - 2 * 86_400_000, now), "2 days ago");
});

test("formatBytes: scales units", () => {
	assert.equal(formatBytes(500), "500 B");
	assert.equal(formatBytes(2048), "2.0 KB");
	assert.equal(formatBytes(5 * 1024 * 1024), "5.0 MB");
});

test("formatVaultStats: pluralizes files", () => {
	assert.equal(formatVaultStats(1, 1024), "1 file, 1.0 KB");
	assert.equal(formatVaultStats(3, 1024), "3 files, 1.0 KB");
});

test("formatPermissions: full access", () => {
	assert.equal(formatPermissions(["read", "write"]), "Read + write");
	assert.match(formatPermissions(["read", "write"], "agent-n8n"), /agent-n8n/);
});

test("formatPermissions: partial access", () => {
	assert.match(formatPermissions(["read"]), /Read only/);
	assert.match(formatPermissions(["write"]), /Write only/);
	assert.match(formatPermissions([]), /No read or write/);
});

test("formatPermissions: undefined scopes is an assumed-full-access legacy server, not a warning", () => {
	const s = formatPermissions(undefined);
	assert.match(s, /Read \+ write/);
	assert.match(s, /doesn't report scopes/);
});

test("suggestDeviceName: desktop uses the hostname", () => {
	const platform = { isDesktopApp: true, isIosApp: false, isAndroidApp: false, isTablet: false };
	assert.equal(suggestDeviceName(platform, "pks-macbook"), "pks-macbook");
});

test("suggestDeviceName: desktop with no hostname falls back", () => {
	const platform = { isDesktopApp: true, isIosApp: false, isAndroidApp: false, isTablet: false };
	assert.equal(suggestDeviceName(platform, null), "This device");
});

test("suggestDeviceName: iOS phone vs tablet", () => {
	const phone = { isDesktopApp: false, isIosApp: true, isAndroidApp: false, isTablet: false };
	const tablet = { isDesktopApp: false, isIosApp: true, isAndroidApp: false, isTablet: true };
	assert.equal(suggestDeviceName(phone, null), "iPhone");
	assert.equal(suggestDeviceName(tablet, null), "iPad");
});

test("suggestDeviceName: Android phone vs tablet", () => {
	const phone = { isDesktopApp: false, isIosApp: false, isAndroidApp: true, isTablet: false };
	const tablet = { isDesktopApp: false, isIosApp: false, isAndroidApp: true, isTablet: true };
	assert.equal(suggestDeviceName(phone, null), "Android phone");
	assert.equal(suggestDeviceName(tablet, null), "Android tablet");
});

test("clampInterval: 0 stays 0 (disabled)", () => {
	assert.equal(clampInterval(0), 0);
	assert.equal(clampInterval(-5), 0);
});

test("clampInterval: below 10 is rounded up to 10", () => {
	assert.equal(clampInterval(1), 10);
	assert.equal(clampInterval(9), 10);
});

test("clampInterval: 10 and above pass through, floored", () => {
	assert.equal(clampInterval(10), 10);
	assert.equal(clampInterval(300.9), 300);
});
