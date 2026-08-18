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

// Reported from a real vault: Excalidraw stores modifierKeyOverrides[n].key,
// where "key" is a KEYBOARD key. Flagging that refuses a whole plugin over a
// keystroke, which is the false-positive cost the design accepted in the
// abstract and is plainly wrong here.
test("a keyboard key called 'key' is not a credential", () => {
	const found = scanForSecrets({
		modifierKeyOverrides: [
			{ key: "Alt", shift: false },
			{ key: "Meta", shift: true },
			{ key: "Control", shift: false },
		],
	});
	assert.deepEqual(found, [], JSON.stringify(found));
});

test("but a 'key' holding an opaque value still is", () => {
	const found = scanForSecrets({ key: "sk-abcdefghijklmnopqrstuvwxyz012345" });
	assert.equal(found.length, 1);
	assert.match(found[0].why, /opaque/i);
});

test("'auth' and 'session' need corroboration too", () => {
	assert.deepEqual(scanForSecrets({ auth: "basic", session: "30m" }), []);
	assert.equal(scanForSecrets({ auth: "ZXlKaGJHY2lPaUpJVXpJMU5pSjkuYWJj" }).length, 1);
});

// A strong name is enough on its own -- "token" is never a keyboard setting.
test("a short token is still reported", () => {
	const found = scanForSecrets({ token: "abc123" });
	assert.equal(found.length, 1);
	assert.match(found[0].why, /name/i);
});
