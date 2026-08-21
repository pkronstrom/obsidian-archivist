# Revision Browser and Pins — Design Note

**Status:** Designed, not scheduled. Reviewed adversarially (sol, 2026-08-21,
15 findings); this revision folds them in. The largest one inverted a
load-bearing decision — see "Pins must be prune roots from day one".

**Date:** 2026-08-21

---

## What it is

A status-bar **revisions icon** in the plugin, scoped to the active note.
Clicking it opens a modal listing that note's history as **sessions**, not
commits, with the user's own **pins** interleaved. Clicking an entry
materialises that version *beside* the original — `Note.rev-ae56b.md` — never
overwriting anything.

Two pin scopes, one store:

- **File pin** — names one revision of one file ("draft-sent-to-anna"). Shown
  in that file's modal.
- **Vault-wide pin** (`path: "*"`) — a restore point for the whole vault, taken
  before a reorganisation. NOT shown in the file modal — it is not about that
  note. It lives in the settings tab's Maintenance section.

## The decisions, and why

**Materialise-beside means nothing here needs `ops:` gating.** The feature
began life as "restore an old revision", the flagship `ops:`/TOTP candidate.
Writing the old version to a NEW file makes every operation a read (pin
creation is an ordinary write); no in-place restore code path exists at all.
Precisely: **no `ops:` step-up** — on a protected vault the normal `vault:`
gate still applies to every route here, exactly as it does to reading a note.

**Materialised files are real but local-only until renamed.**
`Note.rev-ae56b.md` is a genuine note — openable, editable, linkable — but its
filename pattern is excluded from sync in BOTH skip predicates, so browsing
never litters other devices. Restoring is renaming: the file stops matching,
reads as a new note, and syncs normally. (Verified against the sync engine:
the excluded source is neither listed nor emitted as a deletion, so the rename
arrives as one clean `put` — no `move` misfire. Needs a regression test for
both the never-tracked source and a source lingering in pre-upgrade
`state.files`.) Contrast: `.conflict-*` files deliberately DO sync — a conflict
must be visible everywhere; a browsed revision must not.

**Clustering is client-side, at render time.** `/v1/history` returns
timestamps; "a burst with ≥30 min gap to the next is one session" is a fold in
the modal. Two corrections from review: the list is **newest-first**, so the
fold must be defined against descending order — the representative is the
first (newest) revision of each cluster — and it must be the newest
**non-deleted** revision, because `History` marks deletion revisions and
`/v1/at` necessarily 404s on them. Deletion rows render as events, not
openable entries.

**Pins must be prune roots from day one.** The first draft claimed pins
survive pruning "for free" via `Repo.Resolve` and `pruneMap`. Both halves are
false: `Resolve` never consults `pruneMap` (`history.go:114` — full hashes pass
through unchecked), and prune's per-commit mapping is *explicitly discarded* —
"only the head pair is durable" (`prune.go:76`). A pin pointing at any
historical commit dies at the first prune. So protection cannot be deferred:
`reclaim`/`Prune` must treat pinned revisions as roots (or atomically rewrite
every pin through the full mapping before discarding it) in the same release
that introduces pins. Pin creation resolves and stores the full 40-char hash,
validated to contain the pinned path.

**Pin authorization must be decided before pins become roots.** Write scope is
fine while a pin is annotation — a write token can already consume storage.
The moment pins prevent reclamation, unlimited pin creation is permanent-
retention authority, and a compromised write token could pin every commit and
defeat reclaim. Since roots ship with pins (above), this decision is due at
build time, not later: per-vault pin quotas at minimum, `ops:` gating or a
dedicated capability if quotas feel wrong.

## Surface inventory

Server:
- Pin store: JSON in the vault state dir, **owned by a per-vault mutex on
  `vaults.Instance`** — atomic rename alone (the `tokens.json` pattern) only
  prevents torn reads; two devices pinning concurrently would silently lose
  one update without the lock.
- Pins carry an immutable random **id**; `POST /v1/pins` creates, `DELETE
  /v1/pins/{id}` removes. Names are display text, not identity — two files may
  both have a pin called "sent", and names in URLs invite escaping bugs.
- `GET /{vault}/v1/pins?path=` — read scope.
- `/v1/history` gains bounded cursor pagination (`before=<commit>`, capped
  limit, `hasMore`) — today it defaults to 50 with no cursor and accepts an
  uncapped limit that walks the whole commit history. The modal clusters
  incrementally across pages.

Plugin:
- `client.ts` gains `history()` and `readAt()`. Path escaping is a correctness
  requirement, not a nicety: history paths go in query position, `/v1/at`
  paths segment-by-segment — the Go client already demonstrates the split
  (`client.go:415`). Materialise from the full hash; the 8-char form is only
  for the filename.
- A **separate** status-bar item (the existing one is the sync indicator and
  opens settings), with `file-open`/active-leaf listeners so the modal tracks
  the active note, and defined behaviour when that note is renamed or deleted
  while the modal is open.
- Maintenance section: vault-wide pins.

## Rollout hazards (found by review, easy to lose)

- **Already-tracked `.rev-*` paths strand.** If a matching file was committed
  before the exclusion ships, `Commit` thereafter ignores its modifications
  AND its deletion — HEAD keeps it forever and old clients keep syncing it.
  The release that adds the pattern must first commit the removal of matching
  tracked entries (working tree preserved), then enable the predicate. Needs
  an upgrade test with a matching path already in HEAD.
- **Predicate-version skew loops.** An old plugin pushing a `.rev-*` file to a
  new server gets a per-path `refused` result; the client counts it as pushed
  and rediscovers the same put every cycle, forever. The server must publish
  its exclusion pattern (on `/v1`), and the client must treat "excluded from
  sync" refusals as a terminal per-path state, not a retry.
- **Server-side `.rev-*` files are invisible unmanaged files.** The watcher
  drops them, `Check` suppresses them, nothing counts them; thousands of
  agent-created ones would silently tax every status scan. Diagnostics gain a
  local-only file count; cleanup semantics documented.

## Open when built

- Cluster gap: 30 min hardcoded first; a setting only if it annoys.
- Materialise collisions: suffix, never overwrite.
- The exact reserved pattern, chosen so no real note matches by accident, and
  stated in the README — matching files silently stop syncing.
- **Pins are pathname-scoped, decided.** Renaming `Draft.md` to `Final.md`
  orphans Draft's pins and history view — `History` filters by exact path and
  does not follow renames, and server-side diffs expose del+put. Following
  renames would need lineage metadata; out of scope, said out loud in the UI
  ("pins stay with the old name").
- Whether `Revision` grows origin/operation metadata so the modal can label
  merges and conflicts, instead of inferring from commit messages.
