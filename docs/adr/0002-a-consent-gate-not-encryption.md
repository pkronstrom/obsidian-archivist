# ADR-0002: A consent gate, not encryption at rest

**Status:** accepted
**Date:** 2026-08-20
**Context:** `docs/2026-08-20-step-up-auth-design.md`

## Context

"Secure vault access" reads to most people as encryption — the files on the
server being unreadable without a key. That is not what step-up authentication
does, and the difference is worth recording once rather than re-arguing.

## Decision

Step-up is a **consent gate**. The server can always read the vault; it declines
to *serve* it until a human proves presence. Archivist encrypts nothing. Where
at-rest protection is wanted, it belongs at the OS layer — full-disk encryption
on the host.

## Why

The threat being defended against is an agent with a valid token reading or
changing notes without the owner knowing. The server itself is trusted: it is the
owner's own machine, and it must read the files to do its job at all.

End-to-end encryption defends a different threat — not trusting your own server —
and in this codebase it would cost:

- **Three-way merge.** `internal/merge` runs diff3 over lines. Ciphertext has no
  lines, so every concurrent edit becomes a conflict copy instead of a merge.
- **The JSON key-level merge** that config sync depends on, for the same reason.
- **Server-side search.** `search_notes` greps content. It would have to move to
  the client, meaning download-everything-to-search — the opposite of the MCP
  read cap that the same design wants.
- **Content-addressed dedup**, unless encryption is deterministic, which leaks
  equality. With a per-write nonce, every re-save is a new blob and the
  repository grows faster — which `reclaim` and the size guards exist to fight.
- **History legibility.** `read_note_at`, diffs and `git log -p` all go opaque.

And it is self-defeating for the case that motivated the work: if the agent holds
the key, encryption bought nothing against the agent; if it does not, MCP cannot
read notes at all, which is the entire relay.

Server-side encryption with a server-held key is strictly dominated by full-disk
encryption: the same protection against a stolen disk, none of the code.

## Consequences

- The server reads every note in plaintext. Anyone with root on the host reads
  the vault. This is accepted, and it is why the host matters.
- Nothing in step-up makes encryption harder to add later; it gates serving, not
  storage.
- The TOTP secret must not be repurposed as a key if that day comes. It is shared
  with the verifier by design — the server has it — so a key derived from it is
  known to the server too. An encryption key would come from a passphrase.
