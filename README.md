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

Because a directory of files is useful to everything else you own.

```bash
grep -ri "that idea from 2019" ~/knowledge/personal
```

Point a web viewer at it. Let an AI agent read and write notes in it. Run a
script over it. Serve it. Back it up with the same tool as everything else. None
of that needs an adapter, an API client, or an export step, because there is
nothing to adapt — they are just files.

Every change is committed to git, so you get history and point-in-time restore
for free, and can recover a note you mangled three weeks ago.

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

**Server.** Grab a binary from [releases](../../releases) or run the container.
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

**Plugin.** Install [BRAT](https://github.com/TfTHacker/obsidian42-brat) from
Community Plugins, add `pkronstrom/obsidian-archivist` as a beta plugin, then
put the server URL and token into Archivist's settings and press **Test
connection**.

If the vault you are connecting **already has notes** and the server does too,
the plugin stops and asks rather than merging two unrelated vaults. That is
deliberate; the choices are explained in
[OPERATIONS](docs/OPERATIONS.md#connecting-a-vault-that-already-has-notes).

## Using it

Once connected, syncing is the part you stop thinking about: edits go up a few
seconds after you stop typing, and changes from other devices arrive within
about a second. The rest of this section is what the plugin gives you beyond
that.

### In Obsidian: the revision browser

The status bar carries a history icon scoped to the note you are looking at.
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

**Settings → Maintenance → Restore a deleted note** lists them, newest first,
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

**Plugin settings.** A plugin's `data.json` often holds an API key, so it is
scanned before it can sync. Two modes: leave *Sync settings for all plugins*
off and enable each plugin yourself, or turn it on and every plugin syncs
except the ones the scan flags. Either way, a flagged plugin needs an explicit
decision from you, and the scan runs again at the push rather than only in the
settings pane, so a blanket default can never send a credential nobody looked
at. Scanning is best-effort: it reads names and values it recognises, and it
cannot recognise everything.

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
archivist-server history notes/idea.md        # revisions that touched it
archivist-server deleted                      # notes history holds that the vault does not
archivist-server show notes/idea.md 4f3538ca  # print an old version, changing nothing
archivist-server restore notes/idea.md 4f3538ca
archivist-server check                        # working tree versus history; non-zero on drift
```

Or over HTTP: `GET /v1` lists every endpoint with a one-line description,
generated from the same table that builds the routes, so it cannot describe
something that does not exist.

## What this is good for

**Capture anywhere, file it later.** Jot into a capture app on your phone; a
small job on the server writes the keepers into `Inbox/` as Markdown. Your
laptop has them the next time you open Obsidian.

**Let an AI agent actually use your notes.** An agent on the server reads and
writes the vault as files — no API client, no export, no sync SDK. It can answer
"what did I decide about X in 2019" by grepping, and file its own notes back
into the vault where you will see them on your phone.

**React to changes.** Subscribe to `/v1/events`, and when a note changes,
re-embed it, update an index, run a linter, post to a channel. The cursor in
`/v1/changes` means a consumer that was down for a week catches up correctly.

**Read and write your notes on the web** without another sync system. Point an
editor at the directory — this repo's own server runs NoteDiscovery that way. A
server-side edit is committed by the watcher like any other, and a concurrent
edit from a device is merged rather than lost.

**Generate notes from scripts.** A cron job writing a daily note, a job pulling
in your calendar, a script filing receipts. Write a file, and it is on your
phone a second later.

**Search across everything, with normal tools.** `grep`, `rg`, `fzf`, and every
Unix thing you already know, over the actual files.

**Keep history without thinking about it.** Every change is a commit, so
"restore the version from before I deleted half of it" is always available.

## Caveats — read these before trusting it

**Single user.** No accounts, no sharing, no permissions. One person, one token
per vault.

**Not for the public internet.** It expects to sit behind Tailscale, a VPN, or
a private network. There is one bearer token and no rate limiting, lockout, or
audit log.

**Conflicts are resolved, not prevented.** Edit the same lines on two devices and
you get both versions — the server's, plus yours in a `.conflict-<device>-<fragment>`
file next to it. Nothing is lost, but you resolve it by hand.

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
care where the notes live. It is genuinely excellent.

**Self-hosted LiveSync** is the mature self-hosted option and does more than
this: end-to-end encryption, peer-to-peer, several storage backends. Choose it
if you want a project with many users behind it. The trade is that CouchDB is
the real store and the filesystem copy is a projection maintained by a separate
daemon — measured at about 4× the vault size across the three copies, and the
server cannot arbitrate conflicts because every client is a full replica.

**Obsidian Git** is the closest in spirit and the simplest thing that works. It
syncs on a timer rather than on save, every device carries the full history, and
iOS support is its weakest point.

**Syncthing** is excellent and has no iOS client, which ends the discussion if
you have an iPhone.

## Philosophy

**Small enough to read in an afternoon.** ~3,000 lines across both halves. If it
grows past what one person can hold in their head, it has failed at its purpose.

**One static binary. No database, no dependencies.** The container image is
`FROM scratch` and 8 MB. Nothing to install on the server, nothing to keep
running alongside it. The relay is a separate binary precisely so a laptop never
carries a git implementation it will not use.

**The server-side files are real and yours to modify.** Edit them with anything.
The changes sync back to your devices.

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
