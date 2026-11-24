// Drives the plugin inside a real Obsidian against a real archivist-server.
//
// The other end-to-end tests run the sync engine under Node with a stand-in
// for Obsidian. This one exists for what they cannot see: the dialogs, the
// settings pane, the commands, and Obsidian's own local storage and file
// events. It covers encrypted vaults and encrypted files in an ordinary vault,
// which are the parts of the plugin where a wrong click costs the most.
//
// Run it through compose, which supplies Obsidian, the server and a display:
//
//   docker compose -f test/e2e/compose.yaml run --build --rm e2e
//
// The server's data root is read directly, because the property under test is
// what the SERVER ends up holding: no readable name and no readable content.

import { execFileSync, spawn } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { launch, makeVault, running, startDisplay } from "./obsidian.mjs";

const WORK = "/tmp/e2e";
const ROOT = path.join(WORK, "root");
const TOKENS = path.join(ROOT, "tokens.json");
const SERVER = "http://127.0.0.1:8090";
const ENVELOPE = "archivist-vault/v1\n";
const ARTIFACTS = process.env.ARTIFACTS ?? "/artifacts";

let failures = 0;
function check(name, ok, detail = "") {
	console.log(`${ok ? "  OK  " : "  FAIL"} ${name}${!ok && detail ? "  — " + detail : ""}`);
	if (!ok) failures++;
}

async function until(what, fn, ms = 30_000) {
	const end = Date.now() + ms;
	for (;;) {
		const v = await fn();
		if (v) return v;
		if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
		await new Promise((r) => setTimeout(r, 200));
	}
}

// ---------- the server ----------

function server(...args) {
	return execFileSync("archivist-server", args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
}

async function startServer() {
	await fs.mkdir(ROOT, { recursive: true });
	server("vault", "add", "-root", ROOT, "-encrypted", "work");
	server("vault", "add", "-root", ROOT, "-encrypted", "moved");
	server("vault", "add", "-root", ROOT, "old");
	server("vault", "add", "-root", ROOT, "personal");
	server("vault", "add", "-root", ROOT, "mixed");
	const out = spawnSyncBoth("archivist-server", ["token", "add", "-tokens", TOKENS, "-label", "e2e", "-vaults", "*", "-profile", "obsidian-plugin"]);
	const token = out.match(/arch_[A-Za-z0-9_-]+/)?.[0];
	if (!token) throw new Error(`no token minted: ${out}`);
	const log = await fs.open(path.join(ARTIFACTS, "server.log"), "w");
	const proc = spawn("archivist-server", ["-root", ROOT, "-tokens", TOKENS, "-listen", "127.0.0.1:8090"], {
		stdio: ["ignore", log.fd, log.fd],
	});
	await until("the server", () => fetch(`${SERVER}/healthz`).then((r) => r.ok, () => false));
	return { token, stop: () => proc.kill() };
}

function spawnSyncBoth(cmd, args) {
	try {
		return execFileSync(cmd, args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
	} catch (err) {
		return `${err.stdout ?? ""}${err.stderr ?? ""}`;
	}
}

const index = (vault, token) =>
	fetch(`${SERVER}/${vault}/v1`, { headers: { Authorization: `Bearer ${token}` } }).then((r) => r.json());

/** Every file the server holds for a vault, as paths relative to it. */
async function serverFiles(vault) {
	const dir = path.join(ROOT, "vaults", vault);
	const all = await fs.readdir(dir, { recursive: true, withFileTypes: true });
	return all
		.filter((e) => e.isFile())
		.map((e) => path.relative(dir, path.join(e.parentPath, e.name)))
		.filter((p) => p !== ".git" && !p.startsWith(".git/"));
}

/** Null when the vault holds only encrypted names and enveloped content,
 *  otherwise what is wrong with it. */
async function readable(vault) {
	const files = await serverFiles(vault);
	if (files.length === 0) return "the vault is empty";
	for (const f of files) {
		if (!/^[a-z2-7/]+$/.test(f)) return `readable name: ${f}`;
		const head = await fs.readFile(path.join(ROOT, "vaults", vault, f));
		if (!head.subarray(0, ENVELOPE.length).equals(Buffer.from(ENVELOPE))) return `no envelope: ${f}`;
	}
	return null;
}

/** Whether a string appears anywhere in the vault's history: any name or any
 *  content of any commit, not only what is on disk now. */
function inHistory(vault, text) {
	const dir = path.join(ROOT, "vaults", vault);
	const git = (...a) => spawnSyncBoth("git", ["-C", dir, ...a]);
	const revs = git("rev-list", "--all").trim().split("\n").filter(Boolean);
	if (revs.length === 0) return false;
	if (git("log", "--all", "--name-only", "--format=").includes(text)) return true;
	return git("grep", "-a", "-l", "-F", text, ...revs).trim() !== "";
}

/** The vault's latest commit, read from its repository. */
const head = (vault) => spawnSyncBoth("git", ["-C", path.join(ROOT, "vaults", vault), "rev-parse", "HEAD"]).trim();

// ---------- recovery keys, made here so wrong ones can be typed ----------

const B32 = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
function recoveryKeyFor(key) {
	const framed = Buffer.concat([key, crypto.createHash("sha256").update(key).digest().subarray(0, 2)]);
	let bits = "";
	for (const b of framed) bits += b.toString(2).padStart(8, "0");
	let out = "";
	for (let i = 0; i < bits.length; i += 5) out += B32[parseInt(bits.slice(i, i + 5).padEnd(5, "0"), 2)];
	return out.match(/.{1,5}/g).join("-");
}

// ---------- shared steps ----------

const settings = (vault, device) => ({
	serverUrl: SERVER,
	vault,
	device,
	// Nothing syncs unless the test says so, until the last scenario turns
	// the automatic paths back on to watch them work.
	intervalSeconds: 3600,
	syncOnChange: false,
	watchRemote: false,
	syncQuietSeconds: 1,
	revisionGapMinutes: 30,
});

const state = (d) => d.eval(() => globalThis.app.loadLocalStorage("archivist.state"));
/** Finds the open dialog, wherever Obsidian put it, and reads its title. */
const heading = async (d) => {
	await d.findModal();
	return d.modal().locator("h2").innerText({ timeout: 15_000 });
};
const button = (d, name) => d.modal().getByRole("button", { name, exact: true });
const clipboard = (d) =>
	d.eval(() => globalThis.require?.("electron")?.clipboard?.readText() ?? navigator.clipboard.readText());

const write = (d, p, text) =>
	d.eval(async ([p, text]) => {
		const { vault } = globalThis.app;
		const file = vault.getAbstractFileByPath(p);
		if (file) return vault.modify(file, text);
		const folder = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
		if (folder && !vault.getAbstractFileByPath(folder)) await vault.createFolder(folder);
		await vault.create(p, text);
	}, [p, text]);

/** Clicks through "Create key" to the syncing vault. Returns the recovery key.
 *  `done` says when the sync that follows has landed. */
async function createKey(d, vault, done) {
	await d.findModal();
	await button(d, "Create key").click();
	check("the recovery key is shown before anything is uploaded", (await heading(d)) === "Save this recovery key");
	const recovery = await d.modal().locator("textarea").inputValue();
	check("it is 11 groups of five characters", /^([A-Z2-7]{5}-){10}[A-Z2-7]{5}$/.test(recovery), recovery);
	const start = button(d, "Start syncing");
	check('"Start syncing" is disabled until the key is confirmed saved', await start.isDisabled());
	await d.shot("recovery-key");
	await d.modal().locator(".checkbox-container").click();
	check("and enabled once it is", await start.isEnabled());
	await start.click();
	await until(
		`${d.name} to sync with ${vault}`,
		done ??
			(async () => {
				const s = await state(d);
				return s?.vault === vault && s.base !== "";
			}),
	);
	return recovery;
}

// ---------- scenarios ----------

const SECRET_A = "SECRET-ALPHA the launch plan";
const SECRET_B = "SECRET-BRAVO salary notes";
const PIXEL = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==", "base64");

async function firstDevice(token) {
	console.log("\nfirst device: create the key");
	const dir = path.join(WORK, "a");
	await makeVault(dir, settings("work", "mac"), {
		"Plan.md": `${SECRET_A}\n`,
		"Projects/Ünïcode näme.md": `${SECRET_B}\n`,
		"Attachments/pixel.png": PIXEL,
	});
	const a = await launch("a", dir, token);

	check("the key dialog opens on its own", (await heading(a)) === 'Create the key for "work"');
	await a.shot("create-key");

	await button(a, "Not now").click();
	await until("the paused status", async () => (await a.status()).includes("paused"));
	check('"Not now" pauses the device and says so', (await a.notices()).some((n) => n.includes("not syncing this encrypted vault")));
	check("nothing was uploaded", (await serverFiles("work")).length === 0 && (await index("work", token)).keyCheck === "");

	const pane = await a.openSettings();
	await a.shot("settings-paused");
	const reopen = pane.getByRole("button", { name: "Create the key" });
	check("settings offer the key question again", await reopen.isVisible());
	await reopen.click();
	check("and reopen the dialog", (await heading(a)) === 'Create the key for "work"');

	await button(a, "Create key").click();
	await a.modal().locator("textarea").waitFor();
	check("still nothing uploaded while the recovery key is on screen", (await serverFiles("work")).length === 0 && (await index("work", token)).keyCheck === "");
	await button(a, "Copy").click();
	const shown = await a.modal().locator("textarea").inputValue();
	check('"Copy" puts the recovery key on the clipboard', (await clipboard(a).catch((e) => String(e))) === shown);
	await button(a, "Cancel").click();
	check("cancelling there stores no key", (await a.eval(() => globalThis.app.loadLocalStorage("archivist.vaultKeys"))) == null);

	await pane.getByRole("button", { name: "Create the key" }).click();
	const recovery = await createKey(a, "work");
	check("a cancelled key is not reused", recovery !== shown);

	check("the server now holds a key check", /^[0-9a-f]{64}$/.test((await index("work", token)).keyCheck));
	// The fourth is this device's plugin list, which syncs like a note.
	check("the server holds the three files and the plugin list, all encrypted", (await serverFiles("work")).length === 4 && (await readable("work")) === null, `${await readable("work")}: ${(await serverFiles("work")).join(", ")}`);
	for (const text of [SECRET_A, SECRET_B, "Plan", "Projects", "pixel"]) {
		check(`"${text}" appears nowhere in the vault's history`, !inHistory("work", text));
	}
	check("the notes on the device are untouched", (await a.read("Plan.md")) === `${SECRET_A}\n`);

	// Settings were open while the key arrived; reopen them to redraw.
	await a.closeSettings();
	const after = await a.openSettings();
	const row = after.locator(".setting-item", { hasText: "Recovery key" });
	await row.getByRole("button", { name: "Show" }).click();
	await a.shot("settings-recovery-key");
	check("settings show the same recovery key again", (await row.locator(".setting-item-description").innerText()).trim() === recovery);
	await a.closeSettings();
	return { a, recovery };
}

async function secondDevice(token, a, recovery) {
	console.log("\nsecond device: enter the key");
	const dir = path.join(WORK, "b");
	await makeVault(dir, settings("work", "laptop"));
	const b = await launch("b", dir, token);

	check("the second device is asked to enter the key, not create one", (await heading(b)) === 'Enter the key for "work"');
	const box = b.modal().locator("textarea");
	const problem = b.modal().locator("p.archivist-key-problem");
	const unlock = button(b, "Unlock");

	await box.fill("not a key at all");
	await unlock.click();
	await until("a refusal", async () => (await problem.innerText()) !== "");
	check("nonsense is refused", /recovery key/.test(await problem.innerText()), await problem.innerText());

	const typo = recovery.replace(/[A-Z]/, (c) => (c === "A" ? "B" : "A"));
	await box.fill(typo);
	await unlock.click();
	await until("a typo refusal", async () => (await problem.innerText()).includes("typo"));
	check("one wrong character is caught as a typo", true);

	await box.fill(recoveryKeyFor(crypto.randomBytes(32)));
	await unlock.click();
	await until("a wrong-key refusal", async () => (await problem.innerText()).includes("not the key for this vault"));
	check("a valid key for a different vault is refused", true);
	await b.shot("wrong-key");
	check("no key was stored and nothing was downloaded", (await b.eval(() => globalThis.app.loadLocalStorage("archivist.vaultKeys"))) == null && !(await exists(path.join(dir, "Plan.md"))));

	// Typed the way a person would: lowercase, spaces instead of dashes.
	await box.fill(recovery.toLowerCase().replaceAll("-", " "));
	await unlock.click();
	await until("the second device to sync", async () => (await state(b))?.base);
	check("the right key unlocks it and the notes arrive readable", (await b.read("Plan.md")) === `${SECRET_A}\n`);
	check("with their real names, accents included", (await b.read("Projects/Ünïcode näme.md")) === `${SECRET_B}\n`);
	check("and attachments byte for byte", (await fs.readFile(path.join(dir, "Attachments/pixel.png"))).equals(PIXEL));

	console.log("\ntwo devices: edit, rename, delete, conflict");
	await write(b, "Plan.md", `${SECRET_A}\nedited on the laptop\n`);
	await b.eval(() => globalThis.app.fileManager.renameFile(globalThis.app.vault.getAbstractFileByPath("Projects/Ünïcode näme.md"), "Projects/Renamed.md"));
	await b.eval(() => globalThis.app.vault.delete(globalThis.app.vault.getAbstractFileByPath("Attachments/pixel.png")));
	await write(b, "Inbox/New.md", "SECRET-CHARLIE a new note\n");
	await b.sync();
	await a.sync();
	check("an edit reaches the other device", (await a.read("Plan.md")) === `${SECRET_A}\nedited on the laptop\n`);
	check("a rename does", (await a.read("Projects/Renamed.md")) === `${SECRET_B}\n` && !(await exists(path.join(a.dir, "Projects/Ünïcode näme.md"))));
	check("a delete does", !(await exists(path.join(a.dir, "Attachments/pixel.png"))));
	check("a new note in a new folder does", (await a.read("Inbox/New.md")) === "SECRET-CHARLIE a new note\n");

	await write(a, "Plan.md", "SECRET-DELTA the mac's version\n");
	await write(b, "Plan.md", "SECRET-ECHO the laptop's version\n");
	await a.sync();
	await b.sync();
	const copies = (await fs.readdir(dir)).filter((f) => f !== "Plan.md" && f.startsWith("Plan"));
	check("two devices editing one note leaves a conflict copy on the second", copies.length === 1, (await fs.readdir(dir)).join(", "));
	const both = [await b.read("Plan.md"), copies[0] ? await b.read(copies[0]) : ""].sort();
	check("both versions survive, readable", both[0] === "SECRET-DELTA the mac's version\n" && both[1] === "SECRET-ECHO the laptop's version\n", JSON.stringify(both));
	check("and the device says so", (await b.notices()).some((n) => n.includes("conflict")), (await b.notices()).join(" | "));
	await b.shot("conflict");
	await b.sync();
	await a.sync();
	check("the conflict copy reaches the first device", copies[0] !== undefined && (await exists(path.join(a.dir, copies[0]))));

	check("after all of that the server still holds only ciphertext", (await readable("work")) === null, String(await readable("work")));
	for (const text of ["SECRET-", "Renamed", "Inbox", "conflict"]) {
		check(`"${text}" appears nowhere in the vault's history`, !inHistory("work", text));
	}
	return b;
}

async function restart(token, a) {
	console.log("\nrestart: the key survives");
	await a.close();
	a = await launch("a", a.dir);
	await a.sync();
	check("a restarted Obsidian syncs without asking for the key again", !(await a.modal().isVisible()) && /↓\d+ ↑\d+/.test(await a.status()), await a.status());
	return a;
}

async function automatic(a, b) {
	console.log("\nautomatic sync: no commands");
	for (const d of [a, b]) {
		await d.eval(async () => {
			const plugin = globalThis.app.plugins.plugins.archivist;
			plugin.settings.syncOnChange = true;
			plugin.settings.watchRemote = true;
			await plugin.saveSettings();
			await globalThis.app.plugins.disablePlugin("archivist");
			await globalThis.app.plugins.enablePlugin("archivist");
		});
	}
	await write(a, "Live.md", "SECRET-FOXTROT typed on the mac\n");
	const arrived = await until("the note on the other device", () => exists(path.join(b.dir, "Live.md")), 30_000).catch(() => false);
	check("a note typed on one device appears on the other by itself", arrived && (await b.read("Live.md")) === "SECRET-FOXTROT typed on the mac\n");
	check("and is ciphertext on the server", (await readable("work")) === null && !inHistory("work", "SECRET-FOXTROT"));
}

async function phone(token, recovery) {
	console.log("\nphone layout (Obsidian's mobile emulation, not a real phone)");
	const dir = path.join(WORK, "phone");
	await makeVault(dir, settings("work", "phone"));
	const p = await launch("phone", dir);
	await p.page.setViewportSize({ width: 390, height: 844 });
	await p.eval(() => globalThis.app.emulateMobile(true));
	await p.page.waitForFunction(() => globalThis.app?.isMobile === true && globalThis.app.workspace.layoutReady, null, { timeout: 60_000 });
	await p.page.waitForFunction(() => Boolean(globalThis.app.plugins.plugins.archivist));
	check("Obsidian is in its mobile layout and the plugin loaded there", await p.eval(() => globalThis.app.isMobile));
	await p.eval(async (t) => {
		globalThis.app.saveLocalStorage("archivist.token", t);
		await globalThis.app.plugins.disablePlugin("archivist");
		await globalThis.app.plugins.enablePlugin("archivist");
	}, token);

	check("the key dialog opens", (await heading(p)) === 'Enter the key for "work"');
	// A phone dialog is a sheet that slides up; measure it once it has landed.
	await new Promise((r) => setTimeout(r, 1000));
	await p.shot("enter-key");
	const fits = async (locator) => {
		const box = await locator.boundingBox();
		return box !== null && box.x >= 0 && box.y >= 0 && box.x + box.width <= 390 && box.y + box.height <= 844;
	};
	check("its field and both buttons fit a phone screen", (await fits(p.modal().locator("textarea"))) && (await fits(button(p, "Unlock"))) && (await fits(button(p, "Not now"))));
	await p.modal().locator("textarea").fill(recovery);
	await button(p, "Unlock").click();
	await until("the phone to sync", async () => (await state(p))?.base);
	check("the notes arrive readable", (await p.read("Inbox/New.md")) === "SECRET-CHARLIE a new note\n");

	const pane = await p.openSettings();
	const row = pane.locator(".setting-item", { hasText: "Recovery key" });
	await row.scrollIntoViewIfNeeded();
	await p.shot("settings");
	check("settings show the recovery key entry", await row.getByRole("button", { name: "Show" }).isVisible());
	await p.close();
}

async function switching(token) {
	console.log("\nswitch: move an existing vault to an encrypted one");
	const dir = path.join(WORK, "c");
	const notes = { "Journal.md": "SECRET-GOLF journal\n", "People/Anna.md": "SECRET-HOTEL anna\n", "Ideas.md": "SECRET-INDIA ideas\n" };
	await makeVault(dir, settings("old", "desk"), notes);
	const c = await launch("c", dir, token);
	await until("the first sync with the ordinary vault", async () => (await state(c))?.base);
	await c.sync();
	const plain = (await serverFiles("old")).filter((f) => !f.startsWith(".archivist/")).sort();
	check("the ordinary vault holds the notes as readable files", plain.join() === "Ideas.md,Journal.md,People/Anna.md", plain.join());
	// The control for every "appears nowhere" check: the same search does find
	// names and content in a vault that is not encrypted.
	check("and the history search finds them there", inHistory("old", "SECRET-GOLF") && inHistory("old", "Anna"));
	const oldHead = head("old");

	const ask = async (vault) => {
		await c.command("archivist:switch-vault");
		check("the switch dialog opens", (await heading(c)) === "Switch this folder to another vault");
		await c.modal().locator("input[type=text]").first().fill(vault);
		await button(c, "Check").click();
	};
	const problem = c.modal().locator("p.archivist-key-problem");

	await ask("work");
	await problem.waitFor();
	await c.shot("switch-refused");
	check("a vault that already has notes is refused", /empty|already/i.test(await problem.innerText()), await problem.innerText());
	check("with no way to push past it", !(await button(c, "Continue without syncing").isVisible()) && !(await button(c, "Switch").isVisible()));
	await button(c, "Cancel").click();

	await ask("moved");
	await until("the confirmation", async () => (await heading(c)) === "Confirm the switch");
	const lines = await c.modal().locator("li").allInnerTexts();
	await c.shot("switch-confirm");
	check("the confirmation names both vaults and their kinds", lines[0] === 'From "old" (not encrypted)' && lines[1] === 'To "moved" (encrypted, empty)', lines.join(" | "));
	await button(c, "Switch").click();
	check("then the key question for the new vault", (await heading(c)) === 'Create the key for "moved"');
	await button(c, "Not now").click();

	let pane = await c.openSettings();
	const back = pane.getByRole("button", { name: 'Go back to "old"' });
	await back.waitFor();
	await c.shot("settings-switch-unfinished");
	check("an unfinished switch can be undone from settings", true);
	await back.click();
	await until("the folder to be back on the old vault", async () => (await c.eval(() => globalThis.app.plugins.plugins.archivist.settings.vault)) === "old" && (await state(c))?.vault === "old");
	check("going back restores the old vault and its sync state", (await state(c)).base === oldHead && (await c.eval(() => globalThis.app.loadLocalStorage("archivist.switchBackup"))) == null, `base ${(await state(c)).base}, head was ${oldHead}`);
	check("and left both vaults as they were", head("old") === oldHead && (await serverFiles("moved")).length === 0);
	await c.closeSettings();
	await c.sync();
	check("the folder still syncs with the old vault", /↓0 ↑0/.test(await c.status()), await c.status());

	await ask("moved");
	await until("the confirmation", async () => (await heading(c)) === "Confirm the switch");
	await button(c, "Switch").click();
	await until("the key question", async () => (await heading(c)) === 'Create the key for "moved"');
	await createKey(c, "moved");
	const done = await until("the published notice", async () => (await c.notices()).find((n) => n.includes("published")));
	await c.shot("switch-done");
	check("the device reports what it published and that the old vault is unchanged", done.includes('published 4 file(s) to "moved"') && done.includes('"old" is unchanged'), done);
	check("the new vault holds the three files and the plugin list, all encrypted", (await serverFiles("moved")).length === 4 && (await readable("moved")) === null, String(await readable("moved")));
	check("none of it readable anywhere in the new vault's history", !["SECRET-", "Journal", "Anna", "People"].some((t) => inHistory("moved", t)));
	check("the old vault is untouched on the server", head("old") === oldHead);
	check("the notes on the device were not changed", (await Promise.all(Object.keys(notes).map((n) => c.read(n)))).join() === Object.values(notes).join());
	await c.sync();
	check("the switched folder then settles", /↓0 ↑0/.test(await c.status()), await c.status());

	console.log("\nswitch: from encrypted to ordinary needs the name typed");
	await ask("personal");
	await until("the confirmation", async () => (await heading(c)) === "Confirm the switch");
	await c.shot("switch-exposes");
	check("the dialog warns that notes will be uploaded readable", (await problem.innerText()).includes("as readable text"));
	const go = button(c, "Switch");
	check('"Switch" is disabled until the vault name is typed', await go.isDisabled());
	await c.modal().locator("input[type=text]").fill("persona");
	check("a near miss keeps it disabled", await go.isDisabled());
	await c.modal().locator("input[type=text]").fill("personal");
	check("the exact name enables it", await go.isEnabled());
	await button(c, "Back").click();
	await button(c, "Cancel").click();
	check("closing the dialog there changes nothing", (await c.eval(() => globalThis.app.plugins.plugins.archivist.settings.vault)) === "moved" && (await serverFiles("personal")).length === 0);
	await c.close();
}

/** An ordinary vault with a few files marked .encrypted. */
async function markedFiles(token) {
	console.log("\nencrypted files in an ordinary vault");
	const sealedOn = async (p) => {
		const bytes = await fs.readFile(path.join(ROOT, "vaults", "mixed", p)).catch(() => null);
		return bytes !== null && bytes.subarray(0, ENVELOPE.length).equals(Buffer.from(ENVELOPE));
	};
	const notes = async () => (await serverFiles("mixed")).filter((f) => !f.startsWith(".archivist/")).sort();

	const dir = path.join(WORK, "m1");
	await makeVault(dir, settings("mixed", "mac"), {
		"Plan.md": "the plan, readable\n",
		"Salary.encrypted.md": "SECRET-JULIET salary\n",
		"Clients.encrypted/Acme/Contract.md": "SECRET-KILO contract\n",
	});
	const m1 = await launch("m1", dir, token);

	check("the key dialog opens for the marked files", (await heading(m1)) === 'Create the key for "mixed"');
	check("and says the rest of the vault is unaffected", (await m1.modal().innerText()).includes("rest of the vault syncs as before"));
	await m1.shot("files-create-key");
	check("the ordinary note synced without waiting for the key", (await notes()).join() === "Plan.md", (await notes()).join());
	await button(m1, "Not now").click();
	await until("the notice", async () => (await m1.notices()).some((n) => n.includes("not synced on this device")));
	await m1.sync();
	check('"Not now" is remembered: the next sync does not ask again', !(await m1.modal().isVisible()) && /↓\d+ ↑\d+/.test(await m1.status()), await m1.status());
	check("and the marked files stay off the server", (await notes()).join() === "Plan.md");

	let pane = await m1.openSettings();
	await m1.shot("settings-files-no-key");
	check("settings explain the marker", await pane.locator(".setting-item", { hasText: "Files encrypted on the server" }).isVisible());
	const offer = pane.locator(".setting-item", { hasText: "Encrypted files are not syncing on this device" });
	check("and offer the key", await offer.isVisible());
	await offer.getByRole("button", { name: "Create the key" }).click();
	const recovery = await createKey(m1, "mixed", () => sealedOn("Salary.encrypted.md"));
	await m1.closeSettings();

	const held = await notes();
	check("the marked file keeps its name and is ciphertext on the server", held.includes("Salary.encrypted.md") && (await sealedOn("Salary.encrypted.md")));
	const under = held.filter((f) => f.startsWith("Clients.encrypted/"));
	check("the file in the marked folder has an encrypted name and is ciphertext", under.length === 1 && /^Clients\.encrypted\/[a-z2-7]+\/[a-z2-7]+$/.test(under[0]) && (await sealedOn(under[0])), held.join(", "));
	check("the ordinary note is still readable there", (await fs.readFile(path.join(ROOT, "vaults", "mixed", "Plan.md"), "utf8")) === "the plan, readable\n");
	for (const text of ["SECRET-", "Acme", "Contract"]) {
		check(`"${text}" appears nowhere in the vault's history`, !inHistory("mixed", text));
	}

	const dir2 = path.join(WORK, "m2");
	await makeVault(dir2, settings("mixed", "laptop"));
	const m2 = await launch("m2", dir2, token);
	check("a second device is asked to enter the key", (await heading(m2)) === 'Enter the key for "mixed"');
	check("having already pulled the ordinary note", (await m2.read("Plan.md")) === "the plan, readable\n" && !(await exists(path.join(dir2, "Salary.encrypted.md"))));
	await m2.modal().locator("textarea").fill(recovery);
	await button(m2, "Unlock").click();
	await until("the encrypted files on the second device", () => exists(path.join(dir2, "Clients.encrypted/Acme/Contract.md")));
	check("with the key the marked files arrive readable, under their real names", (await m2.read("Salary.encrypted.md")) === "SECRET-JULIET salary\n" && (await m2.read("Clients.encrypted/Acme/Contract.md")) === "SECRET-KILO contract\n");
	pane = await m2.openSettings();
	const row = pane.locator(".setting-item", { hasText: "Recovery key" });
	await m2.shot("settings-files-key");
	check("settings show the recovery key entry for the marked files", (await row.locator(".setting-item-description").innerText()).includes("marked .encrypted"));
	await m2.closeSettings();

	await write(m1, "Salary.encrypted.md", "SECRET-LIMA from the mac\n");
	await write(m2, "Salary.encrypted.md", "SECRET-MIKE from the laptop\n");
	await m1.sync();
	await m2.sync();
	const copies = (await fs.readdir(dir2)).filter((f) => f.includes("conflict"));
	check("a conflict copy of a marked note keeps the marker", copies.length === 1 && /^Salary\.conflict-.*\.encrypted\.md$/.test(copies[0]), (await fs.readdir(dir2)).join(", "));
	await m2.sync();
	check("so it reaches the server as ciphertext", copies[0] !== undefined && (await sealedOn(copies[0])));

	await m1.sync();
	await m1.eval(() => globalThis.app.fileManager.renameFile(globalThis.app.vault.getAbstractFileByPath("Plan.md"), "Plan.encrypted.md"));
	await m1.sync();
	check("renaming a note into the marker replaces the readable file with ciphertext", !(await notes()).includes("Plan.md") && (await sealedOn("Plan.encrypted.md")), (await notes()).join(", "));
	const warned = await until("the history notice", async () => (await m1.notices()).find((n) => n.includes("still readable in the server's history"))).catch(() => "");
	await m1.shot("files-marked");
	check("and says its earlier versions are still readable in history", warned.includes("Plan.encrypted.md"), warned);
	await m1.eval(() => globalThis.app.fileManager.renameFile(globalThis.app.vault.getAbstractFileByPath("Plan.encrypted.md"), "Plan.md"));
	await m1.sync();
	const said = await until("the exposure notice", async () => (await m1.notices()).find((n) => n.includes("uploaded as readable text"))).catch(() => "");
	await m1.shot("files-exposed");
	check("renaming it back out uploads it readable, and says so", said.includes("Plan.md") && (await fs.readFile(path.join(ROOT, "vaults", "mixed", "Plan.md"), "utf8")) === "the plan, readable\n", said);

	check("after all of that no secret is in the vault's history", !inHistory("mixed", "SECRET-") && !inHistory("mixed", "Acme"));
	await m1.close();
	await m2.close();
}

const exists = (p) => fs.access(p).then(() => true, () => false);

// ---------- a server older than the plugin ----------

const OLD_SERVER = "http://127.0.0.1:8091";
const OLD_ROOT = path.join(WORK, "old-root");

/** The released 0.20 server, which knows nothing of encryption. It has no
 *  `vault add`: a directory under vaults/ is a vault. */
async function startOldServer() {
	const tokens = path.join(OLD_ROOT, "tokens.json");
	await fs.mkdir(path.join(OLD_ROOT, "vaults", "personal"), { recursive: true });
	const out = spawnSyncBoth("archivist-server-old", ["token", "add", "-tokens", tokens, "-label", "e2e", "-vaults", "*", "-profile", "obsidian-plugin"]);
	const token = out.match(/arch_[A-Za-z0-9_-]+/)?.[0];
	if (!token) throw new Error(`no token minted by the old server: ${out}`);
	const log = await fs.open(path.join(ARTIFACTS, "old-server.log"), "w");
	const proc = spawn("archivist-server-old", ["-root", OLD_ROOT, "-tokens", tokens, "-listen", "127.0.0.1:8091"], {
		stdio: ["ignore", log.fd, log.fd],
	});
	await until("the old server", () => fetch(`${OLD_SERVER}/healthz`).then((r) => r.ok, () => false));
	const { version } = await fetch(`${OLD_SERVER}/healthz`).then((r) => r.json());
	return { token, version, stop: () => proc.kill() };
}

async function oldServer() {
	console.log("\na server older than the plugin");
	const old = await startOldServer();
	try {
		const held = async () => {
			const dir = path.join(OLD_ROOT, "vaults", "personal");
			const all = await fs.readdir(dir, { recursive: true, withFileTypes: true });
			return all
				.filter((e) => e.isFile())
				.map((e) => path.relative(dir, path.join(e.parentPath, e.name)))
				.filter((p) => !p.startsWith(".git") && !p.startsWith(".archivist"))
				.sort();
		};
		const dir = path.join(WORK, "o1");
		await makeVault(dir, { ...settings("personal", "mac"), serverUrl: OLD_SERVER }, {
			"Plan.md": "the plan\n",
			"Salary.encrypted.md": "SECRET-LIMA salary\n",
			"Clients.encrypted/Acme.md": "SECRET-MIKE contract\n",
		});
		const o1 = await launch("o1", dir, old.token);

		await until("the notice", async () => (await o1.notices()).some((n) => n.includes("too old to store them")));
		const notice = (await o1.notices()).find((n) => n.includes("too old to store them"));
		check("the notice names the server's version", notice.includes(old.version), notice);
		await o1.shot("old-server-notice");
		check("the vault synced", /↓\d+ ↑\d+/.test(await o1.status()), await o1.status());
		check("no key is offered, since that server could not record one", !(await o1.modal().isVisible()));
		check("the ordinary note reached the server and nothing marked did", (await held()).join() === "Plan.md", (await held()).join());

		const pane = await o1.openSettings();
		const row = pane.locator(".setting-item", { hasText: "Encrypted files are not syncing" });
		check("settings say why, with the version", (await row.isVisible()) && (await row.innerText()).includes(old.version));
		const versions = pane.locator(".archivist-status-row", { hasText: "Versions" });
		await until("the versions", async () => (await versions.innerText()).includes("server"));
		check("the status card shows both versions", (await versions.innerText()).includes(`server ${old.version}`), await versions.innerText());
		check("and offer no key there either", (await pane.getByRole("button", { name: /the key$/ }).count()) === 0);
		await o1.shot("settings-old-server");
		await o1.closeSettings();

		await fs.writeFile(path.join(dir, "Plan.md"), "the plan, edited\n");
		await o1.sync();
		check(
			"an ordinary note keeps syncing",
			(await fs.readFile(path.join(OLD_ROOT, "vaults", "personal", "Plan.md"), "utf8")) === "the plan, edited\n",
		);
		check("and the marked files are still only on the device", (await held()).join() === "Plan.md" && (await o1.read("Salary.encrypted.md")) === "SECRET-LIMA salary\n");
		await o1.close();
	} finally {
		old.stop();
	}
}

// ---------- run ----------

await fs.mkdir(ARTIFACTS, { recursive: true });
for (const f of await fs.readdir(ARTIFACTS)) if (/\.(png|log)$/.test(f)) await fs.rm(path.join(ARTIFACTS, f));
await fs.rm(WORK, { recursive: true, force: true });

const stopDisplay = startDisplay();
const srv = await startServer();
try {
	let { a, recovery } = await firstDevice(srv.token);
	const b = await secondDevice(srv.token, a, recovery);
	a = await restart(srv.token, a);
	await automatic(a, b);
	await phone(srv.token, recovery);
	await switching(srv.token);
	await markedFiles(srv.token);
	await oldServer();

	// Every request names the plugin and its version; the server logs each
	// client once.
	const { version } = JSON.parse(await fs.readFile("/plugin/manifest.json", "utf8"));
	const log = await fs.readFile(path.join(ARTIFACTS, "server.log"), "utf8");
	check("the server's log names the plugin and its version", log.includes(`client=plugin/${version}`), log.split("\n").filter((l) => l.includes("client")).join(" | "));
} catch (err) {
	failures++;
	console.log(`  FAIL stopped: ${err.stack ?? err}`);
	for (const d of running) await d.shotAll("failure");
} finally {
	for (const d of [...running]) await d.close().catch(() => {});
	srv.stop();
	stopDisplay();
}

console.log(failures === 0 ? "\nall checks passed" : `\n${failures} check(s) FAILED`);
process.exit(failures === 0 ? 0 : 1);
