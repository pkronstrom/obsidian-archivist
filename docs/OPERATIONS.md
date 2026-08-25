# Running Archivist

This guide starts with the normal Docker Compose deployment and moves toward
less common administration. For everyday Obsidian use, start with the
[README](../README.md).

## Start the Compose stack

The included Compose file runs both `archivist-server` and `archivist-relay`.
The server owns the vault; the relay stays idle until something uses MCP, its
file API or webhooks.

```bash
git clone https://github.com/pkronstrom/obsidian-archivist.git
cd obsidian-archivist
mkdir -p data/vaults/personal data/.archivist

printf 'ARCHIVIST_UID=%s\nARCHIVIST_GID=%s\n' "$(id -u)" "$(id -g)" > .env
docker compose build archivist relay
docker compose run --rm --no-deps archivist token add \
  -root /data -label first-device -vaults personal -profile obsidian-plugin
```

Save the printed `arch_...` token in the first device. It appears only once.
Then start and check both services:

```bash
docker compose up -d
docker compose ps
curl --retry 20 --retry-delay 1 --retry-connrefused -fsS http://127.0.0.1:8090/healthz
curl --retry 20 --retry-delay 1 --retry-connrefused -fsS http://127.0.0.1:8091/healthz
```

Compose keeps everything below `./data`:

```text
data/vaults/personal/       plain Markdown and attachments
data/.archivist/personal/   git history and server state
```

Useful lifecycle commands:

```bash
docker compose logs -f archivist relay
docker compose build --pull
docker compose up -d
docker compose down
```

Both published ports bind to `127.0.0.1`. Leave them that way unless a private
proxy is already protecting access.

## Reach it from other devices

Archivist assumes a private network. It has bearer-token authorization, but no
rate limiting, lockout or public-internet hardening. Do not port-forward it and
do not use Tailscale Funnel.

### Tailscale Serve

The shortest recommended setup is
[Tailscale Serve](https://tailscale.com/docs/features/tailscale-serve). Run it
on the Docker host:

```bash
tailscale serve --bg --https=443 127.0.0.1:8090
tailscale serve --bg --https=8443 127.0.0.1:8091
tailscale serve status
```

Tailscale reports a hostname such as `notes.example.ts.net`. Use:

- `https://notes.example.ts.net` for Obsidian and the server API;
- `https://notes.example.ts.net:8443/mcp` for MCP through the relay.

Serve provisions HTTPS for the tailnet hostname and applies the tailnet's access
rules. It may prompt you to enable tailnet HTTPS the first time.

### Caddy on the tailnet

If the host already runs Caddy, it can proxy the loopback services instead.
Replace the hostname and `100.x.y.z` with `tailscale status` and
`tailscale ip -4` output:

```caddyfile
{
    auto_https disable_redirects
}

notes.example.ts.net {
    bind 100.x.y.z
    reverse_proxy 127.0.0.1:8090
}

notes.example.ts.net:8443 {
    bind 100.x.y.z
    reverse_proxy 127.0.0.1:8091
}
```

The explicit `bind` keeps Caddy off the LAN and public interfaces. Caddy can
obtain certificates for `*.ts.net` names from the local Tailscale daemon when
tailnet HTTPS is enabled; the Caddy process must be allowed to access
`tailscaled`. See Caddy's
[Tailscale certificate documentation](https://caddyserver.com/docs/caddyfile/directives/tls#tailscale).

Keep the Compose ports on loopback in both setups.

## Manage tokens

Archivist stores only token hashes. Give each device or agent its own token and
save the printed secret: it appears only once.

```bash
docker compose exec archivist /archivist-server token add -root /data -label laptop \
  -vaults personal -profile obsidian-plugin
docker compose exec archivist /archivist-server token list
docker compose exec archivist /archivist-server token revoke <id-prefix>
```

The running server reloads the file after `add` or `revoke`; no restart is
needed.

A token carries a label, allowed vaults, expiry and the scopes `read`, `write`
and `delete`. Its label is recorded in commit history. Prefer the built-in
profiles, which choose the expected scopes and step-up posture:

| Profile | Intended holder | Access |
| --- | --- | --- |
| `obsidian-plugin` | attended Obsidian device | read, write, delete |
| `mcp-client` | attended agent such as Claude Code | read, write |
| `mcp-scheduled` | unattended agent or job | read, write |
| `relay-background` | webhook event stream | read |

Add `-expires-in 720h` for an expiry. A lost secret cannot be recovered; revoke
it and mint another.

## Add a device

Give every Obsidian installation its own token:

```bash
docker compose exec archivist /archivist-server token add \
  -root /data -label phone -vaults personal -profile obsidian-plugin
```

Install the plugin, enter the same server URL and vault name, then use the new
token. An empty vault pulls the server copy automatically. A previously synced
vault recognizes the same vault even if its hostname changes.

## Connect a vault that already has notes

First pairing is automatic when at least one side is empty. When an unsynced
device and the server both contain notes, the plugin stops before writing and
offers three choices:

- **Adopt server** moves the device's files into a dated
  `_archivist-rescued-…/` folder, then pulls the server copy.
- **Publish local** makes local versions win where paths collide. Server-only
  files remain and arrive on the next sync.
- **Merge anyway** keeps the union. Same-path collisions become conflict copies
  for manual resolution.

Nothing changes until you choose. Dismissing the dialog is safe.

| This device | Server | Result |
| --- | --- | --- |
| Empty | Empty | nothing to transfer |
| Empty | Has notes | pulls the server copy |
| Has notes, never synced | Empty | publishes the device copy |
| Has notes, never synced | Has notes | stops and asks |
| Already synced | Different vault | refuses without writing |

## Import an existing vault

Copy the vault into the server's ordinary data directory:

```bash
mkdir -p data/vaults/personal
rsync -a /path/to/existing-vault/ data/vaults/personal/
docker compose up -d
```

Then connect devices normally. Matching files are recognized by content hash
and are not transferred twice.

The Compose setup enables Unicode NFC filename normalization. This prevents a
Mac and iPhone from creating visually identical accented filenames with
different byte representations. It may rename existing paths on disk, so check
the first commit after importing.

## More than one vault

Each directory below `data/vaults` is a vault with its own history:

```text
data/vaults/personal/
data/vaults/work/
data/.archivist/personal/
data/.archivist/work/
```

Create one by adding its directory:

```bash
mkdir -p data/vaults/work
```

The running server discovers it automatically. Mint tokens with only the vaults
their holder needs:

```bash
docker compose exec archivist /archivist-server token add \
  -root /data -label work-laptop -vaults work -profile obsidian-plugin
```

`GET /v1/vaults` lists only the vaults opened by the presented token. Vault
names are one path segment: no leading dot, slash or `..`. URL-safe names are
easiest to use by hand. `ARCHIVIST_MAX_VAULTS` defaults to five.

## When sync looks wrong

Work from the least disruptive check downward.

1. Compare the working tree with history:

   ```bash
   docker compose exec archivist /archivist-server check \
     -root /data -name personal
   ```

   A healthy vault reports no `missing`, `extra` or `stranded` paths.

2. In the plugin, inspect the Status card and run **Test connection**. This
   verifies the URL, vault and token without changing content.

3. Use **Settings → Troubleshooting → Re-bootstrap** only when the device's
   cursor is genuinely stale, such as after restoring the server. It preserves
   `.local` files and renames differing local notes into conflict copies.

Re-bootstrap is recovery, not a fix for slow sync.

## Backups

Back up the vault directory as ordinary files. To preserve all revisions, also
export the repository through the running server:

```bash
curl -fsS -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8090/personal/v1/export > vault-personal.tar
```

The export freezes commits while building a consistent, uncompressed tar. That
format deduplicates well in tools such as restic. Use `?gzip=1` only when the
destination benefits from transport compression.

For an offline export without a host-installed binary:

```bash
docker compose stop archivist
docker compose run --rm --no-deps -T archivist \
  export -root /data -name personal > vault-personal.tar
docker compose start archivist
```

Inspect or extract the default export with `tar tf` or `tar xf`; it is not
gzip-compressed unless you requested `?gzip=1`. Test restoration into a
separate empty data directory. Do not file-copy
`data/.archivist/<vault>` while the server is running: a backup can catch a ref
and its objects at different moments.

## Protecting a vault

A protected vault requires an authenticator code before selected tokens can
read it or perform destructive operations. This is a consent gate, not
encryption: the server still holds plaintext files.

Re-mint every token that opens the vault before creating the marker. State the
posture explicitly because profile defaults apply only after a vault is already
protected:

```bash
docker compose exec archivist /archivist-server token add \
  -root /data -label work-laptop -vaults work \
  -profile obsidian-plugin -step-up ops:work

docker compose exec archivist /archivist-server token add \
  -root /data -label agent-claude -vaults work,personal \
  -profile mcp-client -step-up vault:work,ops:work
```

Update the plugin and agent with their new credentials and verify each can see
the intended vault before creating the marker:

```bash
NEW_TOKEN=arch_replace_with_the_new_token
curl -fsS -H "Authorization: Bearer $NEW_TOKEN" \
  http://127.0.0.1:8090/v1/vaults
touch data/.archivist/work/step-up
curl -fsS -H "Authorization: Bearer $NEW_TOKEN" \
  http://127.0.0.1:8090/work/v1/head
```

If webhooks follow this vault, mint a separate `relay-background` token with
`-no-step-up work`, set `RELAY_BG_TOKEN`, and recreate the relay before adding
the marker. The [webhook setup](INTEGRATIONS.md#webhooks) shows the full flow.

Once the marker exists, old tokens are refused because they have no explicit
posture. New tokens minted with a profile receive that profile's default posture
automatically, but an explicit `-step-up` or `-no-step-up` remains clearer when
changing protection policy.

Minting the token prints an `otpauth://` URL for an authenticator. Profiles use
these postures:

| Posture | Gates | Lifetime |
| --- | --- | --- |
| `vault:work` | read and write access | grant, 15 minutes by default |
| `ops:work` | destructive operations | one code for one operation |

An attended agent can call the relay's `unlock` MCP tool with a code. A deploy
drops active grants. Unattended `mcp-scheduled` and `relay-background` profiles
refuse vault step-up because nobody is present to unlock them.

Webhooks continue when a protected vault is not unlocked: they use the relay's
background token, not an attended caller's grant.

## Write guards

Git retains every revision, so a broken client can grow history until the disk
fills. These defaults limit remote write loops and low-disk writes:

| Variable | Default | Effect |
| --- | --- | --- |
| `ARCHIVIST_QUARANTINE_WRITES` | `300` | writes to one path per window |
| `ARCHIVIST_QUARANTINE_PATH_BYTES` | `100MB` | bytes to one path per window |
| `ARCHIVIST_QUARANTINE_TOTAL_BYTES` | `2GB` | bytes to the vault per window |
| `ARCHIVIST_QUARANTINE_WINDOW` | `5m` | rolling counter window |
| `ARCHIVIST_QUARANTINE_COOLDOWN` | `15m` | quarantine duration |
| `ARCHIVIST_THROTTLE_MAX_DEBOUNCE` | `60s` | local-commit delay ceiling |
| `ARCHIVIST_MIN_FREE_BYTES` | `20GB` | refuse remote writes below this floor |
| `ARCHIVIST_NTFY_URL` | unset | full ntfy URL for alerts |

`0` disables an individual counter. A quarantine blocks only the affected path.
One large upload does not count as a loop, and single uploads have separate
size caps: 512 MB through the server and 32 MB through the relay.

Filesystem writes have already landed by the time the watcher sees them, so
Archivist never refuses them. It lengthens the commit debounce instead. Remote
refusals return `429` (`path_quarantined` or `throttled`) or `507` (`disk_low`).

## Reclaiming space

Deleting a file does not delete its historical blobs. Start with a report:

```bash
docker compose exec archivist /archivist-server reclaim \
  -root /data -name personal
```

If the total is small, stop. Reclaiming rewrites history and is deliberately
manual. Deleted paths newer than 90 days are excluded by default.

Before pruning, stop the server, back up `data/.archivist/personal` and preview
the exact one-off Compose command:

```bash
docker compose stop archivist
cp -a data/.archivist/personal data/.archivist/personal.pre-prune

docker compose run --rm --no-deps archivist \
  reclaim -root /data -name personal --prune
docker compose run --rm --no-deps archivist \
  reclaim -root /data -name personal --prune --yes

docker compose start archivist
```

The backup is required because a crash or power loss during repacking can leave
the rewritten repository incomplete. `prune-map` beside the repository maps the
old head to the new one so current devices do not download an unchanged vault
again; back it up with the repository.
