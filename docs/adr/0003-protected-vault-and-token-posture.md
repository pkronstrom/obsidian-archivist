# ADR-0003: The gate has two halves — a protected vault and a token posture

**Status:** accepted
**Date:** 2026-08-20
**Context:** `docs/2026-08-20-step-up-auth-design.md`

## Context

`docs/2026-08-19-scoped-tokens-review.md` states plainly that the lock is a
property of the **token**, "not the vault, so a phone syncing Work is not gated
by an agent's policy". This ADR contradicts half of that sentence, and an
unexplained contradiction between two design documents is how a future session
ends up in circles.

The deployment that forced the change: one MCP token reaching `personal` freely
and `work` gated, while an Obsidian token syncs that same `work` vault with no
code at all, and still confirms a destructive operation when one is triggered
from the plugin.

## Decision

Both, as two halves of one rule.

- A **vault** is protected when it carries an empty marker at
  `$ROOT/.archivist/<vault>/step-up`.
- A **token** carries a recorded decision about each protected vault it opens:
  entries in `requiresStepUpAuth` (`vault:<name>` gates access, `ops:<name>`
  gates destructive operations), or the vault named in `stepUpExempt`.
- `token add` **refuses** to issue a token that opens a protected vault without
  one. `-no-step-up <vault>` records the exemption.
- The **gate denies** a token that carries neither. Absence is not permission.

## Why

Each half alone fails, in opposite directions.

**Token-only fails open, and silently.** Mint a token for `work` a year from now,
forget the flag, and it is ungated. Nothing refuses, nothing logs, and the
failure mode of a control that exists to prevent unobserved access is itself
unobservable. A mint-time warning was considered — "other tokens require step-up
for work; this one will not" — but a warning is skippable, and it cannot fire for
tokens minted before the vault was first protected.

**Vault-only cannot express the deployment.** A marker that gates everyone gates
the phone too, which re-creates the exact problem the original per-token
reasoning existed to avoid: an agent's policy dictating whether a device syncs.

Making the marker force an *explicit* answer, rather than imposing one, keeps
both properties: forgetting is impossible because mint refuses, and the phone is
ungated deliberately rather than accidentally.

**The exemption must be recorded, and absence must deny.** The first draft of
this decision relied on the mint-time refusal alone, which was wrong in a way
worth naming: mint only binds tokens created *after* the marker exists, so every
token predating it stayed ungated with nothing to notice — reintroducing the
exact silent fail-open that ruled out the token-only model. Writing the exemption
down makes "decided not to gate" distinguishable from "never asked", and only the
second is denied.

## Alternatives considered

**Token-only, plus a mint-time warning.** Rejected above: fails open.

**Vault-only, with an exemption flag.** Equivalent in power, but it makes
"ungated" the exception rather than a stated intent, and inverts which mistake is
safe — you must then remember to exempt, and forgetting produces a device that
stops syncing.

## Consequences

- Protecting a vault that already has tokens is a migration, and it is enforced
  by the server rather than by discipline: every existing token opening that
  vault starts failing with `step_up_required` the moment the marker appears,
  naming the re-mint. Loud and immediate, which is the intended direction — the
  alternative is a vault everyone believes is protected and is not.
- The tokens file gains a second field, `stepUpExempt`. A hand-written file that
  omits both fields for a protected vault denies rather than allows.
- Two places carry policy — the marker and the principal — so both must be read
  to answer "is this caller gated". The gate reads both on every request.
- Hand-editing `tokens.json` bypasses the mint-time refusal. Accepted: the
  operator is the policy, and offline editing is already how everything else here
  is repaired.
