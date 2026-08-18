import { test } from "node:test";
import assert from "node:assert/strict";
import { loadToken, saveToken, migrateToken } from "../../dist-test/entry.mjs";

function fakeApp() {
	const store = new Map();
	return {
		store,
		loadLocalStorage: (k) => store.get(k) ?? null,
		saveLocalStorage: (k, v) => (v === null ? store.delete(k) : store.set(k, v)),
	};
}

test("the token round-trips through device-local storage", () => {
	const app = fakeApp();
	saveToken(app, "secret-value");
	assert.equal(loadToken(app), "secret-value");
});

test("an absent token reads as empty, not undefined", () => {
	assert.equal(loadToken(fakeApp()), "");
});

test("migration lifts a token out of data.json and blanks the field", () => {
	const app = fakeApp();
	const data = { serverUrl: "https://vault.example", token: "old-secret", device: "mac" };
	const changed = migrateToken(app, data);
	assert.equal(changed, true, "migration should report that it changed data.json");
	assert.equal(data.token, "", "the token must be blanked in the file that gets written back");
	assert.equal(loadToken(app), "old-secret");
});

test("migration is idempotent and never clobbers a stored token", () => {
	const app = fakeApp();
	saveToken(app, "current");
	const data = { serverUrl: "https://vault.example", token: "", device: "mac" };
	assert.equal(migrateToken(app, data), false);
	assert.equal(loadToken(app), "current");
});
