# Revision Browser and Pins — Design Note

**Status:** Designed, not scheduled. Reviewed twice: sol (2026-08-21, 15
findings) against the original pins.json design, after which the pin mechanism
was replaced with named empty commits; then terra (2026-08-22, 7 findings)
against the replacement. The replacement's load-bearing claim held — prune
copies `Message: c.Message` verbatim (`prune.go:99`), so trailers genuinely
survive rewrites — and its remaining findings are folded in below.

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

**Pins are lines in a tracked `pins.jsonl` in the vault.** A pin is one JSON
object appended to a visible `pins.jsonl` at the vault root, committed through
the ordinary push path. The pin's snapshot is simply the tree of the commit
that added its line. Third design; each replaced its predecessor for cause:

- *pins.json in the state dir* (v1) died in review: hash-referenced pins do not
  survive prune's rewrite — `Resolve` never consults `pruneMap`
  (`history.go:114`) and the per-commit mapping is discarded (`prune.go:76`) —
  and the store needed a lock, an id scheme, and routes of its own.
- *Named empty commits* (v2) fixed identity — prune copies `Message` verbatim
  (`prune.go:99`, verified) — but needed `AllowEmptyCommits`, a dedicated
  `Reconciler.Pin`, trailer sanitisation (`Pin-Path` was an injection hole:
  `ValidPath` is only `filepath.IsLocal` and accepts newlines), and listing by
  decoding every commit object, cache required.
- *Tracked file* (v3, this one) makes pins ordinary data. Push, three-way
  merge, `/v1/history`, `/v1/at` and prune-liveness all apply with zero new
  machinery. Listing is one file read at HEAD. Rewrite-proof by LIVENESS —
  `pins.jsonl` is live at HEAD, prune never drops it — rather than by a
  message-copying property that had to be verified. Self-healing: the pin file
  has its own history, through the very feature it serves.

**JSONL, not a JSON array, deliberately.** An array append touches three lines
(entry, previous comma, closing bracket), so even non-overlapping edits collide
structurally. A JSONL append is one clean line: sequential pins from different
devices merge trivially, each pin is a one-line diff — so the entry-to-commit
mapping is literal in `history?path=pins.jsonl` — and a corrupt line is
skippable on read. Truly simultaneous appends still conflict under diff3 (no
format fixes both-append-at-EOF), which is why creation goes through
`POST /v1/pin` with the SERVER appending under the reconciler lock: the format
makes the file robust, the lock makes writes serial.

**Placement.** Visible `pins.jsonl` at the vault root — it cannot be a dotfile
(the server excludes dotfiles from sync), and Obsidian's explorer hides `.json*`
by default, so it is invisible in daily use while remaining plain data a person
can read and even repair.

**"Pin this version" still flushes first** and sends `expectedHead`: with the
15s debounce, a click could otherwise pin the server's older tree than the note
on screen. Under this design the push path's own staleness handling does the
rest.

**Pin authorization:** a pin is a put to one file; write scope is right by
construction. The retention question (pins as prune roots) is unchanged: prune
learns "keep paths live in the tree of each pin's commit" later, found via the
pin file's own history, and the quota/gating decision lands with that change.

## Surface inventory

Server:
- `POST /{vault}/v1/pin` — write scope; `{name, path?, expectedHead?}`. Appends
  one line to `pins.jsonl` and commits, under the reconciler lock, through the
  ordinary commit path — no empty-commit machinery, no trailers. Line content
  is JSON, so name/path need only JSON encoding; the newline-injection concern
  from the trailer design disappears (a newline in a JSON string is `\n`).
- A file pin requires the path to exist at HEAD, checked under the lock —
  otherwise the pin is born pointing at nothing.
- `GET /{vault}/v1/pins?path=` — read scope; reads `pins.jsonl` at HEAD and
  filters. One file read; skips unparseable lines rather than failing.
- Mapping a pin to its snapshot commit: `/v1/history?path=pins.jsonl` — each
  pin is exactly one added line in exactly one commit.
- Deleting a pin in v1 is removing its line — an ordinary edit, even doable by
  hand. (Unpinning only becomes prune-shaped work once pins are prune roots.)
- `/v1/history` gains bounded cursor pagination (`before=<commit>`, capped
  limit, `hasMore`) — today it defaults to 50 with no cursor and accepts an
  uncapped limit that walks the whole commit history. The modal clusters
  incrementally across pages.

Plugin:
- `client.ts` gains `history()`, `readAt()` AND `pins()` — file pins never
  appear in `/v1/history` (its `LogOptions.FileName` filter excludes a commit
  that touches nothing), so the modal interleaves two sources by timestamp:
  history clusters and the pin list. Without `pins()` the file modal cannot
  see its own pins at all.
- **Pinning flushes first.** "Pin this version" must flush and await sync
  before POSTing, and send `expectedHead` for `Pin` to compare under the lock
  — otherwise the debounce means a click can pin the server's OLDER tree, not
  the note the user is looking at, silently.
- Path escaping is a correctness
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
- Two prune/pin regression tests: trailer survives a rewrite of an unrelated
  path; and pruning the PINNED path preserves pin identity while its content
  becomes unavailable — that asymmetry should be documented behaviour, not a
  surprise.
