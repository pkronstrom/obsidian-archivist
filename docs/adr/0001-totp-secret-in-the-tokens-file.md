# ADR-0001: The TOTP secret lives in the tokens file, stored reversibly

**Status:** accepted
**Date:** 2026-08-20
**Context:** `docs/2026-08-20-step-up-auth-design.md`

## Context

`internal/auth/tokens.go` documents the tokens file as holding "nothing
replayable": tokens are keys of a map, hashed with sha256, and the plaintext
exists only in the mint output. That property is the reason the file can sit in a
container bind mount without being a credential store.

Step-up needs a TOTP secret. A TOTP secret **cannot** be hashed — the server
recomputes HMAC from it on every verification, so it must be recoverable. Putting
it in `tokens.json` contradicts the sentence above, and a future reader will find
the contradiction.

## Decision

Store it there anyway, as `totpSecret` on the principal, base32, in plaintext.
One secret per token.

## Why

**The invariant's purpose survives even though its wording does not.** The file
exists to hold nothing an attacker can *present to the server*. A TOTP secret is
a second factor: someone who reads the file can generate valid codes and still
authenticate nothing, because they do not have the bearer token — which is still
only there as a hash.

**Revocation stays atomic.** `Set.Revoke(hash)` deletes the whole principal, and
`Mint` writes the file atomically under a lock. A secret stored under the
principal dies with the token in one write. Any other location orphans secrets on
revocation, and an orphaned authenticator entry for a token that no longer exists
is a thing nobody will ever clean up.

**One secret per token, not per vault.** A per-vault secret would mean one
authenticator enrollment per vault for the same person and the same phone,
buying no isolation — the holder is the same either way. Per-*token* separation
is the one that matters, and it is what this gives: revoking one holder never
re-enrolls another.

## Alternatives considered

**A separate `totp.json`.** Keeps the original sentence literally true, at the
cost of a second file, a second watcher, a second thing to back up, and
non-atomic revocation. Defends the wording rather than the property.

**Per-vault secrets under `$ROOT/.archivist/<vault>/`.** Considered and rejected
after the revocation problem above; a token revoked centrally would leave secrets
scattered across every vault directory it could open.

**One server-wide secret.** Every protected token shares one authenticator entry,
so revoking one holder means re-enrolling all of them.

## Consequences

- The doc comment in `internal/auth/tokens.go` must be amended rather than left
  to contradict the code. Its claim narrows from "nothing replayable" to "nothing
  presentable as a credential".
- Anyone tempted to "fix" this by moving the secret elsewhere should read the
  revocation argument first. That is what this record is for.
- The secret must never be reused to derive an encryption key. The server holds
  it, so it is not a key worth having. See ADR-0002.
