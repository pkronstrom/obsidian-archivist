# ADR-0005: Step-up is a property of the token alone

**Status:** accepted
**Date:** 2026-09-30
**Supersedes:** ADR-0003, ADR-0004

## Context

ADR-0003 made the gate two halves: a marker file protecting a vault, and a
recorded posture on every token that opens it, with absence denying. In use that
made protecting a vault a migration. Gating ONE agent on `work` meant re-minting
and redistributing every other token that opened `work` -- the phone, the
laptop, an inbox integration, the admin token -- because the marker refused
each of them until it carried an explicit exemption.

ADR-0003's reason was that token-only "fails open if you forget the flag". That
matters with several operators minting tokens for each other. Archivist has one:
the person who mints is the person who decided the policy, tokens are minted
rarely, and the mint output states what it granted.

ADR-0004's `ops:` kind was minted and validated but enforced by no handler. A
token carrying it promised a gate that did not exist.

## Decision

- A token carries `stepUp: [<vault>, ...]`. A request to a vault in that list is
  refused with `step_up_required` until the token holds a grant for it, obtained
  by POSTing a TOTP code to `/<vault>/v1/unlock`. Every other token opening the
  same vault is unaffected.
- No vault-side state. The marker, `stepUpExempt`, postures and `ops:` are gone.
- Entries are plain vault names the token opens. `*` is refused (it would gate
  vaults created later without anyone deciding to), and so is the retired
  `vault:<name>` / `ops:<name>` syntax, so old muscle memory fails loudly.
- `token add` always prints a `Step-up:` line, `none` included, and `token list`
  has a `STEP-UP` column. That is the defence against a forgotten flag.
- Profiles are scope presets only; none implies step-up.

## Consequences

- Gating an agent is one `token add -step-up <vault>`; no other credential moves.
- Forgetting `-step-up` mints an ungated token, visible in the mint output and
  the listing. Accepted for a single-operator deployment.
- `token update <id>` changes an existing token in place (vaults, scopes,
  step-up, expiry, label) and keeps its bearer secret, so gating an agent that
  already has a token needs no client change. This reverses the step-up
  design's "principals are immutable": the audit moment it bought was a forced
  re-inventory of every holder, which a single operator does not need. Adding
  step-up to a token without a secret mints one and prints it once; moving it
  between vaults keeps the secret; clearing it drops the secret.
- A long-lived stream re-checks the CURRENT principal each keepalive, so an
  update that narrows a token or adds step-up ends streams it had open.
- A destructive-operation confirmation gate, if one is ever needed, comes back
  with its enforcement in the same change.
