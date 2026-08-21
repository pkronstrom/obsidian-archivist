# Commit Granularity vs Push Latency — Design Note

**Status:** Resolved with the shipped client-side debounce (15s quiet / 30s
ceiling). Two server-side designs were explored, one to the point of a working
experiment, and both are recorded here with the reasons they lost — so they do
not get re-derived, and so the amend design's failure modes are on file before
anyone reaches for it again.

**Date:** 2026-08-21

---

## The problem

Two requirements pull apart:

- **Push latency must be ~zero.** The user finishes writing and immediately
  closes the app; the server must be current by then, because the other device
  is picked up moments later.
- **Commit granularity must be coarse.** Measured on the live vault: one
  50-minute drafting session produced **250 commits** — one per ~12 seconds,
  which is just the old 2-second debounce sampling natural typing pauses.
  `note_history` becomes useless for exactly the note being worked on.

A plain client debounce trades the first for the second. Everything below was
an attempt to get both.

## What shipped

`src/sync-schedule.ts`: a trailing debounce with a ceiling. The quiet timer
(default **15s**, configurable 1–29s) restarts on every edit; the ceiling
(**30s**, fixed) is armed by the first edit of a burst and never extended.
Whichever fires first syncs. Blur forces an immediate flush — closing the
window, switching apps, or backgrounding on mobile syncs at once, which is why
the finish-and-close pattern is not delayed at all. Obsidian's `quit` hook
flushes best-effort; the ceiling is the only real guarantee.

Arithmetic: 250 commits/50min becomes roughly 15–25. The residual staleness
window exists only while the user is still sitting in the note, idle, having
gone nowhere.

## Explored: amend the tip (rejected)

**The idea.** Push and commit synchronously exactly as today, but if the tip
commit was written by this same writer within a coalesce window, commit with
the *tip's parents* — an amend. The ref moves; the old tip becomes unreachable
but stays in the object store. Zero added latency, coarse history.

**What was proven.** A genuinely orphaned commit (no children, unreachable from
HEAD) remains fully servable by go-git: `Changes(orphan, head)` and
`Snapshot(orphan)` both work. Verified experimentally against `internal/repo`,
with the amend expressed as `wt.Commit(msg, &git.CommitOptions{Parents:
tip.ParentHashes})` — a one-argument change. So the design was *feasible*.

**Why it lost** (an adversarial review confirmed and extended these):

1. **Orphan cursors are leases, not durable.** A client's base survives only
   until a prune collects the orphan. Prune's translation map cannot help:
   translating is only sound when old and new heads have identical trees (the
   documented invariant at `repo.go:124`), and amended siblings differ by
   construction. Translating anyway produces silent data loss — a client based
   on orphan A, translated to sibling B, diffs `B..B` as empty and its push
   overwrites B's content without a merge.
2. **Collection funnels clients into re-bootstrap, and re-bootstrap has a known
   rough edge.** A stale-but-untouched device gets conflict copies of files it
   never edited (`sync.ts` keeps both sides because it has no common ancestor
   to merge through — itself a deliberate fix for a worse silent-overwrite
   bug). Rare today, because only a prune invalidates cursors; amending would
   make it routine.
3. **Partial pushes amend an acknowledged tip with partial content**, then
   return 500 with no notify. Recoverable while the orphan exists; funnels into
   (2) once it does not.
4. **"Same writer" is subtler than "same token".** Token labels are not unique,
   and two devices sharing a credential would amend each other's commits. The
   workable key is `(tokenHash, Origin.Device)` — available on every push with
   no protocol change, and forgeability is irrelevant because amending your own
   commits is not a security boundary. Residual gap: mobile device names
   default to bare "iPhone"/"Android phone", and an unconfigured plugin sends
   the literal string "device", so collisions are possible in principle.
5. **An undo back to the parent's exact contents is an empty commit**, which
   go-git rejects (`ErrEmptyCommit`) — needs an explicit case.
6. **It does not reduce disk growth.** All 250 commit/tree objects are still
   created; amending changes reachability, not storage. It converts permanent
   history into collectable garbage — worth nothing until safe collection
   exists (blocked; see the packing plan), and repack-only would not touch
   unreachable objects anyway.

Items 1–3 are the disqualifiers. 4–6 would have been fixable.

## Explored: history projection (viable, deferred)

The reviewer's counter-proposal, better than amending and held in reserve:

Keep the ref strictly append-only. Stamp each commit with an opaque
`History-Group` trailer; keep `{head, writerID, groupID, lastAt}` in memory and
reuse the group while the head matches, the writer matches, and the gap is
under the window. Then collapse adjacent same-group revisions in `History()` —
`note_history` shows one entry per burst, `raw=true` shows everything.

Cursors, merge bases, staleness checks, notifications, crash recovery and
reachability are all untouched: it changes a *projection*, not the source of
truth. Cost: ~a day on the read path. It does not clean the raw `git log`
(that needs a second, checkpoint ref — substantially more machinery) and does
not reduce disk.

## Verdict

The debounce already removes ~94% of the commits, and the finish-and-close
case was never delayed — blur flushes synchronously. The residual complaint is
a number, not an architecture. If `note_history` still reads badly at 15–25
commits per session, build the history projection; do not amend the ref.

Independent findings worth acting on someday, from the same review:

- The SSE handler reads the head *before* subscribing (`api.go`), leaving a
  missed-final-event race; `wait` does it in the right order.
- go-git's filesystem ref update truncates and rewrites in place — not
  temp-file-rename — so a kill mid-update can leave a malformed ref. Affects
  ordinary commits too.
- Re-bootstrap's conflict copies are recorded in `state.files` at the new head,
  so a *genuinely edited* file rescued during re-bootstrap is never uploaded;
  it stays local-only until touched again.
