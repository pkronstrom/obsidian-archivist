# Integrations

The vault is an ordinary directory, so software on the server can usually open
the files directly. Use the relay when a caller is elsewhere, needs MCP, or
wants webhook notifications.

For deployment and credentials, see [Running Archivist](OPERATIONS.md).

## Claude Code over MCP

The Compose stack already runs `archivist-relay`. Give Claude Code its own
credential so the server can enforce and record that caller's access:

```bash
docker compose exec archivist /archivist-server token add \
  -root /data -label claude-code -vaults personal -profile mcp-client
```

Save the printed token as `AGENT_TOKEN`, then add the relay. Use loopback when
Claude Code runs on the Docker host:

```bash
claude mcp add --transport http archivist http://127.0.0.1:8091/mcp \
  --header "Authorization: Bearer $AGENT_TOKEN"
```

From another tailnet device, use the relay URL configured in
[private remote access](OPERATIONS.md#reach-it-from-other-devices):

```bash
claude mcp add --transport http archivist \
  https://notes.example.ts.net:8443/mcp \
  --header "Authorization: Bearer $AGENT_TOKEN"
```

Run `claude mcp get archivist` or open `/mcp` inside Claude Code to check the
connection. The command follows Claude Code's current
[remote HTTP MCP syntax](https://code.claude.com/docs/en/mcp).

The relay forwards the caller's token; it never supplies an agent's permissions.
The `mcp-client` profile has read and write but no delete scope.

### MCP tool reference

| Tool | Scope | Purpose |
| --- | --- | --- |
| `list_vaults` | read | list the vaults this token opens |
| `list_notes` | read | list notes and attachments, cursor-paged |
| `list_folders` | read | map folders and file counts |
| `read_note` | read | page current Markdown with repository and content revisions |
| `search_notes` | read | search content and paths |
| `note_history` | read | list revisions of one note |
| `read_note_at` | read | page a historical text revision without restoring |
| `read_attachment` | read | read binary content as base64 |
| `write_note` | write | replace the whole document if its repository revision is current |
| `append_note` | write | atomically append to existing text, optionally guarded by content revision |
| `edit_note` | write | replace one unique literal match using a required content revision |
| `write_attachment` | write | write base64 content with the same stale check |
| `move_note` | write | move or rename without delete scope |
| `delete_note` | delete | delete a path |
| `unlock` | none | spend a one-time code for a protected vault |

Every tool accepts an optional `vault`. Omit it when the token opens exactly one
vault. If it opens several, call `list_vaults` and name one explicitly.

### Large notes and safe updates

`read_note` and `read_note_at` return at most 16,000 Unicode characters by
default; set `max_chars` up to 100,000. Start at `start_line`, then, while
`has_more` is true, pass `next_cursor` back as `cursor`. Continuation refuses
rather than mix content if the note changed or moved.

For a targeted update, use `search_notes` → `read_note(start_line, max_chars)` →
`edit_note(path, content_revision, old_text, new_text)`. The repository
`revision` identifies the snapshot and guards whole-document `write_note`;
`content_revision` identifies the exact note body and guards `edit_note` and,
when supplied, `append_note`. `edit_note` requires one unique literal
`old_text` match and refuses stale content or a moved path.

`append_note` works only on existing text notes and is atomic. Supply
`content_revision` for a guarded append; omit it only for an intentional blind
append to the current content. Mutation requests are limited to 16 MiB,
separately from the 512 MiB resulting-note limit. Historical reads support text
only; historical non-text attachments are unavailable.

## Edit notes on the server or web

Programs on the server can work directly in `data/vaults/<name>`:

```text
SSH editor, cron job or web editor
                 |
                 v
data/vaults/personal  --> filesystem watcher --> git commit --> devices
```

There is no bridge or second copy. The watcher commits changes after they sit
quiet for the configured debounce.

For a browser editor, choose one that:

- reads and writes the directory directly instead of importing it into a
  database;
- runs with the UID and GID that own `./data` (the supplied Compose file reads
  them from `ARCHIVIST_UID` and `ARCHIVIST_GID`);
- can be kept private behind the same Tailscale or VPN boundary.

[NoteDiscovery](https://github.com/gamosoft/notediscovery) is one possible
database-free editor, but Archivist does not deploy or require it. Mount
`./data/vaults/personal` into whichever editor you choose.

Direct writes have no merge base. If a long-open web editor saves over a newer
phone edit, the watcher commits the resulting file and history keeps the older
version, but Archivist cannot detect the stale write in advance. Keep direct
editors short-lived or read-mostly. Remote writers that need stale-write
protection should use MCP or the relay file API.

The relay's file API uses ordinary bearer authorization. Reading returns an
`ETag`; send it back as `If-Match` when replacing a file:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8091/file/notes/idea.md

curl -T note.md \
  -H "Authorization: Bearer $TOKEN" \
  -H 'If-Match: "the-etag-from-the-read"' \
  http://127.0.0.1:8091/file/notes/idea.md
```

Add `?vault=work` when a token opens more than one vault. `GET /` on the relay
lists the rest of its HTTP surface.

## React to changes

Archivist offers three notification surfaces:

| Surface | Shape | Use it for |
| --- | --- | --- |
| `/personal/v1/events` | server-sent event per commit | long-lived consumer |
| `/personal/v1/wait` | long-poll until the head moves | simple poller |
| relay webhooks | best-effort POST per commit | n8n and other HTTP automation |

Notifications are hints, not a durable queue. A consumer stores the last commit
it processed and catches up from history:

```bash
curl -fsS -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:8090/personal/v1/changes?since=$CURSOR"
```

For low latency, listen for the next commit and then request its changes:

```bash
curl -N -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8090/personal/v1/events
```

An event looks like this:

```json
{
  "head": "ff5a0b52...",
  "prev": "4f3538ca...",
  "count": 2,
  "changes": [
    {"path":"att/scan.pdf","op":"put","kind":"binary","size":3000},
    {"path":"notes/idea.md","op":"put","kind":"text","size":11}
  ]
}
```

Large commits may truncate the embedded list; `/personal/v1/changes` remains
authoritative. [`examples/watch-vault.py`](../examples/watch-vault.py) is a
standard-library consumer that reconnects and resumes from its cursor.

### Webhooks

Webhooks are the only relay feature that needs a background token. Mint one:

```bash
docker compose exec archivist /archivist-server token add \
  -root /data -label relay-background -vaults personal -profile relay-background
```

Save the printed token and configure the vault and targets in `.env`:

```dotenv
RELAY_BG_TOKEN=arch_replace_with_the_printed_token
ARCHIVIST_RELAY_VAULT=personal
ARCHIVIST_WEBHOOKS=https://n8n.example.ts.net/webhook/archivist
```

Then recreate the relay:

```bash
docker compose up -d relay
docker compose logs relay
```

The relay posts each commit to every target. Delivery has no retry or dead
letter queue. Make the receiver idempotent, store its cursor, and use
`/personal/v1/changes?since=` to recover anything missed while it was down.

Webhook fan-out uses the relay's read-only background token. It follows one
vault's event stream, selected by `ARCHIVIST_RELAY_VAULT`; that token must have
read access to the selected vault. MCP callers can still select any vault their
own token opens.

## Plugins on your devices

Open **Archivist → Plugins on your devices** to compare installed community
plugins across your Mac, phone and other clients. Missing plugins and exact
version differences are shown per plugin; filter by device when needed. Disabled
plugins still count as installed. Devices with identical names have separate IDs,
and each remote list shows when its inventory changed, not when it was last online.

**Open in Community Plugins** opens the official directory listing, whose **Add
to Obsidian** action leads to installation. Obsidian handles installation, updates
and enablement. Private or BRAT plugins may not be listed, and the store version
may differ from the version on another device. Desktop-only plugins are marked
separately on mobile. Use **Refresh plugin list** after installing a plugin; the
next sync shares the changed inventory.

Only manifest fields (ID, name, version and desktop compatibility) and a device
label/installation ID travel. Plugin code, settings, enablement, themes, hotkeys
and other live Obsidian configuration remain local. Inventories are collected
from the vault's configured Obsidian directory, including disabled plugins.

### Upgrading from config sync

Update the server and every client together. Config sync has been retired; old
appearance/plugin opt-ins cannot re-enable it. Local settings and previous server
history are preserved, without deleting config files or rewriting history. The
server refuses config mutations from older clients and omits config from live
sync downloads. Historical content remains readable.

An older server refuses inventory files. The client reports that refusal; notes
can still sync. Install the updated server and check token write access before
expecting inventories from other devices. Offline-device inventories remain
visible until removed; their timestamps describe the last changed inventory.
