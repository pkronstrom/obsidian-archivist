// Drives the real sync engine against a real archivist-server server, with two
// independent "devices" pointed at one server.
//
// Usage:
//   node test/integration.mjs <serverUrl> <token>

import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { FakeApp } from "./obsidian-shim.mjs";
import { Sync, Client } from "../dist-test/entry.mjs";

const [, , SERVER, TOKEN, VAULT_ARG] = process.argv;
if (!SERVER || !TOKEN) {
	console.error("usage: node test/integration.mjs <serverUrl> <token> [vault]");
	process.exit(2);
}
// Addressing is path-qualified now, so every client needs a vault name.
const VAULT = VAULT_ARG ?? "personal";

let failures = 0;
function check(name, ok, detail = "") {
	console.log(`${ok ? "  OK  " : "  FAIL"} ${name}${detail ? "  — " + detail : ""}`);
	if (!ok) failures++;
}

async function device(name) {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), `vs-${name}-`));
	const app = new FakeApp(root);
	const sync = new Sync(app, () => new Client(SERVER, TOKEN, VAULT), () => name, () => {});
	return { name, root, app, sync, read: (p) => fs.readFile(path.join(root, p), "utf8") };
}

const mac = await device("mac");
const phone = await device("phone");
console.log(`mac:   ${mac.root}\nphone: ${phone.root}\n`);

// --- 1. create on one device, appear on the other ---------------------------
await fs.mkdir(path.join(mac.root, "notes"), { recursive: true });
await fs.writeFile(path.join(mac.root, "notes/a.md"), "one\ntwo\nthree\n");
let r = await mac.sync.run();
check("mac pushes a new note", r.pushed === 1, JSON.stringify(r));

r = await phone.sync.run();
check("phone pulls it", r.pulled >= 1, JSON.stringify(r));
check("content matches", (await phone.read("notes/a.md")) === "one\ntwo\nthree\n");

// --- 2. binary attachment ---------------------------------------------------
const blob = Buffer.from(Array.from({ length: 4096 }, (_, i) => i % 251));
await fs.mkdir(path.join(mac.root, "att"), { recursive: true });
await fs.writeFile(path.join(mac.root, "att/img.bin"), blob);
await mac.sync.run();
await phone.sync.run();
const got = await fs.readFile(path.join(phone.root, "att/img.bin"));
check("binary attachment round-trips byte-identically", Buffer.compare(blob, got) === 0);

// --- 3. disjoint edits merge ------------------------------------------------
await fs.writeFile(path.join(mac.root, "notes/a.md"), "ONE\ntwo\nthree\n");
await mac.sync.run();
// The phone edits a different line, from its older base, without pulling first.
await fs.writeFile(path.join(phone.root, "notes/a.md"), "one\ntwo\nTHREE\n");
r = await phone.sync.run();
check("disjoint edits do not conflict", r.conflicts.length === 0, JSON.stringify(r.conflicts));
await mac.sync.run();
const merged = await mac.read("notes/a.md");
check("both edits survive the merge", merged === "ONE\ntwo\nTHREE\n", JSON.stringify(merged));

// --- 4. same-line edits conflict, and keep both -----------------------------
await fs.writeFile(path.join(mac.root, "notes/c.md"), "base\n");
await mac.sync.run();
await phone.sync.run();
await fs.writeFile(path.join(mac.root, "notes/c.md"), "from mac\n");
await mac.sync.run();
await fs.writeFile(path.join(phone.root, "notes/c.md"), "from phone\n");
r = await phone.sync.run();
check("same-line edit raises a conflict", r.conflicts.length === 1, JSON.stringify(r.conflicts));
if (r.conflicts.length === 1) {
	await phone.sync.run();
	const cp = r.conflicts[0].conflictPath;
	const side = await phone.read(cp).catch(() => "");
	// The conflict file holds the marked-up three-way merge inside a code
	// fence, not the raw losing version -- that changed when fenceConflict
	// shipped, and this assertion had been stale ever since. What it must
	// guarantee is that BOTH sides are recoverable from the file, and that it
	// renders as literal text rather than as markdown headings and blockquotes.
	check("the losing version is preserved", side.includes("from phone"), JSON.stringify(side));
	check("and the server's version is in there too", side.includes("from mac"), JSON.stringify(side));
	check("and the markers are fenced, so they render literally",
		side.includes("```text"), JSON.stringify(side));
	check("the server version wins the real path", (await phone.read("notes/c.md")) === "from mac\n");
}

// --- 5. deletion propagates -------------------------------------------------
await fs.rm(path.join(mac.root, "notes/a.md"));
await mac.sync.run();
await phone.sync.run();
check("deletion propagates", !(await phone.app.vault.adapter.exists("notes/a.md")));

// --- 6. offline edits, then reconnect ---------------------------------------
// No syncing at all while "offline", then one cycle. The diff is computed from
// the snapshot, so the length of the outage is irrelevant.
await fs.writeFile(path.join(phone.root, "notes/offline1.md"), "written offline\n");
await fs.writeFile(path.join(phone.root, "notes/offline2.md"), "also offline\n");
await fs.mkdir(path.join(phone.root, "notes/deep/deeper"), { recursive: true });
await fs.writeFile(path.join(phone.root, "notes/deep/deeper/x.md"), "nested offline\n");
r = await phone.sync.run();
check("offline edits are all pushed on reconnect", r.pushed === 3, JSON.stringify(r));
await mac.sync.run();
check("mac receives the nested offline file",
	(await mac.read("notes/deep/deeper/x.md")) === "nested offline\n");

// --- 7. a fresh device never deletes ----------------------------------------
// This device is populated and has never synced, and the server has content by
// now -- the pairing hazard. It is the OLD behaviour that is under test here,
// so resolve it as "merge anyway", which is what that behaviour now is.
const fresh = await device("fresh");
await fs.writeFile(path.join(fresh.root, "local-only.md"), "created before first sync\n");

let freshRefused = null;
try {
	await fresh.sync.run();
} catch (err) {
	freshRefused = err;
}
check("a populated first-run device refuses rather than unioning",
	freshRefused?.name === "PairingHazardError", String(freshRefused));

r = await fresh.sync.resolvePairing("merge");
await mac.sync.run();
check("a first-run device adopts server state", await fresh.app.vault.adapter.exists("notes/c.md"));
check("and keeps its own local-only file", await fresh.app.vault.adapter.exists("local-only.md"));
const stillThere = await mac.app.vault.adapter.exists("notes/offline1.md");
check("and deletes nothing on the server", stillThere);

// --- 8. an unchanged vault does no work -------------------------------------
r = await mac.sync.run();
check("a no-op sync pushes nothing", r.pushed === 0, JSON.stringify(r));

// --- 9. a merged push must converge on the merged bytes ---------------------
// Found by review: the plugin recorded its OWN hash for a "merged" result, so
// it believed it was current while holding pre-merge content, with its cursor
// already past the merge commit. Permanently diverged, silently.
await fs.writeFile(path.join(mac.root, "notes/m.md"), "one\ntwo\nthree\n");
await mac.sync.run();
await phone.sync.run();
await fs.writeFile(path.join(mac.root, "notes/m.md"), "ONE\ntwo\nthree\n");
await mac.sync.run();
await fs.writeFile(path.join(phone.root, "notes/m.md"), "one\ntwo\nTHREE\n");
r = await phone.sync.run();
check("merging push reports no conflict", r.conflicts.length === 0, JSON.stringify(r.conflicts));
check("the merging device ends up with the MERGED content, not its own",
  (await phone.read("notes/m.md")) === "ONE\ntwo\nTHREE\n",
  JSON.stringify(await phone.read("notes/m.md")));
r = await phone.sync.run();
check("and a further sync finds nothing left to do", r.pushed === 0 && r.pulled === 0, JSON.stringify(r));

// --- 10. a local delete must survive a concurrent remote edit ---------------
// Found by review: pull wrote the remote version onto the deleted path, so the
// deletion was never sent and vanished without trace.
await fs.writeFile(path.join(mac.root, "notes/d.md"), "original\n");
await mac.sync.run();
await phone.sync.run();
await fs.rm(path.join(phone.root, "notes/d.md"));            // deleted here
await fs.writeFile(path.join(mac.root, "notes/d.md"), "edited elsewhere\n");
await mac.sync.run();                                         // modified there
r = await phone.sync.run();
const resurrected = await phone.app.vault.adapter.exists("notes/d.md");
const serverStill = await mac.app.vault.adapter.exists("notes/d.md");
check("a local delete racing a remote edit is not silently discarded",
  !resurrected || serverStill,
  `resurrected=${resurrected} serverStill=${serverStill}`);
check("and the remote edit is not lost either", serverStill);

// --- 11. dotfiles never enter the synced set --------------------------------
await fs.mkdir(path.join(mac.root, ".obsidian"), { recursive: true });
await fs.writeFile(path.join(mac.root, ".obsidian/appearance.json"), "{}");
await mac.sync.run();
await phone.sync.run();
check("`.obsidian` does not reach the other device",
  !(await phone.app.vault.adapter.exists(".obsidian/appearance.json")));

// --- 12. re-bootstrap must not overwrite the server's version ---------------
// Found by review: rebootstrap left a locally-different path out of the
// snapshot, so the next diff pushed it as a new put against the adopted head
// and the server took our bytes over its own without merging.
{
  const reboot = await device("reboot");
  await fs.writeFile(path.join(reboot.root, "notes/c.md"), "LOCAL divergent\n").catch(async () => {
    await fs.mkdir(path.join(reboot.root, "notes"), { recursive: true });
    await fs.writeFile(path.join(reboot.root, "notes/c.md"), "LOCAL divergent\n");
  });
  // A cursor the server has never heard of forces the 409 path.
  reboot.app.saveLocalStorage("archivist.state", {
    base: "0123456789012345678901234567890123456789",
    files: { "notes/c.md": { hash: "deadbeef", mtime: 1, size: 1 } },
  });
  const serverBefore = await mac.read("notes/c.md");
  r = await reboot.sync.run();
  check("re-bootstrap happened", r.rebootstrapped, JSON.stringify(r));
  await mac.sync.run();
  const serverAfter = await mac.read("notes/c.md");
  check("the server version survived a re-bootstrap",
    serverAfter === serverBefore, `before=${JSON.stringify(serverBefore)} after=${JSON.stringify(serverAfter)}`);
  const files = await fs.readdir(path.join(reboot.root, "notes"));
  check("and the local divergent copy was kept aside",
    files.some((f) => f.includes("conflict")), files.join(", "));
  // Found by review: the aside copy was recorded in state.files with its own
  // hash, so the next diff saw it as unchanged and never uploaded it. Rescued
  // content stayed on the one device that rescued it.
  const asideName = "notes/" + files.find((f) => f.includes("conflict-reboot"));
  await reboot.sync.run();
  await mac.sync.run();
  const asideOnMac = await mac.app.vault.adapter.exists(asideName);
  check("the rescued copy reached the other device", asideOnMac, asideName);
}

// A .local file must survive a re-bootstrap untouched. The settings pane
// promises exactly this, and the promise is the whole reason the namespace is
// safe to use: a scratch note you keep on one device must not vanish because
// you repaired sync state. Re-bootstrap adopts the server's snapshot, and a
// .local path is by construction absent from it -- so the danger is not that
// the server deletes it, but that a local pass treats "not in the snapshot" as
// "should not exist here".
{
  const keep = await device("keeper");
  await keep.sync.run();
  await fs.writeFile(path.join(keep.root, "Scratch.local.md"), "never leaves this device\n");
  await fs.mkdir(path.join(keep.root, "Journal.local"), { recursive: true });
  await fs.writeFile(path.join(keep.root, "Journal.local/entry.md"), "nor does this\n");

  // Force the re-bootstrap path with a cursor the server never issued.
  keep.app.saveLocalStorage("archivist.state", {
    base: "0123456789012345678901234567890123456789",
    files: {},
  });
  const r2 = await keep.sync.run();
  check("re-bootstrap happened (local-only survival)", r2.rebootstrapped, JSON.stringify(r2));

  check("a .local file survived re-bootstrap",
    await keep.app.vault.adapter.exists("Scratch.local.md"));
  check("a .local folder's contents survived re-bootstrap",
    await keep.app.vault.adapter.exists("Journal.local/entry.md"));
  check("the .local file still holds its content",
    (await fs.readFile(path.join(keep.root, "Scratch.local.md"), "utf8")) === "never leaves this device\n");

  // And it never reached the server, which is the other half of the promise.
  await mac.sync.run();
  check("the .local file was never uploaded",
    !(await mac.app.vault.adapter.exists("Scratch.local.md")));
  check("the .local folder was never uploaded",
    !(await mac.app.vault.adapter.exists("Journal.local/entry.md")));
}

// --- pairing safety ---------------------------------------------------------
// By this point the server has content, which is exactly the hazard: a fresh
// device with its own notes must refuse rather than union the two.

const stranger = await device("stranger");
await fs.mkdir(path.join(stranger.root, "notes"), { recursive: true });
await fs.writeFile(path.join(stranger.root, "notes/a.md"), "a totally different a.md\n");

let refused = null;
try {
	await stranger.sync.run();
} catch (err) {
	refused = err;
}
check("a populated device refuses a populated server",
	refused !== null && refused.name === "PairingHazardError", String(refused));
check("nothing was pulled before refusing",
	!(await stranger.app.vault.adapter.exists("att/img.bin")));
check("the local file is untouched",
	(await stranger.read("notes/a.md")) === "a totally different a.md\n");

// Adopt server: local files move aside, the server's arrive.
await stranger.sync.resolvePairing("adopt");
const strangerEntries = await fs.readdir(stranger.root);
const rescue = strangerEntries.find((e) => e.startsWith("_archivist-rescued-"));
check("adopt created a rescue folder", Boolean(rescue), JSON.stringify(strangerEntries));
check("the rescued copy survived",
	(await stranger.read(`${rescue}/notes/a.md`)) === "a totally different a.md\n");
// The stranger's own notes/a.md is gone from the real path -- that is what
// makes room for the server's tree. The server has no notes/a.md of its own
// here, so the path is simply absent rather than replaced; what matters is
// that the server's content did arrive.
check("the stranger's version no longer sits at the real path",
	!(await stranger.app.vault.adapter.exists("notes/a.md")));
check("and the server's tree arrived",
	await stranger.app.vault.adapter.exists("att/img.bin"));

// And the rescue folder reaches the other devices, because it is in the vault.
await mac.sync.run();
check("the rescue folder syncs to other devices",
	(await mac.read(`${rescue}/notes/a.md`)) === "a totally different a.md\n");

// Publish local: a second fresh device whose content wins every collision.
const publisher = await device("publisher");
await fs.mkdir(path.join(publisher.root, "notes"), { recursive: true });
await fs.writeFile(path.join(publisher.root, "notes/a.md"), "published wins\n");

let publisherRefused = null;
try {
	await publisher.sync.run();
} catch (err) {
	publisherRefused = err;
}
check("the second device also refuses", publisherRefused?.name === "PairingHazardError");

await publisher.sync.resolvePairing("publish");
await mac.sync.run();
check("publish local overwrote the collision without a conflict file",
	(await mac.read("notes/a.md")) === "published wins\n");

const macNotes = await fs.readdir(path.join(mac.root, "notes"));
check("publish local created no conflict file",
	!macNotes.some((f) => f.includes(".conflict-") && f.startsWith("a.")),
	JSON.stringify(macNotes));

// --- config sync ------------------------------------------------------------

async function configDevice(name, level, acceptedPlugins = []) {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), `cfg-${name}-`));
	const app = new FakeApp(root);
	const config = { level, acceptedPlugins };
	const sync = new Sync(app, () => new Client(SERVER, TOKEN, VAULT), () => name, () => {}, () => config);
	return { name, root, app, sync, config, read: (p) => fs.readFile(path.join(root, p), "utf8") };
}

const deskA = await configDevice("deskA", "appearance");
const deskB = await configDevice("deskB", "appearance");

await fs.mkdir(path.join(deskA.root, ".obsidian/snippets"), { recursive: true });
await fs.writeFile(path.join(deskA.root, ".obsidian/snippets/dark.css"), "body { color: red }\n");
await fs.writeFile(path.join(deskA.root, ".obsidian/appearance.json"), '{\n  "theme": "minimal"\n}\n');
await fs.writeFile(path.join(deskA.root, ".obsidian/workspace.json"), '{\n  "left": {}\n}\n');

await deskA.sync.run();
await deskB.sync.run();

check("a snippet reaches the other device",
	(await deskB.read(".obsidian/snippets/dark.css")) === "body { color: red }\n");
check("appearance.json reaches the other device",
	JSON.parse(await deskB.read(".obsidian/appearance.json")).theme === "minimal");
check("workspace.json does NOT travel",
	!(await deskB.app.vault.adapter.exists(".obsidian/workspace.json")));

// A device on "files only" gets none of it.
const phoneFilesOnly = await configDevice("phoneFilesOnly", "files");
await phoneFilesOnly.sync.run();
check("a files-only device receives no config",
	!(await phoneFilesOnly.app.vault.adapter.exists(".obsidian/appearance.json")));

// Two devices editing different keys of one settings file both keep their edit.
await fs.writeFile(path.join(deskA.root, ".obsidian/appearance.json"),
	'{\n  "theme": "minimal",\n  "baseFontSize": 18\n}\n');
await deskA.sync.run();
await fs.writeFile(path.join(deskB.root, ".obsidian/appearance.json"),
	'{\n  "theme": "minimal",\n  "accentColor": "#fff"\n}\n');
await deskB.sync.run();
await deskA.sync.run();
const finalAppearance = JSON.parse(await deskA.read(".obsidian/appearance.json"));
check("the settings file is still valid JSON after a merge",
	typeof finalAppearance === "object" && finalAppearance !== null);
check("both devices' disjoint key edits survived",
	finalAppearance.baseFontSize === 18 && finalAppearance.accentColor === "#fff",
	JSON.stringify(finalAppearance));

// Archivist's own data.json is refused even if a device tries to push it.
const sneaky = await configDevice("sneaky", "plugins", ["archivist"]);
await fs.mkdir(path.join(sneaky.root, ".obsidian/plugins/archivist"), { recursive: true });
await fs.writeFile(path.join(sneaky.root, ".obsidian/plugins/archivist/data.json"),
	'{"token":"THE-BEARER-TOKEN"}\n');
await sneaky.sync.run();
await deskA.sync.run();
check("archivist's own data.json never travels",
	!(await deskA.app.vault.adapter.exists(".obsidian/plugins/archivist/data.json")));

// --- multi-vault -----------------------------------------------------------
// Needs a server started with two vaults and a tokens file. Skipped otherwise.

const SECOND_VAULT = process.env.ARCHIVIST_TEST_VAULT_2;
const SECOND_TOKEN = process.env.ARCHIVIST_TEST_TOKEN_2;

if (SECOND_VAULT && SECOND_TOKEN) {
	const otherRoot = await fs.mkdtemp(path.join(os.tmpdir(), "vs-other-"));
	const otherApp = new FakeApp(otherRoot);
	const other = {
		root: otherRoot,
		app: otherApp,
		sync: new Sync(
			otherApp,
			() => new Client(SERVER, SECOND_TOKEN, SECOND_VAULT),
			() => "other",
			() => {},
		),
		read: (p) => fs.readFile(path.join(otherRoot, p), "utf8"),
	};

	await fs.writeFile(path.join(other.root, "only-here.md"), "second vault\n");
	await other.sync.run();

	await mac.sync.run();
	check("a note in the second vault does not appear in the first",
		!(await mac.app.vault.adapter.exists("only-here.md")));

	// A token scoped to one vault must not reach another.
	let forbidden = null;
	try {
		await new Client(SERVER, SECOND_TOKEN, VAULT).head();
	} catch (err) {
		forbidden = err;
	}
	check("a token scoped to one vault cannot open another",
		forbidden !== null && forbidden.status === 403, String(forbidden));

	// The picker reports only what the token opens.
	const listed = await new Client(SERVER, SECOND_TOKEN, "").listVaults();
	check("list_vaults returns only the scoped vault",
		listed.vaults.length === 1 && listed.vaults[0] === SECOND_VAULT,
		JSON.stringify(listed));
} else {
	console.log("  SKIP multi-vault (set ARCHIVIST_TEST_VAULT_2 and ARCHIVIST_TEST_TOKEN_2)");
}

console.log(`\n${failures === 0 ? "ALL PASS" : failures + " FAILURES"}`);
process.exit(failures === 0 ? 0 : 1);
