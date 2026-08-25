# Archivist

Keep using Obsidian. Keep your notes as ordinary files on your own server.

Obsidian is a good editor and a bad place to store the only copy of a decade of
thinking. Its own sync is a subscription, and the self-hosted alternatives all
ask for something back: the server copy stops being ordinary files, or sync is
not real-time, or mobile is unreliable, or your vault ends up stored three times
over.

Archivist is a small Go server and an Obsidian plugin. Your vault lives on your
server as **plain Markdown, images and PDFs in a normal directory** — the real
thing, not an export — while Obsidian on your laptop and phone syncs against it
in the background.

| | | |
| --- | --- | --- |
| `archivist-server` | owns the vault, keeps the history | **working** |
| Obsidian plugin | syncs your laptop and phone | **working** |
| `archivist-relay` | optional sidecar: MCP tools, webhooks, a friendlier API | **working** |

The relay is for reaching the vault from somewhere that is *not* the server. If
your tools run on the same machine, you do not need it — the vault is a
directory, and opening a file beats calling an API.

## Why that matters

A directory of files is useful to everything else you own:

```bash
grep -ri "that idea from 2019" ~/archivist/vaults/personal
```

Point a web viewer at it, let an agent read and write it, run a script over it,
back it up with the same tool as everything else. None of that needs an
adapter or an export step, because there is nothing to adapt.

Every change the server syncs is committed to git, so history and point-in-time
restore come for free.

## How it flows

![archivist architecture](docs/diagrams/architecture.svg)

<sub>Source and regeneration: [`docs/diagrams/`](docs/diagrams/).</sub>

**Thick lines** carry vault traffic over HTTP — device sync, and the relay
forwarding each caller's own token. **Thin lines are ordinary file I/O** — that
is the whole point: anything on the server opens files rather than calling an
API. Dotted lines are optional.

The relay usually runs on the same box as the server — that is the simplest
deployment and the one to start with. Nothing ties it there: it is stateless,
holds no vault and no cursor, so it runs equally well on your laptop, in a
container elsewhere, or on another host entirely. It keeps no credential of its
own beyond a read-only background token, and forwards each caller's. The server
stays the only authority wherever the relay sits.

## Getting started

**Server.** Grab a binary from [releases](../../releases), or build the image
from the `Dockerfile`.
A vault is a directory under `$ARCHIVIST_ROOT/vaults/`:

```bash
export ARCHIVIST_ROOT=~/archivist
mkdir -p "$ARCHIVIST_ROOT/vaults/personal"

export ARCHIVIST_TOKEN=$(openssl rand -hex 32)
archivist-server                       # serves every vault under $ARCHIVIST_ROOT
```

That single token opens every vault, which is fine to start with and the server
warns about it. For real use, mint one per device: see
[OPERATIONS](docs/OPERATIONS.md#minting-tokens).

**Plugin.** Archivist is not in the community plugin store, so it installs
through [BRAT](https://github.com/TfTHacker/obsidian42-brat), which tracks a
GitHub repository and keeps the plugin updated from its releases.

1. **Community plugins → Browse**, search **BRAT**, install and enable it.
2. Run **BRAT: Add a beta plugin for testing** from the command palette (or
   *BRAT settings → Add beta plugin*).
3. Paste `pkronstrom/obsidian-archivist`, leave the version as **latest**, and
   confirm. Leave *Enable after installing* ticked.
4. **Community plugins → Archivist → Options**, set the server URL and token,
   and press **Test connection**.

Repeat on every device, phone included: BRAT works the same on mobile.

While this repository is private, BRAT also needs a GitHub personal access
token with `repo` scope, set in its settings. That requirement goes away when
the repository is public.

To update later, run **BRAT: Check for updates to all beta plugins**, or turn on
*Auto-update plugins at startup* in BRAT's settings. Two things worth knowing
when an update seems not to arrive: BRAT installs from a release's attached
files, so a version with no release is invisible to it, and BRAT can quietly
lose its link to a repository, in which case re-add it with the same steps.

If the vault you are connecting **already has notes** and the server does too,
the plugin stops and asks rather than merging two unrelated vaults. That is
deliberate; the choices are explained in
[OPERATIONS](docs/OPERATIONS.md#connecting-a-vault-that-already-has-notes).

## Using it

Once connected, syncing is the part you stop thinking about: edits go up a few
seconds after you stop typing, and changes from other devices arrive within
about a second. That is **Automatic** mode; *Sync behavior* also offers
**Periodic** (a fixed interval, no connection held open) and **Manual** (the
ribbon icon, the *Sync now* command, or when Obsidian regains focus).

Everything below is reachable from the command palette, and from the ribbon on
mobile — a phone has no status bar.

### In Obsidian: the revision browser

The status bar carries a history icon scoped to the note you are looking at,
and on mobile the same thing is *Archivist: Show versions* in the ribbon.
It groups the note's revisions into **editing sessions** rather than listing
every commit — a vault commits whenever you pause, so one evening's drafting is
twenty entries that all say the same thing.

Clicking a session writes that version **beside** the note as
`Note.ae56b1c.local.md` and opens it. Nothing is overwritten, and there is no
"restore" button anywhere: to bring an old version back, rename the copy so it
no longer says `.local`, and it syncs like any new note. That is the whole
restore path, which is why browsing history cannot cost you anything.

**Pins** name a version worth keeping — "draft sent to Anna", or a restore
point before a big reorganisation. They are lines in `pins.jsonl` in your vault
root, so they sync, merge and survive history rewrites like any other note. A
pin always marks the current state; to keep an old version, open it first, then
pin that.

### Recovering a deleted note

Deleting a note has never removed it from the server — a delete is just a commit
where the path stops existing, and every version stays in the repository. What
was missing was a way to *find* one without already knowing its filename.

**Settings → History and recovery → Deleted notes → Browse deleted** lists them, newest first,
with who deleted each and when. Restoring writes the file back where it was and
it syncs like any new note. On the server, `archivist-server deleted` prints the
same list along with the exact `restore` command for each row.

Entries are grouped by the folder they were deleted from — usually how you
remember what you lost ("the thing in Inbox") rather than by filename.

**Look before committing.** Tick *Open a local-only copy instead of restoring*
and the note is written as a `.local` file at the vault root instead: you can
read it, and it never syncs anywhere. The root rather than the original folder
is deliberate — that folder is often gone too, and rebuilding a directory tree
to hold a copy you may discard would recreate the structure you deleted.

Restoring never overwrites: if something now lives at that path, the note was
re-created since and the restore is refused rather than destroying the newer
version to recover the older one. Moves are filtered out, since a moved note
was never lost and "restoring" it would leave you with two copies.

If you want something *actually* gone, this is not the feature — that needs
`reclaim`, which rewrites history and drops the objects.

### Syncing plugin settings

A plugin's `data.json` often holds an API key, so it is scanned before it can
sync. Leave *Sync settings for all plugins* off and enable each plugin
yourself, or turn it on and every plugin syncs except the ones the scan flags.
Either way a flagged plugin needs an explicit decision, and the scan runs again
at the push rather than only in the settings pane, so a blanket default cannot
send a credential nobody looked at. Scanning is best-effort: it reads names and
values it recognises, and cannot recognise everything.

### Files that never sync

**Anything marked `.local` stays on the device that made it** — never
uploaded, by either side:

| | |
|---|---|
| `Scratch.local.md` | a file, marked before its extension |
| `Journal.local/` | a **folder** — nothing inside it ever syncs |
| `Notes/Plan.local` | a file with no extension, same idea |

Use it for scratch notes, machine-specific captures, a whole working folder, or
anything you do not want on your phone. Rename without the `.local` part and it
starts syncing immediately, as new notes.

A folder has to *end* in `.local` to count. That is deliberate: a folder called
`project.local.assets` is an ordinary folder, and everything in it syncs
normally — otherwise a naming coincidence could hide a whole tree.

This is also how the revision browser keeps browsing free of consequence: the
copies it writes land in that namespace by construction.

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

## What people use it for

Because the server copy is a directory, anything already on that machine can
use it without an adapter: an AI agent reading and writing notes as files, a
cron job filing a daily note, a web editor pointed at the same directory, `rg`
across everything. Changes made that way are committed by the watcher and reach
your phone a second later. Subscribing to `/personal/v1/events` covers the rest
— re-embed a note, update an index, post to a channel — and the cursor in
`/personal/v1/changes` lets a consumer that was down for a week catch up
correctly.

## Caveats — read these before trusting it

**Single user.** No accounts, no sharing, no identity model. Tokens are minted
per device and carry read/write/delete scopes and a vault list, but they say
what a device may do, not who anyone is.

**Not for the public internet.** It expects to sit behind Tailscale, a VPN, or
a private network. There is no rate limiting, lockout or audit log.

**Conflicts are resolved, not prevented.** Edit the same lines on two devices and
you get both versions — the server's, plus yours in a `.conflict-<device>-<fragment>`
file next to it. Where a three-way merge was possible that parked file contains
git-style conflict markers rather than a clean copy. Nothing is lost, but you
resolve it by hand.

**Editing on the server is last-writer-wins.** A push from Obsidian carries a
base version, so it can be merged. A program writing directly to the directory
does not, so if you leave a note open in a web editor for a day and then save,
it overwrites what your phone wrote. Git history has the overwritten version,
but nothing warns you. Keep server-side editors read-mostly.

**It is new.** Written in 2026, used by one person. It is well tested, including
against the real thing rather than mocks — but you should keep backups you have
actually tried restoring, which is true of any sync tool and especially this one.

## Alternatives

| | Cost | Server copy | Real-time | Mobile |
| --- | --- | --- | --- | --- |
| **Obsidian Sync** | subscription | none — hosted | yes | excellent |
| **Self-hosted LiveSync** | free | a projection of a CouchDB, ~4× disk | yes | good |
| **Obsidian Git** | free | a git checkout | no — on a timer | poor on iOS |
| **Syncthing** | free | plain files | yes | no iOS client |
| **Archivist** | free | **plain files, 1×** | yes | good |

**Obsidian Sync** is the right answer if you want it to just work and do not
care where the notes live. **LiveSync** is the mature self-hosted option and
does more than this — end-to-end encryption, peer-to-peer, several backends —
at the cost of CouchDB being the real store and the files being a projection of
it. **Obsidian Git** is the closest in spirit and the simplest thing that
works, on a timer rather than on save. **Syncthing** has no iOS client, which
ends the discussion if you have an iPhone.

## Philosophy

**One static binary, no database.** Nothing to install on the server, nothing
running alongside it. The relay is a separate binary precisely so a laptop never
carries a git implementation it will not use.

**The server-side files are real and yours to modify.** Edit them with anything;
the changes sync back.

**Boring where it counts.** Git stores the history. Standard three-way merge
resolves conflicts. Content is addressed by git's own object hash, so you can
verify it with `git hash-object`.

## Going further

| | |
| --- | --- |
| [Running a server](docs/OPERATIONS.md) | tokens, multiple vaults, TOTP-protected vaults, exposing it beyond localhost, write guards, reclaiming space, backups |
| [Other tools](docs/INTEGRATIONS.md) | MCP for agents, webhooks, editing on the server, syncing Obsidian's own config |
| [Internals](docs/INTERNALS.md) | how sync, merging and the git layer actually work |
| [Design notes](docs/) | why things are the way they are, including the ideas that were rejected |
