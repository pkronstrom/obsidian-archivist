# ADR-0004: Step-up is one primitive with two lifetimes

**Status:** accepted
**Date:** 2026-08-20
**Context:** `docs/2026-08-20-step-up-auth-design.md`

## Context

Two things want a proof of presence, and they want it on opposite terms.

Vault access happens hundreds of times in a working session. Asking for a code
per call is unusable, so a proof has to cover a window.

A destructive operation — pruning history, changing server settings — happens
once and cannot be undone. A proof that covers a window means the operation stays
authorized for the rest of it, which is not what anyone means by confirming it.

## Decision

One verification primitive, two lifetimes on top of it, chosen by the kind of
catalog entry rather than configured.

| entry | produces | lifetime |
|---|---|---|
| `vault:<name>` | a **grant** keyed `(token, vault)` | absolute TTL, default 15 minutes |
| `ops:<name>` | a **one-shot** | nothing is remembered |

The lifetime is not configurable per entry.

## Why

**The lifetime follows from what is protected**, so making it configurable only
buys the ability to get it wrong. The one combination a per-entry setting would
newly allow — a grant covering destructive operations — is precisely the one that
should not exist.

**One-shot-for-everything is coherent but unusable**: a code per MCP call, which
no agent workflow survives.

**Grants must be absolute, not sliding.** An agent polling `/v1/wait` every
30 seconds would hold a sliding 15-minute grant open forever, which is a
permanent grant wearing a TTL. Sliding renewal converts consent-per-window into
consent-once, and the clients most likely to hold one open are exactly the
automated ones the gate exists to bound.

**Single-use codes are load-bearing here, not merely prudent.** One secret guards
both lifetimes, so without rejecting a used code, a code pasted to unlock a vault
could be replayed to authorize a prune. Rejecting costs a 30-second wait after a
fumble, and two codes when unlocking and then immediately confirming an
operation. Scoping the used-code cache per purpose would remove that cost and
reopen exactly this replay.

## Consequences

- Phase 3 ships the primitive and the catalog but wires no destructive operation:
  `vault:` is enforced, `op:`-style entries are rejected as unknown until
  something wires them. The shape is built; the speculation is not.
- Collapsing one-shot into "a very short grant" will look like a tidy-up to
  someone later. It is not: it leaves a window in which a destructive operation
  stays authorized. That is what this record exists to prevent.
- Long-lived streams (`/v1/events`, `/v1/wait`) must be cut when their grant
  lapses, or a grant becomes effectively unbounded for the one caller shape that
  can hold a connection open.
