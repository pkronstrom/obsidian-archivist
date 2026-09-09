import { Setting } from "obsidian";
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
	openStore: (url: string) => void = url => { window.open(url, "_blank", "noopener,noreferrer"); },
): void {
	container.empty();
	container.addClass("archivist-inventory");
	const peers = data.inventories.filter(i => i.installationId !== data.local.installationId);
	const labels = new Map<string,number>();
	for (const i of [data.local,...peers]) labels.set(i.deviceName,(labels.get(i.deviceName) ?? 0)+1);
	const label = (i: PluginInventory) => (labels.get(i.deviceName) ?? 0) > 1 ? `${i.deviceName} (${i.installationId.slice(0,8)})` : i.deviceName;
	const toolbar = new Setting(container).setName(`${data.local.plugins.length} installed`);
	toolbar.settingEl.addClass("archivist-inventory-toolbar");
	toolbar.addExtraButton(button => button.setIcon("refresh-cw").setTooltip("Refresh plugin list").onClick(onRefresh));
	if (peers.length) {
		toolbar.addDropdown(dropdown => {
			dropdown.addOption("", "All other devices");
			for (const peer of peers) dropdown.addOption(peer.installationId, label(peer));
			dropdown.setValue(selected).onChange(onSelect);
			dropdown.selectEl.setAttribute("aria-label", "Compare with device");
		});
	} else container.createDiv({text:"No other device has shared its plugin list yet.", cls:"archivist-inventory-note"});
	if (data.problem) container.createDiv({text:data.problem, cls:"archivist-inventory-note"});
	if (data.unavailable) container.createDiv({text:
		`${data.unavailable} device inventory record(s) unavailable. Missing or different plugins cannot be determined for those records.`,
		cls:"archivist-inventory-note",
	});
	const rows = compareInventories(data.local, peers, selected || undefined);
	// Put actionable differences first, preserving the stable name order within groups.
	rows.sort((a, b) => Number(b.status === "missing" || b.status === "different-version") - Number(a.status === "missing" || a.status === "different-version"));
	if (!rows.length) container.createDiv({text:"No installed plugins in these inventories.", cls:"archivist-inventory-note"});
	for (const row of rows) {
		const description: string[] = [];
		if (row.status === "missing") description.push("Missing on this device");
		if (row.status === "different-version") description.push("Different version");
		if (row.compatibility === "desktop-only") description.push("Desktop only");
		if (row.compatibility === "uncertain") description.push("Compatibility uncertain: device manifests disagree. Check the store listing.");
		for (const version of row.versions) description.push(`${version.version} — ${version.devices.map(label).join(", ")}`);
		const setting = new Setting(container).setName(row.name).setDesc(description.join("\n"));
		setting.settingEl.addClass("archivist-inventory-row");
		setting.nameEl.createEl("span", {text:row.localVersion || "Not installed", cls:"archivist-inventory-version"});
		setting.descEl.addClass("archivist-inventory-description");
		if (row.status === "missing" || row.status === "different-version") setting.descEl.addClass("archivist-inventory-difference");
		if (!(data.local.platform === "mobile" && row.compatibility === "desktop-only")) {
			setting.addExtraButton(button => button.setIcon("external-link")
				.setTooltip(`Open ${row.name} in Community Plugins (if listed)`)
				.onClick(() => openStore(communityPluginUrl(row.id))));
		}
	}
	if (peers.length) {
		const sources = container.createEl("details", {cls:"archivist-inventory-sources"});
		sources.createEl("summary", {text:`${peers.length + 1} devices`});
		sources.createDiv({text:`This device: ${data.local.deviceName} — scanned now`});
		for (const peer of peers) sources.createDiv({text:
			`${label(peer)} — Inventory updated ${new Date(peer.updatedAt).toLocaleString()}`,
		});
	}
	container.createDiv({text:"Settings stay local. Store versions may differ; private plugins may not be listed.", cls:"archivist-inventory-note"});
}
