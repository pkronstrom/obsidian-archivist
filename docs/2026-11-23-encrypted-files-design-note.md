# Encrypted files in an ordinary vault — design note

**Status:** Built on the `encrypted-vaults` branch, 2026-11-23, after a
four-reviewer panel. What the panel changed is listed under
[Review outcomes](#review-outcomes) and folded into the text. Run in a real
Obsidian for Linux by `npm run test:e2e`; not yet run on a real Mac or phone.

**Date:** 2026-11-23

---

## User need

"Most of my vault should stay as it is: searchable, mergeable, readable by my
agents. A few notes and folders should be on the server only as ciphertext."

A file or folder is marked by name, the way `.local` already marks what never
leaves a device:

- `Salary.encrypted.md` is one encrypted note;
- `Clients.encrypted/` encrypts everything inside it, at any depth.

On every device that holds the vault's key the file is an ordinary readable
file, edited normally. The plugin encrypts it before it is sent and decrypts it
on the way in. The server stores ciphertext and refuses anything else at a
marked path.

## Relation to earlier decisions

- **Encrypted vaults** (`2026-11-19-encrypted-vaults-design-note.md`) encrypt a
  whole vault. This reuses its key, envelope, name encryption, recovery key and
  two-hash sync state, applied per path in a vault that is otherwise ordinary.
  An encrypted vault ignores the marker: everything in it is encrypted already.
- **Encrypted paths** (`2026-08-21-encrypted-paths-design-note.md`) proposed the
  server as a guard at marked paths, against malware on a device. This builds
  the guard, but not that threat model: files here are plaintext on the device.
- **Hidden paths** (`2026-10-01-hidden-paths-idea-note.md`) will use `.hidden`
  for notes the server can read but integrations cannot. An `.encrypted` path
  needs no `.hidden`: integrations cannot read what the server cannot.
- **ADR-0002** stays in force. The costs it names (merge, search, MCP reads)
  are paid only for marked paths.

## Threat model

The same as an encrypted vault's, for the marked paths only: the server's disk,
git history and backups hold ciphertext, and a token without the key reads
nothing useful.

Not defended, in addition to what the encrypted-vault note lists:

- **The marked name itself.** `Salary.encrypted.md` and `Clients.encrypted`
  are readable on the server: the marker has to be, for the server to enforce
  it. Names *inside* a marked folder are encrypted.
- **What a note held before it was marked.** Renaming `Salary.md` to
  `Salary.encrypted.md` leaves every earlier version readable in history under
  the old name. The old path is then deleted at head, so `reclaim` can prune it
  once its age gate passes; until then it is in history and in backups.
- **A compromised device**, as before. This is not the infostealer defence of
  the 2026-08-21 note.
- **A copy that loses the marker.** Obsidian's "Make a copy" names the copy
  `Salary.encrypted 1.md`, which is not marked, so the copy syncs readable.
  The same goes for a mistyped marker. The plugin's own copies (conflict
  copies, rescued files) keep the marker; a copy the user makes is theirs to
  name.
- **A stale device bringing the old name back.** If another device has an
  unsynced edit to `Salary.md` when it is renamed to `Salary.encrypted.md`,
  that edit wins over the delete, as any edit does, and `Salary.md` reappears
  readable beside the marked note. The device that did the rename is told.
- **Swapped or rolled-back ciphertext.** The seal authenticates a file's
  content, not which path it belongs to. A server that tampers can swap two
  encrypted files or serve an older version of one. Equal names in different
  marked folders are also visibly equal.

## The marker

One rule, identical in Go and TypeScript, and the same grammar as `.local`:

- any path segment that ends in `.encrypted` (and is longer than the marker)
  marks itself and everything beneath it;
- the last segment is also marked when `.encrypted` sits directly before its
  final extension: `Note.encrypted.md`.

A folder must END in the marker. `project.encrypted.assets/` is an ordinary
folder, for the reason `project.local.assets/` is. The marker is fixed in
code: not a setting, so nothing in the vault and no agent can turn it off.

Case is ignored: `Clients.Encrypted/` is marked. This differs from `.local`
on purpose, because the two fail in opposite directions. A `.local` that does
not match merely syncs; an `.encrypted` that does not match is uploaded
readable. A phone keyboard that capitalises the word must not do that.

`.local` wins over `.encrypted`: a path that is local-only never leaves the
device, so there is nothing to encrypt.

## Names on the server

The path up to and including the first marked segment is kept as it is.
Every segment after it is encrypted exactly as in an encrypted vault (AES-SIV
per segment, NFC first, lowercase base32).

| On the device | On the server |
|---|---|
| `Salary.encrypted.md` | `Salary.encrypted.md` |
| `Work/Clients.encrypted/Acme/Contract.md` | `Work/Clients.encrypted/mfzw…/onxw…` |
| `Plain/Note.md` | `Plain/Note.md` |

The server can therefore tell a marked path from its own copy of the rule, and
check that everything beneath a marked folder is a base32 name.

## Keys

One key per vault, the same 32-byte key and the same derivation as an encrypted
vault. The key check moves from "encrypted vaults only" to "any vault": an
ordinary vault gains one the first time a device creates a key for it.

A device stores the key with its scope: `vault` for an encrypted vault, `files`
for marked paths in an ordinary one. The scope comes from the device, never
from what the server says on a later day. A key stored before scopes existed
is a `vault` key.

| Key on the device | Server says encrypted vault | Server says ordinary vault |
|---|---|---|
| none | ask for the key; nothing syncs | sync, skipping marked paths |
| `vault` | sync, everything encrypted | refuse: would upload plaintext |
| `files` | refuse: would encrypt nothing | sync, marked paths encrypted |

The key is created in the same dialog as an encrypted vault's: the recovery
key is shown and must be confirmed saved before the key is stored, the key
check installed, or anything sealed. The server refuses a marked put while the
vault has no key check, so no ciphertext can exist under an unrecorded key.

## Devices without the key

A device does not need the key to sync the rest of the vault.

- Without the key, marked paths are skipped in both directions, as if excluded:
  not downloaded, not uploaded, and never deleted on the server because they
  are absent here.
- The plugin says so once, and settings carry an **Enter the key** (or
  **Create the key**) entry. "Not now" is remembered per vault, so a phone that
  is deliberately kept without the key is not asked at every start.
- When the key arrives, the device fetches the server's snapshot and adopts
  every marked path it was skipping: downloads what it lacks, and for a file
  that exists on both sides compares plaintext and keeps both versions if they
  differ. Marked files that existed only on this device are then pushed, sealed.
  Adoption does not move the sync cursor: it runs after the cycle's pull, takes
  only paths the state does not track, and leaves ordinary changes to be pulled
  in order as always. A flag in the sync state (`keyed`) records that it has
  run, so a restart between storing the key and adopting does not lose it.
- A marked file made on a keyless device exists only there until the key is
  entered. It is not backed up. The plugin asks for the key the first time it
  sees one.
- A key that does not match the vault's key check stops the whole sync with an
  error, as in an encrypted vault. It cannot be stored through the dialog,
  which verifies before storing.

One consequence to accept: on a device without the key, renaming a synced note
to a marked name deletes the plain note on the server (correct) while the
marked file waits here, unsynced, until the key is entered. The plugin prompts
for the key when it first sees a marked file it cannot send.

## Server

The server holds no key. Its job is the guard, now decided per path:
`sealed(path)` is true for every path of an encrypted vault and for marked
paths of an ordinary one.

- **Push.** A put to a sealed path is refused unless its content starts with
  the envelope, and, under a marked folder, unless the rest of the path is
  base32. In an ordinary vault it is also refused while the vault has no key
  check. Refusals are per change, as today; the rest of the push proceeds.
- **Moves.** A move must stay on one side: both paths sealed or neither. A move
  across the boundary is refused, because it would carry plaintext to a marked
  path or ciphertext to a readable one. The plugin sends delete plus put. The
  two halves can fare differently: if another device changed the note first,
  the delete is refused and the readable file stays. The plugin reports that
  in a notice that stays up, and restores the file locally. A token without
  the delete scope cannot mark an existing note at all, for the same reason.
- **Conflicts.** For a sealed path the server does not merge and does not name
  a conflict copy. It keeps its version, writes nothing, and reports
  `conflict` with the hash it kept. The plugin makes the copy. Unchanged from
  encrypted vaults.
- **Server-side writes.** Append and edit of a marked path are refused
  (`encrypted_path`), and so is a pin on one. Every write still passes the one
  `write` function, which refuses plaintext at a sealed path as a last layer.
- **Files dropped on the server by hand.** The watcher commits only what is
  syncable, and for a marked path that check reads the file's first bytes from
  disk. A file there without the envelope, or with a readable name under a
  marked folder, is never committed. It stays in the directory, unsynced.
- **Key check.** `PUT /v1/keycheck` is accepted for any vault, still once only.
- **Relay.** The shared Go client refuses to read, write, move or delete a
  marked path (`encrypted_path`) and leaves marked paths out of listings, so
  MCP tools, the file API and search never touch them. A caller that talks to
  the server directly with its own token gets ciphertext on read and the
  server's refusals on write.

Not guarded: `PUT /v1/content/{hash}` in an ordinary vault. Content is uploaded
by hash before the push names a path, so the server cannot know the bytes are
headed for a marked path. A client that uploads plaintext and is then refused
at push leaves an unreferenced object behind. The plugin never does this (it
seals before it hashes), so it takes a broken or hostile client holding a write
token, which could have written the same text to an unmarked path anyway.

## Plugin

- **Crypt.** `VaultCrypt` gains a scope. `covers(path)` is true for every path
  in `vault` scope and for marked paths in `files` scope. Path encryption keeps
  the prefix through the first marked segment and encrypts the rest.
- **Client.** Still the boundary: plaintext paths and content above it,
  ciphertext below. A crypt on the client no longer means "the whole vault is
  ciphertext", so every method asks `covers(path)`: history, old versions,
  the deleted list, snapshot, changes, push and pins. Content is fetched by
  hash, so `getContent` takes the path it is for, to know whether to open it.
  Guessing from the envelope prefix is not safe: an ordinary note may begin
  with those bytes.
- **Sync.** Every "is this an encrypted vault" branch becomes "does the crypt
  cover this path": sealing puts, recording the plaintext hash, comparing on
  plaintext at re-bootstrap and pairing, and the device-made conflict copy.
- **Renames.** Renames are recognised by matching plaintext hashes. A pair that
  crosses the boundary is not folded into a move; it stays delete plus put, and
  the put is sealed or plain as its new path requires. A rename OUT of the
  marker uploads the note readable, and the plugin says so in a notice.
- **Conflict copies.** A copy of a marked note must itself be marked, or it
  would be uploaded readable on the next cycle. The conflict suffix goes
  before the marker: `Salary.conflict-<device>-<time>-local.encrypted.md`.
  Under a marked folder the folder covers the copy whatever it is called.
- **Key dialog and settings.** The existing dialog, with wording for the two
  scopes. Settings explain `.encrypted` beside `.local`, including that the
  file on this device is not encrypted.
- **Pins.** Not offered for a covered path.

## Costs accepted

For marked paths only: no server merge (concurrent edits make a conflict copy),
no search, no relay or agent access, no pins, one full copy per save in history.
Names under a marked folder are limited to 143 bytes per segment, as in an
encrypted vault.

## Out of scope

- Merging concurrent edits on the device.
- Encrypting the marked name itself.
- Pruning the plaintext history of a note that was renamed into the marker,
  beyond what `reclaim` already does.
- A different key per folder.

## Review outcomes

Four reviewers (GPT, Claude, Grok, GLM) read the draft. All four found the
first item independently.

1. **Conflict copies lost the marker.** The existing conflict name put its
   suffix between `.encrypted` and the extension, which unmarks the file, so
   the copy of a secret would have been uploaded readable. Fixed as described
   under Plugin, with a test over every marker shape.
2. **The marker failed open on case.** Now case-insensitive.
3. **The scope and vault-kind combinations were incomplete.** The table under
   Keys is the result; a `files` key against an encrypted vault is refused.
4. **A crypt on the client was assumed to mean a whole encrypted vault** in
   pins and elsewhere. Every client method now decides per path.
5. **Rename races were unspecified.** A refused delete, a write-only token and
   a stale device are now stated, and the first is reported to the user.
6. **Adoption and the sync cursor.** Stated: adoption never moves the cursor.
7. **Limits that were missing from the threat model:** copies that lose the
   marker, ciphertext swap and rollback, unbacked-up files on a keyless device.

Not taken: a reviewer read the watcher's filter as path-only and unable to
check content. It reads the file from disk, as the encrypted-vault filter
already did, and a test covers it.

The draft's three open questions were settled as drafted: a keyless device
skips marked paths, the relay hides them from listings, and the content upload
route stays unguarded in ordinary vaults.

## Size

L, as estimated. The keyless-device mode, the named risk, did not turn out
fragile: it is one predicate in the sync engine and one adoption step.

## Build order

All seven steps are done.

1. Protocol: the marker rule, the path check, the error code. Go and
   TypeScript, with a shared table of cases
   (`protocol/testdata/marked-paths.json`).
2. Server: per-path guard in push, moves, conflicts, server-side writes and
   the watcher; key check for any vault.
3. Relay client: refuse and hide marked paths.
4. Plugin crypt and client: scope, per-path translation.
5. Plugin sync: per-path sealing and comparison, rename boundary, keyless
   skipping, adoption when the key arrives.
6. Dialog, settings, and the stored key's scope.
7. Tests: Go unit tests, the Node end-to-end test against a real server, and a
   scenario in the Obsidian Docker test. Documentation.
