# Backlog

Potential features, not commitments. Each entry says what it is for and links
the design note when one exists. Move an item to a design note when work starts.

## Hidden paths — sync and back up, never expose to integrations

**Added:** 2026-10-01 · **Status:** idea

Some notes are sensitive: they should sync between my own devices, be backed up
and keep their history, but never reach an AI agent over MCP, a webhook, or any
other integration through the relay.

Marked by name, e.g. `my-note.secret.md` or a `secret/` folder (convention to
decide). Possibly encrypted as well, but that is a separate, optional layer.

Things to settle when designing:

- **Enforced by the server, not the relay.** The relay forwards each caller's
  own token, so filtering there is bypassable by calling the server directly.
  Follow ADR-0005: visibility is a property of the token. A token sees hidden
  paths only if it was minted or updated with an explicit grant (e.g.
  `-hidden`); plugin tokens get it, agent and integration tokens do not.
- **Every read surface must filter**, or the path leaks through the one that
  was missed: `snapshot`, `export`, `history`, `at/{rev}`, `deleted`, `check`,
  the `changes` feed, `events` and `wait` payloads, the relay's REST file API,
  and every MCP tool (`list_notes`, `search_notes`, `note_history`,
  `read_note_at`, attachments). Webhooks run on the relay's background token,
  so they are filtered for free if that token has no grant.
- **Content is addressed by hash.** `GET /v1/content/{hash}` serves any blob
  whose hash a caller knows, and hashes appear in snapshots and history. Either
  never reveal a hidden blob's hash to an ungranted token, or check the blob's
  paths on fetch.
- **Commit metadata leaks paths.** Commit messages and trailers name changed
  files, so history listings for an ungranted token must redact or skip them.
- **Writes and moves.** An ungranted token must not create, overwrite, delete
  or rename into or out of a hidden path; otherwise an agent can unhide a note
  by renaming it, or overwrite one it cannot read.
- **The marking must not be agent-editable.** A naming convention is fixed in
  code, which satisfies this; a config file inside the vault would not.
- **History and backup are unchanged.** Hidden notes stay in git and in restic;
  only serving them is restricted.
- **Relation to encryption.** This is an exposure gate, like step-up (ADR-0002),
  against integrations with a valid token. It does nothing against an
  infostealer on a device, which is what
  [encrypted paths](docs/2026-08-21-encrypted-paths-design-note.md) addresses.
  The two compose: `.secret.md` hidden from integrations, optionally also
  client-side encrypted.
- **Relation to `.local`.** `.local` never leaves the device; hidden syncs to
  every granted device. Different rules, same naming-convention style.

## Step-up setup wizard

**Added:** 2026-09-30 · **Status:** idea

`token add/update -step-up` prints an `otpauth://` URL and a `qrencode` command
that is installed on neither aarni nor the Mac. Draw the QR in the terminal and
save only after one code from the authenticator verifies, so a mistyped secret
cannot lock an agent out.

## Online prune

**Status:** investigated, not built ·
[design note](docs/2026-09-05-online-prune-design-note.md)

Reclaim history space without stopping the server, without a data-loss window.

## Time scrubber

**Status:** idea, not scheduled ·
[revision browser note, "Later"](docs/2026-08-21-revision-browser-design-note.md)

Drag a slider through one note's revisions. Builds on the shipped revision
browser and restore.

## Structural Markdown patch tool for MCP

**Status:** deferred ·
[large-note MCP design, "Deferred work"](docs/2026-08-26-large-note-mcp-design.md)

Heading, block or frontmatter targeted edits, only if exact-replace `edit_note`
proves insufficient.

## Encrypted paths

**Status:** explored, not planned ·
[design note](docs/2026-08-21-encrypted-paths-design-note.md)

Client-side encryption of marked paths; the server refuses plaintext there.
Defends against an infostealer on a device. See also *Hidden paths* above.
