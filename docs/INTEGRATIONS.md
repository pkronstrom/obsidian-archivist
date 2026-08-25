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

The relay forwards the caller's token. Its background token is used only for
health checks and webhook streams; it never supplies an agent's permissions.
The `mcp-client` profile has read and write but no delete scope.

### MCP tool reference

| Tool | Scope | Purpose |
| --- | --- | --- |
| `list_vaults` | read | list the vaults this token opens |
| `list_notes` | read | list notes and attachments, cursor-paged |
| `list_folders` | read | map folders and file counts |
| `read_note` | read | read current Markdown and its revision |
| `search_notes` | read | search content and paths |
| `note_history` | read | list revisions of one note |
| `read_note_at` | read | read a historical revision without restoring |
| `read_attachment` | read | read binary content as base64 |
| `write_note` | write | write only if the supplied revision is current |
| `write_attachment` | write | write base64 content with the same stale check |
| `move_note` | write | move or rename without delete scope |
| `delete_note` | delete | delete a path |
| `unlock` | none | spend a one-time code for a protected vault |

Every tool accepts an optional `vault`. Omit it when the token opens exactly one
vault. If it opens several, call `list_vaults` and name one explicitly.

`read_note` returns the revision that `write_note` expects. Passing it prevents
a stale agent from overwriting a change that landed after its read.

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
- runs with the UID and GID that own `./data` (the supplied Compose file uses
  `1000:1000`);
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

Set one or more comma-separated targets in `.env`:

```dotenv
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

## Sync Obsidian configuration

Configuration sync is off by default and selected independently on each device:

| Level | What syncs |
| --- | --- |
| **Files only** | notes and attachments |
| **Files + appearance** | appearance, hotkeys, snippets and themes |
| **Files + appearance + plugins** | appearance plus plugin lists and approved settings |

Workspaces, graph state, caches and plugin code never sync. Archivist's own
settings never sync because they contain its server configuration.

Plugin `data.json` files are opt-in because they commonly contain API keys. A
best-effort scanner refuses settings that look credential-bearing and explains
why. You can approve a plugin explicitly, but a clean scan is not a guarantee
that a file contains no secret.

Plugin lists sync without plugin code. A receiving device names missing plugins
and asks before installing them from the community store; desktop-only plugins
are skipped on mobile.

Settings files merge by JSON key. Arrays replace wholesale. If two devices
change the same key differently, the last write wins and the other version is
kept as a conflict copy. Config sync requires the default `.obsidian` directory.
