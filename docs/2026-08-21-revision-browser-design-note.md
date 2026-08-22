# Revision Browser and Pins — Design Note

**Status:** Buildable spec. Reviewed three times (sol 2026-08-21; terra
2026-08-22 twice). The pin mechanism was replaced twice in response — store →
empty commits → tracked file — and the third review's "not buildable" findings
are resolved below. Rewritten clean, because successive edits had left the
note contradicting itself about paths and filenames.

**Scope note:** alpha, one user, server and plugin ship together. Version
skew, protocol negotiation and migrations are deliberately out of scope.
Concurrency between the user's own devices is in scope.

**Date:** 2026-08-21, rewritten 2026-08-22

---

## What it is

A status-bar **revisions icon** scoped to the active note. It opens a modal
listing that note's history as **sessions**, with **pins** interleaved.
Clicking an entry materialises that version *beside* the original as
`Note.ae56b1c.local.md` — never overwriting anything.

## 1. The `.local` namespace

**Rule:** anything marked `.local` is NEVER synced, by either side, for anyone.
Two shapes, one idea:

- **File:** basename with ≥3 dot-separated segments and `local` second-to-last
  (`Scratch.local.md`), case-sensitive.
- **Directory, or an extensionless file:** any path segment ENDING in `.local`
  (`Journal.local/`), which takes the whole subtree with it.

A folder must end in `.local` rather than use the file spelling because
`project.local.assets` matches the file grammar by coincidence. Excluding that
would strand every note inside a tree nobody meant to hide — and inconsistently,
since a startup walk that only skips dot-directories descends into it anyway.
Traversal therefore uses the directory rule alone (`LocalOnlyDir`/`localOnlyDir`),
never the file grammar.

This is a **user-facing feature**, not a private marker: mark any file
`.local.md` yourself and it stays on that device. Scratch notes, machine-
specific captures, drafts you do not want on the phone.

- Evaluated **first** in both predicates, before dotfile and config rules.
- The exempt pin file is matched by exact path, so `pins.local.jsonl` is NOT
  exempt — the `.local` rule wins, always. Precedence is stated because the
  two rules are otherwise symmetric-looking.
- Implemented in BOTH `skip()` (`src/sync.ts`) and the Go predicate
  (`internal/vault`). Neither currently excludes any non-dot file, so this is
  new code on both sides, and it needs the same grammar in both.
- **Discoverability is the safety mechanism.** A file that silently stops
  syncing must be documented where it is read: a line in the plugin settings
  sync section, and the README. Not a tooltip.
- Checked against the live vault 2026-08-22: zero existing `*.local.*` files,
  so nothing strands. (`Commit` ignores an excluded path's DELETION too, which
  would make a pre-existing match unremovable.)
- Server-side marked files are invisible to the watcher, `Commit` and `Check`.
  Diagnostics gain a local-only file count so they cannot accumulate unseen.

**Materialisation is the only writer of `.local` files**, and restoring is
renaming the marker away: the file stops matching, reads as a new note, syncs
normally. There is no in-place restore code path anywhere.

Collision algorithm (not "suffix, never overwrite" — that is not an
algorithm): target is `<base>.<7-char hash>.local.<ext>`. If it exists and its
content is byte-identical, open it and write nothing. Otherwise append `-2`,
`-3`, … before `.local` until free.

Rename-to-restore was traced through the sync engine and works: the excluded
source is neither listed by `listAll` nor emitted as a deletion (skipped state
entries are forgotten), so `pairRenames` sees one clean `put` and cannot
misfire as a move. Needs a regression test asserting exactly one put, zero
dels, zero moves.

## 2. Pins

**A pin is a line appended to `pins.jsonl` in the vault root.** An ordinary
tracked file: push, three-way merge, `/v1/history`, `/v1/at` and prune
liveness all apply with no new machinery, and listing is one read at HEAD.

Rejected placements and why (do not re-derive): a store in the vault state dir
died because hash references do not survive prune — `Resolve` never consults
`pruneMap` (`history.go:114`) and the per-commit mapping is discarded
(`prune.go:76`). Named empty commits fixed identity but needed
`AllowEmptyCommits`, trailer sanitisation (`Pin-Path` was an injection hole:
`ValidPath` is only `filepath.IsLocal` and accepts newlines) and a cached
full-log scan. `.obsidian/plugins/obsidian-archivist/pins.jsonl` was chosen
briefly and rejected on review: `configSyncable` returns false at
`level: "files"` before any path check (`config-sync.ts:72`) and `listAll`
descends the config dir only via `mayHoldConfig` (`sync.ts:530`), so it needs
three coordinated exemptions, and missing the descend one makes a pulled pins
file read as a DELETION. The root file needs none of that, and is invisible in
Obsidian's explorer anyway (`.jsonl` is not a recognised extension).

**Entry shape:** `{id, name, path?, created}`. `id` is a server-generated
opaque random string. **No commit hashes** — a stored hash resurrects the
prune problem. JSONL rather than a JSON array because an array append touches
three lines (entry, previous comma, closing bracket) and collides structurally
even when edits do not overlap; a JSONL append is one clean line, and a corrupt
line is skippable on read.

**Snapshot resolution.** A pin's snapshot is the tree of the commit that first
introduced its `id`. The earlier note claimed this fell out of `/v1/history`
for free; it does not — `History` returns commit metadata, not line diffs. So
the server owns it: walk the pins-file history oldest-first, read the blob at
each revision, and map each `id` to the first commit whose blob contains it.
Many ids may map to one commit (a manual edit can add two lines at once), which
is fine — the mapping is id→commit, never commit→id. Entries are immutable
except removal, which is what makes first-appearance a stable identity. The
scan sits behind a HEAD-keyed cache.

**All mutations go through the server; direct edits are unsupported.**
`POST /v1/pin` appends under the reconciler lock. Hand-editing the file is not
a documented path — it will not corrupt anything, but ids and history scanning
are the server's business. Conflict copies (`pins.conflict-<device>-<hash>
.jsonl`) sync normally like any root file, so a losing side is visible and
recoverable by hand; this was a real hole in the `.obsidian` placement, where
the conflict copy would have been excluded and lost.

**Concurrency.** `expectedHead` is REQUIRED, not optional. `Reconciler.Pin`
holds `rc.mu`, verifies HEAD matches, validates a file pin's path exists at
that HEAD, appends, commits, notifies, and returns the new head. Mismatch is
`409` with a stable code; the plugin flushes, refreshes head, and retries once
before surfacing an error. API code cannot reach the mutex directly, which is
why this is a reconciler method rather than a handler.

**Pin scopes — file pins first.** File pins (`path` set) show in that note's
modal and are the primary feature: they are what the revision browser is for,
and what gets built and shipped first. Vault-wide pins (`path` absent) are
restore points for the whole vault, live in the settings Maintenance section,
and are deliberately NOT interleaved into a note's modal because they are not
about that note. Same route, same file, same id scheme — the scope is just
whether `path` is set — so vault-wide pins are a small addition afterwards
rather than a parallel design.

**Pins mark now.** There is no way to pin a historical revision: no use case
survived scrutiny (preserve an old version by materialising it, then pin the
result), and it was both the worst UI in the design and the only fragile
durability case.

**Pinning flushes first.** With the 15s debounce a click would otherwise pin
the server's older tree than the note on screen.

**Authorization:** a pin is a put to one file, so write scope is right by
construction. The retention question — pins as prune roots, where unlimited
pin creation becomes permanent-retention authority — lands with that future
change, not here.

## 3. History and clustering

Clustering is client-side at render time: a burst with ≥30 min gap to the next
is one session. The list is **newest-first**, so the fold runs against
descending order and each cluster's representative is its **newest non-deleted**
revision — `History` marks deletion revisions and `/v1/at` necessarily 404s on
them, so a deleted representative would make the whole session unopenable.
Deletion rows render as events, not openable entries.

`/v1/history` needs bounded pagination before this ships: today it defaults to
50, has no cursor, no `hasMore`, and accepts an uncapped limit that walks the
entire commit history (`api.go:719`). Spec: `before=<commit>` exclusive,
`limit` capped at 200, response `{revisions, hasMore, next}`. An unknown
`before` is a 400, not an empty list. The modal clusters incrementally and
keeps the boundary item across pages.

**A pin whose content is gone.** Prune can drop the blobs of a deleted path
while the pin entry survives. `GET /v1/pins` therefore returns `available` per
pin (resolved snapshot exists and contains the path). Unavailable pins render
greyed with their name and date, and are not clickable — the pin is a record
that something existed, and saying so is better than a 404 on click.

## 4. Surface inventory

Server:
- `POST /{vault}/v1/pin` — write scope; `{name, path?, expectedHead}` → `{id,
  head}`. 409 on head mismatch, 404 if a file pin's path is absent at HEAD.
- `GET /{vault}/v1/pins?path=` — read scope; `{id, name, path, created, commit,
  available}`. Cached by HEAD.
- `/v1/history` pagination as above.
- `Reconciler.Pin` as above.
- `.local` exclusion in the Go predicate + a local-only count in diagnostics.

Plugin:
- `client.ts` gains `history()`, `readAt()`, `pins()`, `pin()`. Path escaping
  is a correctness requirement: history paths in query position, `/v1/at`
  paths segment-by-segment (the Go client shows the split, `client.go:415`).
  Materialise from the full hash; the 7-char form is only for the filename.
- `.local` exclusion in `skip()`, applied before existing rules.
- A **separate** status-bar item (the existing one is the sync indicator and
  opens settings), with `file-open`/active-leaf listeners; defined behaviour
  when the active note is renamed or deleted while the modal is open.
- Settings: the `.local` rule stated in the sync section; vault-wide pins in
  Maintenance.

Out of scope: in-place restore, diff view, historical-revision pinning, unpin
(removing a line is a future route; hand-editing works meanwhile), `ops:`
gating. On a protected vault the normal `vault:` step-up gate still applies to
every route here, exactly as it does to reading a note.

## 5. Build order

1. `.local` predicate, both sides, with the rename-to-restore regression test.
2. `/v1/history` pagination.
3. `pins.jsonl`: `Reconciler.Pin`, the two routes, the id→commit scan + cache.
4. Plugin: client methods, status-bar item, modal (history + FILE pins).
5. Vault-wide pins in the settings Maintenance section — last, and skippable
   if the file-pin half proves enough in practice.
