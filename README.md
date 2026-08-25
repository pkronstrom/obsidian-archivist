# Archivist

Keep using Obsidian. Keep your notes as ordinary files on your own server.

Obsidian is a good editor and a bad place to keep the only copy of a decade of
thinking. Its own sync is a subscription; the self-hosted options each give up
plain files, real-time sync, or mobile.

Archivist is a small Go server and an Obsidian plugin. Your vault lives on your
server as **plain Markdown, images and PDFs in a normal directory**, the real
thing rather than an export, while Obsidian on your laptop and phone syncs
against it in the background.

| Piece | What it does |
| --- | --- |
| `archivist-server` | owns the vault, keeps the history |
| `archivist` | the Obsidian plugin: syncs your laptop and phone |
| `archivist-relay` | optional: MCP tools, webhooks, a friendlier API |

The relay is only needed when a tool cannot reach the server's directory
directly.

## Why that matters

The directory on the server is not a cache or an export. It is the vault:
ordinary Markdown, images and PDFs.

Anything else on that machine works on the same files. Search them with `rg`,
edit them from a script or an agent, serve them with a web viewer, back them up
with everything else. None of it needs an Archivist integration or a second
copy.

Archivist watches the directory, commits each change to git, and syncs it to
your devices.

## How it flows

```mermaid
flowchart TB
    mac["Obsidian<br/>laptop"]
    phone["Obsidian<br/>phone"]
    agents["AI agents<br/>Claude Code, nanoclaw"]
    targets["n8n, memo-ai,<br/>automations"]

    subgraph host ["your server"]
        mcp["MCP tools"]
        relay(["archivist-relay<br/>stateless, usually here,<br/>can run anywhere"])
        hooks["webhook fan-out"]
        mcp --- relay
        relay --- hooks

        subgraph core ["archivist-server: the only authority"]
            vs(["archivist-server<br/>one binary"])
            vp[/"vaults/personal"/]
            vw[/"vaults/work"/]
            gp[("git history<br/>personal")]
            gw[("git history<br/>work")]
            local["grep, scripts, cron,<br/>agents on the box"]
            vs --- vp
            vs --- vw
            vp -->|"a commit<br/>per change"| gp
            vw --> gw
            vp <--> local
            vw <--> local
        end
    end

    relay <==>|"caller's token"| vs
    agents <==>|"its own token"| mcp
    hooks -.->|"change events"| targets
    mac <==> vs
    phone <==> vs

    classDef plain stroke:#999
    classDef hot stroke:#c9a227,stroke-width:2px
    class vp,vw,vs hot
    class mac,phone,local,relay,mcp,hooks,agents,targets plain
```

Thick lines carry vault traffic over HTTP. Thin lines are ordinary file I/O on
the server. Dotted lines are optional.

## Caveats

**Single user.** No accounts, no sharing, no identity model. Tokens are minted
per device and carry read/write/delete scopes and a vault list, but they say
what a device may do, not who anyone is.

**Not for the public internet.** It expects to sit behind Tailscale, a VPN, or
a private network. There is no rate limiting, lockout or audit log.

**Conflicts are resolved, not prevented.** Edit the same lines on two devices
and you get both versions: the server's stays at the real path, and yours lands
beside it as `.conflict-<device>-<fragment>`. That file opens with a line saying
what happened and the two versions marked up against their common ancestor, so
you can see what actually differs. Binary files, and paths both sides created
independently, get a clean copy instead, because there is nothing to mark up.

**Editing on the server is last-writer-wins.** A push from Obsidian carries a
base version, so it can be merged. A program writing directly to the directory
does not, so if you leave a note open in a web editor for a day and then save,
it overwrites what your phone wrote. Git history has the overwritten version,
but nothing warns you. Keep server-side editors read-mostly.

**New.** Written in 2026 and used by one person. Keep backups, and test
restoring them.

## Quickstart

One server, one vault, one desktop Obsidian. Phones and extra devices are the
same steps again.

**1. Run the server.** Grab a binary from [releases](../../releases), or use the
`compose.yaml` in this repo.

```bash
export ARCHIVIST_ROOT=~/archivist
mkdir -p "$ARCHIVIST_ROOT/vaults/personal"

export ARCHIVIST_TOKEN=$(openssl rand -hex 32)   # print it, you need it below
archivist-server
```

That token opens every vault, which is fine for one. For more than one device,
mint a token each: [OPERATIONS](docs/OPERATIONS.md#minting-tokens).

**2. Install the plugin.** Archivist is not in the community store, so it comes
through [BRAT](https://github.com/TfTHacker/obsidian42-brat).

1. **Community plugins → Browse**, search **BRAT**, install and enable it.
2. Run **BRAT: Add a beta plugin for testing** from the command palette.
3. Paste `pkronstrom/obsidian-archivist`, keep the version as **latest**, confirm.

**3. Connect it.** In **Community plugins → Archivist → Options**, set the
server URL and the token, then press **Test connection**. It reports which
vault it reached.

That is the whole setup. Edits now sync a few seconds after you stop typing.

Two things worth knowing before you point it at a vault you care about:

- If the vault **already has notes** and the server does too, the plugin stops
  and asks rather than merging two unrelated vaults. The choices are in
  [OPERATIONS](docs/OPERATIONS.md#connecting-a-vault-that-already-has-notes).
- While this repository is private, BRAT needs a GitHub token with `repo`
  scope in its settings. That goes away when the repository is public.

### Optional: reach the vault from Claude Code

The relay exposes the vault as MCP tools, so an agent that is not running on
the server can read and write notes. It forwards each caller's own token rather
than holding one for everybody.

```bash
# A token for the relay's own background work, read-only is enough.
archivist-relay -url http://localhost:8090 -token "$ARCHIVIST_TOKEN" -listen :8091
```

Then point Claude Code at it. The caller's token is what decides the scopes, so
use one minted for the agent rather than the relay's:

```bash
claude mcp add --transport http archivist http://localhost:8091/mcp \
  --header "Authorization: Bearer $AGENT_TOKEN"
```

`list_vaults`, `read_note`, `write_note`, `search_notes` and `note_history`
then appear as tools. Give the agent a token without `delete` unless you mean
it: see [INTEGRATIONS](docs/INTEGRATIONS.md).

To update the plugin later, run **BRAT: Check for updates to all beta plugins**
or turn on *Auto-update plugins at startup*. If an update does not arrive,
check that the version has a release attached and that BRAT still lists the
repository; it can drop the link silently.

## Using it

By default Archivist uploads edits a few seconds after you stop typing, and
receives changes from other devices within about a second. That is **Automatic**
mode; *Sync behavior* also offers **Periodic** (a fixed interval) and **Manual**
(the ribbon icon, the *Sync now* command, or when Obsidian regains focus).

Everything below is reachable from the command palette, and from the ribbon on
mobile, where there is no status bar.

### Files that never sync

**Anything marked `.local` stays on the device that made it.** Never uploaded,
by either side:

| Name | Effect |
|---|---|
| `Scratch.local.md` | a file, marked before its extension |
| `Journal.local/` | a **folder**; nothing inside it ever syncs |
| `Notes/Plan.local` | a file with no extension, same idea |

Use it for scratch notes, machine-specific captures, a whole working folder, or
anything you do not want on your phone. Rename without the `.local` part and it
starts syncing immediately, as new notes.

A folder has to *end* in `.local` to count, so `project.local.assets` is an
ordinary folder and everything in it syncs. Otherwise a naming coincidence
could hide a whole tree.

The revision browser writes its copies into this namespace, which is why
browsing an old version never reaches your other devices.

### In Obsidian: the revision browser

The status bar carries a history icon scoped to the note you are looking at.
On mobile the same thing is *Archivist: Show versions* in the ribbon, and from
the command palette it is *Browse revisions of this note*.
It groups revisions into **editing sessions** rather than listing every commit.
A vault commits whenever you pause, so one evening's drafting is otherwise
twenty entries that all say the same thing.

Clicking a session writes that version **beside** the note as
`Note.ae56b1c.local.md` and opens it. Nothing is overwritten, and there is no
"restore" button anywhere. To bring an old version back, remove `.local` from
the copy's filename; it then syncs as a new note.

**Pins** name a version worth keeping: "draft sent to Anna", or a marker before
a big reorganisation. They are lines in `pins.jsonl` in your vault
root, so they sync, merge and survive history rewrites like any other note. A
pin always marks the current state; to keep an old version, open it first, then
pin that.

### Recovering a deleted note

Deleting a note removes its path, not its history. Archivist can list deleted
notes even when you no longer remember their filenames.

**Settings → History and recovery → Deleted notes → Browse deleted** lists them, newest first,
with who deleted each and when. Restoring writes the file back where it was and
it syncs like any new note. On the server, `archivist-server deleted` prints the
same list along with the exact `restore` command for each row.

Entries are grouped by the folder they were deleted from.

**Preview without restoring.** Tick *Open a local-only copy instead of restoring*
and the note is written as a `.local` file at the vault root instead: you can
read it, and it never syncs anywhere. It lands at the root rather than its
original folder because that folder is often gone too, and rebuilding a
directory tree for a copy you may discard would recreate what you deleted.

Restoring never overwrites: if something now lives at that path, the note was
re-created since and the restore is refused rather than destroying the newer
version to recover the older one. Moves are filtered out, since a moved note
was never lost and "restoring" it would leave you with two copies.

If you want something actually gone, this is not the feature. That needs
`reclaim`, which rewrites history and drops the objects.

### Syncing plugin settings

A plugin's `data.json` often holds an API key, so it is scanned before it can
sync. Leave *Sync settings for all plugins* off and enable each plugin
yourself, or turn it on and every plugin syncs except the ones the scan flags.
Either way a flagged plugin needs an explicit decision, and the scan runs again
at the push rather than only in the settings pane, so a blanket default cannot
send a credential nobody looked at. Scanning is best-effort: it reads names and
values it recognises, and cannot recognise everything.

### History from the command line

On the server, without git installed:

```bash
export ARCHIVIST_ROOT=~/archivist          # -name picks the vault under it

archivist-server history -name personal notes/idea.md   # revisions that touched it
archivist-server deleted -name personal                 # notes history holds, the vault does not
archivist-server show -name personal notes/idea.md 4f3538ca
archivist-server restore -name personal notes/idea.md 4f3538ca
archivist-server check -name personal                   # tree versus history; non-zero on drift
```

Or over HTTP. Routes are vault-qualified, so `GET /personal/v1` lists every
endpoint with a one-line description, generated from the same table that builds
the routes.

## Common tasks

| I want to | Where |
| --- | --- |
| get back a note I deleted | [Recovering a deleted note](#recovering-a-deleted-note) |
| go back to an earlier version | [the revision browser](#in-obsidian-the-revision-browser) |
| keep a note on one device only | [Files that never sync](#files-that-never-sync) |
| mark a version worth keeping | pins, in [the revision browser](#in-obsidian-the-revision-browser) |
| understand a `.conflict-` file | [Caveats](#caveats) |
| add another device | [minting tokens](docs/OPERATIONS.md#minting-tokens) |
| let an agent read and write notes | [agents over MCP](docs/INTEGRATIONS.md#agents-over-mcp) |
| require a code before a vault opens | [protecting a vault](docs/OPERATIONS.md#protecting-a-vault) |
| keep work and personal apart | [more than one vault](docs/OPERATIONS.md#more-than-one-vault) |
| start from a vault that already has notes | [connecting an existing vault](docs/OPERATIONS.md#connecting-a-vault-that-already-has-notes) |
| run something when a note changes | [reacting to changes](docs/INTEGRATIONS.md#reacting-to-changes) |
| sync Obsidian's own settings and plugins | [Obsidian config](docs/INTEGRATIONS.md#syncing-obsidians-own-config) |
| stop the same note arriving twice | [the same note appearing twice](docs/OPERATIONS.md#the-same-note-appearing-twice) |
| work out why sync looks wrong | [when sync looks wrong](docs/OPERATIONS.md#when-sync-looks-wrong) |
| remove a huge file or a leaked secret from history | [reclaiming space](docs/OPERATIONS.md#reclaiming-space) |
| reach the server from another machine | [reaching it from elsewhere](docs/OPERATIONS.md#reaching-it-from-elsewhere) |
| back the whole thing up | [backups](docs/OPERATIONS.md#backups) |

## Alternatives

| | Cost | Server copy | Real-time | Mobile |
| --- | --- | --- | --- | --- |
| **Obsidian Sync** | subscription | none, hosted | yes | built in |
| **Self-hosted LiveSync** | free | a projection of a CouchDB, ~4× disk | yes | plugin |
| **Obsidian Git** | free | a git checkout | no, on a timer | limited on iOS |
| **Syncthing** | free | plain files | yes | no iOS client |
| **Archivist** | free | **plain files, 1×** | yes | plugin |

## Documentation

| Doc | Covers |
| --- | --- |
| [Running a server](docs/OPERATIONS.md) | tokens, multiple vaults, TOTP-protected vaults, exposing it beyond localhost, write guards, reclaiming space, backups |
| [Other tools](docs/INTEGRATIONS.md) | MCP for agents, webhooks, editing on the server, syncing Obsidian's own config |
| [Internals](docs/INTERNALS.md) | how sync, merging and the git layer actually work |
| [Design notes](docs/) | why things are the way they are, including the ideas that were rejected |
