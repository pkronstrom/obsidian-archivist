# Using the vault from other tools

The vault is a directory of ordinary files, so most integration is not
integration at all: something on the server opens a file. This covers the cases
where that is not enough, because the caller is somewhere else or wants to be
told when something changes.

For running the server itself, see [OPERATIONS](OPERATIONS.md).

---


## Deployment scenarios

Four shapes people actually run. They compose — most setups end up as two or
three of them at once.

### Obsidian on several devices

The baseline, and the one to start with.

```
Mac (plugin) ──┐
               ├──> archivist-server + vault on a remote box
iPhone (plugin)┘
```

Install the server next to the vault directory, mint one token per device, point
the plugin at the URL. Nothing else is required — no relay, no webhooks, no
agents. Each device holds a cursor and pulls what it missed, so a phone that was
off for a fortnight catches up rather than fighting.

The one thing to get right is the first connect on a device that *already* has
notes. Pairing a populated, never-synced vault against a populated server is the
case that silently merges two unrelated vaults, so the plugin refuses it and
offers three explicit recoveries. See [Connecting a vault that already has
notes](#connecting-a-vault-that-already-has-notes).

### Agents over MCP

Add `archivist-relay` when something that is not on the server needs the vault.

```
Claude Code, nanoclaw ──> archivist-relay (MCP) ──> archivist-server
```

The relay forwards each caller's own token rather than standing in for everyone,
so an agent token's scopes are evaluated by the server that enforces them. Mint
the agent a token with `read,write` and no `delete`, and a leaked token cannot
erase notes. The relay usually runs on the same box; it is stateless, so it can
equally run on your laptop or in the container that needs it.

### Editing files directly on the server

The vault is a plain directory, so anything that writes Markdown works — `vim`
over ssh, a cron job, or a web editor.

```
browser ──> NoteDiscovery ─┐
                           ├──> the vault directory ──> archivist-server ──> commit
ssh, cron, scripts ────────┘
```

This repository's own server runs NoteDiscovery (`ghcr.io/gamosoft/notediscovery`)
with the vault mounted read-write, so the same notes are editable in a browser
and in Obsidian. There is no bridge and no API between them: both write plain
Markdown to one directory, and the server's watcher commits whatever appears
there.

Read-write on purpose, and with Obsidian on two devices that makes three writers
on one directory. Two of them editing one note is exactly the case the three-way
merge exists for, and the loser of a genuine conflict gets a conflict copy rather
than losing text.

Two things to check when picking a web editor for this. It must keep **no
database** — an editor that indexes the vault into its own store goes stale the
moment a device syncs a change underneath it, and NoteDiscovery is usable here
precisely because it has none. And it must run as the user that owns the vault:
an image with no `USER` runs as root and writes root-owned files that neither
Obsidian nor the server can then touch.

### Reacting to changes

Three surfaces, none of them a websocket.

| surface | shape | use it for |
|---|---|---|
| `GET /v1/events` | SSE, one message per commit | a long-lived consumer on the box |
| `GET /v1/wait` | long-poll until head moves | a shell script, a poller with no SSE client |
| relay webhooks | best-effort POST per commit | n8n, memo-ai, anything with an HTTP endpoint |

All three tell you *that* something changed and roughly what. None of them is a
durable queue — webhook delivery has no retry and no dead-letter on purpose.
Durability comes from the cursor instead: a consumer that was down for a week
asks `/v1/changes?since=<commit>` and gets exactly what it missed. Keep the
cursor, treat the notification as a hint to go look.

## Hooking things up to it

Because every change is a commit, **history is already a durable event feed**.
Anything that wants to react to edits — an indexer, an AI agent, a webhook
bridge — stores a cursor and asks what it missed:

```bash
curl -s -H "$AUTH" "https://vault.example/v1/changes?since=$CURSOR"
```

That works however long the consumer was away, so there is nothing to queue and
no backlog to manage.

For low latency, subscribe to the change stream:

```bash
curl -N -H "$AUTH" https://vault.example/v1/events
data: {"head":"4ff143d6..."}
```

It carries a **notification, not the change** — "something moved, go look". A
missed event costs nothing, because the cursor still says what changed. Use the
stream to know *when*, and `/v1/changes` to know *what*.

Each event carries enough to triage without a follow-up call:

```json
{ "head": "ff5a0b52...", "prev": "4f3538ca...", "count": 2, "changes": [
  { "path": "att/scan.pdf",  "op": "put", "ext": "pdf", "kind": "binary", "size": 3000,  "hash": "0dc8eb..." },
  { "path": "notes/idea.md", "op": "put", "ext": "md",  "kind": "text",   "size": 11,    "hash": "a1988d..." } ] }
```

`kind` is sniffed from the content, not guessed from the name, so an agent can
skip what it cannot read. A commit touching more than 100 files is truncated
with a count — read `/v1/changes` for those.

`/v1/changes` returns the **same shape**, so a consumer can use the stream and
the catch-up path interchangeably.

[`examples/watch-vault.py`](examples/watch-vault.py) is a working consumer in
~120 lines of standard library — subscribe, triage, act, reconnect, resume from
the cursor. Adapt the `handle()` function and you have an agent.

An agent on the same machine should read the changed files straight off disk
rather than fetching them; the vault is an ordinary directory.

## Syncing Obsidian's own config

Off by default. Each device chooses its own level in the plugin's settings, and
that choice is never itself synced — a phone can stay on **Files only** while a
laptop syncs everything.

| Level | What travels |
|---|---|
| **Files only** | notes and attachments; no config at all |
| **Files + appearance** | `app.json`, `appearance.json`, `hotkeys.json`, `snippets/*.css`, `themes/*/` |
| **Files + appearance + plugins** | the above, plus `community-plugins.json`, `core-plugins.json`, and any plugin's `data.json` you turn on individually |

Never, at any level:

| Never syncs | Why |
|---|---|
| `workspace.json`, `workspace-mobile.json` | per-device by nature; a synced layout fights across screens |
| `graph.json`, caches, anything unlisted | per-device, and an allowlist fails safe |
| plugin **code** (`main.js`, `manifest.json`, `styles.css`) | ~50 MB a year of binary churn, unrecoverable except by prune |
| `plugins/*/data.json` unless you turn it on | the most likely place in a vault to find a live credential |
| **Archivist's own `data.json`** | it holds the bearer token for this server. Hard-excluded, no override, enforced on the server as well as in the plugin — and since this release the token is not in that file at all |

The allowlist is enforced in the plugin **and** on the server, independently, so
an older or buggy plugin cannot push something into history that this version
would not send.

**Plugin code is not shipped through the vault.** The list travels; the
receiving device offers to install what is missing from the community store,
naming every plugin first. Decline and you get the list with the code absent,
which is what Obsidian Sync gives you. Desktop-only plugins are skipped on
mobile automatically.

**Plugin settings are opt-in, per plugin.** Each `data.json` is scanned first,
and a plugin whose settings look like they hold a credential is **refused with a
reason** — nothing is stripped or rewritten, because a filtered settings file is
a broken file that looks fine. You can override per plugin, and the override
states what is being accepted. Finding nothing is not a guarantee: the scanner
reports what it recognises, and it cannot recognise everything.

Settings files are merged by key rather than by line, with arrays replaced
wholesale. When both devices change the same key to different values, the last
writer wins and the other version is kept beside it as a conflict copy —
ordinary JSON you can copy straight back over the winner.

Config sync needs the default `.obsidian` directory. If you renamed it, the
plugin says so and syncs notes only.
