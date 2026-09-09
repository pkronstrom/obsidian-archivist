import { compareInventories, type PluginInventory } from "./plugin-inventory";

export type InventoryViewData = {
	local: PluginInventory;
	inventories: PluginInventory[];
	unavailable: number;
	problem?: string;
};
/** Official directory fallback: its listing offers Add to Obsidian. */
export function communityPluginUrl(id: string): string {
	return `https://community.obsidian.md/plugins/${encodeURIComponent(id)}`;
}
export function renderInventoryView(
	container: HTMLElement, data: InventoryViewData, selected: string,
	onSelect: (id: string) => void, onRefresh: () => void,
): void {
	container.empty();
	container.createEl("p", {text:"Share installed plugin names and versions across your devices. Plugin settings, installation, updates and enablement stay on each device."});
	const toolbar = container.createDiv({cls:"archivist-inventory-toolbar"});
	toolbar.createEl("button", {text:"Refresh plugin list"}).addEventListener("click",onRefresh);
	const peers = data.inventories.filter(i => i.installationId !== data.local.installationId);
	const labels = new Map<string,number>();
	for (const i of [data.local,...peers]) labels.set(i.deviceName,(labels.get(i.deviceName) ?? 0)+1);
	const label = (i: PluginInventory) => (labels.get(i.deviceName) ?? 0) > 1 ? `${i.deviceName} (${i.installationId.slice(0,8)})` : i.deviceName;
	if (peers.length) {
		const filterLabel = toolbar.createEl("label",{text:"Compare with "});
		const filter = filterLabel.createEl("select",{attr:{"aria-label":"Compare with device"}});
		filter.createEl("option",{text:"All other devices",value:""});
		for (const peer of peers) filter.createEl("option",{text:label(peer),value:peer.installationId});
		filter.value=selected;
		filter.addEventListener("change",()=>onSelect(filter.value));
	} else container.createEl("p",{text:"No other device has shared its plugin list yet."});
	if (data.problem) container.createEl("p",{text:data.problem,cls:"archivist-inventory-warning"});
	if (data.unavailable) container.createEl("p",{text:`${data.unavailable} device inventory record(s) unavailable. Missing or different plugins cannot be determined for those records.`});
	const sources=container.createEl("details",{cls:"archivist-inventory-sources"});
	sources.createEl("summary",{text:"Device inventories"});
	sources.createEl("p",{text:`This device: ${data.local.deviceName} — scanned now`});
	for (const peer of peers) sources.createEl("p",{text:`${label(peer)} — Inventory updated ${new Date(peer.updatedAt).toLocaleString()}`});
	const rows=compareInventories(data.local,peers,selected || undefined);
	if (!rows.length) container.createEl("p",{text:"No installed plugins in these inventories."});
	for (const row of rows) {
		const el=container.createDiv({cls:"archivist-inventory-plugin"});
		el.createEl("h4",{text:row.name});
		if (row.name !== row.id) el.createEl("p",{text:row.id,cls:"archivist-inventory-meta"});
		el.createEl("p",{text:row.localVersion ? `This device: ${row.localVersion}` : "This device: not installed"});
		if (row.status === "missing") el.createEl("p",{text:"Missing on this device"});
		if (row.status === "different-version") el.createEl("p",{text:"Different version"});
		if (row.compatibility === "desktop-only") el.createEl("p",{text:"Desktop only"});
		if (row.compatibility === "uncertain") el.createEl("p",{text:"Compatibility uncertain: device manifests disagree. Check the store listing."});
		for (const version of row.versions) el.createEl("p",{text:`${version.version} — ${version.devices.map(label).join(", ")}`,cls:"archivist-inventory-meta"});
		if (!(data.local.platform === "mobile" && row.compatibility === "desktop-only")) {
			el.createEl("a",{text:"Open in Community Plugins",href:communityPluginUrl(row.id),cls:"archivist-inventory-store",attr:{target:"_blank",rel:"noopener noreferrer"}});
		}
	}
	container.createEl("p",{text:"The store may offer a different version. Private and unlisted plugins may not have a Community Plugins listing.",cls:"archivist-inventory-meta"});
}
