import { test } from "node:test";
import assert from "node:assert/strict";
import {
	formatDuration,
	deriveSyncMode,
	applySyncMode,
	describeSyncMode,
	formatRelativeTime,
	formatBytes,
	formatVaultStats,
	formatPermissions,
	suggestDeviceName,
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

test("deriveSyncMode: any live trigger means automatic", () => {
	assert.equal(deriveSyncMode({ syncOnChange: true, watchRemote: true, intervalSeconds: 300 }), "automatic");
	assert.equal(deriveSyncMode({ syncOnChange: true, watchRemote: false, intervalSeconds: 0 }), "automatic");
	assert.equal(deriveSyncMode({ syncOnChange: false, watchRemote: true, intervalSeconds: 0 }), "automatic");
});

test("deriveSyncMode: interval alone is periodic", () => {
	assert.equal(deriveSyncMode({ syncOnChange: false, watchRemote: false, intervalSeconds: 300 }), "periodic");
});

test("deriveSyncMode: nothing on is manual", () => {
	assert.equal(deriveSyncMode({ syncOnChange: false, watchRemote: false, intervalSeconds: 0 }), "manual");
});

test("applySyncMode: automatic turns everything on with the standard backstop", () => {
	const s = applySyncMode("automatic", { syncOnChange: false, watchRemote: false, intervalSeconds: 0 });
	assert.deepEqual(s, { syncOnChange: true, watchRemote: true, intervalSeconds: 300 });
});

test("applySyncMode: periodic keeps a chosen interval and drops the live triggers", () => {
	const s = applySyncMode("periodic", { syncOnChange: true, watchRemote: true, intervalSeconds: 600 });
	assert.deepEqual(s, { syncOnChange: false, watchRemote: false, intervalSeconds: 600 });
});

test("applySyncMode: periodic falls back to the default when the interval was 0", () => {
	const s = applySyncMode("periodic", { syncOnChange: true, watchRemote: true, intervalSeconds: 0 });
	assert.equal(s.intervalSeconds, 300);
});

test("applySyncMode: manual turns everything off", () => {
	const s = applySyncMode("manual", { syncOnChange: true, watchRemote: true, intervalSeconds: 300 });
	assert.deepEqual(s, { syncOnChange: false, watchRemote: false, intervalSeconds: 0 });
});

test("round trip: applying a mode derives back to the same mode", () => {
	for (const mode of ["automatic", "periodic", "manual"]) {
		assert.equal(deriveSyncMode(applySyncMode(mode, { syncOnChange: true, watchRemote: false, intervalSeconds: 45 })), mode);
	}
});

test("describeSyncMode: automatic mentions edits and the ~1s arrival", () => {
	const s = describeSyncMode("automatic", 300);
	assert.match(s, /right after you edit/);
	assert.match(s, /about a second/);
});

test("describeSyncMode: periodic names the interval as a duration", () => {
	assert.match(describeSyncMode("periodic", 300), /every 5 minutes/);
	assert.match(describeSyncMode("periodic", 90), /every 1.5 minutes/);
});

test("describeSyncMode: manual names the ways to trigger a sync", () => {
	const s = describeSyncMode("manual", 0);
	assert.match(s, /command/);
	assert.match(s, /focus/);
});
