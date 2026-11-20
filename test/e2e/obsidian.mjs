// Starts a real Obsidian on a vault folder and hands back a Playwright page
// attached to its window.
//
// Each device gets its own HOME, so its own Obsidian profile: its own local
// storage (where the plugin keeps the token, the sync state and the vault
// key) and its own single-instance lock. Two devices are two processes that
// share nothing but the server.

import { spawn } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { chromium } from "playwright-core";

const OBSIDIAN = process.env.OBSIDIAN_BIN ?? "/opt/obsidian/obsidian";
const PLUGIN = process.env.PLUGIN_DIR ?? "/plugin";
const ARTIFACTS = process.env.ARTIFACTS ?? "/artifacts";

/** Every Obsidian currently up, so a failed run can photograph and stop them. */
export const running = new Set();

/** A plugin dialog, as opposed to the settings pane, which is also a modal. */
const DIALOG = ".modal-container .modal:not(.mod-settings)";

let nextPort = 9222;
let shots = 0;

/** The virtual display every Obsidian draws on. */
export function startDisplay() {
	const xvfb = spawn("Xvfb", [":99", "-screen", "0", "1400x900x24", "-nolisten", "tcp"], { stdio: "ignore" });
	process.env.DISPLAY = ":99";
	return () => xvfb.kill();
}

/**
 * Lays down a vault folder with the plugin installed and its settings seeded.
 * The token is not a setting -- it lives in local storage -- so launch() puts
 * it there once the app is up.
 */
export async function makeVault(dir, settings, notes = {}) {
	const plugin = path.join(dir, ".obsidian", "plugins", "archivist");
	await fs.mkdir(plugin, { recursive: true });
	for (const f of ["main.js", "manifest.json", "styles.css"]) {
		await fs.copyFile(path.join(PLUGIN, f), path.join(plugin, f));
	}
	await fs.writeFile(path.join(dir, ".obsidian", "community-plugins.json"), JSON.stringify(["archivist"]));
	await fs.writeFile(path.join(plugin, "data.json"), JSON.stringify(settings));
	for (const [name, content] of Object.entries(notes)) {
		await fs.mkdir(path.dirname(path.join(dir, name)), { recursive: true });
		await fs.writeFile(path.join(dir, name), content);
	}
}

export async function launch(name, vaultDir, token) {
	const home = path.join(path.dirname(vaultDir), `home-${name}`);
	const config = path.join(home, ".config", "obsidian");
	await fs.mkdir(config, { recursive: true });
	await fs.writeFile(
		path.join(config, "obsidian.json"),
		JSON.stringify({ vaults: { [`e2e${name}`]: { path: vaultDir, ts: Date.now(), open: true } } }),
	);

	const port = nextPort++;
	const log = await fs.open(path.join(ARTIFACTS, `obsidian-${name}.log`), "a");
	const proc = spawn(
		OBSIDIAN,
		["--no-sandbox", "--disable-gpu", `--remote-debugging-port=${port}`, "--remote-allow-origins=*"],
		{
			env: { ...process.env, HOME: home, XDG_CONFIG_HOME: path.join(home, ".config") },
			stdio: ["ignore", log.fd, log.fd],
		},
	);

	const browser = await retry(() => chromium.connectOverCDP(`http://127.0.0.1:${port}`), 60);
	const page = await retry(async () => {
		const found = browser.contexts().flatMap((c) => c.pages()).find((p) => p.url().startsWith("app://obsidian.md"));
		if (!found) throw new Error(`no Obsidian window for ${name}`);
		return found;
	}, 60);
	await page.waitForFunction(() => globalThis.app?.workspace?.layoutReady === true, null, { timeout: 60_000 });

	// A vault that arrives with plugins already in it opens in restricted
	// mode and asks first.
	const trust = page.getByRole("button", { name: /Trust author/ });
	if (await trust.isVisible({ timeout: 5000 }).catch(() => false)) {
		await trust.click();
		await page.waitForFunction(() => Boolean(globalThis.app.plugins.plugins.archivist), null, { timeout: 30_000 });
		await page.keyboard.press("Escape");
	}
	await page.waitForFunction(() => Boolean(globalThis.app.plugins.plugins.archivist), null, { timeout: 30_000 });

	const windows = () => browser.contexts().flatMap((c) => c.pages()).filter((p) => !p.isClosed());
	const device = {
		name,
		page,
		dir: vaultDir,
		/** Runs in the Obsidian window. */
		eval: (fn, arg) => page.evaluate(fn, arg),
		command: (id) => page.evaluate((id) => globalThis.app.commands.executeCommandById(id), id),
		/** One full sync cycle, awaited. */
		sync: () => page.evaluate(() => globalThis.app.plugins.plugins.archivist.forceSync()),
		status: () => page.evaluate(() => document.querySelector(".archivist-status-item")?.getAttribute("aria-label") ?? ""),
		/**
		 * The dialog that is open. Obsidian puts a dialog over whichever of its
		 * windows is in front, which is the settings window when a settings
		 * button opened it, so findModal() looks in all of them first.
		 */
		modal: () => (device.modalPage ?? page).locator(DIALOG).last(),
		async findModal() {
			device.modalPage = await retry(async () => {
				for (const p of windows()) {
					if (await p.locator(DIALOG).count().catch(() => 0)) return p;
				}
				throw new Error(`no dialog open on ${name}`);
			}, 30);
		},
		async notices() {
			const all = await Promise.all(windows().map((p) => p.locator(".notice").allInnerTexts().catch(() => [])));
			return all.flat();
		},
		/**
		 * Opens the plugin's settings and returns the pane. Since Obsidian 1.15
		 * settings are a window of their own, so the pane is looked for in every
		 * window rather than assumed to be in the main one.
		 */
		async openSettings() {
			await page.evaluate(() => {
				globalThis.app.setting.open();
				globalThis.app.setting.openTabById("archivist");
			});
			const win = await retry(async () => {
				for (const p of windows()) {
					if (await p.locator(".vertical-tab-content").count().catch(() => 0)) return p;
				}
				throw new Error(`no settings pane for ${name}`);
			}, 20);
			device.settingsPage = win;
			return win.locator(".vertical-tab-content");
		},
		closeSettings: () => page.evaluate(() => globalThis.app.setting.close()),
		/** Every window, for when something went wrong and it is not clear where. */
		async shotAll(label) {
			let n = 0;
			for (const p of windows()) {
				await p.screenshot({ path: path.join(ARTIFACTS, `${label}-${name}-${n++}.png`) }).catch(() => {});
			}
		},
		async shot(label) {
			const file = path.join(ARTIFACTS, `${String(++shots).padStart(2, "0")}-${name}-${label}.png`);
			// Whichever window the last thing looked at was in.
			const settings = label.startsWith("settings") ? device.settingsPage : undefined;
			await (settings ?? device.modalPage ?? page).screenshot({ path: file });
		},
		read: (p) => fs.readFile(path.join(vaultDir, p), "utf8"),
		async close() {
			running.delete(device);
			await browser.close().catch(() => {});
			proc.kill();
			await new Promise((r) => proc.once("exit", r));
			await log.close();
		},
	};

	running.add(device);
	if (token) {
		await page.evaluate((t) => globalThis.app.saveLocalStorage("archivist.token", t), token);
		// Reload the plugin so it starts the way it does on a configured device.
		await page.evaluate(async () => {
			await globalThis.app.plugins.disablePlugin("archivist");
			await globalThis.app.plugins.enablePlugin("archivist");
		});
	}
	return device;
}

async function retry(fn, tries) {
	let last;
	for (let i = 0; i < tries; i++) {
		try {
			return await fn();
		} catch (err) {
			last = err;
			await new Promise((r) => setTimeout(r, 500));
		}
	}
	throw last;
}
