# Hidden paths — sync and back up, never expose to integrations

Idea note, moved from BACKLOG.md. Not scheduled.

**Added:** 2026-10-01 · **Status:** idea

Some notes are sensitive: they should sync between my own devices, be backed up
and keep their history, but never reach an AI agent over MCP, a webhook, or any
other integration through the relay.

Marked by name: `my-note.hidden.md`, or a folder such as `Clients.hidden/`.
Encryption is a separate marker, `.encrypted`, with its own rule: the server
stores those paths but cannot read them.

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
  [encrypted paths](2026-08-21-encrypted-paths-design-note.md) addresses.
  An `.encrypted` path needs no `.hidden` marker: integrations cannot read
  what the server cannot.
- **Relation to `.local`.** `.local` never leaves the device; hidden syncs to
  every granted device. Different rules, same naming-convention style.
