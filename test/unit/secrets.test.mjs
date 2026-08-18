import { test } from "node:test";
import assert from "node:assert/strict";
import { scanForSecrets } from "../../dist-test/entry.mjs";

test("a suspicious key name is reported", () => {
	const found = scanForSecrets({ apiKey: "sk-abcdefghijklmnopqrstuvwxyz" });
	assert.equal(found.length, 1);
	assert.equal(found[0].path, "apiKey");
	assert.match(found[0].why, /name/i);
});

test("a credential in a URL is reported even under an innocent key", () => {
	const found = scanForSecrets({ endpoint: "https://user:hunter2@example.com/api" });
	assert.equal(found.length, 1);
	assert.match(found[0].why, /URL/i);
});

test("a JWT is reported even under a key called config", () => {
	const jwt =
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk";
	const found = scanForSecrets({ config: jwt });
	assert.equal(found.length, 1);
	assert.match(found[0].why, /JWT/i);
});

test("a long opaque string is reported even under a key called k", () => {
	const found = scanForSecrets({ k: "9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a" });
	assert.equal(found.length, 1);
	assert.match(found[0].why, /entropy|opaque|long/i);
});

test("nesting and arrays are searched, and the path names the location", () => {
	const found = scanForSecrets({ services: [{ name: "x", password: "swordfish123456" }] });
	assert.equal(found.length, 1);
	assert.equal(found[0].path, "services[0].password");
});

test("ordinary settings are not reported", () => {
	const found = scanForSecrets({
		enabled: true,
		fontSize: 16,
		folder: "2. Areas/",
		dateFormat: "YYYY-MM-DD",
		keyboardShortcut: "Mod+K",
	});
	assert.deepEqual(found, []);
});

test("the scanner never edits its input", () => {
	const input = { token: "abcdefghijklmnopqrstuvwxyz012345" };
	const before = JSON.stringify(input);
	scanForSecrets(input);
	assert.equal(
		JSON.stringify(input),
		before,
		"the scanner must detect and never strip: a filtered data.json is a broken file that looks fine",
	);
});
