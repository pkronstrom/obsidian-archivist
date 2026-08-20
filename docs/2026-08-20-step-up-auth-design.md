# Step-Up Authentication — Design

Phase 3 of the scoped-tokens work. Phases 1 (scoped tokens) and 2 (relay
pass-through, `move`, provenance) shipped in v0.6.0.

**Goal:** an agent holding a valid token cannot read or change a protected vault
until a human proves presence with a one-time code, and a destructive operation
asks again every time.

**Not the goal:** encryption. See ADR-0002.

---

## The model in one paragraph

A vault is *protected* when it carries a marker. A token that opens a protected
vault must carry a **recorded decision** about it — either a posture naming a
capability, or an explicit exemption — and a token carrying neither is refused
at the gate, not waved through. Proving presence means presenting a TOTP code
computed from a secret held by that token alone. Access produces a **grant**: a
window during which that token may use that vault. A destructive operation
produces no grant at all: one code, one operation.

## Why both a vault marker and a token field

Neither half alone expresses what is wanted.

Token-only fails **open**: mint a token for `work` a year from now, forget the
flag, and it is ungated with nothing to notice. The failure of a control whose
job is to prevent unobserved access must not itself be unobservable.

Vault-only fails the actual deployment: one token is supposed to sync `work`
freely from a laptop while an agent token is gated on the same vault at the same
moment. A vault that gates everyone cannot express that.

So the vault marker forces the question, mint refuses to guess the answer, and
the gate refuses a token that never answered it.

That last clause is load-bearing and was missing from the first draft of this
design. A mint-time refusal only binds tokens minted *after* the marker exists.
Every token that predates the marker would have sailed through ungated, which is
precisely the silent fail-open that ruled out the token-only model. The decision
must therefore be **recorded on the principal** and absence must **deny**.

## Data

### The vault marker

An empty file at `$ROOT/.archivist/<vault>/step-up`. It sits with that vault's
other state, outside the vault, so it is never synced and it travels with the
vault when the vault is moved or deleted. It holds no secret — presence is the
whole content.

### The principal

```json
{
  "label": "agent-claude",
  "vaults": ["personal", "work"],
  "scopes": ["read", "write"],
  "totpSecret": "JBSWY3DPEHPK3PXP",
  "requiresStepUpAuth": ["vault:work", "ops:work"],
  "stepUpExempt": []
}
```

`stepUpExempt` lists protected vaults this token deliberately does not gate. It
exists so that "decided not to gate" is distinguishable from "never asked",
because only the second may be denied. A token opening a protected vault with
the vault in neither list is refused with `step_up_required` and a message
saying it predates the marker and must be re-minted.

`totpSecret` is base32, stored reversibly because HMAC needs it (ADR-0001). One
secret per token, not per vault: the holder is one person with one authenticator,
and per-vault secrets would mean an enrollment per vault for no security gained.

`requiresStepUpAuth` is a flat list validated against a catalog, exactly as
`scopes` is validated by `ValidScope`. Phase 3 ships two kinds:

| entry | meaning | lifetime |
|---|---|---|
| `vault:<name>` | reading or writing this vault needs a code | grant |
| `ops:<name>` | a destructive operation on this vault needs a code | one-shot |

Adding `op:reclaim` or `op:settings` later is a catalog entry plus a call site.
No format change, no re-enrollment, no protocol bump.

### Postures this expresses

| token | entries | behaviour |
|---|---|---|
| agent over MCP | `vault:work`, `ops:work` | must unlock `work`; `personal` is unprotected so never gated |
| work Mac Obsidian | `ops:work` | syncs `work` freely; a prune triggered from the plugin asks |
| relay background | `stepUpExempt: ["work"]` | reads `work` with no code; refused a secret entirely, since it runs with no human present |
| minted before the marker | neither | **denied**, with a message naming the re-mint |

## Minting

`token add` refuses to issue a token that opens a protected vault without saying
what it intends:

```
work requires an explicit step-up posture.
Pass -step-up vault:work[,ops:work], or -no-step-up work to opt out deliberately.
```

`-no-step-up work` records `work` in `stepUpExempt`. It is written down rather
than merely permitted, because the gate cannot otherwise tell a deliberate
decision from a token that predates the marker — and it must deny the second.

Minting also **requires** a readable vault root. Without one the protection check
cannot run, and a check that silently skips itself when a variable is unset is
not a check.

A token carrying any `requiresStepUpAuth` entry gets a generated `totpSecret`.
`token add` prints it once, as a ready-to-run command rather than a QR:

```
Label:      agent-claude
Profile:    mcp-client
Vaults:     work, personal
Scopes:     read, write
Step-up:    vault:work, ops:work
Expires:    never

Scan this, or run:
  qrencode -t ANSIUTF8 'otpauth://totp/Archivist:agent-claude?secret=JBSWY3DPEHPK3PXP&issuer=Archivist'

arch_kJ8sPq2mWx...
```

Permissions first, token last: you read what you granted before you copy it, and
the token is the final line, which is the easiest to select and the one that
survives scrolling.

Principals are **immutable**. There is no `token update`. Changing a permission
means minting a new token and replacing it wherever it is held, which costs two
propagations — the credential and a fresh QR scan — and buys an audit moment by
construction plus a forced re-inventory of every holder. Revisit when this stops
being a one-operator system.

### Profiles

`-profile <name>` sets defaults that explicit flags override. `token add` prints
the expansion, so a profile never grants something unseen.

| profile | scopes | step-up | attended |
|---|---|---|---|
| `obsidian-plugin` | read, write, delete | `ops:<vault>` only | yes |
| `mcp-client` | read, write | `vault:` + `ops:` | yes |
| `mcp-scheduled` | read, write | refused | no |
| `relay-background` | read | refused | no |

The split that matters is attended versus unattended. A token with `vault:`
step-up and no human behind it is a service that dies at its first restart and
cannot recover. `mcp-scheduled` exists because memo-ai is an MCP client running
under dagu with nobody watching — the same hazard as `relay-background`, reached
through a profile named for the surface rather than for who is present.

`delete` on `obsidian-plugin` because deleting a note is routine there; `move`
covers renames under `write`, but not deletions.

## The gate

Enforced in `withVault` (`internal/api/api.go`), immediately after the scope
check, so every per-vault route inherits it and no handler can forget.

```
authenticate -> resolve vault -> Opens? -> scope? -> step-up? -> handler
```

It gates **every verb, reads included**. Exfiltration is the leaked-agent-token
scenario, not vandalism, and reading the notes is the thing most worth
consenting to.

A refused call returns **403** with `CodeStepUpRequired`, distinct from
`CodeForbidden`, so a client can turn it into a request for a code rather than
guessing from a status.

### The unlock route

`POST /{vault}/v1/unlock`, per-vault, because a grant binds to one vault and the
path is where vaults are named. It is the one route exempt from the gate it
opens — a `stepUp bool` column on the `routes()` table, with a test asserting
exactly one route is exempt. Putting the exemption in the table keeps it beside
`TestEveryRouteDeclaresAScope`, which exists because a route that forgets a check
is the failure this codebase already worries about.

Unlock still requires a valid token that `Opens` the vault. It requires no scope.

## Codes

**Single-use.** The last accepted time step is remembered per token and a repeat
is refused. Without this the design's own justification is false: TOTP was chosen
over an approval prompt because a code is worthless once used, and one secret now
guards several capabilities, so a code pasted to unlock `work` would otherwise be
replayable to authorize a prune. The cost is a 30-second wait after a fumbled
unlock, and unlocking then immediately confirming a destructive operation needs
two codes. Do not scope the cache per purpose to dodge that; it reopens exactly
the replay it closes.

**Skew:** one step either side, 90 seconds of tolerance. Zero fails whenever you
start typing at second 28.

**Backoff:** three failures, then 30, 60, 90, 180, 360, 720 seconds, capped.
Keyed on token **and** vault — the same key as the grant, so fumbling `work` does
not lock you out of `personal`, and no caller can lock out another by failing on
purpose.

## Grants

Keyed `(token, vault)`. In memory. They die with the process, so every deploy
drops every grant and each protected token needs a fresh code before its next
call. That is the conservative direction — you did not consent to the new binary
— and the cost lands on automated callers that can ask again.

**Absolute, not sliding.** Expiry is fixed at unlock. Sliding renewal converts
consent-per-window into consent-once, and the clients most likely to hold a grant
open forever are exactly the automated ones the gate exists to bound.

**TTL** is a server-wide setting, default 15 minutes.

**Long-lived streams are cut when their grant lapses.** `GET /v1/events` and
`GET /v1/wait` pass the gate once at connect and then stream changed paths, kinds
and sizes for as long as they stay open. Anything else lets a client connect at
second one of a grant and keep receiving the vault's activity long after consent
ended.

## One-shot operations

`ops:<vault>` entries create no grant. The code is verified, the operation runs,
nothing is remembered. Phase 3 wires no destructive operation to this — the
primitive and the catalog ship, `vault:` is enforced, and `op:` entries are
rejected as unknown until something wires them. Building `reclaim --prune` gating
on speculation is not in scope; building a shape that cannot express it later
would be.

## Clients

**Relay.** An `unlock` MCP tool taking a vault and a code. The agent hits a 403,
asks you for a code, calls unlock, retries. The relay stores nothing and stays
stateless; it already forwards the caller's token and passes the server's 403
back unchanged.

**Plugin.** A settings-time warning only, no unlock UI, extending the existing
`scopeWarning` pattern. The warning names `vault:` entries specifically and says
nothing about `ops:` — a plugin token gated on vault access is a mistake worth
catching, because the plugin persists the vault to disk and a synced protected
vault is plaintext on that device forever. A plugin token that confirms a
destructive operation is the opposite: a feature.

**Webhooks keep working.** They run on the relay's own background token with no
caller present, so they are outside the gate. This is a narrower leak than it
sounds: a webhook consumer subscribes in order to act on changes, so it holds a
read token anyway and the payload leaks nothing it could not fetch. `GET
/v1/vaults` gains a `requiresStepUpAuth` bit so the relay can warn at startup
when it has targets for a protected vault — the sentence reaching whoever set
`ARCHIVIST_WEBHOOKS`, at the moment they can change it. Nothing is warned at
mint: `token add` runs in the archivist container and cannot know whether any
relay has targets, so the warning could only be unconditional, and an
unconditional warning is one people learn to skim.

## Recovery

No recovery codes. Losing the authenticator means minting a new token over
`docker compose exec`, which is the recovery path for every other token problem
and needs no new mechanism. An escape-hatch environment variable that disables
the gate is the worst option available: a permanent bypass on the
internet-facing side for a lockout a shell already fixes.

## Not changed

**No protocol bump.** `protocol.Version` stays 2. One new error code and one new
route, and an unmarked token's wire contract is byte-identical before and after.
The version moves only when a client *must* change.

**No HTTP mint route**, unchanged from phase 1.

## Open to revisit later

- `op:` catalog entries, once a destructive operation wants gating.
- Per-token TTL, once two protected tokens want different windows.
- `token update`, once this stops being a one-operator system.
