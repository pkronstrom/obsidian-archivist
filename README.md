# vaultsync

One Go binary that owns a plain Markdown vault on disk, keeps its history in a
real git repository, and syncs it to Obsidian over HTTP.

**No database. No git binary. No runtime dependencies at all** — the container
image is `FROM scratch` and 8 MB.

```
Obsidian (Mac, iPhone)        SilverBullet / scripts / Claude
        │  HTTP                            │  ordinary file I/O
        └──────────────┬───────────────────┘
                       ▼
                   vaultsync
                       │
        /srv/knowledge/personal   ← an ordinary directory, this is the vault
        /var/lib/vaultsync/git    ← history, deliberately outside the vault
```

The working tree **is** the vault. Git is the state store: the tree at a commit
is the snapshot, a diff between commits is the change feed, the object database
is the content-addressed blob store, and a commit hash is the sync cursor.
Nothing else is needed, so nothing else exists.

## Running it

```bash
go build -o vaultsync ./cmd/vaultsync
VAULTSYNC_TOKEN=secret ./vaultsync -vault /srv/knowledge/personal
```

| Flag | Environment | Default |
| --- | --- | --- |
| `-vault` | `VAULTSYNC_VAULT` | *(required)* |
| `-git` | `VAULTSYNC_GIT` | `/var/lib/vaultsync/git` |
| `-listen` | `VAULTSYNC_LISTEN` | `:8090` |
| `-token` | `VAULTSYNC_TOKEN` | *(required)* |
| `-debounce` | `VAULTSYNC_DEBOUNCE` | `1s` |
| `-watch` | `VAULTSYNC_WATCH` | `true` |

Flags beat the environment: the environment is the deployment's baseline, a
flag is a deliberate override of it.

```bash
docker build -t vaultsync .
docker run -e VAULTSYNC_TOKEN=secret -e VAULTSYNC_VAULT=/vault -e VAULTSYNC_GIT=/git \
  -v /srv/knowledge/personal:/vault -v /var/lib/vaultsync:/git -p 8090:8090 vaultsync
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
| `GET` | `/v1/export` | consistent gzipped archive of the git directory |

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
  | restic backup --stdin --stdin-filename vault-personal.tar.gz
```

Restore with `tar xzf` into an empty directory and point `--git-dir` at it; the
working tree rebuilds from `git checkout` or simply by starting vaultsync
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
git --git-dir=/var/lib/vaultsync/git --work-tree=/srv/knowledge/personal log --oneline
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

## Not built yet

The Obsidian plugin, `.obsidian/` config sync, and history/blob retention. The
server is complete and drivable with `curl` alone, which is what makes the
plugin testable when it arrives.
