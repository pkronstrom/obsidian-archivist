# Multi-vault, pairing safety, and config sync — design

2026-08-18. Settled in a grilling interview; this document is the record.
Every decision below was put to the user and answered.

Three parts. They are independent enough to implement separately and should
be three plans, but they were designed together because part B is a
prerequisite for part C: enabling config sync on top of a broken pairing
path is how an empty vault's settings overwrite a real one.

---

## Part A — multi-vault

### The problem

One vault per process today, so Personal and Work mean two containers, two
tokens, two hostnames and two relays. Nothing lists what vaults exist, and an
agent holding one relay token cannot address a second vault even when that is
wanted.

### Decisions

**Collapse when the second vault arrives, not before.** With one vault the
change buys nothing. Adding Work is when the multiplication is paid for real.

**Discovery is the filesystem: `$ROOT/vaults/<name>`, flat.** Git state stays
at `$ROOT/.archivist/<name>`. This is the layout **as deployed on the server**,
where compose sets `ARCHIVIST_GIT=/data/.archivist/${VAULT_NAME}` — it is not
the binary's default, which is `/var/lib/archivist/git`
(`internal/config/config.go`). Multi-vault makes the per-vault layout the
server's own convention rather than a deployment convention, so that default
changes with it.

The layout is better than the earlier proposal of scanning `$ROOT` directly,
because `vaults/` is a dedicated container — there is no way `2. Areas` is mistaken
for a vault. Keep the "refuse to start if `$ROOT` itself contains `.git`"
guard anyway, for someone who points `ROOT` at a vault.

**Addressing is path-qualified: `/{vault}/v1/...`, with `GET /v1/vaults`.**
Subdomains keep the Caddy multiplication the change exists to remove; a
header is invisible in logs and curl and is the thing people forget. Only a
path makes a vault picker possible.

**No compatibility alias.** Cut over. This is cheaper than it sounds: every
plugin request goes through a single join point in the plugin
(`src/client.ts`, `url()` called only by `call()`), so a device switches vault
by editing one settings field. The server and the Go client each have one such
point too — three layers, one place each, which is the same fact stated per
layer rather than a different count. The plugin will
store **Server URL and Vault as two fields**, not one, because a picker needs
`GET /v1/vaults` at the server root and cannot reach it if the vault is baked
into the base URL.

An un-updated device 404s rather than corrupting anything, which is a visible
failure. Acceptable because both devices are the author's and BRAT updates
are easy; it would not be acceptable for a plugin with users.

**Per-vault tokens, and `GET /v1/vaults` returns only what your token opens.**
One mechanism yields both the picker and the authorisation check, with no
separate admin concept. A path/token mismatch is 403. This is the only option
that keeps isolation when an agent token leaks — the property a single shared
token silently destroys.

**One relay serving all vaults**, with `vault` as an optional tool parameter
defaulting to a configured vault, plus a `list_vaults` tool. Two relays would
mean registering two MCP servers in every client, and every tool schema is in
the agent's context on every request. Bounding `list_notes` cut that cost;
running two relays would hand it straight back.

**Vault creation from the plugin is allowed**, as a capability that is off by
default and never granted to relay or agent tokens.

This reverses a Round 1 decision, deliberately. The masterplan said
server-side vault creation stays out, because "an API that can create state
outside what it was configured with is a much larger surface than one that
reads and writes notes". On re-examination that objection aimed wider than
what is being built: a `POST /v1/vaults` that only creates a directory
*inside* `$ROOT/vaults/` cannot create state outside the configured root,
which is the property the reasoning protected. The residual risk — a leaked
token creating vaults endlessly — is bounded by the disk floor shipped in
v0.4.0 and by the limit below.

**`ARCHIVIST_MAX_VAULTS`, default 5.** Creation past the limit is refused;
discovery finding more logs a warning rather than failing, so raising the
value is always a way out. The number is a guard against mistakes, not
against an adversary.

**Vault names: one path segment, no leading dot, no `/`, no `..`, and reject
a name that collides with an existing vault after NFC normalisation.**
`this-vault-name`, `MyVault` and `My Own Vault` are all legal; the last is
URL-encoded (`My%20Own%20Vault`), which the plugin does automatically and a
human writing curl must remember. Document that URL-safe names are easier by
hand.

The NFC clause is not theoretical. macOS and iOS already disagreed about `ä`
in this vault, and `työ` created from a Mac against `työ` created from a phone
would be two directories that look identical and serve different content.

**A new vault directory does not need a restart.** Scan at startup, and
re-scan on `GET /v1/vaults` behind a short cache. A `readdir` is nearly free,
and it avoids the failure where a vault is rsynced in, nothing appears, and
nothing says why.

---

## Part B — pairing safety

### The hole

Traced through `sync.ts`. Every pairing combination is handled except one:

| Device | Server | Today |
|---|---|---|
| Empty | Empty | nothing happens ✅ |
| Empty | Populated | pulls everything — intended second-device onboarding ✅ |
| Populated, never synced | Empty | pushes everything — intended first bootstrap ✅ |
| **Populated, never synced** | **Populated** | **silent union of two unrelated vaults** ❌ |
| Already synced | *different* vault | refuses, touches nothing ✅ |
| Already synced | same vault, new host | works — state records the vault, not the host ✅ |
| Already synced | base predates a prune | re-bootstraps, or translates ✅ |

On first connect `state.base` is `""`, so `changes("")` returns the server's
entire contents as puts; the device writes them all, then pushes everything it
had. Both ends end up with the union and same-path collisions become conflict
files. Nothing warns.

The existing identity guard cannot catch it: it compares an *adopted* vault
name, and on first connect there is not one yet.

This is the case a person reaches by doing the obvious thing — "connect my
existing vault to my new server". Self-hosted LiveSync documents the same
hazard in its own domain, warning that enabling config sync before notes are
synced lets an empty vault's settings overwrite the real ones.

### Decisions

**Refuse, and name the choices.** Same shape as the identity guard: stop
before touching anything. Three explicit actions, none of them default —
**Adopt server**, **Publish local**, **Merge anyway**. Warning after the fact
is useless here, because the damage lands in the same cycle that would warn.

**Adopt server moves local files to `_archivist-rescued-<date>/` inside the
vault, then pulls.** Reversible, visible, needs no trust in a backup, and
because it lands inside the vault it syncs — so the rescued files reach the
other devices too. Deleting them is not an option for a tool whose pitch is
that git makes nothing unrecoverable. The date prevents a second adoption
colliding with the first.

**Publish local needs no rescue folder, and the asymmetry is the point.** A
push cannot destroy server content: every version of every server file is
already in git history. The local vault has no such safety net. Only the side
without a repository needs rescuing.

**The README gets a scenarios table**, not prose — the table above with a
recommended path per row. Getting started currently says install the plugin,
set URL and token, press Test connection, and says nothing about either side
already having content.

---

## Part C — config sync

### Scope

Requested explicitly: a vault whose snippets, themes and plugins differ per
device is a daily papercut. Sequenced **after part B**, because enabling it on
top of the pairing hole is exactly the failure LiveSync warns about.

### What the server already does — corrected

An earlier draft of this spec claimed a latent bug here: that a pushed
`.obsidian/` path is written to disk and then silently not committed. **That is
false, and the correction matters because it changes the work.**

Dotfiles are already refused, twice:

- `Reconciler.applyOne` (`internal/reconcile/reconcile.go:219`) calls
  `vault.Skip(ch.Path)` **before any write**, for every op, and returns
  `StatusRefused` with reason `"excluded from sync"`. Nothing is written and
  the client is told.
- `Repo.Commit` (`internal/repo/repo.go:199`) filters non-syncable paths at
  staging, as a backstop. `syncable` is *also* consulted at
  `internal/repo/history.go:96`, to keep excluded paths out of drift
  reporting — so "only consulted when staging" was wrong too.

So config sync is not "fix a bug, then relax the rule". It is **widening an
existing, correct refusal**: `syncable` gains a narrow allowlist for specific
`.obsidian/` paths, and `applyOne` consults the same predicate so the refusal
and the allowance can never disagree. There is no prerequisite fix.

### Decisions

**Config lives in-vault at `.obsidian/`**, with the allowlist enforced in the
plugin *and* in the server's `syncable`. Only there do files land where
Obsidian looks with no translation step. Double enforcement means an older or
buggy plugin cannot push `data.json` into history. A separate vault would be
architecturally cleaner but couples this to part A landing first.

**Categories as the UI, an allowlist as the implementation**, mirroring
Obsidian Sync's own vocabulary — Main settings, Appearance, Themes and
snippets, Hotkeys, core plugin list and settings — because that is the
vocabulary the user already knows from Obsidian's settings screen.

Allowlist rather than denylist, because the failure modes are asymmetric: a
missed allowlist entry means a theme does not travel, a missed denylist entry
means a credential is in git history permanently.

| Syncs | Never syncs |
|---|---|
| `snippets/*.css` | `workspace.json`, `workspace-mobile.json` |
| `themes/*/` | `plugins/*/data.json` unless opted in per plugin |
| `hotkeys.json`, `appearance.json`, `app.json` | `graph.json` |
| `community-plugins.json`, `core-plugins.json` | caches (`.smart-env`, and anything unlisted) |

Obsidian's config *file names* have been stable for years, and whole files are
synced, so schema drift inside them does not matter. A future config file we
do not know about simply does not sync — the allowlist fails in the safe
direction.

**Three states, per device, never synced:** `Files only` (default, today's
behaviour) · `Files + appearance` · `Files + appearance + plugins`. Obsidian
keeps its own sync settings per device and never syncs them, for the same
reason: a synced setting fights across devices.

Per-plugin device selection is **not** needed. Plugin manifests carry
`isDesktopOnly` and mobile Obsidian refuses to load those, so the installer
skips them on mobile automatically. Finer granularity is deferred until that
proves insufficient.

**Allowlisted JSON is merge-exempt: whole file, last writer wins, always with
a conflict copy.** Archivist merges text with diff3; a text-merged settings
file can come out syntactically valid and semantically wrong, which is the
worst failure shape because nothing reports it and Obsidian behaves oddly
later.

Key-level merge, as Obsidian does it, is the better answer and is scoped
during implementation: walking base/ours/theirs by key is straightforward, and
with the rule **arrays replace wholesale, never merge** it is roughly 60–100
lines plus tests. Ship it if it lands in that range; otherwise whole-file
stands and the upgrade stays localised to one place in the merge path.

**Plugin code: sync the list, install on arrival.** Obsidian Sync syncs only
the list and installs nothing, which is why third-party plugin managers exist —
so shipping the list alone produces a to-do list, not a synced setup.

Shipping the code is the worst thing to put in this history: a realistic
15-plugin set is several MB, and every plugin update rewrites `main.js`
wholesale as a fresh blob. That is roughly 50 MB a year, unrecoverable except
by prune, and it would make plugin churn the dominant driver of vault growth.
Building reclaim in v0.4.0 and then introducing the largest available source
of binary churn would be perverse.

On the receiving device: read the synced list, diff against installed, skip
`isDesktopOnly` on mobile, fetch the rest, write into `.obsidian/plugins/<id>/`,
enable. **Always behind a confirmation** — this downloads code from the
internet at the direction of another device, and if someone got write access
to the vault they could add an id to that list. On failure, list what is
missing and let the user install manually, which is no worse than Obsidian
Sync.

**Plugin data: per-plugin opt-in, default off, with a scanner that refuses
rather than strips.**

A filter that strips suspected secrets was proposed and rejected, for three
reasons. Detectors cannot be right on arbitrary plugin JSON — a credential in
`"endpoint": "https://user:pass@host"`, a JWT under `"config"`, or a key the
author called `"k"` all evade name matching. A filtered `data.json` is a
broken file that looks fine: the plugin reads valid JSON with a hole and
re-prompts, silently resets, or throws. And the real harm is false
confidence — a feature that promises secrets are filtered will be enabled
everywhere and then stop being thought about.

The asymmetry is unforgiving: a false positive costs a setting, a false
negative puts a live key in a git history permanently, and nothing reports it.

So the scanner keeps its detection and loses its edit: on suspicion it
**refuses that plugin and says what it saw**, with a per-plugin override that
states what is being accepted. The user still stops having to know which
plugins hold secrets, but there are no files with holes and no promise that
cannot be kept.

Keep it minimal and modular — its own module, one entry point, removable in a
single commit if it proves not to earn its place.

**Archivist's own `data.json` is excluded unconditionally and cannot be opted
in.** This is not a default; it is a hard exclusion with no override.

The plugin persists its settings with `saveData`, so
`.obsidian/plugins/archivist/data.json` holds `serverUrl` and the **bearer
token** (verified: keys are `device, intervalSeconds, serverUrl, syncOnChange,
token, watchRemote`). Under a generic per-plugin opt-in, ticking Archivist
would commit the server's own credential into the vault it protects, sync it to
every device, and place it in git history permanently — recoverable only by
noticing and then pruning.

A scanner would very likely catch a key called `token`. Relying on that is the
mistake: this one is knowable in advance, so it is excluded by name rather than
left to a heuristic. The general lesson holds beyond this plugin — a sync tool
must never be able to sync its own credentials.

Better still, and worth doing in the same change: split the plugin's
credentials out of `saveData` into device-local storage, so the file being
opted into does not contain them at all.

---

## Sequencing

1. **Part B, pairing safety** — a data-loss bug, and a prerequisite for C.
2. **Part C, config sync** — the largest piece.
3. **Part A, multi-vault** — when a second vault is actually wanted.

An earlier draft had a fourth item first, fixing a non-syncable-path bug that
turned out not to exist. It is gone; see Part C.

Part A is last not because it is hard. URL construction is already centralised
at every layer that needs changing — one method in the plugin
(`src/client.ts`, `url()` used only by `call()`), one join in the Go client
(`internal/client/client.go:81`), and one route table on the server
(`internal/api/api.go`) — so it is far smaller than the masterplan's "17
handlers, 21 call sites" suggests. It is last because nothing needs it until
Work exists, and collapsing later is cheap: the plugin records which *vault* it
adopted, not which host.

## Non-goals

- **Syncing `workspace.json` or plugin caches.** Per-device by nature.
- **Automatic plugin installation without confirmation.** Code from the
  internet, directed by another device.
- **A secret-stripping filter.** See above.
- **Subdomain or header-based vault addressing.**
- **Server-side vault creation by agent or relay tokens.**
- **Syncing Archivist's own plugin data.** Hard-excluded; see Part C.
