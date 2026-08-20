# Context

The vocabulary this codebase uses, and what each term does *not* mean. One term
per concept: if two words describe the same thing, one of them is wrong.

## Auth

**Token** — the `arch_`-prefixed bearer credential a client presents. Stored
hashed; the plaintext exists only in the mint output. Never means the six-digit
step-up code — see [[code]].

**Principal** — what one token may do: its label, vaults, scopes, expiry and
step-up posture. Immutable. Changing a permission means minting a new token, not
editing an existing one.

**Scope** — a verb a token holds: `read`, `write`, `delete`. No wildcard. Every
route declares the scope it needs in the `routes()` table.

**Bootstrap token** — a single `ARCHIVIST_TOKEN` from the environment, opening
every vault, used when no tokens file exists. Keeps `delete`, because a `write`
token can blank a note anyway and the plugin renames routinely.

## Step-up

**Step-up authentication** — an already-authenticated principal proving more
before a sensitive operation. Deliberately not called "2FA": nothing is
authenticating an identity here, the token already did that, and both factors are
things you have. Field name: `requiresStepUpAuth`.

**Protected vault** — a vault carrying the marker at
`$ROOT/.archivist/<vault>/step-up`. The marker holds no secret; presence is the
whole content. A protected vault forces every token that opens it to state a
posture at mint time. It does *not* mean every token is gated — see
[[posture]].

**Posture** — what one token intends toward one protected vault, as entries in
`requiresStepUpAuth`: `vault:<name>` gates access, `ops:<name>` gates destructive
operations. A token may hold either, both, or neither, and "neither" is legal
only because mint made you say so.

**Grant** — the time-boxed permission an unlock creates, bound to one token and
one vault. In memory, absolute expiry, dies with the process. Not "session":
this is not a cookie and not per-connection.

**Code** — the six digits from an authenticator. Never "token", which is already
the bearer credential, and never "one-time token" for the same reason. Single-use:
the last accepted time step is remembered per token.

**One-shot** — a step-up that authorizes exactly one operation and creates no
grant. What `ops:` entries produce. The counterpart to a grant, not a short one.

**`totpSecret`** — the base32 shared secret, one per token, stored reversibly
because HMAC needs it. Never reused as an encryption key: the server holds it, so
it is not a key in any sense worth having.

**Locked** — avoid. It reads as a current state that flips when you unlock, and
it invites "the vault is locked", which describes a state that does not exist —
two tokens get different answers for the same vault at the same instant.

## Sync

**Vault** — one directory of plain Markdown and attachments. Its git history
lives outside it, at `$ROOT/.archivist/<vault>/`, one repository per vault.

**Push** — a client applying a change set computed against a base commit.

**Change set** — the ops in one push: `put`, `del`, `move`. `move` exists so a
rename needs only `write`, where the `del`+`put` it replaces would need `delete`.

**Origin** — who pushed and how. `Device` is client-supplied and forgeable,
`Token` is resolved by the server and is the only unforgeable part, `Via` is the
road taken and is set by the relay. Rendered as commit trailers.

**Conflict copy** — the losing side of a merge that could not be resolved,
written beside the winner rather than discarded.

**Cursor** — the commit a consumer last saw, replayed through
`/v1/changes?since=`. What makes durability possible without a queue: webhooks
and SSE are hints, the cursor is the truth.

## Processes

**archivist-server** — the only authority. Owns the vaults, enforces every check,
makes every commit. Never makes outbound requests.

**archivist-relay** — a stateless forwarder for callers that are not on the
server. Holds no vault and no cursor, and no credential of its own beyond a
read-only background token. Forwards each caller's own token, so the server
evaluates the scopes that were actually minted.

**Background token** — the relay's own read-only credential, used for the two
jobs that run with no caller present: the startup compatibility check and the
webhook event stream. Must never carry step-up.
