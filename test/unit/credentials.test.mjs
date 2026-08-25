import { test } from "node:test";
import assert from "node:assert/strict";
import { loadToken, saveToken } from "../../dist-test/entry.mjs";

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
