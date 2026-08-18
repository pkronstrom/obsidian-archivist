# Vault size guards — design

2026-08-18. Approved in conversation; this document is the record.

## Problem

Nothing bounds vault growth. The per-request caps (512 MB server,
32 MB relay) bound a single upload and nothing else. Three unbounded
paths exist:

1. **A write loop.** A buggy client or agent rewriting the same note
   forever. Git keeps every revision, and for binary content every
   revision is a full copy — the vault can stay small while
   `.archivist/` grows without bound.
2. **Disk exhaustion by any means.** Archivist shares the server's disk with
   every other service. Nothing checks free space before a write.
3. **Deleted attachments.** A 9 MB PDF added and then deleted costs
   9 MB forever. Deletion reclaims nothing, and there is no tool to
   see or recover the waste.

Measured 2026-08-18 on the live `personal` vault: 63 MB vault, 58 MB
git dir, 321 files, 27 commits, 413 loose objects, zero packs (go-git
never runs `gc`). A full `git gc --aggressive` on a clone reclaimed
2% — the content is mostly already-compressed PDFs with no revision
churn yet. So today's git dir is not bloat; the guards exist for the
growth paths above, not for the current state.

## Non-goals

- **Per-token write ceilings.** The server has exactly one bearer token
  (`Config.Token`); every device, the relay and every agent share it.
  Ceilings per identity require multi-token auth, which is its own
  design. The self-declared `Device` field is not an identity and must
  not be treated as one.
- **AUTO_PRUNE.** Unattended history rewriting, and it destroys exactly
  the recoverability that history exists to provide. Prune is manual.
- **Excluding attachments from git.** Git is the entire state store; a
  file outside git is not synced. That would be a separate
  content-addressed store — a feature, not a flag, and unjustified at
  63 MB against 146 GB free.
- **Squashing burst commits.** Commit objects are a few hundred bytes;
  13 burst commits cost ~nothing. Rewriting history to merge them
  invalidates device bases for no material gain.

## Design

Five parts. 1–2 are server-side write guards, 3–4 are a CLI
subcommand, 5 is the server-side complement to 4.

### 1. Per-path loop quarantine

The reconciler tracks writes per vault path in a rolling window.
More than **20 writes to the same path within 5 minutes** quarantines
the path: further writes to it are refused with a new protocol error
code `path_quarantined`, carrying the remaining cooldown. Everything
else in the vault stays writable. The quarantine clears after a
**15-minute cooldown** with no write attempts, and on server restart
(state is in-memory only — a loop that survives a restart re-trips it
within a minute, which is fine).

Counts *writes, never bytes*: a 9 MB attachment uploaded once trips
nothing; a bulk migration writing hundreds of distinct paths trips
nothing. The observed legitimate worst case (a two-device editing
burst) was 13 commits in 3 minutes, under the threshold — but note
those were sync commits, each of which may carry several changes; the
counter counts per-path changes inside `Push`, not commits.

All three thresholds are configurable
(`ARCHIVIST_QUARANTINE_WRITES`, `ARCHIVIST_QUARANTINE_WINDOW`,
`ARCHIVIST_QUARANTINE_COOLDOWN`); the defaults above apply when unset.
Setting writes to `0` disables the guard.

### 2. Free-disk floor

Before applying a push, the server checks free space on the vault
filesystem (`syscall.Statfs` on the vault root — vault and git dir
share the mount). Below **5 GB** (`ARCHIVIST_MIN_FREE_BYTES`,
`0` disables) the entire push is refused with error code `disk_low`,
naming the floor and the observed free space. Reads are unaffected.

This is deliberately dumb. It is the one control that fires no matter
the shape of the problem — multi-path loops, a hostile token holder,
or something unanticipated — and its existence is why the other guards
do not need to be exhaustive.

### 3. `archivist reclaim` — the report

New CLI subcommand beside `history`/`show`/`restore`/`check`/`export`,
operating on the git dir directly. Read-only. Walks history and lists
every blob whose path is **deleted at HEAD**:

```
PATH                                    SIZE     ADDED       DELETED     GONE
5. Sources/Attachments/old-scan.pdf     9.2 MB   2026-06-01  2026-07-14  35d
...
total reclaimable: 41.3 MB in 12 files
```

Selection is by deleted-at-HEAD only — not by folder or extension.
The attachments directory is a convention that will drift; "large and
gone" is the property that matters, wherever the file lives. A path
deleted and later re-created counts only the revisions before the
re-creation, and only if their blobs are unreferenced by any live
revision. `--json` emits the same data for tooling. `--min-size` and
`--older-than` filter the listing.

The report is the decision input for pruning: run it, look at the
number, and stop there if the number is small.

### 4. `archivist reclaim --prune [--older-than 90d]`

The same binary performs the rewrite in go-git — no Python, no git
binary, no host-side scripts. The image is `FROM scratch`, and the CLI
already ships as a host-runnable release asset; prune runs on the host
against the mounted git dir **with the server stopped** (the command
refuses to run if the repo lock is held). Stop-the-world is accepted:
this is a manual operation run a few times a year.

Mechanism: walk all commits in topological order; for each, build the
tree minus the pruned paths' historical blobs; recreate the commit
with rewritten parents; update refs; then prune unreferenced objects
and repack (`Prune` + `RepackObjects` — this is also where the loose-
object accumulation from go-git's no-gc life gets collected, so no
separate repack timer exists). `--older-than` (default **90d**)
refuses to touch blobs deleted more recently, so recent deletions stay
recoverable via `restore` regardless of what is asked for.

Because pruned paths are already deleted at HEAD, the new HEAD's
*tree* is byte-identical to the old HEAD's tree; only commit SHAs
change. Prune finishes by appending one line — `<old-head> <new-head>`
— to `prune-map` beside the git dir, then prints both SHAs and the
bytes reclaimed.

After a prune, `history`/`show`/`restore` on a pruned revision report
`pruned` as a distinct state, not an error — it must read as
deliberate, not as corruption.

### 5. Head translation — no device re-bootstrap

Devices remember the commit they last synced (`Base`) and send it with
every push. A rewrite invalidates every historical SHA, so without
help the next push gets `unknown_base` → full re-bootstrap from
`/v1/snapshot`.

The server loads `prune-map` at boot. When a push (or pull/diff
request) presents an unknown base that matches a recorded old head,
the server substitutes the corresponding new head and proceeds. In the
steady state every device's base *is* the old head, the substituted
diff is empty, and nothing re-downloads — the device's next successful
sync updates its bookmark and the entry is never needed for that
device again.

A device whose base was *behind* HEAD at prune time (offline across a
prune) misses the map and falls back to the existing re-bootstrap.
That is rare, self-healing, and correct — it may genuinely need the
deletion diffs. The map is one line per prune event, ever; it is
covered by any backup that covers the git dir's parent.

## Error handling summary

| Condition | Code | Client behaviour |
|---|---|---|
| Path over write threshold | `path_quarantined` | back off; retry after cooldown |
| Free space under floor | `disk_low` | stop writing; alert the human |
| Base matches prune-map | (none — translated) | invisible |
| Base unknown, not in map | `unknown_base` (existing) | re-bootstrap (existing) |
| History of pruned blob | `pruned` state in response | display as pruned |

## Testing

- Quarantine: unit tests on the window/counter (trip, no-trip at
  threshold, cooldown expiry, distinct paths independent, disabled at
  0). The bulk-import case — hundreds of distinct paths in one push —
  must not trip.
- Disk floor: fake the statfs seam; verify refuse-below / allow-above
  and that reads are unaffected.
- Reclaim report: repo fixtures with added/deleted/re-created paths;
  assert selection, sizes, and that live blobs are never listed.
- Prune: fixture repo → prune → assert old HEAD tree == new HEAD tree,
  reclaimed objects gone, `--older-than` respected, prune-map line
  written, repo passes `check`.
- Translation: push with old-head base → empty diff, no snapshot;
  push with an older base → `unknown_base` as today.
- End-to-end (manual, on the server against a copy): report → prune →
  restart server → both real devices sync without re-downloading.

## Rollout

Ships in the next release after implementation. Server-side guards
are inert until the env vars are read (defaults on), and `reclaim`
is invisible until run. No compose changes required beyond optional
threshold overrides; no migration; no device update needed — head
translation is entirely server-side.
