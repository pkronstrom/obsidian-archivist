# Internals

The detail behind the [README](../README.md): protocol, guarantees, and the
measurements that shaped them.

**One repository, two counterparts:**

| | |
| --- | --- |
| **Server** — `cmd/`, `internal/` | one static Go binary; owns the vault, keeps history in git |
| **Plugin** — `src/`, `manifest.json` | the Obsidian client, for desktop and mobile |

They share a wire protocol, so they live together: a protocol change touches
both sides and belongs in one commit.

---

# The server

One Go binary that owns a plain Markdown vault on disk, keeps its history in a
real git repository, and syncs it to Obsidian over HTTP.

**No database. No git binary. No runtime dependencies at all** — the container
image is `FROM scratch` and 8 MB.

```
Obsidian (Mac, iPhone)        SilverBullet / scripts / Claude
        │  HTTP                            │  ordinary file I/O
        └──────────────┬───────────────────┘
                       ▼
                   archivist
                       │
        ~/knowledge/personal   ← an ordinary directory, this is the vault
        ~/archivist/git    ← history, deliberately outside the vault
```

The working tree **is** the vault. Git is the state store: the tree at a commit
is the snapshot, a diff between commits is the change feed, the object database
is the content-addressed blob store, and a commit hash is the sync cursor.
Nothing else is needed, so nothing else exists.

## Running it

Prebuilt binaries are attached to every [release](../../releases) for
`linux/amd64`, `linux/arm64` and `darwin/arm64`, with a `SHA256SUMS` file.
Nothing needs a Go toolchain on the target host:

```bash
curl -fsSLO https://github.com/pkronstrom/obsidian-archivist/releases/latest/download/archivist-server-linux-amd64
curl -fsSLO https://github.com/pkronstrom/obsidian-archivist/releases/latest/download/SHA256SUMS
shasum -a 256 -c SHA256SUMS --ignore-missing
chmod +x archivist-server-linux-amd64
```

Or build it yourself:

```bash
go build -o archivist-server ./cmd/archivist-server
ARCHIVIST_TOKEN=secret ./archivist-server -vault ~/knowledge/personal
```

| Flag | Environment | Default |
| --- | --- | --- |
| `-vault` | `ARCHIVIST_VAULT` | *(required)* |
| `-git` | `ARCHIVIST_GIT` | `~/archivist/git` |
| `-listen` | `ARCHIVIST_LISTEN` | `:8090` |
| `-token` | `ARCHIVIST_TOKEN` | *(required)* |
| `-debounce` | `ARCHIVIST_DEBOUNCE` | `1s` |
| `-watch` | `ARCHIVIST_WATCH` | `true` |

Flags beat the environment: the environment is the deployment's baseline, a
flag is a deliberate override of it.

```bash
docker build -t archivist .
docker run -e ARCHIVIST_TOKEN=secret -e ARCHIVIST_VAULT=/vault -e ARCHIVIST_GIT=/git \
  -v ~/knowledge/personal:/vault -v ~/archivist:/git -p 8090:8090 archivist
```

## API

Seven routes, all behind `Authorization: Bearer <token>`.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/head` | current commit |
| `GET` | `/v1/snapshot` | full manifest — the bootstrap |
| `GET` | `/v1/changes?since=<sha>` | incremental delta; **409** if the cursor is unknown |
| `POST` | `/v1/have` | which of these hashes are missing |
| `PUT` | `/v1/content/{hash}` | upload; 400 if the bytes do not hash to `{hash}` |
| `GET` | `/v1/content/{hash}` | download |
| `POST` | `/v1/push` | apply a change set against a base |
| `GET` | `/v1/export` | consistent archive of the git directory |
| `GET` | `/v1` | the endpoint list, plus `protocol` and `version` |

Content is addressed by **git object hash**, so a client can compute an address
with plain `git hash-object` and nothing bespoke:

```bash
HASH=$(printf '%s' "$CONTENT" | git hash-object --stdin)
curl -X PUT -H "$AUTH" --data-binary @- localhost:8090/v1/content/$HASH <<<"$CONTENT"
curl -H "$AUTH" -d "{\"base\":\"$BASE\",\"device\":\"mac\",\"changes\":[
      {\"path\":\"notes/a.md\",\"op\":\"put\",\"hash\":\"$HASH\"}]}" localhost:8090/v1/push
```

`/changes` answers **409** rather than 500 when it does not recognise a cursor —
the client is from another repository or predates a history rewrite, and its
recovery is to re-bootstrap from `/snapshot`, not to retry.

## Tokens and scopes

A token carries the vaults it opens and the verbs it holds. Verbs are `read`,
`write` and `delete`; there is no wildcard for verbs, deliberately.

| verb | covers |
|---|---|
| `read` | every GET, plus `POST /v1/have`. Includes `/v1/export`, which hands over the entire history in one call. |
| `write` | `POST /v1/push` and `PUT /v1/content/{hash}`. Staging a blob counts: an unreferenced blob still consumes disk, and the write guards count writes per path at push time — they never see an orphan. |
| `delete` | a `del` op inside a push. Not a route — checked in the push handler over the whole change set, before anything is staged. |

The scope a route needs is a field on the `routes()` table, which is also what
builds the mux. A route therefore cannot exist without declaring what it needs.

A rename is a `del` plus a `put`, so a token with `write` but not `delete`
cannot rename. That is a known limitation, resolved by a `move` op in a later
change.

Tokens are stored as sha256 hashes in the tokens file, so nothing in it can be
replayed. Lookup hashes the presented token and does a map lookup, where the old
format compared every entry in constant time — not a regression, because
recovering a token from lookup timing would now mean inverting the hash rather
than guessing a string one byte at a time.

The file is format v2. The original unversioned format held tokens in plaintext
and is a startup error: converting would require reading the secrets the format
exists to stop storing, so each token is re-minted once instead.

The file is re-read when it changes — fsnotify on its **directory**, not the
file, because minting renames a new file over the old one and replaces the
inode. A file that fails to parse is logged and ignored and the running table is
kept, so a typo cannot lock every device out at once.

Minting is offline only, via `archivist-server token add`. There is no HTTP
route that mints. The server is internet-facing behind Caddy, and a mint
endpoint would let a leaked admin token issue itself a successor that survives
revoking the original.

## Backups

**Do not snapshot the git directory with a file-level backup tool.** Git writes
objects first and updates the ref afterwards, so a backup can capture a ref
pointing at a commit whose objects it has not read yet. Measured on this
repository under continuous commits: **1 in 8 naive copies** restored as

```
error: refs/heads/master: invalid sha1 pointer 9da131df...
```

Use `/v1/export` instead. It builds the archive while commits are frozen, so
the objects and refs are always consistent with each other. **8 of 8 exports
taken under the identical workload restored `fsck`-clean.**

```bash
curl -sf -H "Authorization: Bearer $TOKEN" https://vault.example.net/v1/export \
  | restic backup --stdin --stdin-filename vault-personal.tar
```

**The export is uncompressed on purpose.** Compressing before a
deduplicating backup tool defeats it: a single deflate stream re-randomises
everything after the first changed byte, so nothing matches the previous
snapshot. Measured with restic 0.16.4, repository v2, two snapshots one small
edit apart:

| Format | Two snapshots cost |
| --- | --- |
| plain tar | **1.09×** — the second is a 9% delta |
| tar.gz | **2.00×** — the second is a full copy |

Thirty daily snapshots would be thirty full copies. restic compresses
repository-side anyway. Add `?gzip=1` only when piping somewhere that will not.

Restore with `tar xzf` into an empty directory and point `--git-dir` at it; the
working tree rebuilds from `git checkout` or simply by starting archivist
against it.

**The working tree is safe to back up directly.** Every server write is
temp-file-plus-rename, so each file is atomically old-or-new and never torn.
Backing up both is belt and braces, but redundant: the archive contains every
version of every file.

## How conflicts are handled, and where they are not

**Pushes carry a base, so they can be merged.** If the same path changed on the
server since that base, the server three-way merges against the content at the
base. Clean merges are committed. Otherwise the server's version stays put and
the client's copy lands beside it as `notes/idea.conflict-mac-20260816T093012.md`
— an ordinary note, so it syncs everywhere and can be resolved on a phone by
editing and deleting it.

**Binaries are never merged.** Conflict markers in a PNG are worse than either
version, so both are kept.

**Filesystem writes are last-writer-wins, and cannot be otherwise.** When
SilverBullet or a script writes into the vault, there is no base version — the
watcher sees only the resulting bytes, and a stale-buffer overwrite is
byte-identical to a fresh edit. Every such write is still committed, so the
overwritten content is recoverable from history; it just cannot be merged. Keep
long-lived editors read-mostly, or have them speak the HTTP API.

## Two rules with teeth

**A client with no base can never delete.** A device that has not bootstrapped
has no idea what exists on the server, so any deletion it implies is unfounded.
This is exactly the bug Self-hosted LiveSync shipped in 1.0.12, and it has its
own test.

**Every server write to the vault is atomic** — temp file in the same
directory, fsync, rename. POSIX rename is atomic, so a concurrent reader never
sees a partial file.

## Paths

Path handling uses `filepath.IsLocal` plus `os.Root`, not string comparison.
The obvious lexical check is broken, and measurably so:

```go
filepath.Clean("/" + "../escape.md") == "/escape.md"   // joins back INSIDE root
```

`os.Root` additionally refuses to follow a symlink out of the vault, which no
lexical check can do.

## History

The repository is a standard git repository. Nothing at runtime needs the git
CLI, but a human debugging it can use one:

```bash
git --git-dir=~/archivist/git --work-tree=~/knowledge/personal log --oneline
```

go-git writes a small `.git` pointer file into the vault (the linked-worktree
mechanism), so plain `git log` also works from inside the vault directory. It is
excluded from sync; history itself lives outside.

## Development

```bash
go test -race ./...
```

`internal/merge/diff3/` is vendored — see its `PROVENANCE.md` for why, and for
the twelve `git merge-file` comparison cases encoded as tests.

# The Obsidian plugin

## The plugin: how it decides what changed

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
3. BRAT → *Add beta plugin* → `pkronstrom/obsidian-archivist`.

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

## Vaults

One process, one registry, N vaults. Each vault has its own `Vault`, `Repo`,
`Reconciler`, guard and filesystem watcher — nothing is shared but the process
and the socket, which is what makes a leaked token's blast radius exactly the
vaults that token names.

Both write paths are per vault, and that is the thing to check when touching
this: `Reconciler.Push` is reached through the route, and `Reconciler.Scan`
through that vault's own watcher. A registry that opened repositories but
started fewer watchers would give a vault working remote sync and silently no
local sync. An unexpected watcher exit is fatal, as it was with one vault.

Discovery is the filesystem — `$ROOT/vaults/<name>`, re-scanned behind a
five-second cache — so a vault rsynced in appears without a restart. The server
refuses to start if `$ROOT` itself contains a `.git`, or if any vault does:
both mean something was pointed at the wrong directory, and starting anyway
would write a repository into content that syncs to every device. `os.Lstat`,
not `IsDir`, because a linked worktree writes `.git` as a file.

`/healthz` and `/v1/vaults` are server-root routes and are never qualified. The
Go client has `atRoot()` for exactly those two.

## What is not synced

Everything under a dot-directory, with one exception: an allowlist of
`.obsidian/` paths, when the device has opted in. `internal/vault/config.go`
holds the allowlist and `vault.Skip` is the single predicate that consults it —
which is what keeps the four places that ask (the reconciler's `applyOne`,
`Repo.Commit`, `Repo.Check` and the watcher's event filter) from ever
disagreeing.

The watcher's `addTree` is a fifth consumer that does **not** go through
`vault.Skip`: it decides which directories are worth an inotify watch by name,
and `handle` judges directory events separately for the same reason. Both have
their own exception for the config directory, and all three have to be kept in
step by hand. Without them, local config edits produce no event, so no `Scan`,
so no commit — the local write path would carry notes and silently not carry
config.

## Three invariants worth knowing

**Sync state never lives in the vault.** It goes in `app.saveLocalStorage`, not
`data.json`. If `.obsidian/` were ever synced, a shared `data.json` would give
every device another device's cursor, and each would then reason about local
changes from a snapshot it never built.

**A device that has never synced cannot delete anything.** It has no idea what
the server holds, so an absence tells it nothing. The server refuses such
deletes too.

**A first connect between two populated sides refuses.** Every other pairing
combination is safe by construction, and this one is not: with `state.base` at
`""`, `changes("")` returns the server's whole contents as puts, so the device
writes them all and then pushes everything it had -- the union, with every
same-path collision becoming a conflict file. The vault-identity guard cannot
see it, because that compares an *adopted* vault name and on first connect
there is not one yet. `src/pairing.ts` holds the check and the three
resolutions, and it is deliberately free of any Obsidian import so the sync
engine stays drivable from Node.

## Development

```bash
npm install
npm run build

# End-to-end against a running server. Exercises the real client and sync
# engine; only Obsidian's requestUrl and DataAdapter are substituted.
node test/integration.mjs http://localhost:8090 <token>
```

## Not built yet

History squashing and object retention.

## Licence

MIT.
