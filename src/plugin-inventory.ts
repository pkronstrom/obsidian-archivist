import type { App, DataAdapter } from "obsidian";

export const INVENTORY_DIR = ".archivist/plugin-inventory";
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const ID = /^[A-Za-z0-9][A-Za-z0-9_-]{0,199}$/;
const MAX_BYTES = 1_048_576;
const MAX_PLUGINS = 2000;
const ID_KEY = "archivist.installation-id";
export type InventoryPlugin = { id: string; name: string; version: string; isDesktopOnly: boolean };
export type PluginInventory = {
	schemaVersion: 1;
	installationId: string;
	deviceName: string;
	platform: "desktop" | "mobile";
	updatedAt: string;
	plugins: InventoryPlugin[];
};
export function inventoryPath(id: string): string { return `${INVENTORY_DIR}/${id}.json`; }
export function inventoryPathAllowed(path: string): boolean {
	if (!path.startsWith(INVENTORY_DIR + "/") || !path.endsWith(".json")) return false;
	return UUID.test(path.slice(INVENTORY_DIR.length + 1, -5));
}
export function inventoryDirectory(path: string): boolean {
	return path === ".archivist" || path === INVENTORY_DIR;
}
export function installationId(app: App): string {
	const old: unknown = app.loadLocalStorage(ID_KEY);
	if (typeof old === "string" && UUID.test(old)) return old;
	const id = crypto.randomUUID();
	app.saveLocalStorage(ID_KEY, id);
	return id;
}
function object(value: unknown): value is Record<string, unknown> {
	return value !== null && typeof value === "object" && !Array.isArray(value);
}
function shortText(value: unknown, max: number): value is string {
	return typeof value === "string" && value.trim().length > 0 && value.length <= max && !/[\u0000-\u001f\u007f]/.test(value);
}
function projectPlugin(value: unknown): InventoryPlugin | null {
	if (!object(value) || typeof value.id !== "string" || !ID.test(value.id) ||
		!shortText(value.name, 200) || !shortText(value.version, 100) || typeof value.isDesktopOnly !== "boolean") return null;
	return {id:value.id, name:value.name, version:value.version, isDesktopOnly:value.isDesktopOnly};
}
/** Remote input is untrusted. Return only the documented fields. */
export function parseInventory(path: string, text: string): PluginInventory | null {
	if (!inventoryPathAllowed(path) || text.length > MAX_BYTES || new TextEncoder().encode(text).byteLength > MAX_BYTES) return null;
	try {
		const value: unknown = JSON.parse(text);
		if (!object(value) || value.schemaVersion !== 1 || typeof value.installationId !== "string" ||
			inventoryPath(value.installationId) !== path || !shortText(value.deviceName, 200) ||
			(value.platform !== "desktop" && value.platform !== "mobile") ||
			typeof value.updatedAt !== "string" || value.updatedAt.length > 30 ||
			!/^\d{4}-\d{2}-\d{2}T/.test(value.updatedAt) || !Number.isFinite(Date.parse(value.updatedAt)) ||
			!Array.isArray(value.plugins) || value.plugins.length > MAX_PLUGINS) return null;
		const plugins: InventoryPlugin[] = [];
		const ids = new Set<string>();
		for (const raw of value.plugins) {
			const p = projectPlugin(raw);
			if (!p || ids.has(p.id)) return null;
			ids.add(p.id); plugins.push(p);
		}
		plugins.sort((a,b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
		return {schemaVersion:1, installationId:value.installationId, deviceName:value.deviceName,
			platform:value.platform, updatedAt:value.updatedAt, plugins};
	} catch { return null; }
}
async function readBounded(adapter: DataAdapter, path: string): Promise<string> {
	const stat = await adapter.stat(path);
	if (!stat || stat.type !== "file" || stat.size > MAX_BYTES) throw new Error("Plugin inventory file is unavailable or too large");
	const text = await adapter.read(path);
	if (text.length > MAX_BYTES) throw new Error("Plugin inventory file is too large");
	return text;
}
/** Scan disk, including disabled plugins; never read data.json or plugin code. */
export async function collectInventory(app: App, deviceName: string, platform: PluginInventory["platform"]): Promise<PluginInventory> {
	const adapter = app.vault.adapter;
	const dir = `${app.vault.configDir}/plugins`;
	const plugins: InventoryPlugin[] = [];
	let step = `listing ${dir}`;
	try {
		if (await adapter.exists(dir)) {
			const {folders} = await adapter.list(dir);
			if (folders.length > MAX_PLUGINS) throw new Error("too many plugins");
			for (const folder of folders.sort()) {
				const manifest = `${folder}/manifest.json`;
				step = `checking ${manifest}`;
				// Obsidian ignores directories without manifests. Legacy config
				// sync can leave data.json here without installing the plugin.
				if (!(await adapter.exists(manifest))) continue;
				step = `reading ${manifest}`;
				const text = await readBounded(adapter, manifest);
				step = `parsing ${manifest}`;
				let raw: unknown;
				try { raw = JSON.parse(text); } catch { throw new Error("Invalid JSON"); }
				step = `validating ${manifest}`;
				// Older manifests may omit the optional desktop-only flag.
				const p = projectPlugin(object(raw) ? {...raw, isDesktopOnly:raw.isDesktopOnly ?? false} : raw);
				if (!p || plugins.some(other => other.id === p.id)) throw new Error("invalid manifest");
				plugins.push(p);
			}
		}
		plugins.sort((a,b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
		step = "creating the local installation ID";
		const id = installationId(app);
		step = "validating the device name and inventory";
		const result: PluginInventory = {schemaVersion:1, installationId:id,
			deviceName:deviceName.trim() || "Device", platform, updatedAt:new Date().toISOString(), plugins};
		if (!parseInventory(inventoryPath(result.installationId), JSON.stringify(result))) throw new Error("invalid inventory");
		return result;
	} catch (error) {
		const reason = error instanceof Error ? `: ${error.message.slice(0, 180)}` : "";
		throw new Error(`Plugin inventory scan incomplete while ${step}${reason}. The previous list has been kept.`);
	}
}
/** Called inside the sync cycle, after pulling and before scanning local changes. */
export async function publishInventory(app: App, deviceName: string, platform: PluginInventory["platform"]): Promise<PluginInventory> {
	const adapter = app.vault.adapter;
	const id = installationId(app);
	const path = inventoryPath(id);
	const temp = `${INVENTORY_DIR}/.archivist-tmp-${id}`;
	const backup = `${temp}.previous`;
	// Obsidian refuses rename over an existing destination on desktop and mobile.
	// Recover a process interrupted between the two renames before scanning:
	// even an incomplete scan must leave the previous publication available.
	if (await adapter.exists(backup)) {
		if (!(await adapter.exists(path))) await adapter.rename(backup, path);
		else await adapter.remove(backup);
	}
	const next = await collectInventory(app, deviceName, platform);
	const serialized = JSON.stringify(next, null, 2) + "\n";
	if (new TextEncoder().encode(serialized).byteLength > MAX_BYTES) {
		throw new Error("Plugin inventory is too large; the previous list has been kept.");
	}
	let previous: PluginInventory | null = null;
	if (await adapter.exists(path)) {
		try { previous = parseInventory(path, await readBounded(adapter, path)); } catch { /* Replace a broken local record. */ }
	}
	if (previous && JSON.stringify({...previous, updatedAt:""}) === JSON.stringify({...next, updatedAt:""})) return previous;
	for (const dir of [".archivist", INVENTORY_DIR]) {
		if (!(await adapter.exists(dir))) await adapter.mkdir(dir);
	}
	await adapter.write(temp, serialized);
	if (await adapter.exists(path)) await adapter.rename(path, backup);
	try {
		await adapter.rename(temp, path);
	} catch (error) {
		if (await adapter.exists(backup) && !(await adapter.exists(path))) await adapter.rename(backup, path);
		throw error;
	}
	if (await adapter.exists(backup)) await adapter.remove(backup);
	return next;
}
export async function readInventories(adapter: DataAdapter): Promise<{inventories: PluginInventory[]; unavailable: number}> {
	const inventories: PluginInventory[] = [];
	let unavailable = 0;
	if (!(await adapter.exists(INVENTORY_DIR))) return {inventories, unavailable};
	const {files} = await adapter.list(INVENTORY_DIR);
	for (const path of files) {
		if (!inventoryPathAllowed(path)) continue;
		try {
			const inventory = parseInventory(path, await readBounded(adapter,path));
			if (inventory) inventories.push(inventory); else unavailable++;
		} catch { unavailable++; }
	}
	inventories.sort((a,b) => a.installationId.localeCompare(b.installationId));
	return {inventories, unavailable};
}
export type InventoryRow = {
	id: string; name: string; localVersion?: string;
	status: "installed" | "missing" | "different-version" | "desktop-only";
	compatibility: "compatible" | "desktop-only" | "uncertain";
	versions: {version:string; devices:PluginInventory[]}[];
};
export function compareInventories(local: PluginInventory, inventories: PluginInventory[], selected?: string): InventoryRow[] {
	const peers = inventories.filter(i => i.installationId !== local.installationId && (!selected || i.installationId === selected));
	const localPlugins = new Map(local.plugins.map(p => [p.id,p]));
	const all = new Map(localPlugins);
	for (const peer of peers) for (const p of peer.plugins) if (!all.has(p.id)) all.set(p.id,p);
	return [...all.values()].sort((a,b) => a.id.localeCompare(b.id)).map(p => {
		const here = localPlugins.get(p.id);
		const versions = new Map<string,PluginInventory[]>();
		const compatibility = new Set<boolean>(here ? [here.isDesktopOnly] : []);
		for (const peer of peers) {
			const installed = peer.plugins.find(other => other.id === p.id);
			if (!installed) continue;
			compatibility.add(installed.isDesktopOnly);
			const devices = versions.get(installed.version) ?? [];
			devices.push(peer); versions.set(installed.version,devices);
		}
		const compatible = compatibility.size > 1 ? "uncertain" : compatibility.has(true) ? "desktop-only" : "compatible";
		return {id:p.id, name:p.name, localVersion:here?.version, compatibility:compatible,
			status: !here ? (local.platform === "mobile" && compatible === "desktop-only" ? "desktop-only" : "missing") :
				[...versions.keys()].some(v => v !== here.version) ? "different-version" : "installed",
			versions:[...versions].map(([version,devices]) => ({version,devices}))};
	});
}
