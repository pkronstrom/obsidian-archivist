// Drives the real sync engine against a real vaultsync server, with two
// independent "devices" pointed at one server.
//
// Usage:
//   node test/integration.mjs <serverUrl> <token>

import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { FakeApp } from "./obsidian-shim.mjs";
import { Sync } from "../dist-test/sync.mjs";
import { Client } from "../dist-test/client.mjs";

const [, , SERVER, TOKEN] = process.argv;
if (!SERVER || !TOKEN) {
	console.error("usage: node test/integration.mjs <serverUrl> <token>");
	process.exit(2);
}

let failures = 0;
function check(name, ok, detail = "") {
	console.log(`${ok ? "  OK  " : "  FAIL"} ${name}${detail ? "  — " + detail : ""}`);
	if (!ok) failures++;
}

async function device(name) {
	const root = await fs.mkdtemp(path.join(os.tmpdir(), `vs-${name}-`));
	const app = new FakeApp(root);
	const sync = new Sync(app, () => new Client(SERVER, TOKEN), () => name, () => {});
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
	check("the losing version is preserved", side === "from phone\n", JSON.stringify(side));
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
const fresh = await device("fresh");
await fs.writeFile(path.join(fresh.root, "local-only.md"), "created before first sync\n");
r = await fresh.sync.run();
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

console.log(`\n${failures === 0 ? "ALL PASS" : failures + " FAILURES"}`);
process.exit(failures === 0 ? 0 : 1);
