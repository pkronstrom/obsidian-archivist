# Running an Archivist server

Everything here is server-side administration: setting up vaults, issuing
tokens, exposing the server beyond your own machine, and keeping the repository
healthy. For what the plugin does day to day, see the [README](../README.md).

---


## Connecting a vault that already has notes

Getting started assumes an empty vault on one side. When both sides have
content, the plugin stops on the first sync and asks, because the union of two
unrelated vaults is not something you can undo by pressing something.

| This device | The server | What happens | Do this |
|---|---|---|---|
| Empty | Empty | nothing to do | — |
| Empty | Has notes | pulls everything | nothing; this is second-device onboarding |
| Has notes, never synced | Empty | pushes everything | nothing; this is the first bootstrap |
| **Has notes, never synced** | **Has notes** | **stops and asks** | pick one of the three below |
| Already synced | a *different* vault | refuses, touches nothing | fix the URL, or use **Re-bootstrap from server** |
| Already synced | same vault, new hostname | works | nothing; the plugin records the vault, not the host |

The three choices, when it asks:

- **Adopt server** — this vault's files move into a dated
  `_archivist-rescued-…/` folder and the server's version is pulled. Nothing is
  deleted, and because the folder is inside the vault it syncs to your other
  devices too. Adopting twice never disturbs an earlier rescue.
- **Publish local** — this vault is pushed over the server's. It needs no rescue
  folder: every version of every server file is already in git history. Files
  only the server has are kept and arrive on the next sync.
- **Merge anyway** — the union of both. Same-name files on both sides become
  conflict pairs to resolve by hand.

Nothing is written until you choose, and dismissing the dialog changes nothing.

## Importing an existing vault

Copy it in and start the server:

```bash
rsync -a ~/existing-vault/ "$ARCHIVIST_ROOT/vaults/personal/"
archivist-server
```

Then point each device at it. **Files that already match are recognised by hash
and never transferred**; files only one side has move across; files that
genuinely differ merge, or become a conflict pair. There is no import command
because there is nothing for one to do.

One caveat with accented filenames: macOS writes them decomposed and Linux
tools write them composed, and those are different bytes — so `Kronström.md`
seeded from a Linux box and `Kronström.md` from a Mac would be two notes. Seed
from the machine that owns the vault and it stays consistent.

## More than one vault

One server, one relay and one hostname serve every vault. Vaults are
directories:

```
$ROOT/vaults/personal/       the vault: plain Markdown and attachments
$ROOT/vaults/work/
$ROOT/.archivist/personal/   its history, never inside the vault
$ROOT/.archivist/work/
```

Addressing is path-qualified, so a vault is visible in every log line and every
curl:

```bash
curl -H "Authorization: Bearer $TOKEN" https://vault.example/v1/vaults
curl -H "Authorization: Bearer $TOKEN" https://vault.example/personal/v1/head
```

`GET /v1/vaults` returns **only what your token opens**, which is both the
picker and the authorisation check. A token used against a vault it does not
open gets 403; one that does not exist gets 404.

Names are one path segment, no leading dot, no `/` and no `..`. `MyVault` and
`My Own Vault` are both legal — the second is `My%20Own%20Vault` in a URL, which
the plugin handles and a human writing curl must remember, so URL-safe names are
easier by hand. A name that matches an existing one after Unicode NFC
normalisation is refused: `työ` from a Mac and `työ` from a phone would
otherwise be two directories that look identical and serve different content.

Vault creation is off by default and is a capability on the token, never given
to a relay or agent token:

```bash
curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
     -d '{"name":"archive"}' https://vault.example/v1/vaults
```

`ARCHIVIST_MAX_VAULTS` (default 5) refuses creation past a limit. Finding more
than that already on disk warns and serves them all, so raising it is always a
way out.

A vault directory added while the server runs appears without a restart —
discovery re-scans behind a short cache.

## Minting tokens

Tokens live in the file named by `ARCHIVIST_TOKENS`, and are minted rather than
hand-written — they are stored hashed, so there is nothing to type in by hand:

```bash
docker compose exec archivist archivist-server token add \
  -label mac -vaults personal -scopes read,write

docker compose exec archivist archivist-server token list
docker compose exec archivist archivist-server token revoke <id-prefix>
```

A token carries the vaults it opens and the verbs it holds: `read`, `write`,
`delete`. `-expires-in 720h` gives it a deadline; `-can-create-vaults` lets it
create one.

`token add` prints the secret **once**. It is stored as a sha256 hash and cannot
be read back — losing it means minting another. The running server picks up the
change without a restart.

The Obsidian plugin needs **both** `read` and `write` — it syncs down, and asks
the read-scoped `/v1/have` which blobs are missing before uploading. `token add`
warns when it mints a token that cannot drive the plugin.

Without `ARCHIVIST_TOKENS`, `ARCHIVIST_TOKEN` opens **every** vault with read and
write. That is the one-vault convenience, not isolation — it is exactly what a
leaked agent token would give away — and the server says so once at startup.

In the plugin, **Server URL** and **Vault** are two separate fields. A device
switches vault by editing one of them. There is deliberately no compatibility
alias for the old unqualified paths: an un-updated device gets a 404, which is
visible, rather than writing into the wrong vault, which is not.

## Protecting a vault

A protected vault refuses to serve a token until a human presents a one-time
code from an authenticator app. It is a **consent gate, not encryption** — the
server can always read the vault, it just declines to hand it over until you say
so. Point of it: an agent holding a valid token cannot read or change your notes
without asking.

Mark a vault by creating an empty file:

```bash
touch $ARCHIVIST_ROOT/.archivist/work/step-up
```

**Do the re-minting first.** The moment that file exists, every token that opens
`work` starts failing with `step_up_required`, including the ones already in your
devices. That is deliberate — the alternative is a vault everyone believes is
protected and is not — but it means the order is: list the tokens, re-mint each
one, update its holders, and only then create the marker.

From then on `token add` refuses to mint for `work` without being told what to
do about it:

```
work requires an explicit step-up posture.
Pass -step-up vault:work[,ops:work], or -no-step-up work to opt out deliberately.
```

Two postures, and they are independent:

| entry | gates | lifetime |
|---|---|---|
| `vault:work` | reading and writing the vault | a grant, 15 minutes by default |
| `ops:work` | destructive operations on it | one code, one operation, no grant |

`-no-step-up work` records a deliberate exemption. It is written to the token
rather than merely permitted, because the server has to tell "decided not to
gate this" from "minted before the marker existed" — and it denies the second.

Profiles set both, named for who holds the token:

```bash
token add -root $ARCHIVIST_ROOT -label agent-claude -vaults work,personal   -profile mcp-client
```

| profile | scopes | posture | attended |
|---|---|---|---|
| `obsidian-plugin` | read, write, delete | `ops:` only | yes |
| `mcp-client` | read, write | `vault:` + `ops:` | yes |
| `mcp-scheduled` | read, write | refused | no |
| `relay-background` | read | refused | no |

The attended column is the one that breaks things. A token gated on vault access
with nobody behind it is a service that dies at its first restart and cannot
recover, so the two unattended profiles refuse a posture outright. `mcp-scheduled`
exists for exactly this: memo-ai is an MCP client running on a schedule with
nobody watching.

Minting prints the permissions first and the token last, with an `otpauth://` URL
for the authenticator:

```
Scan this, or run:
  qrencode -t ANSIUTF8 'otpauth://totp/Archivist:agent-claude?secret=...&issuer=Archivist'
```

archivist ships no QR encoder — that would be a dependency in an internet-facing
binary for something you run three times.

**Unlocking.** Over HTTP:

```bash
curl -sX POST -H "Authorization: Bearer $TOKEN"   -d '{"code":"123456"}' https://archivist.example/work/v1/unlock
```

Through the relay, an agent calls the `unlock` tool with the code you read out.
A code is single-use, so telling an agent one is safe — it cannot spend it twice.

**Two things that surprise people.**

A deploy drops every grant. They live in memory and die with the process, so
after a restart each protected token needs a fresh code. That is the
conservative direction: you did not consent to the new binary.

Webhooks keep firing. Fan-out runs on the relay's own background token with no
caller present, so a protected vault keeps publishing change events whether or
not anyone has unlocked it. The relay says so at startup when it has targets for
a protected vault. It is a narrower leak than it sounds — a webhook consumer
holds a read token anyway — but it is not zero.

## Reaching it from elsewhere

Everything on the server uses the files directly. For anything that is not on
the server — an agent on your laptop, an n8n flow in another container —
`archivist-relay` offers MCP over HTTP, webhook fan-out and a friendlier write
API, holding no vault, no sync state and **no caller credential** of its own.

Callers present their own archivist token and the relay forwards it, so the
scopes on that token are what decide the call. Mint one per caller:

```bash
docker compose exec archivist archivist-server token add \
  -label agent-n8n -vaults personal -scopes read,write
```

```
archivist-relay -url http://archivist:8090 -token "$RELAY_BG_TOKEN" -listen :8091
```

`-token` is the relay's **own** credential and is used only where there is no
caller: the compatibility check, the webhook stream and the `/healthz` probe.
Mint it read-only. It is never used on a caller's behalf.

`ARCHIVIST_RELAY_TOKEN` no longer exists. The relay refuses to start while it is
still set, rather than starting and 401ing every caller.

Writing a note is `curl -T`, and reading one gives you an `ETag`:

```
curl -T note.md -H "Authorization: Bearer $MY_ARCHIVIST_TOKEN" \
     localhost:8091/file/notes/idea.md
```

Omit `?vault=` and the relay uses the single vault your token opens; if it opens
several, it asks you to name one rather than guessing.

**If you read a note before editing it, send its ETag back as `If-Match`.**
Without it the write is a blind overwrite and a change that landed in between is
lost rather than merged. The MCP tools do the same thing with the `revision`
that `read_note` returns and `write_note` accepts. `GET /` on the relay
describes the rest.

The server's own HTTP API remains directly usable; `GET /v1` describes it.

## Write guards

Git keeps every revision, and for binary content every revision is a full
copy. A client stuck in a write loop grows the repository until the disk is
full, whether or not the vault itself looks large. These bound that.

Every threshold is read from the environment, so tuning is a restart rather
than a rebuild. `0` disables an individual counter; it never means "block
everything". The defaults apply when unset, so a deployment that changes
nothing still gets the guards.

| Variable | Default | Effect |
|---|---|---|
| `ARCHIVIST_QUARANTINE_WRITES` | `300` | writes to one path per window |
| `ARCHIVIST_QUARANTINE_PATH_BYTES` | `100MB` | bytes to one path per window (needs 2+ writes) |
| `ARCHIVIST_QUARANTINE_TOTAL_BYTES` | `2GB` | bytes to the whole vault per window |
| `ARCHIVIST_QUARANTINE_WINDOW` | `5m` | rolling window shared by all three |
| `ARCHIVIST_QUARANTINE_COOLDOWN` | `15m` | how long a quarantine holds |
| `ARCHIVIST_THROTTLE_MAX_DEBOUNCE` | `60s` | ceiling when deferring local commits |
| `ARCHIVIST_MIN_FREE_BYTES` | `20GB` | refuse writes below this much free disk |
| `ARCHIVIST_NTFY_URL` | unset | full ntfy URL including the topic |

Four things worth knowing before you tune them:

**A quarantined path blocks writes to that path only.** The rest of the vault
keeps syncing, and an oversized single file is already rejected per file
rather than per sync.

**The per-path byte counter needs at least two writes.** One write is not a
loop however large, so a single 120 MB attachment can never quarantine its own
path. Single-upload size is bounded separately, by the 512 MB server cap and
the relay's 32 MB one.

**Local writes are never refused.** The watcher sees bytes that are already on
disk, so refusing would not reclaim any of them and would silently diverge the
vault from its history. It widens its commit debounce instead, which means
only a file's final state in each window becomes a blob.

**The write threshold is derived, not guessed.** Obsidian auto-saves and the
plugin syncs 2s after typing stops, so one device cannot exceed roughly 150
writes to a path in five minutes. 300 clears two devices; a loop runs orders
of magnitude faster. It catches fast loops only — a slow one is the disk
floor's job, deliberately, because a threshold low enough to catch it would
catch a person typing.

A refused push answers `429` with `path_quarantined` or `throttled`, or `507`
with `disk_low`.

## Reclaiming space

Deleting a file reclaims nothing. Every revision of it stays reachable from the
commits it appeared in, and for binary content each revision is a full copy, so
a 9 MB PDF added and then deleted costs 9 MB forever.

Start with the report. It changes nothing:

```bash
archivist-server reclaim
```

```
PATH                  SIZE      VERSIONS  ADDED       DELETED     GONE
5. Sources/scan.pdf   390.6 KB  2         2026-05-01  2026-06-14  65d

1 deleted path(s), 390.6 KB total.
0 eligible to prune (deleted over 90d ago), 0 B.
```

If the number is small, stop. That is the report doing its job.

To actually reclaim it, **stop the server first** — this rewrites history, and
the server would be committing against a history being replaced underneath it:

```bash
docker compose stop archivist
archivist-server reclaim --prune          # explains, changes nothing
archivist-server reclaim --prune --yes    # does it
docker compose start archivist
```

**Back up the repository directory before the `--yes` run.** Pruning repacks
through go-git, and go-git deletes the reachable loose objects *before* the new
packfile is installed — the pack is only moved into place when the writer is
closed, which happens after the deletion. A crash, an OOM kill or a power cut in
that window leaves HEAD pointing at objects that no longer exist anywhere, and
nothing can repair it afterwards.

The window is small and only open while a prune is actually running, which is
why stopping the server is not enough on its own — it prevents concurrent
access, not an ill-timed power cut. A copy of `$ROOT/.archivist/<vault>/` taken
immediately beforehand costs seconds and makes the whole question moot:

```bash
cp -a "$ROOT/.archivist/personal" "$ROOT/.archivist/personal.pre-prune"
```

Selection is by deleted-at-HEAD, not by folder: an attachments directory is a
convention that will drift, and the property that matters is large-and-gone
wherever the file lives. Content still referenced by a live file is never
counted, because git stores it once and removing the deleted name reclaims
nothing.

**Deletions newer than `--older-than` (90d by default) are never touched.**
History is what makes a mistaken deletion recoverable, and that is worth more
than the disk. There is no automatic mode, deliberately: unattended history
rewriting would remove exactly the files you might want back, at 3 a.m.

**Your devices do not re-download anything.** A prune changes every commit
hash, so a device arriving with the head it last synced would normally
re-bootstrap the whole vault. Because pruned paths are already absent at HEAD,
the rewritten HEAD's tree is byte-identical and only the hash differs, so the
server records the old-head/new-head pair in `prune-map` beside the git
directory and translates on arrival. The diff comes out empty. A device that
was offline across the prune and behind HEAD falls back to the ordinary
re-bootstrap, which is correct — it may genuinely need the deletions.

`prune-map` is small (one line per prune) but it is real state: back it up with
the git directory.

Pruning is also the only garbage collection this repository ever gets. go-git
never runs `gc` and the image has no `git` binary, so objects otherwise stay
loose forever; `--prune` collects and repacks as part of the same pass.

## Backups

```bash
curl -sf -H "Authorization: Bearer $TOKEN" https://vault.example/v1/export \
  | restic backup --stdin --stdin-filename vault.tar
```

Do not back up the git directory with a file-level tool while the server is
running — you can capture a half-written state. The export exists to avoid that.
