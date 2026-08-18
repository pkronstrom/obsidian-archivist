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

Seven parts. 0 establishes where the guards sit, 1–2 are the write
guards, 3 is notification, 4–5 are a CLI subcommand, 6 is the
server-side complement to 5.

### 0. Both write paths, not just Push

The guards sit below `Push`, not inside it. There are **two** ways
content enters the repository and an earlier draft of this design
covered only one:

| Path | Entry | Used by |
|---|---|---|
| Remote | `Reconciler.Push` (`reconcile.go:110`) | devices, relay, agents |
| Local | `Reconciler.Scan` (`reconcile.go:441`), called by `watcher.go:217` | SilverBullet, memo-bridge, anything writing vault files on disk |

The local path never touches `Push`. Guarding only `Push` would leave
SilverBullet and memo-bridge — both of which write vault files
directly — able to grow the repository without limit. Counters are
therefore updated at the reconciler level and consulted by both
entries.

The two paths cannot respond the same way, because on the local path
the bytes are already on disk by the time the guard sees them:

- **Remote**: refuse the push with an error code. Nothing lands.
- **Local**: never refuse. Deferring is the only useful response —
  see part 2.

### 1. Per-path loop quarantine

The reconciler tracks writes per vault path in a rolling window.
More than **300 writes to the same path within 5 minutes** quarantines
the path: further writes to it are refused with a new protocol error
code `path_quarantined`, carrying the remaining cooldown. Everything
else in the vault stays writable. The quarantine clears after a
**15-minute cooldown** with no write attempts, and on server restart
(state is in-memory only — a loop that survives a restart re-trips it
within a minute, which is fine).

Counts *writes, never bytes*: a 9 MB attachment uploaded once trips
nothing; a bulk migration writing hundreds of distinct paths trips
nothing. A write only counts when the content actually changed, so
idle syncs and interval ticks cost nothing.

**Where 300 comes from.** Obsidian auto-saves without user action, and
the plugin debounces sync at `main.ts:15`:
`debounce(() => runSync(), 2000, true)`. `resetTimer: true` restarts
the timer on every edit, so a sync fires 2 s after typing stops. One
device therefore cannot exceed roughly one write per 2 s to a path —
about 150 per 5 minutes — and the 300 s interval backstop
(`settings.ts:19`) adds one idle tick per device. 300 clears the
two-device ceiling. A loop writes at network speed, orders of
magnitude above it.

An earlier draft of this design proposed 20, which would have
quarantined ordinary editing: a real observed two-device session
produced 13 commits in 3 minutes, and each commit may carry several
per-path changes. The counter counts per-path changes inside `Push`,
not commits.

This guard catches fast loops only. A slow loop — one write every ten
seconds — stays under the threshold and is left to the disk floor.
That is deliberate: a threshold low enough to catch slow loops would
catch human editing.

All three thresholds are configurable
(`ARCHIVIST_QUARANTINE_WRITES`, `ARCHIVIST_QUARANTINE_WINDOW`,
`ARCHIVIST_QUARANTINE_COOLDOWN`); the defaults above apply when unset.
Setting writes to `0` disables the guard.

#### Byte counters on the same window

Write count alone cannot see the case that matters most. A 9 MB
attachment rewritten every 10 seconds is 30 writes per 5 minutes —
comfortably under 300 — and 78 GB per day. So the same rolling window
carries a byte counter with two thresholds:

| Counter | Default | Env | Trips |
|---|---|---|---|
| Bytes, one path | 100 MB / 5 min | `ARCHIVIST_QUARANTINE_PATH_BYTES` | quarantines that path |
| Bytes, whole vault | 2 GB / 5 min | `ARCHIVIST_QUARANTINE_TOTAL_BYTES` | throttles all writes |

A legitimate single note never accumulates 100 MB of revisions in five
minutes. The vault-wide counter is the wide guard, for
many-paths-at-once traffic that no per-path counter can see. Both
disable at `0`.

**The per-path byte trigger requires at least 2 writes to that path
in the window.** Without this a single legitimate 120 MB attachment
would exceed a 100 MB threshold on its first upload and quarantine the
path immediately. One write is never a loop, whatever its size. Single
uploads are bounded by `protocol.MaxUploadBytes` (512 MB) and the
relay's own 32 MB cap, which are separate controls and unchanged here.

An oversized single file is already rejected **per file, not per
sync**: `putContent` (`api.go:147`) is a per-content endpoint keyed by
hash, so one file failing with `too_large` leaves every other file in
the same sync unaffected. Quarantine preserves that property — a
quarantined path blocks writes to that path only, and the rest of the
vault keeps syncing.

Bytes and writes share one window and one quarantine mechanism; the
byte counter is a second trigger, not a second system.

### 2. Free-disk floor

Before applying a push, the server checks free space on the vault
filesystem (`syscall.Statfs` on the vault root — vault and git dir
share the mount). Below **20 GB** (`ARCHIVIST_MIN_FREE_BYTES`,
`0` disables) the entire push is refused with error code `disk_low`,
naming the floor and the observed free space. Reads are unaffected.

20 GB against 146 GB free leaves ample room for the vault to grow
while still reserving enough that the server's other services do not hit a
full disk before archivist stops writing. This floor, not the
quarantine, is what bounds a slow loop or a hostile token holder.

#### Deferral on the local path

The local path cannot refuse: `watcher.go:217` sees changes that are
already on disk. Refusing to commit would not reclaim a byte, and
would silently diverge the vault from its history. So the local
response to any tripped threshold is to **defer** — widen the commit
cadence and let changes coalesce.

The mechanism for this already exists and does not need building.
`watcher.go` runs a debounce with a hard ceiling:

```go
maxDelay := 10 * debounce        // watcher.go:57
```

The comment above it records why the ceiling is there: *"A plain
debounce re-arms on every event, so a steady stream of writes -- an
rsync, a bulk import -- produced ZERO commits until the writes
stopped."* That bug was found and fixed once already; do not
reintroduce it by adding a second, uncapped debounce.

Under pressure the guard multiplies the existing `debounce` and
`maxDelay` (up to `ARCHIVIST_THROTTLE_MAX_DEBOUNCE`, default 60 s)
rather than adding a mechanism beside them. A loop rewriting one file
then costs one commit per minute instead of one per second, and only
the file's final state in each window becomes a blob — the
intermediate revisions are never stored. A legitimate bulk import
still completes, in fewer and larger commits.

Cadence returns to normal once the window falls below threshold.

#### Why the remote path is not deferred

`Push` commits synchronously and returns `newHead`
(`reconcile.go:190`), which the device stores as its next `Base`. No
commit means no head to return, so a deferred push cannot answer its
client. Coalescing there would require either inventing a SHA that
does not exist or blocking the caller until the window closed. The
plugin's own 2 s debounce already bounds push frequency, and the loop
risk on this path is an agent through the relay, which quarantine and
the byte counters address directly.

This is deliberately dumb. It is the one control that fires no matter
the shape of the problem — multi-path loops, a hostile token holder,
or something unanticipated — and its existence is why the other guards
do not need to be exhaustive.

### 3. ntfy notifications

A guard that fires while nobody is watching is a log line nobody
reads. When `ARCHIVIST_NTFY_URL` is set, the server posts a
notification on:

| Event | Priority | Tags |
|---|---|---|
| Path quarantined (writes or bytes) | high | `rotating_light` |
| Vault-wide throttle engaged | high | `rotating_light` |
| Free disk under the floor | urgent | `rotating_light` |
| Prune completed (from the CLI) | default | `floppy_disk` |

`ARCHIVIST_NTFY_URL` is a full URL including the topic, unset means
disabled.

**It is archivist's own variable and should point at its own topic**,
separate from the backup notifier's. The two are deliberately not
shared: restic posts on a schedule and its notifications are routine,
so a vault throttle landing in the same topic would be read as more
backup noise. Archivist's alerts are exceptional by construction — if
that topic pings, something is wrong.

The *shape* follows `lib/backup/notify.sh:10`, which the restic units
already use: a full URL rather than a bare topic, `Title`/`Priority`/
`Tags` headers, unset disables. Same convention, separate channel. A
topic-only variable would be a second convention for no gain, and
would not work against a self-hosted ntfy on a non-default host.

**Delivery is best-effort and must never affect a write.** Fire and
forget on a goroutine, 15 s timeout, all errors swallowed and logged
at debug. The restic notifier states the rule that applies here too:
a broken notifier must not turn a successful operation into a failed
one. A notifier that can fail a sync is worse than no notifier.

**Notifications are rate-limited independently of the guards.** A loop
tripping quarantine repeatedly must not produce a push per attempt: at
most one notification per event type per path per cooldown, with a
count of suppressed occurrences included when the cooldown expires.

### 4. `archivist reclaim` — the report

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

### 5. `archivist reclaim --prune [--older-than 90d]`

The same binary performs the rewrite in go-git — no Python, no git
binary, no host-side scripts. The image is `FROM scratch`, and the CLI
already ships as a host-runnable release asset; prune runs on the host
against the mounted git dir **with the server stopped** (the command
refuses to run if the repo lock is held). Stop-the-world is accepted:
this is a manual operation run a few times a year.

Mechanism: walk all commits in topological order; for each, build the
tree minus the pruned paths' historical blobs; recreate the commit
with rewritten parents; update refs. `--older-than` (default **90d**)
refuses to touch blobs deleted more recently, so recent deletions stay
recoverable via `restore` regardless of what is asked for.

**Garbage collection is part of prune, in this order:**

1. `Repository.Prune` (`prune.go:33`) deletes unreferenced objects —
   including everything the rewrite just orphaned.
2. `Repository.RepackObjects` (`repository.go:1761`) packs what
   remains into a single packfile.

Both verified present in go-git v5.19.2. The ordering matters and the
reason is not obvious: `Prune` only deletes **loose** objects
(`DeleteLooseObject`). Repacking first would be safe anyway, because
`createNewObjectPack` walks refs and packs only *reachable* objects
before deleting the loose copies it packed — unreachable objects stay
loose and remain prunable, and any packed garbage is dropped by the
next repack, since repack always rebuilds from reachable objects only.
Prune-then-repack is still preferred: less to walk, and one pack at
the end.

This is the only garbage collection the repository ever gets. go-git
never runs `gc`, the image is `FROM scratch` so `gc.auto` cannot fire,
and standalone repacking measured a 2% gain on the current repo. There
is therefore no separate repack timer: collection happens when you
prune, and prune is when there is something to collect.

Because pruned paths are already deleted at HEAD, the new HEAD's
*tree* is byte-identical to the old HEAD's tree; only commit SHAs
change. Prune finishes by appending one line — `<old-head> <new-head>`
— to `prune-map` beside the git dir, then prints both SHAs and the
bytes reclaimed.

After a prune, `history`/`show`/`restore` on a pruned revision report
`pruned` as a distinct state, not an error — it must read as
deliberate, not as corruption.

### 6. Head translation — no device re-bootstrap

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
| Path over write or byte threshold | `path_quarantined` | back off; retry after cooldown |
| Vault-wide byte threshold | `throttled` | back off; retry after cooldown |
| Free space under floor | `disk_low` | stop writing; alert the human |
| Base matches prune-map | (none — translated) | invisible |
| Base unknown, not in map | `unknown_base` (existing) | re-bootstrap (existing) |
| History of pruned blob | `pruned` state in response | display as pruned |

## Testing

- Quarantine: unit tests on the window/counter (trip, no-trip at
  threshold, cooldown expiry, distinct paths independent, disabled at
  0). The bulk-import case — hundreds of distinct paths in one push —
  must not trip. A single 120 MB write must not trip the per-path byte
  counter; the same bytes across three writes must. A quarantined path
  must not block writes to any other path.
- Both write paths: every guard asserted through `Push` AND through
  `Scan`/the watcher. A test that only exercises `Push` would have
  passed against the draft that left the local path unguarded.
- Deferral: under a tripped threshold the watcher's effective debounce
  widens and still respects a ceiling — assert commits keep landing
  under a continuous write stream, since an uncapped debounce produced
  zero commits in the bug recorded at `watcher.go:51`.
- Notifier: a failing or hanging ntfy endpoint must not fail, delay or
  alter any write. Assert suppression counts across the rate limit.
- Disk floor: fake the statfs seam; verify refuse-below / allow-above
  and that reads are unaffected.
- Reclaim report: repo fixtures with added/deleted/re-created paths;
  assert selection, sizes, and that live blobs are never listed.
- Prune: fixture repo → prune → assert old HEAD tree == new HEAD tree,
  reclaimed objects gone, `--older-than` respected, prune-map line
  written, repo passes `check`. Assert the repo ends with zero loose
  objects and one pack, so a regression that skips collection is
  caught rather than silently leaving the repo uncollected.
- Translation: push with old-head base → empty diff, no snapshot;
  push with an older base → `unknown_base` as today.
- End-to-end (manual, on the server against a copy): report → prune →
  restart server → both real devices sync without re-downloading.

## Rollout

Ships in the next release after implementation. `reclaim` is invisible
until run. No migration, and no device update: every guard and head
translation is server-side.

Every threshold is configurable from the environment, so tuning needs
a compose edit and a restart, never a rebuild:

| Env | Default | Effect |
|---|---|---|
| `ARCHIVIST_QUARANTINE_WRITES` | 300 | writes / path / window |
| `ARCHIVIST_QUARANTINE_PATH_BYTES` | 100 MB | bytes / path / window (needs ≥2 writes) |
| `ARCHIVIST_QUARANTINE_TOTAL_BYTES` | 2 GB | bytes / vault / window |
| `ARCHIVIST_QUARANTINE_WINDOW` | 5m | rolling window for all three |
| `ARCHIVIST_QUARANTINE_COOLDOWN` | 15m | how long a quarantine holds |
| `ARCHIVIST_THROTTLE_MAX_DEBOUNCE` | 60s | ceiling when deferring locally |
| `ARCHIVIST_MIN_FREE_BYTES` | 20 GB | free-disk floor |
| `ARCHIVIST_NTFY_URL` | unset | alert channel; unset disables |

`0` disables any individual counter. Defaults are active when unset,
so a deployment that changes nothing still gets the guards.
