import { Modal, Notice, Platform, requestUrl, Setting, type App } from "obsidian";

/**
 * Sync the plugin LIST, install the code from the store.
 *
 * Obsidian Sync syncs only the list and installs nothing, which is why
 * third-party plugin managers exist -- shipping the list alone produces a to-do
 * list, not a synced setup. Shipping the CODE is worse: a realistic 15-plugin
 * set is several MB, every plugin update rewrites main.js wholesale as a fresh
 * blob, and that is roughly 50 MB a year of churn recoverable only by prune.
 * Building reclaim and then introducing the largest available source of binary
 * churn would be perverse.
 *
 * So: read the synced list, diff against installed, skip isDesktopOnly on
 * mobile, fetch the rest from the community store, and enable. ALWAYS behind a
 * confirmation -- this downloads code from the internet at the direction of
 * another device, and someone with write access to the vault could add an id to
 * that list. On failure, say what is missing and let the user install it by
 * hand, which is no worse than Obsidian Sync.
 */

export type PluginManifest = { id: string; name?: string; isDesktopOnly?: boolean };

export type InstallPlan = {
	install: string[];
	skipped: { id: string; why: string }[];
	/**
	 * True when this is a mobile device and at least one planned id had no local
	 * manifest, so `isDesktopOnly` could not be checked yet.
	 *
	 * This is the normal case, not an edge one: a plugin about to be installed
	 * is by definition not installed, so `app.plugins.manifests` has no entry
	 * for it. The planning-time filter therefore only ever catches an already-
	 * installed-but-disabled plugin. The real check happens in `installPlugins`,
	 * against the manifest it downloads.
	 */
	deferredMobileCheck: boolean;
};

/** This plugin's current id. Never installed from a synced list. */
const ARCHIVIST_ID = "archivist";

/**
 * plannedInstalls diffs the synced list against what is installed here.
 *
 * Pure, so the decisions can be tested without a network or an Obsidian
 * runtime. An id with no known manifest is still attempted: whether it is
 * desktop-only is not knowable until it has been fetched, and mobile Obsidian
 * refuses to load a desktop-only plugin anyway, so the worst case is a plugin
 * that sits installed and disabled.
 */
export function plannedInstalls(
	wanted: string[],
	installed: string[],
	manifests: Record<string, PluginManifest>,
	isMobile: boolean,
): InstallPlan {
	const have = new Set(installed);
	const plan: InstallPlan = { install: [], skipped: [], deferredMobileCheck: false };
	for (const id of wanted) {
		if (id.toLowerCase() === ARCHIVIST_ID) continue;
		if (have.has(id)) continue;
		const manifest = manifests[id];
		if (isMobile && manifest?.isDesktopOnly) {
			plan.skipped.push({ id, why: "desktop-only; mobile Obsidian will not load it" });
			continue;
		}
		if (isMobile && !manifest) plan.deferredMobileCheck = true;
		plan.install.push(id);
	}
	return plan;
}

const REGISTRY =
	"https://raw.githubusercontent.com/obsidianmd/obsidian-releases/master/community-plugins.json";

type RegistryEntry = { id: string; repo: string };

/**
 * installPlugins fetches and writes each planned plugin, then enables it.
 *
 * Every failure is collected rather than thrown: one plugin whose release is
 * missing an artefact must not stop the rest, and the caller reports what did
 * not land so the user can install those by hand.
 */
export async function installPlugins(
	app: App,
	ids: string[],
	log: (msg: string, ...rest: unknown[]) => void,
): Promise<{
	installed: string[];
	failed: { id: string; err: string }[];
	skippedOnMobile: { id: string; why: string }[];
}> {
	const installed: string[] = [];
	const failed: { id: string; err: string }[] = [];
	const skippedOnMobile: { id: string; why: string }[] = [];

	let registry: RegistryEntry[] = [];
	try {
		registry = (await requestUrl({ url: REGISTRY })).json as RegistryEntry[];
	} catch (err) {
		return {
			installed,
			failed: ids.map((id) => ({ id, err: `community registry unreachable: ${err}` })),
			skippedOnMobile,
		};
	}
	const repoById = new Map(registry.map((e) => [e.id, e.repo]));

	for (const id of ids) {
		const repo = repoById.get(id);
		if (!repo) {
			failed.push({ id, err: "not in the community plugin registry" });
			continue;
		}
		try {
			const base = `https://github.com/${repo}/releases/latest/download`;
			const manifest = (await requestUrl({ url: `${base}/manifest.json` })).text;

			// THE mobile check. Planning could not make it: a plugin about to be
			// installed has no local manifest, so isDesktopOnly was unknown
			// until this download. Writing a desktop-only plugin onto a phone
			// leaves an entry mobile Obsidian refuses to load, which reads to
			// the user as a broken install rather than an unsupported plugin.
			if (Platform.isMobile) {
				try {
					if ((JSON.parse(manifest) as PluginManifest).isDesktopOnly) {
						skippedOnMobile.push({
							id,
							why: "desktop-only; mobile Obsidian will not load it",
						});
						continue;
					}
				} catch {
					// An unparseable manifest fails below on its own terms.
				}
			}

			const main = (await requestUrl({ url: `${base}/main.js` })).text;
			let styles: string | null = null;
			try {
				styles = (await requestUrl({ url: `${base}/styles.css` })).text;
			} catch {
				// Optional; most plugins have none.
			}

			const dir = `${app.vault.configDir}/plugins/${id}`;
			await app.vault.adapter.mkdir(dir);
			await app.vault.adapter.write(`${dir}/manifest.json`, manifest);
			await app.vault.adapter.write(`${dir}/main.js`, main);
			if (styles !== null) await app.vault.adapter.write(`${dir}/styles.css`, styles);

			// Undocumented, and the only way in: there is no public API for
			// installing or enabling a community plugin. Same calls BRAT makes,
			// in the same order.
			//
			// Obsidian's plugin manager only knows about plugins from its last
			// directory scan. Writing manifest.json straight to the adapter does
			// not refresh that cache, so calling enablePlugin immediately after
			// an install silently no-ops against a stale manifest list: the
			// files land on disk, but the plugin never turns on. loadManifests
			// forces the rescan first.
			const plugins = (
				app as unknown as {
					plugins?: {
						loadManifests?: () => Promise<void>;
						enablePlugin?: (id: string) => Promise<void>;
					};
				}
			).plugins;
			await plugins?.loadManifests?.();
			await plugins?.enablePlugin?.(id);

			installed.push(id);
			log(`installed ${id} from ${repo}`);
		} catch (err) {
			failed.push({ id, err: String(err) });
		}
	}
	return { installed, failed, skippedOnMobile };
}

/** Whether this device is mobile. Wrapped so the caller need not import Platform. */
export function isMobile(): boolean {
	return Platform.isMobile;
}

/** Report the outcome, including what the user has to do by hand. */
export function reportInstalls(
	result: {
		installed: string[];
		failed: { id: string; err: string }[];
		skippedOnMobile?: { id: string; why: string }[];
	},
	skipped: { id: string; why: string }[],
): void {
	const lines: string[] = [];
	// Both sets of skips: the ones planning could see, and the ones only the
	// downloaded manifest revealed.
	const allSkipped = [...skipped, ...(result.skippedOnMobile ?? [])];
	if (result.installed.length > 0) lines.push(`installed ${result.installed.join(", ")}`);
	if (allSkipped.length > 0) lines.push(`skipped ${allSkipped.map((s) => s.id).join(", ")} (desktop-only)`);
	if (result.failed.length > 0) {
		lines.push(`install by hand: ${result.failed.map((f) => f.id).join(", ")}`);
	}
	if (lines.length > 0) new Notice(`archivist: ${lines.join("; ")}`, 12000);
}


/**
 * The confirmation. Names every plugin about to be downloaded, because that is
 * the information the decision needs: an id nobody recognises in that list is
 * the signal that something is wrong.
 */
export class PluginInstallModal extends Modal {
	constructor(
		app: App,
		private readonly plan: InstallPlan,
		private readonly onConfirm: () => Promise<void>,
	) {
		super(app);
	}

	onOpen(): void {
		const { contentEl } = this;
		contentEl.empty();
		contentEl.createEl("h2", { text: "Install plugins from another device's list?" });
		contentEl.createEl("p", {
			text:
				"These plugin ids arrived in the synced list and are not installed here. " +
				"Installing downloads code from the community store. Only continue if you " +
				"recognise all of them.",
		});
		const list = contentEl.createEl("ul");
		for (const id of this.plan.install) list.createEl("li", { text: id });

		if (this.plan.skipped.length > 0) {
			contentEl.createEl("p", {
				text: `Skipped on this device: ${this.plan.skipped.map((s) => `${s.id} (${s.why})`).join(", ")}`,
			});
		}

		new Setting(contentEl)
			.addButton((b) => b.setButtonText("Not now").onClick(() => this.close()))
			.addButton((b) =>
				b
					.setCta()
					.setButtonText(`Install ${this.plan.install.length}`)
					.onClick(async () => {
						this.close();
						await this.onConfirm();
					}),
			);
	}

	onClose(): void {
		this.contentEl.empty();
	}
}
