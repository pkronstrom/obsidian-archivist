# vaultsync (Obsidian plugin)

Syncs an Obsidian vault with a self-hosted [vaultsync](https://github.com/pkronstrom/vaultsync)
server. Works on desktop and mobile.

## How it decides what changed

The plugin keeps a **snapshot** — `path → {hash, mtime, size}` as of the commit
it last synced — in Obsidian's device-local storage. Every cycle diffs the vault
against that snapshot.

It deliberately does **not** replay an event log. iOS kills apps without warning,
so an in-memory queue evaporates; and Obsidian's `create` event fires for every
existing file when a vault loads, so events are not a trustworthy delta anyway.
Events here only mean *sync soon* — never *here is what changed*. Ten seconds
offline and ten days offline are the same computation.

Content is addressed by **git object hash**, so anything the plugin uploads can
be checked against the real tool:

```bash
printf 'hello\n' | git hash-object --stdin
```

## Install

Not in the community store yet. Use [BRAT](https://github.com/TfTHacker/obsidian42-brat):

1. Install BRAT from Community Plugins.
2. If this repository is private, add a fine-grained read-only GitHub token in
   BRAT's settings.
3. BRAT → *Add beta plugin* → `pkronstrom/obsidian-vaultsync`.

Then set the server URL, token and device name in the plugin's settings, and
press **Test connection**.

## Settings

| Setting | Notes |
| --- | --- |
| Server URL | e.g. `https://vault.example.net` |
| Token | the bearer token the server expects |
| Device name | appears in commit messages and conflict filenames |
| Sync on change | sync shortly after edits, debounced |
| Sync interval | background period; `0` disables it |

Syncing also happens on window focus and blur. **Pull-on-focus is the one that
matters** for the edit-on-phone-then-pick-up-the-laptop pattern: the laptop
fetches the moment it is activated, rather than waiting for a timer.

## Conflicts

The server resolves them, against a real merge base.

Edits to different parts of a note merge silently. Edits to the same lines keep
the server's version at the original path and write yours to
`notes/idea.conflict-phone-20260816T093012.md` — an ordinary note, so it syncs
everywhere and you resolve it by editing and deleting. Binary files are never
merged; both versions are kept.

## What is not synced

Anything under a dot-directory, including `.obsidian/`. Workspace layout is
device-specific and config sync needs different rules; it is not implemented.

## Two invariants worth knowing

**Sync state never lives in the vault.** It goes in `app.saveLocalStorage`, not
`data.json`. If `.obsidian/` were ever synced, a shared `data.json` would give
every device another device's cursor, and each would then reason about local
changes from a snapshot it never built.

**A device that has never synced cannot delete anything.** It has no idea what
the server holds, so an absence tells it nothing. The server refuses such
deletes too.

## Development

```bash
npm install
npm run build

# End-to-end against a running server. Exercises the real client and sync
# engine; only Obsidian's requestUrl and DataAdapter are substituted.
node test/integration.mjs http://localhost:8090 <token>
```

## Licence

MIT.
