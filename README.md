# Archivist

Keep using Obsidian. Keep your notes as ordinary files on your own server.

Archivist gives Obsidian real-time, self-hosted sync without turning your vault
into a database or an opaque server format. Markdown, images and PDFs live in a
normal directory, with every change kept in git history.

- **Server** (`archivist-server`) owns the vault and its history.
- **Obsidian plugin** syncs desktop and mobile vaults.
- **Relay** (`archivist-relay`) adds MCP tools, a simple file API and webhooks.

The directory on the server is the real vault, not a cache or export. Tools on
that machine can search, read, edit, serve and back up the same files.

## Quick start

This starts one vault named `personal` on your current computer. You need Git,
Docker Compose and Obsidian.

```bash
git clone https://github.com/pkronstrom/obsidian-archivist.git
cd obsidian-archivist
mkdir -p data/vaults/personal data/.archivist

printf 'ARCHIVIST_UID=%s\nARCHIVIST_GID=%s\n' "$(id -u)" "$(id -g)" > .env
docker compose build archivist relay
docker compose run --rm --no-deps archivist token add \
  -root /data -label first-device -vaults personal -profile obsidian-plugin
```

Copy the printed `arch_...` token; it is shown only once. Then start the stack:

```bash
docker compose up -d
curl --retry 20 --retry-delay 1 --retry-connrefused -fsS http://127.0.0.1:8090/healthz
curl --retry 20 --retry-delay 1 --retry-connrefused -fsS http://127.0.0.1:8091/healthz
```

Both services listen on loopback, so this setup is reachable only from the
computer running Docker. Give every additional device and agent its own minted
token so access can be scoped, attributed and revoked independently.

### Connect Obsidian

Archivist is not yet in the Obsidian community store. Install it through
[BRAT](https://github.com/TfTHacker/obsidian42-brat):

1. In Obsidian, open **Community plugins → Browse** and install BRAT.
2. Run **BRAT: Add a beta plugin for testing** from the command palette.
3. Enter `pkronstrom/obsidian-archivist`.

Open **Community plugins → Archivist → Options** and set:

- **Server URL:** `http://127.0.0.1:8090`
- **Vault:** `personal`
- **Token:** the token printed above

Press **Test connection**. Start writing: changes now sync automatically a few
seconds after you stop typing.

> **Already have notes on both sides?** Archivist stops and asks which side to
> trust instead of combining unrelated vaults silently. Read
> [connecting an existing vault](docs/OPERATIONS.md#connect-a-vault-that-already-has-notes)
> before choosing.

## Use it from other devices

Archivist assumes a private network. Do not publish the server or relay directly
to the internet.

For a phone or another computer, the recommended addition is
[Tailscale](https://tailscale.com/) with a private HTTPS proxy. The shortest
option is [Tailscale Serve](https://tailscale.com/docs/features/tailscale-serve):

```bash
tailscale serve --bg --https=443 127.0.0.1:8090
tailscale serve --bg --https=8443 127.0.0.1:8091
```

Use the reported `https://…ts.net` URL as Obsidian's server URL. Port `8443` on
the same hostname is the relay. If the server already runs Caddy, keep both
containers on loopback and proxy them only on its Tailscale address.

See [private remote access](docs/OPERATIONS.md#reach-it-from-other-devices) for
the full Tailscale Serve and Caddy setup. Use Tailscale **Serve**, not Funnel:
Funnel makes the service public.

## Add more

- [Manage tokens and add devices](docs/OPERATIONS.md#manage-tokens)
- [Give Claude Code MCP access](docs/INTEGRATIONS.md#claude-code-over-mcp)
- [Edit or serve notes on the server](docs/INTEGRATIONS.md#edit-notes-on-the-server-or-web)
- [Run webhooks and event consumers](docs/INTEGRATIONS.md#react-to-changes)
- [Keep several vaults](docs/OPERATIONS.md#more-than-one-vault)
- [Back everything up](docs/OPERATIONS.md#backups)

## Important limitations

- **Single user.** There are scoped device tokens, but no accounts, sharing or
  self-service administration.
- **Private network only.** There is no rate limiting, lockout or public-internet
  hardening. Use Tailscale, another VPN or a private LAN.
- **Direct edits cannot be merged.** Obsidian pushes carry a merge base; a web
  editor or script writing directly to disk does not. Git keeps the overwritten
  version, but Archivist cannot warn before the write.
- **Still new.** Archivist was written in 2026 and is used by one person. Keep
  backups and test restoring them.

## Documentation

- [Operations](docs/OPERATIONS.md) — deployment, private access, tokens, vaults,
  troubleshooting and backups
- [Integrations](docs/INTEGRATIONS.md) — Claude Code, web editing, webhooks and
  Obsidian configuration sync
- [Internals](docs/INTERNALS.md) — protocol, storage, merging and development
- [Design notes](docs/) — the decisions behind the implementation

## License

[MIT](LICENSE)
