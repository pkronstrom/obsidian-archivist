# Revision Browser and Pins — Design Note

**Status:** Designed, not scheduled. Same shelf as encrypted paths: written so the
decisions survive until someone builds it.

**Date:** 2026-08-21

---

## What it is

A status-bar **revisions icon** in the plugin, scoped to the active note. Clicking
it opens a modal listing that note's history as **sessions**, not commits, with the
user's own **pins** interleaved. Clicking an entry materialises that version
*beside* the original — `Note.rev-ae56b.md` — never overwriting anything.

Two pin scopes, one store:

- **File pin** — names one revision of one file ("draft-sent-to-anna"). Shown in
  that file's modal.
- **Vault-wide pin** (`path: "*"`) — a restore point for the whole vault, taken
  before a reorganisation. Deliberately NOT shown in the file modal — it is not
  about that note. It lives in the settings tab's Maintenance section, beside
  where a future reclaim button would sit: same audience, same rarity.

## The decisions, and why

**Materialise-beside means nothing here is destructive.** The feature began life
as "restore an old revision", the flagship `ops:`/TOTP candidate. Writing the old
version to a NEW file made every operation a read (pin creation is an ordinary
write). No step-up, no overwrite path exists at all. In-place restore is
deliberately absent — materialise-then-copy covers it without the plugin ever
having a "replace current content" code path.

**Clustering is client-side, at render time.** `/v1/history` already returns
timestamps. "A burst of syncs with ≥30 min gap to the next becomes one revision"
is a fold over that list in the modal, each cluster represented by its LAST
commit — the state when that session ended. This delivers what the History-Group
server projection (see the commit-granularity note) would have, for the only
surface that needed it, lazily, for one file at a time, with zero server changes.
A raw per-commit toggle is trivially available since the full list is in hand.

**Pins are `pins.json` in the vault state dir, not git tags.** Tags dangle:
archivist's own `Prune` REWRITES history from HEAD, and a tag into pre-rewrite
commits points at nothing afterwards. A JSON store `[{name, path, rev, created}]`
whose reads resolve through `Repo.Resolve` survives prunes for free — `Resolve`
already consults `pruneMap`, the hash-translation machinery prune maintains.

**Pins should eventually protect from pruning.** A later `reclaim` change can
treat pinned revs as roots, making "pinned" mean "never pruned" — a real restore
point. Not needed on day one; recorded so the pin store is designed as something
`reclaim` will read.

**Materialised files are real but local-only until renamed.** `Note.rev-ae56b.md`
is a genuine note — openable, editable, linkable — but its filename pattern is
excluded from sync, so browsing revisions never litters other devices. Restoring
is renaming: the file stops matching the pattern, looks like a new note, and
syncs normally. This is a third state between the two obvious ones (sync
everything / read-only preview), and it needs no new mechanism — the plugin's
`skip()` and the server's `vault.Skip()` are exactly "do not sync this filename
pattern", and the push path already refuses excluded paths per-file. The pattern
goes in BOTH predicates so `.rev-*` is a reserved local-only namespace
everywhere, including for a file an agent might create server-side. (Contrast:
`.conflict-*` files deliberately DO sync — a conflict must be visible on every
device; a browsed revision must not.)

## Surface inventory

Server (small):
- `GET /{vault}/v1/pins?path=` — read scope; a path's pins (vault-wide included
  only when explicitly asked, e.g. `path=*`).
- `PUT /{vault}/v1/pins` — write scope; `{name, path, rev}`.
- `DELETE /{vault}/v1/pins/{name}` — write scope now; an `ops:` candidate later.
- Storage: atomic-write JSON beside the repo, same pattern as `tokens.json`.

Plugin:
- `client.ts` gains `history(path, limit)` and `readAt(rev, path)` — the server
  routes (`/v1/history`, `/v1/at/{rev}/{path...}`) exist and are read-scoped; the
  TS client simply never spoke them.
- Status-bar icon + modal (clusters, file pins, "Pin this version…" button).
- Maintenance section: create/list/delete vault-wide pins.

Out of scope, deliberately: in-place restore, revision diff view (v2 at best),
any TOTP involvement, attachment-specific UI (attachments pin and materialise
exactly like notes; only the icon placement was ever note-centric).

## Open when built

- Cluster gap: 30 min hardcoded first; a setting only if it ever annoys.
- Name collisions on materialise (`Note.rev-ae56b.md` already exists): overwrite
  is wrong, suffixing is fine.
- The exact reserved pattern, chosen so no real note ever matches it by accident
  — and stated in the README, since files matching it silently stop syncing.
- Whether `DELETE` on a pin that a future reclaim treats as a root needs to move
  to `ops:` before that reclaim change ships (it does).
