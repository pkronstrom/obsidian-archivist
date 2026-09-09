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
	new Setting(container)
		.setName("Installed plugins")
		.setDesc("Compare plugin names and versions. Settings and plugin management stay on each device.")
		.addButton(button => button.setButtonText("Refresh").onClick(onRefresh));
	const peers = data.inventories.filter(i => i.installationId !== data.local.installationId);
	const labels = new Map<string,number>();
	for (const i of [data.local,...peers]) labels.set(i.deviceName,(labels.get(i.deviceName) ?? 0)+1);
	const label = (i: PluginInventory) => (labels.get(i.deviceName) ?? 0) > 1 ? `${i.deviceName} (${i.installationId.slice(0,8)})` : i.deviceName;
	if (peers.length) {
		new Setting(container).setName("Compare with").addDropdown(dropdown => {
			dropdown.addOption("", "All other devices");
			for (const peer of peers) dropdown.addOption(peer.installationId, label(peer));
			dropdown.setValue(selected).onChange(onSelect);
		});
	} else new Setting(container).setDesc("No other device has shared its plugin list yet.");
	if (data.problem) new Setting(container).setDesc(data.problem);
	if (data.unavailable) new Setting(container).setDesc(
		`${data.unavailable} device inventory record(s) unavailable. Missing or different plugins cannot be determined for those records.`,
	);
	const deviceRow = new Setting(container).setName("Device inventories").setDesc(
		`${peers.length + 1} device${peers.length ? "s" : ""}`,
	);
	const sources = container.createDiv();
	sources.hidden = true;
	deviceRow.addButton(button => button.setButtonText("Show").onClick(() => {
		sources.hidden = !sources.hidden;
		button.setButtonText(sources.hidden ? "Show" : "Hide");
	}));
	new Setting(sources).setName(`This device: ${data.local.deviceName}`).setDesc("Scanned now");
	for (const peer of peers) new Setting(sources).setName(label(peer)).setDesc(
		`Inventory updated ${new Date(peer.updatedAt).toLocaleString()}`,
	);
	const rows = compareInventories(data.local, peers, selected || undefined);
	if (!rows.length) new Setting(container).setDesc("No installed plugins in these inventories.");
	for (const row of rows) {
		const description = [row.localVersion ? `This device: ${row.localVersion}` : "This device: not installed"];
		if (row.status === "missing") description.push("Missing on this device");
		if (row.status === "different-version") description.push("Different version");
		if (row.compatibility === "desktop-only") description.push("Desktop only");
		if (row.compatibility === "uncertain") description.push("Compatibility uncertain: device manifests disagree. Check the store listing.");
		for (const version of row.versions) description.push(`${version.version} — ${version.devices.map(label).join(", ")}`);
		const setting = new Setting(container).setName(row.name).setDesc(description.join("\n"));
		setting.descEl.addClass("archivist-inventory-description");
		if (!(data.local.platform === "mobile" && row.compatibility === "desktop-only")) {
			setting.addButton(button => button.setButtonText("Community Plugins")
				.onClick(() => openStore(communityPluginUrl(row.id))));
		}
	}
	new Setting(container).setDesc("The store may offer a different version. Private and unlisted plugins may not have a Community Plugins listing.");
}
