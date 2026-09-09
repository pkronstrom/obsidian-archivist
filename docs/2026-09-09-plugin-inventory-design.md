# Plugin inventory instead of config sync

Approved direction from the 2026-09-08–09 conversation. Supersedes the snapshot
proposal in `2026-09-08-config-sync-convergence-design-note.md`; that document's
diagnosis remains useful. Included in release 0.17.0. Physical Mac/iPhone
acceptance remains unverified. The official directory fallback is used for store
navigation rather than relying on an unverified direct URI.

## User need

“Tell me whether my iPhone is missing a plugin I installed on my Mac, show the
versions on my devices, and let me open the plugin's store page.” Support any
number of Obsidian clients; two clients are common, not a data-model limit.

## Scope

- Stop continuous sync of all `.obsidian/**` files on client and server.
- Share a small inventory of installed community plugins from each client.
- Compare presence and exact version strings across clients.
- Offer an Open in Community Plugins button for compatible listed plugins.
- Keep installation, updates, enablement and all settings local. No automatic
  installation, code transfer, settings snapshots, or plugin uninstall actions.

## Inventory and transport

Use one JSON file per installation under
`.archivist/plugin-inventory/<installation-id>.json`, transported by ordinary
file sync. Add only this narrow hidden-file exception to both client and server,
including directory traversal, watcher handling and repository staging.

Generate a random installation UUID in vault-scoped local storage. Never use the
editable device label as a key. Separate profiles with separate local storage
receive separate IDs; renaming a device changes only its display label. A copied
vault with fresh local storage gets a fresh ID. Restoring the local storage too
can duplicate an identity; this is not an authenticated device identity scheme.
The inventory is informational and confers no authority over another client.

Schema version 1 contains only:

```ts
type PluginInventory = {
  schemaVersion: 1;
  installationId: string;
  deviceName: string;
  platform: "desktop" | "mobile";
  updatedAt: string;
  plugins: Array<{
    id: string;
    name: string;
    version: string;
    isDesktopOnly: boolean;
  }>;
};
```

Read installed plugin manifests from the actual configured Obsidian directory,
including installed-but-disabled plugins. Copy only the fields above, never
settings, credentials, arbitrary manifest fields or source paths. Archivist can
appear in the inventory but does not acquire an automatic updater.

Sort by plugin ID and write only when inventory content, label or platform has
changed. Do not update the timestamp on every sync. Label it “Inventory updated,”
not “Last online.” Scan at startup after layout readiness and before scheduled
syncs; refresh local comparisons when the inventory section opens. A failed or
incomplete scan must preserve the previous inventory rather than advertise an
empty installation. Serialize publication with the existing sync lifecycle.

Use existing adapter-safe parent creation and temporary-file conventions to
avoid publishing partially written JSON. Parse incoming inventory defensively;
validate schema version, UUID/path agreement and bounded field/list sizes. Treat
malformed, unsupported and conflict-copy files as unavailable inventories, not
missing-plugin evidence. Render names as text. Never derive filesystem targets
or arbitrary URLs from incoming names.

## Comparison screen

Replace “Obsidian config” controls with “Plugins on your devices.” State that
only plugin names and versions are shared; settings stay on each device.

- Local installation is “This device.”
- Default view includes plugins present locally or on any reporting device.
- Missing means absent locally and present on at least one other device.
- An installed-but-disabled plugin is present.
- Different version means unequal version strings. Do not call either version
  newer without a separate ordering policy; do not enforce version equality.
- Group other devices by version within each plugin row. This keeps a phone
  readable when there are more than two clients. Provide a comparison-device
  filter rather than assuming exactly one Mac and one phone.
- Show each source inventory's update time and distinguish duplicate labels with
  a short ID. Retain offline-device records; do not silently expire them.
- Desktop-only plugins have a separate informational state on mobile. Conflicting
  compatibility reports are shown as uncertain and leave final compatibility
  decisions to the store.
- No remote inventory yet means “No other device has shared its plugin list,”
  not “All plugins match.”

The store action opens the selected plugin's listing; Obsidian handles Install,
Update and Enable. Verify `obsidian://show-plugin?id=<encoded-id>` on desktop and
iPhone before relying on it. The official community directory URL is the fallback.
For unlisted/private/BRAT plugins, indicate that they may not be available in the
community store; do not invent repository URLs or promise installation. A store
release need not equal another device's installed version.

## Migration

Client exclusion applies even when old local storage says appearance/plugins or
accept-all. Forget legacy config tracking without emitting deletions. Pull and
rebootstrap must also exclude live config. Preserve local files and server history.

The server refuses old clients' puts, deletes and moves involving live config;
notes still sync. Update traversal and watcher behavior so local server config
edits cannot resume the loop. Do not remove old files, rewrite history, or import
legacy config into inventories. Deploy server and clients together; an old server
will refuse the inventory namespace and must not be represented as successfully
sharing it. Existing generic refusal reporting must make that visible.

This retires the defective config paths rather than implementing scoped config
re-enablement or a settings-recovery namespace. Existing preserved conflict files
remain untouched.

## Acceptance evidence

1. Three installations, including duplicate labels: each inventory survives and
   the phone reports the correct missing plugins and versions from both peers.
2. Repeated sync cycles with unchanged inventories produce no further inventory
   uploads or commits, including timestamp-only commits.
3. Installing, updating or uninstalling on one client changes its inventory only;
   no other client's plugin files, settings or enablement change.
4. Desktop-only, disabled, unlisted and differing-version cases render correctly.
5. Invalid inventory data does not prevent note sync or trigger any local action.
6. Old config-enabled state and an old-client server push cannot write, delete or
   rebootstrap live config. Notes continue to sync and old history survives.
7. Test full server commit and second-client download of the narrow namespace.
8. Verify the store action and usable multi-device layout on Mac and iPhone.

References: [Obsidian installation flow](https://obsidian.md/help/community-plugins)
and [community directory](https://docs.obsidian.md/community-directory).

## Implementation evidence — 2026-09-09

Implementation verified before the 0.17.0 release.

- `npm run test:unit`: 129 tests passed.
- `npm run build`: passed.
- `go test ./...`: passed, including local HTTP/listener tests.
- `npm run test:integration -- <temporary-server> <temporary-token> personal`:
  passed against a fresh local Go server with two vaults and three inventory
  clients. Duplicate labels, version changes, uninstalls, malformed inventory,
  note sync and repeated idle cycles were exercised.
- Independent server and client review completed. Regression coverage includes
  legacy live API filtering, ancestor removal events, inventory size limits and
  Obsidian's refusal to rename over existing destinations. Publication uses an
  excluded backup for rollback and interrupted-operation recovery.
- Physical Mac/iPhone layout, store navigation and installation acceptance have
  **not** been run. The official Community Plugins web listing is the implemented
  fallback; no direct `show-plugin` URI dependency was introduced.
