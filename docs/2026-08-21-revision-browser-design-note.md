# Revision Browser and Pins — Design Note

**Status:** Designed, not scheduled. Reviewed adversarially (sol, 2026-08-21,
15 findings), then the pin mechanism was replaced outright — pins are named
empty commits, which dissolves the review's four largest findings instead of
mitigating them. See "Pins are named empty commits".

**Date:** 2026-08-21

---

## What it is

A status-bar **revisions icon** in the plugin, scoped to the active note.
Clicking it opens a modal listing that note's history as **sessions**, not
commits, with the user's own **pins** interleaved. Clicking an entry
materialises that version *beside* the original — `Note.rev-ae56b.md` — never
overwriting anything.

Two pin scopes, one mechanism:

- **File pin** — "pin this version" on the note you are looking at, named
  ("draft-sent-to-anna"). Shown in that file's modal.
- **Vault-wide pin** — a restore point for the whole vault, taken before a
  reorganisation. NOT shown in the file modal — it is not about that note. It
  lives in the settings tab's Maintenance section.

Pins mark **now**. There is deliberately no way to pin a historical revision:
no use case survived scrutiny (preserve an old version by materialising it —
then it is current content you can pin), and retroactive pinning was both the
worst UI in the design and the only fragile durability case.

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

**Pins are named empty commits.** A pin is a commit with the current tree and
a `Pin:` trailer (go-git: `AllowEmptyCommits`), plus `Pin-Path:` for a file
pin. This replaced a `pins.json` store after review, because identity-by-
message dissolves the store's four worst findings at once:

- *Rewrite-proof.* The review's critical finding was that hash-referenced pins
  die at the first prune — `Resolve` never consults `pruneMap`
  (`history.go:114`) and prune explicitly discards its per-commit mapping,
  keeping only the head pair (`prune.go:76`). A pin identified by its trailer
  rides through any rewrite; there is no hash to translate.
- *No store.* No `pins.json`, so no lost-update lock, no id scheme, no
  POST/DELETE routes, no name-escaping. Creating a pin is a push, serialised
  by the reconciler like every other commit; deleting one is out of scope for
  v1 (an unpinning rewrite is prune-shaped work).
- *Content-addressed durability for file pins.* The pinned version IS the pin
  commit's own tree. Nothing points backwards.
- *Chronology for free.* Pins interleave with sessions because they are
  commits.

Costs, accepted: each pin moves HEAD, so every device pulls one zero-change
diff; and `/v1/history` filters by path-touching commits, so listing pins is a
log scan for the trailer (a small `GET /v1/pins` doing that scan, read scope).

What this does NOT do: protect pinned content from pruning. Prune drops old
blobs of deleted paths regardless. Making "pinned" mean "never pruned" still
requires prune to learn the trailer — but "keep paths live in trees of
`Pin:`-trailed commits" is far simpler than the persistent hash-mapping the
store design demanded, and it can now genuinely ship later, because pin
IDENTITY no longer depends on it.

**Pin authorization, revisited under this design.** Creating a pin is
literally a commit, so write scope is exactly right by construction. The
retention-authority concern (a compromised write token pinning everything to
defeat reclaim) moves to the future prune-learns-pins change and is decided
there — quotas or `ops:` gating on what prune respects, not on who may
commit.

## Surface inventory

Server:
- `POST /{vault}/v1/pin` — write scope; `{name, path?}`. Commits an empty
  commit with `Pin: <name>` (and `Pin-Path: <path>` for a file pin). Name
  sanitised exactly like every other trailer value (`sanitiseTrailerValue`).
- `GET /{vault}/v1/pins?path=` — read scope; scans the log for the trailer.
- No delete in v1: unpinning is a history rewrite, prune-shaped work.
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
  does not follow renames. Following renames would need lineage metadata; out
  of scope, said out loud in the UI ("pins stay with the old name").
- The pin trailer scan's cost on a large history, and whether GET /v1/pins
  wants a small cache invalidated on commit.
- Whether `Revision` grows origin/operation metadata so the modal can label
  merges and conflicts, instead of inferring from commit messages.
