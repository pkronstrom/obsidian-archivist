# Scoped Tokens — Decisions

The answered design review that `docs/2026-08-19-scoped-tokens-plan.md` implements. Recorded so the plan's choices can be traced to a reason rather than re-argued.

## Where the work splits

| phase | contents | plan |
|---|---|---|
| 1 | scopes, hashed store, expiry, minting subcommand, live reload, scopes on `/v1/vaults` | `2026-08-19-scoped-tokens-plan.md` |
| 2 | relay pass-through, `move` op, commit provenance, plugin settings guard | not yet written |
| 3 | TOTP vault lock | not yet written |

Phase 1 and 2 release together — the protocol bump in phase 2 forces it — but they are planned separately because each is testable on its own.

## Scope model

- **Verbs are `read`, `write`, `delete`.** No wildcard for verbs. `export` and `history` fold into `read` for now; `export` is the first thing to split if an agent token leaks, because it hands over the whole history in one call.
- **`canCreateVaults` stays a boolean**, not a scope: it is a capability over the server, not over a vault.
- **Staging a blob is a write.** `PUT /v1/content/{hash}` references nothing until a push, but an unreferenced blob still consumes disk, and the per-path write guards never see an orphan.
- **The scope lives on the `routes()` table.** That table already generates both the mux and the index, so a route cannot exist without declaring what it needs. A separate middleware table would be a second list to keep in step.
- **Delete is checked in the push handler**, over the whole change set, before anything is staged. It is an op inside the body, not a route, and putting the check in `Reconciler.Push` would push caller identity into a layer that knows only about vaults.

## Storage

- **Hashed at rest**, sha256, with label and scopes readable for listing.
- **Plain JSON, not SQLite.** Measured: `modernc.org/sqlite` adds 3.6 MB to a 9.75 MB `archivist-server` (+37%) under `CGO_ENABLED=0`; `ncruces/go-sqlite3` is larger still. `bbolt` costs only 0.39 MB but takes an exclusive file lock, which breaks minting from a second process via `docker compose exec`. The dataset is around ten rows, read-mostly, one writer — SQLite's concurrency and query power would both go unused.
- **Expiry as a timestamp**, checked at lookup. Revocation is deleting an entry.
- **A v1 (plaintext) file is a startup error.** No migration: converting would require reading the secrets the format exists to stop storing. Two plugin installs and the relay get re-minted once.

## Reload

fsnotify on the file's **directory**, not the file — minting renames a new file over the old one and replaces the inode, so a file watch goes deaf after the first mint. The atomic rename also removes the half-written-file race that originally argued against a watcher.

A file that fails to parse is logged and ignored; the running table is kept. Emptying it would turn a typo into a lockout from every device at once.

Rejected alternatives: SIGHUP (a step to forget, and the failure mode is a token that exists in the file but not the server); stat-per-request (a syscall on every request to detect a monthly event).

## Minting

- **A subcommand of the server binary**, so it cannot drift from the parser the server uses.
- **Run via `docker compose exec`** — same binary, second process. Safe because the store is a file with atomic writes rather than a locked database.
- **Offline only. There is no HTTP mint route.** The server is internet-facing behind Caddy; a mint endpoint would let a leaked admin token issue itself a successor that survives revoking the original, and it contradicts the principle that the edge holds no credential-issuing power. An in-process minting design was considered and rejected for exactly this.

## Devices

`GET /v1/vaults` returns the principal's scopes and label, so the Obsidian plugin can refuse a write-less token at settings time rather than failing on the first save. Derived from the scopes rather than a separate `kind` field, which would be a second source of truth free to disagree.

## Known limitation carried into phase 2

A rename is a `del` plus a `put`, so a token with `write` but not `delete` cannot rename. Resolved by adding a `move` op to the protocol — a version bump from 1 to 2, which forces server, relay and plugin to ship together.

## Phase 3, for reference

A **consent gate**, not encryption at rest: the server can always read the vault and declines to serve it until unlocked. Encryption would break three-way merge, search, the size guards and history — a different product.

The unlock is a **TOTP code**, not a passphrase and not an ntfy approval. A one-time code is worthless after use, so the agent seeing it is harmless — which was the original worry. `crypto/hmac` and `encoding/base32` cover it with no dependency.

The lock is a property of the **token** (`requiresUnlock`), not the vault, so a phone syncing Work is not gated by an agent's policy. A grant binds to token plus vault for a TTL, lives in memory, and dies with the process. A locked call returns 403 with a distinct code so the agent can turn it into a request rather than guess from a status.
