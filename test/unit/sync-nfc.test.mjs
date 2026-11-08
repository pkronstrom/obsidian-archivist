import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Sync } from "../../dist-test/entry.mjs";
import { FakeApp } from "../obsidian-shim.mjs";
import { FakeServer } from "../fake-server.mjs";

const composed = "Päivä.md";
const decomposed = "Päivä.md";

test("a decomposed filename syncs under the composed spelling and settles", async (t) => {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), "nfc-"));
	await fs.writeFile(path.join(root, decomposed), "body\n");
	try {
		await fs.access(path.join(root, composed));
	} catch {
		t.skip("this filesystem keeps the two spellings apart; the fix applies only where they are one file");
		return;
	}

	const srv = new FakeServer({ nfc: true });
	const sync = new Sync(new FakeApp(root), () => srv.client(), () => "mac", () => {});

	const first = await sync.run();
	assert.equal(first.pushed, 1);
	assert.deepEqual(Object.keys(srv.files()), [composed]);

	// The bug: the result named the composed path, the plugin looked it up by
	// the decomposed one, never recorded the file, and pushed it again.
	const second = await sync.run();
	assert.equal(second.pushed, 0, JSON.stringify(second));
});
