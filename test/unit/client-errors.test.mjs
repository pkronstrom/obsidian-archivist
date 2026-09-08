import { test } from "node:test";
import assert from "node:assert/strict";
import { Client } from "../../dist-test/entry.mjs";

/**
 * A failing response whose body is NOT an error envelope.
 *
 * This is not a hypothetical. A token pasted with a stray newline makes the
 * request itself malformed, and Go's own header validation answers with a
 * plain-text 400 before any handler runs -- no envelope, no JSON.
 */
function replying(status, body, contentType = "text/plain") {
	globalThis.fetch = async () =>
		new Response(body, { status, headers: { "content-type": contentType } });
}

test("a non-JSON failure reports its status, not a parse error", async () => {
	replying(400, "400 Bad Request: invalid header field value\n");
	const c = new Client("https://vault.example", "tok\n", "work");
	await assert.rejects(c.index(), (err) => {
		assert.doesNotMatch(err.message, /JSON/i, `parse error leaked: ${err.message}`);
		assert.match(err.message, /400/);
		assert.equal(err.status, 400);
		return true;
	});
});

test("an empty body still names the route and status", async () => {
	replying(502, "");
	await assert.rejects(new Client("https://vault.example", "tok", "work").head(), (err) => {
		assert.doesNotMatch(err.message, /JSON/i);
		assert.match(err.message, /GET \/v1\/head -> 502/);
		return true;
	});
});

test("an HTML gateway page is reported as unauthorized when that is the status", async () => {
	replying(401, "<html><body>401 Unauthorized</body></html>", "text/html");
	await assert.rejects(new Client("https://vault.example", "tok", "work").head(), (err) => {
		assert.doesNotMatch(err.message, /JSON/i);
		assert.equal(err.code, "unauthorized");
		return true;
	});
});

test("a real error envelope is still preferred over the status", async () => {
	replying(403, '{"error":{"code":"forbidden","message":"token does not open work"}}', "application/json");
	await assert.rejects(new Client("https://vault.example", "tok", "work").head(), (err) => {
		assert.equal(err.code, "forbidden");
		assert.match(err.message, /does not open work/);
		return true;
	});
});
