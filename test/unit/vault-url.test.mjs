import { test } from "node:test";
import assert from "node:assert/strict";
import { Client } from "../../dist-test/entry.mjs";

/** Capture what path the client builds, without a server. */
function capturing() {
	const seen = [];
	globalThis.fetch = async (url) => {
		seen.push(new URL(url).pathname);
		return new Response(
			'{"head":"abc","vaults":[],"canCreate":false,"scopes":["read","write"],"protectedVaults":[],"requiresStepUpAuth":[]}',
			{
			status: 200,
			headers: { "content-type": "application/json" },
			},
		);
	};
	return seen;
}

test("every call is qualified with the vault", async () => {
	const seen = capturing();
	const c = new Client("https://vault.example", "tok", "personal");
	await c.head();
	await c.snapshot();
	for (const p of seen) {
		assert.ok(p.startsWith("/personal/v1/"), `${p} is not vault-qualified`);
	}
});

test("a vault name with a space is percent-encoded", async () => {
	const seen = capturing();
	await new Client("https://vault.example", "tok", "My Own Vault").head();
	// URL.pathname PRESERVES percent-encoding -- it does not decode it.
	assert.equal(seen[0], "/My%20Own%20Vault/v1/head");
});

test("listVaults is a server-root route, reachable before a vault is chosen", async () => {
	const seen = capturing();
	await new Client("https://vault.example", "tok", "personal").listVaults();
	assert.equal(seen[0], "/v1/vaults");
});

test("listVaults requires current scoped-token metadata", async () => {
	globalThis.fetch = async () =>
		new Response('{"vaults":["personal"]}', {
			status: 200,
			headers: { "content-type": "application/json" },
		});
	await assert.rejects(
		() => new Client("https://vault.example", "tok", "").listVaults(),
		/current scoped-token metadata/,
	);
});

test("a trailing slash on the server URL does not double up", async () => {
	const seen = capturing();
	await new Client("https://vault.example/", "tok", "personal").head();
	assert.equal(seen[0], "/personal/v1/head");
});
