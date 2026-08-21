# Encrypted Paths — Design Note

**Status:** Explored, not planned. No code. Written so the reasoning does not
have to be re-derived, and so the discarded options stay discarded.

**Date:** 2026-08-21

---

## The idea

A naming convention marks a note as encrypted — `secrets.enc.md`, or whatever
suffix the encryption tool already uses. The **client** encrypts; the **server**
never holds a key. Archivist's only job is to refuse to store plaintext at such
a path.

The server becomes a guard enforcing a property it cannot itself violate.

## The threat this addresses

**An infostealer on a device.** Malware that walks the filesystem and exfiltrates
anything valuable, running on the Mac or the phone.

Everything server-side is useless against it. The malware runs on an unlocked
machine; full-disk encryption is mounted, archivist is serving, and the synced
plaintext is sitting in the vault directory. The only defence is that the
sensitive bytes are ciphertext **on the device's disk**, with the key not on that
disk.

This is not the same threat as ADR-0002's, and it does not reopen that decision.
ADR-0002 rejected end-to-end encryption of the whole vault because it would cost
three-way merge, server-side search, dedup and MCP reads. Encrypting a handful of
explicitly-marked paths costs those things only for those paths, which is the
point of marking them.

## Why the server has to be involved at all

Client-side encryption alone needs nothing from archivist — it syncs opaque bytes
like any other content. The reason to involve the server is a failure this vault
is specifically prone to.

**The draft leak.** Type a secret into a normal note, then encrypt it afterwards,
and the plaintext was on disk in between. The plugin debounces at 2 seconds
(`src/main.ts`), so a note drafted for any length of time is pushed and committed
many times before you encrypt it. Measured on the live vault: one note drafted
for about 50 minutes produced **250 commits**.

Those plaintext versions are then permanent. `reclaim` selects paths **deleted at
HEAD** (`internal/repo/reclaim.go:29`), so a note that still exists never becomes
eligible — its old plaintext blobs cannot be pruned. Encrypting a note after the
fact leaves the secret readable in `git log -p` forever.

A server that refuses plaintext at an encrypted path makes this structurally
impossible rather than a matter of discipline.

## Shape

**Detection is structural, never heuristic.** No entropy checks — fragile, and
they fail open on short content. The server checks for a known envelope prefix
and nothing more: `age` files begin with a literal `age-encryption.org/v1` line;
other tools have their own headers. The server does not parse, validate or
understand the ciphertext. It checks a magic prefix.

**Refusal is per-path, not per-push.** The machinery already exists:
`internal/reconcile/reconcile.go:301` refuses a single change with
`StatusRefused` and a reason while the rest of the push proceeds. A plaintext
write to an encrypted path becomes a new case in that switch. A backstop that
broke all syncing when it fired would be a bad backstop.

**Consequences for marked paths, all expected:**

| | effect |
|---|---|
| three-way merge | none — opaque bytes, so a genuine conflict yields a conflict copy |
| server-side search | cannot see the contents, correctly |
| `note_history` | works; revisions are ciphertext |
| NoteDiscovery | shows ciphertext |
| MCP agents | read ciphertext they cannot use; writes are refused with a reason |

That last row is worth noticing: agents are handled without any step-up rule.

## Two caveats

**The guard protects history and other devices, not the drafting machine.** If
you type in the normal editor and encrypt afterwards, the plaintext was on your
Mac's disk for that session — and a device infostealer is the stated threat. The
server refusing it later does not unwrite it.

So this wants pairing with **modal-based editing**, where plaintext never touches
disk. Meld Encrypt's whole-note mode claims exactly that: *"Encrypted notes are
never decrypted to disk."* Its select-and-encrypt mode does not, and is the leaky
path described above.

**Retrofitting an existing note cannot be done cleanly.** The plaintext is
already in history and `reclaim` cannot reach a live path. The only routes are
delete → prune → re-add encrypted (subject to the 90-day `-older-than` gate), or
a history rewrite. Encryption has to be chosen when the note is created.

## Open questions, if this is ever built

- Is the suffix hardcoded, configured server-side, or advertised on `GET /v1` so
  clients can discover it?
- Which envelope(s) are accepted? One, or a list?
- Does the plugin need to know, or is the server's refusal enough? A plugin that
  understands the convention can avoid sending plaintext at all; one that does
  not gets a clear error.
- What happens to a marked path that arrives with a *valid* envelope but from a
  client that cannot decrypt it? Nothing — the server neither knows nor cares.

## Rejected alternatives

**Archivist holds the key and decrypts for stepped-up agents.** This was the
original framing: ciphertext at rest, TOTP unlocks a decrypted view. It fails on
its own terms. TOTP is six digits derived from a secret in `tokens.json` — it
proves presence and carries no key material, so the key would have to live on the
same disk as the notes. At-rest encryption with an on-disk key defends against
nobody. And the moment archivist can decrypt, malware on the server can too.

Its only genuine value is letting an agent read the secret. That is worth
revisiting **if and when a concrete case appears**, and not before.

**Encrypted Docker volume or LUKS-backed mount.** Defends offline threats — a
stolen disk, a seized machine — not a running compromise: once mounted, every
process on the box reads through the mountpoint. It also cannot be automated
here (no passwordless sudo on the server), Docker bind mounts resolve at container
start, and the watcher would interpret an unmount as every file being deleted and
commit it. If at-rest coverage is wanted, full-disk encryption on the whole box
is simpler and has none of these hazards.

**Whole-vault E2E.** Settled in ADR-0002. Unchanged.
