# Encrypted vaults — design note

**Status:** Built on the `encrypted-vaults` branch, 2026-11-19, after a
four-reviewer panel. Run in a real Obsidian for Linux by `npm run test:e2e`,
including its mobile layout; not yet run on a real Mac or phone. Where the
build departed from this note, the note was corrected.

**Date:** 2026-11-19

---

## User need

"I sync my work vault to my own server. I want it backed up there, but readable
only on the one or two devices that hold a key."

A whole vault is marked encrypted. The plugin encrypts file contents and file
names before anything leaves the device. The server stores, versions and backs
up ciphertext, and never holds a key.

## Relation to earlier decisions

- **ADR-0002** rejected end-to-end encryption because it costs three-way merge,
  server-side search, MCP reads and history legibility. That holds for a vault
  agents and tools read. A vault that is only synced and backed up uses none of
  them, so the costs fall only where they are not missed. ADR-0002 stays in
  force for ordinary vaults; this adds an opt-in vault kind beside them.
- **Encrypted paths** (`2026-08-21-encrypted-paths-design-note.md`) proposed the
  server as a guard that refuses plaintext at marked paths. This design reuses
  that guard, applied to every path of a marked vault.
- **Hidden paths** (`2026-10-01-hidden-paths-idea-note.md`) is unrelated: it
  hides plaintext from integrations. An encrypted vault is closed to
  integrations entirely.

## Threat model

Defends against:

- anyone who can read the server's disk, the vault directory, the git history
  or a backup of them, including root on the host;
- a token that opens the vault but belongs to a client without the key.

Does not defend against:

- **A compromised device.** The device holds the notes in plaintext in its local
  vault folder, and the key in local storage.
- **Metadata.** The server sees the folder tree shape, file count, ciphertext
  sizes, edit times and device labels in commit messages. Name encryption is
  deterministic, so it also sees which folder and file names repeat across the
  tree, though not what they are.
- **A server that tampers.** Modified ciphertext is detected (authenticated
  encryption). Rolling a file back to an older version, swapping two files'
  contents, or deleting files is not.

A lost device is handled by revoking its token, as today. There is no key
rotation: the key is useless without the ciphertext, and the lost device
already held the plaintext.

## Shape

```text
 Obsidian vault          plugin                    server + git
 (plaintext)                                       (ciphertext only)
┌──────────────┐   ┌──────────────────┐   ┌─────────────────────────┐
│ Clients/     │   │ encrypt name     │   │ k7f3q…/x2m9a…           │
│   Acme.md    │──▶│ encrypt content  │──▶│ refuses anything        │
│              │◀──│ decrypt on pull  │◀──│ without the header      │
└──────────────┘   └──────────────────┘   └─────────────────────────┘
                     key: local storage     no key, ever
```

### Keys

- One random 256-bit vault key, generated on the first device.
- Three subkeys derived with HKDF-SHA-256: 32 bytes for contents, 64 bytes
  for names (AES-256-SIV takes a double-length key), 32 bytes for the key
  check.
- Stored with `app.saveLocalStorage`, next to the token (`src/credentials.ts`),
  so it is never in a syncable file.
- The same string is the recovery key and the key entered on a second device:
  base32 in groups, with a checksum so a typo is caught before use. Also shown
  as a QR code.

### Contents

- AES-256-GCM, fresh random 96-bit nonce per write.
- Stored bytes: a fixed magic header, then nonce, then ciphertext and tag.
- Not bound to the path. A `move` carries no content
  (`src/client.ts`, `Change.op`), and binding would force a re-upload on every
  rename. The price is the content-swap gap named in the threat model.

### Names

- Each path segment is encrypted on its own, so the folder tree survives and
  the server's path operations (move, delete, history of a path) keep working.
- Deterministic authenticated encryption (AES-SIV), so one name always maps to
  one ciphertext. WebCrypto has no SIV mode; this needs one small vetted
  dependency.
- Segments are normalised to NFC before encryption, then encoded as lowercase
  base32 without padding. That is safe on case-insensitive filesystems and
  never starts with a dot.
- The extension is encrypted with the name. The server sees no extensions.
- Encrypted names are about 1.6 times longer plus a fixed overhead. A segment
  that would exceed 255 bytes is not synced, and the plugin reports it like any
  other refused path.

### Server

**Vault kind.** Whether a vault is encrypted is recorded in the vault's git
config (`archivist.encrypted`), inside `$ROOT/.archivist/<vault>/`. The export
already archives `config` (`internal/repo/archive.go`), so a restored vault
comes back encrypted with its guards. There is no precedent to copy: the
step-up marker of ADR-0003 was removed by ADR-0005.

- The kind is set only when the vault is created. The documented way is the
  offline command `archivist-server vault add -encrypted NAME`, run on the
  host. A directory added by hand under `vaults/` is always an ordinary vault.
- `POST /v1/vaults` also accepts `encrypted`, for the tokens minted with
  `-can-create-vaults`. That capability is off by default and the plugin does
  not use it.
- `GET /{vault}/v1` reports `encrypted: true`.

**Key check.** The server stores one value per encrypted vault, also in the git
config: an HMAC of a fixed string under the key-check subkey. It reveals
nothing about the key.

- It is written once, by the first device, through a dedicated endpoint. A
  second write is refused, so two devices pairing at the same moment cannot
  both install a key.
- Whether a vault has a key is decided by this value, never by whether the
  file tree is empty. A vault whose files were all deleted still has history
  under its key.
- Until the value is set, the server accepts no content for the vault.

**Guards.** Plaintext must never reach the object store.

- The content upload endpoint checks the magic header before writing the blob
  (`internal/api/api.go`, `putContent`). The plugin uploads content before it
  pushes a change, so a check only at push time would leave refused plaintext
  in git objects and in exports.
- A `put` whose content lacks the header is also refused per path in
  `internal/reconcile/reconcile.go`, as a second layer.
- Every path segment must be lowercase base32. The server's own path rules
  (`vault.Skip`, `LocalOnly`, the dotfile exclusions) match on plaintext names
  and never fire on encrypted ones, so this allowlist replaces them: it stops a
  faulty client from writing a readable name.
- The watcher commits nothing that fails either check; it logs it.

**Conflicts.** No three-way merge. When the base does not match, the server
writes nothing and returns `status: conflict` with the `hash` and `size` of
what it kept at the path, and no `conflictPath` or `conflictHash`. Today it
writes the conflict copy itself and names it; it cannot produce a valid
encrypted name. A delete or move of a path that changed on the server is still
`refused`, as now.

**Closed for encrypted vaults.**

- The relay: no MCP tools, file API or webhooks.
- Pins. The server writes pin names as plaintext JSON to `pins.jsonl` in the
  vault and parses that file itself (`internal/pins`), which would bypass the
  guards.

The plugin inventory keeps working: its file is encrypted and synced like any
other, and never needed the server's exception for its path.

Tokens, step-up, export, backup, history and reclaim are unchanged. They
operate on ciphertext.

### Plugin

- Names and contents are translated at the boundary with the server
  (`src/client.ts`). The rest of the plugin works on real paths.
- The exclusion rules (`skip()`, `.local`) run before names are encrypted.
  They are the only place those rules are enforced for an encrypted vault.
- `FileState` gains a plaintext hash. `hash` stays the hash of what the server
  holds (ciphertext) and is what a push sends as its base. Every comparison of
  local content against the server uses the plaintext hash: change detection,
  pairing, and `rebootstrap` (`src/sync.ts`), which today compares
  `cur.hash === entry.hash` and would set every file aside as a conflict copy.
- `rebootstrap` treats a file as unchanged when the server's hash matches the
  ciphertext hash in the previous state and the local plaintext hash matches
  that state too. Otherwise it downloads and decrypts before deciding.
- On `conflict`, the plugin first writes its local version to a conflict copy,
  then fetches the server's version by the returned hash, then pushes the copy
  as a new file. Copy first, so a crash loses nothing. The existing handling
  that reads `conflictPath` is skipped for encrypted vaults.
- Conflict copies use the plugin's own `conflictName` (timestamp and `-local`
  suffix), not the server's naming.
- History, the deleted-files list and the revision browser decrypt names and
  contents for display.
- When both sides of a conflict hold the same note, no copy is made. Two
  devices that already share their notes meet this on every file when they
  first pair, since each sealed its own copy.

## Setup and migration

An existing vault is never converted in place: its history already holds
plaintext, and the server has no key to encrypt with. Both cases create a new,
empty encrypted vault and fill it from a device.

**New vault.**

1. Create the encrypted vault on the server and mint a token.
2. Point the plugin at it. The vault has no key check yet, so the plugin
   generates a key, shows the recovery key, and waits for "I've saved this".
   Only then does it install the key check.
3. The device publishes its notes.

**Second device.** The vault has a key check, so the plugin asks for the key
and verifies it against the check before anything is read or written. If the
device already has notes, pairing compares by plaintext, which means
downloading and decrypting the server copy once.

**Migrating an existing vault.** The folder on the device stays where it is and
is pointed at the new vault.

1. Create the encrypted vault on the server and mint a token for it.
2. In the existing Obsidian vault, run **Switch to another vault**, and give
   the new vault name and token.
3. Generate the key and save the recovery key.
4. The plugin encrypts and publishes everything, then reports what it sent.
5. Once the report is checked, delete the old vault's directories on the
   server. Older backups hold the plaintext until they are pruned.

History does not carry over; keep one export of the old vault if it matters.
The server-side `rsync` import cannot be used, since it would put plaintext on
the server.

### Switch to another vault

Today an already-synced folder refuses a different vault (`src/state.ts`), to
stop two unrelated vaults being merged by accident. The switch is the one
deliberate way past that refusal. It clears the plugin's sync state and never
touches notes. Its checks:

| Check | Why |
|---|---|
| The folder must have just completed a sync with its current vault. | Edits made on another device and not yet pulled would be missing from the new vault. Overridable only when the old vault is unreachable, with a warning. |
| The new vault must be reachable with the new token before anything is cleared. | A typo must not leave the folder attached to nothing. |
| The new vault must be empty: no files and no history. | The first sync can then only upload. A non-empty target is refused outright. |
| The confirmation names both vaults and their kinds. | The user sees `work` (plain) → `work-enc` (encrypted) before agreeing. |
| For an encrypted target, the key check is installed and the recovery key confirmed before the first upload. | No ciphertext exists that the user cannot recover. |
| Switching from an encrypted vault to a plain one needs the vault name typed in. | It uploads the notes as plaintext. |
| The old state is kept until the first sync with the new vault succeeds. | A failed switch returns to the old vault unchanged. |
| The plugin never deletes or alters the old vault. | Removal stays a manual server step, taken after checking. |

After the first sync the plugin shows a report: files on the device, files
published, and anything skipped or refused, with reasons. The old vault should
be deleted only when published equals expected.

Other devices keep syncing with the old vault until they are switched too.
Anything they write there after the switch is not carried over.

**Recovery.** A backup restores to ciphertext, with the vault kind and key
check intact. Reading it needs the recovery key and a plugin. If the key is
lost and no device holds it, the vault is unrecoverable.

## Costs accepted

| Area | Effect |
|---|---|
| Concurrent edits | Always a conflict copy, never a merge. |
| Repository growth | A full blob per save. Longer debounce, and reclaim, limit it. |
| Search, MCP, webhooks, pins | Not available for the vault. |
| Server-side reading | `git log -p`, NoteDiscovery and direct edits show ciphertext. |
| Second-device pairing | Downloads everything once when the device already has notes. |

## Open questions

- Should the plugin merge conflicts on its own side? It can fetch the base by
  hash and run a three-way merge on plaintext. Left out of the first version.
- Is a 255-byte limit on encrypted segments acceptable, or do long names need
  a fallback such as a hashed name with the real one stored inside the file?
- Should pins get a client-encrypted form later?
- The longer debounce for encrypted vaults is not built. Every save still
  stores a full blob at the ordinary cadence.
- Does the recovery key need a passphrase-wrapped form, or is the raw key in a
  password manager enough?

## Size

**L.** One design covering contents and names; it changes the stored data and
the plugin's per-file state. It grows if the server must keep writing conflict
copies itself, or if long file names need the hashed-name fallback.

## Build order

1. Server: vault kind in git config, `encrypted` on the index, key check,
   upload and push guards, path allowlist, no-write conflicts, watcher guard,
   relay and pins closed.
2. Plugin: key generation and entry, recovery key display, content encryption,
   two-hash state, `rebootstrap` on plaintext.
3. Plugin: name encryption, conflict copies, history and deleted-files views.
4. Plugin: the switch action with its checks and report.
5. Operations documentation, including the migration steps. Amend the
   "Whole-vault E2E" line in `2026-08-21-encrypted-paths-design-note.md` to
   point here.
