# Large-note MCP access

Date: 2026-08-26
Status: approved design

## Problem

`read_note` and `write_note` currently transfer a whole Markdown document. Notes around 100,000 characters exceed some MCP hosts' tool-result limits, so an agent cannot inspect or safely update them through MCP. The observed workaround calls the relay file API through `tools/vault_http.py`; this bypasses the model-facing surface and still requires custom whole-file manipulation.

The first affected workflow is appending entries to long triage ledgers. Agents also need bounded reads and precise corrections to existing entries without reconstructing the entire note.

MCP does not define paging for `tools/call`, partial writes, conditional writes, or a result-size limit. Archivist must define these semantics in its tool schemas.

## Goals

- Read every text note through bounded MCP results.
- Continue a read without joining pages from different note versions.
- Start near a `search_notes` line hit without reading the preceding document.
- Append to an existing ledger atomically without reading it first.
- Replace one exact text fragment without sending the whole note to the model.
- Refuse stale, moved, deleted, ambiguous, or missing edit targets without changing data.
- Preserve existing `read_note` and `write_note` callers.
- Return a per-note content revision after every successful mutation.

## Non-goals

- Regular-expression replacement.
- Fuzzy or whitespace-normalized matching.
- Heading, block, or frontmatter-aware Markdown patches.
- Automatically following a note after a rename.
- Streaming partial MCP tool results or relying on `resource_link` support.
- Removing whole-document `write_note`.

## Tool surface

### `read_note`

Extend the existing tool; do not add a competing `read_note_page` tool.

Input:

```json
{
  "vault": "optional vault",
  "path": "2. Areas/Tech/NanoClaw Triage.md",
  "start_line": 1740,
  "cursor": "opaque continuation token",
  "max_chars": 16000
}
```

Rules:

- `path` remains required.
- `start_line` is one-based, defaults to 1, and is valid only without `cursor`.
- `cursor` and `start_line` are mutually exclusive.
- `max_chars` defaults to 16,000 and has a server maximum of 100,000. A caller asking for a larger result owns the MCP host-cap risk.
- `max_chars` counts Unicode code points. A page always ends at a valid UTF-8 boundary.
- A cursor records position independently of `max_chars`, so the next call may request a different page size.
- A short note still returns in one call. An oversized note returns its first bounded page rather than failing at the host boundary.
- Binary input remains refused and points to `read_attachment`.

Output:

```json
{
  "path": "2. Areas/Tech/NanoClaw Triage.md",
  "content": "bounded page",
  "revision": "repository commit used as write_note's merge base",
  "content_revision": "40-character Git blob hash",
  "has_more": true,
  "next_cursor": "23-character base64url token",
  "note": "pass next_cursor as cursor to continue"
}
```

`revision` keeps its current meaning for compatibility. `content_revision` is the per-note validator used by `edit_note`, guarded `append_note`, and cursor validation.

The continuation prose is present only when `has_more` is true and names the schema key exactly. Structured fields remain authoritative.

### `read_note_at`

Adopt the same `start_line`, `cursor`, and `max_chars` inputs and the same bounded output. Its requested historical `revision` continues to identify the repository commit; `content_revision` identifies the note bytes at that commit.

### `append_note`

Input:

```json
{
  "vault": "optional vault",
  "path": "2. Areas/Tech/NanoClaw Triage.md",
  "content": "\n## 2026-08-26 08:00 — ...\n...",
  "content_revision": "optional 40-character Git blob hash"
}
```

Rules:

- The target must already exist and be text. Append never creates a note.
- The append is one atomic server mutation.
- Without `content_revision`, the operation intentionally appends to the current text note at that exact path.
- With `content_revision`, the current path and content must still match it.
- Archivist never follows a rename. A moved or deleted target returns `not_found`.

Output:

```json
{
  "path": "...",
  "status": "applied",
  "revision": "new repository commit",
  "content_revision": "new Git blob hash"
}
```

### `edit_note`

Input:

```json
{
  "vault": "optional vault",
  "path": "2. Areas/Tech/NanoClaw Triage.md",
  "content_revision": "required 40-character Git blob hash",
  "old_text": "exact existing UTF-8 text",
  "new_text": "literal replacement UTF-8 text"
}
```

Rules:

- `content_revision` is required.
- The path must still exist and contain text.
- The current file's Git blob hash must equal `content_revision`.
- `old_text` must be non-empty and occur exactly once.
- Matching is byte-exact UTF-8. There is no regex, fuzzy match, normalization, or replace-all mode.
- Validation and replacement occur within one server mutation lock.
- The operation never follows history to a renamed path and never recreates an absent path.

Output on success matches `append_note`. A refusal returns a stable error code and, for `stale`, the current `content_revision`; it never returns the full current note.

### `write_note`

Keep whole-document replacement and its current repository-commit `revision` merge-base contract. Add the resulting `revision` and `content_revision` to successful output so callers can chain later operations without another read.

## Cursor format

The relay remains stateless. A continuation cursor is 17 binary bytes:

| Bytes | Meaning |
| --- | --- |
| 1 | cursor format version, initially `1` |
| 12 | first 96 bits of the raw Git blob SHA-1 |
| 4 | big-endian unsigned byte offset of the next page |

Encode the bytes with unpadded URL-safe base64. The result is always 23 characters.

The cursor uses a 96-bit hash prefix because it is a read-continuation consistency check, not an authorization credential or mutation validator. Accidental collision risk is negligible. Mutations continue to require the full 160-bit blob hash.

The offset is a byte position even though `max_chars` counts Unicode code points. Byte offsets make continuation constant-time and exact. The server validates that the offset is within the file and at a UTF-8 boundary.

On continuation, the server:

1. decodes the token and validates its version and offset;
2. reads a consistent snapshot of the requested path;
3. computes the current Git blob hash;
4. compares its first 96 bits with the cursor;
5. returns `stale_cursor` when they differ;
6. returns `not_found` when the requested path moved or disappeared;
7. reads from the recorded byte offset using the newly requested `max_chars`.

The cursor does not include a path digest. Reusing it on another path can only continue when that path is byte-identical, which exposes no additional bytes or authority. A path digest would lengthen the token without strengthening mutation safety.

## Identity and stale-data model

Archivist notes have paths and history, but no stable note IDs. This design therefore treats identity as `(path, content_revision)`:

- same path, different content: stale;
- missing path after rename or deletion: not found;
- new file at the old path: stale unless it has byte-identical content;
- same content moved elsewhere: not followed automatically.

This deliberately prefers a visible retry over guessing where an edit belongs.

The repository commit remains necessary as `write_note`'s three-way merge base. It cannot silently become a per-note revision. The existing Git blob object ID is already a content-addressed per-note validator, so no new hash algorithm or revision store is introduced.

## Server architecture

Read paging belongs in the relay because it bounds MCP output and needs no new storage. The relay reads a consistent `(repository revision, body)` snapshot through the client, derives `content_revision`, validates or creates the cursor, and returns a slice.

Mutations must not be implemented as relay-side read/concatenate/write sequences. `append_note` and `edit_note` require new server operations executed by `Reconciler` while holding its existing mutation mutex. That lock already serializes pushes and filesystem scans. Each operation validates the current path and content, writes through the existing guarded write path, commits once, records origin, and emits the normal event.

The client package exposes typed methods for those server operations. MCP handlers remain thin adapters that select the vault, validate tool-level input, call the client, and map protocol results.

## Errors

Stable machine-readable codes:

| Code | Meaning | Mutation |
| --- | --- | --- |
| `invalid_cursor` | malformed token, unsupported version, impossible offset, or non-UTF-8 boundary | none |
| `stale_cursor` | the note changed between pages | none |
| `not_found` | path moved, was deleted, or never existed | none |
| `not_text` | target is binary | none |
| `stale` | full `content_revision` no longer matches | none |
| `no_match` | `old_text` does not occur | none |
| `multiple_matches` | `old_text` occurs more than once | none |
| `malformed` | empty target, invalid line, invalid size, or incompatible inputs | none |

No error response includes a full note. `stale` may include the current `content_revision` to make the required retry explicit.

## Concurrency invariants

- Page continuation never combines bytes from two content revisions.
- `edit_note` either applies against the exact content revision supplied or changes nothing.
- Two concurrent unguarded appends both survive in lock-acquisition order.
- A guarded append either applies against its exact supplied revision or changes nothing.
- A move racing an edit or guarded append wins or loses the lock; the second operation observes the completed first operation and refuses when the original path is gone or stale.
- Mutation output identifies both the new repository commit and new per-note content revision.

## Compatibility

Adding optional `read_note` and `read_note_at` inputs is schema-compatible. Existing short-note callers receive the whole content as before. Existing oversized calls change from a host-level failure to a bounded first page.

The existing `revision` field retains its repository-commit meaning. New callers use `content_revision` for targeted operations. Existing `write_note` callers remain valid.

The two new tools require write scope. They do not require delete scope. Tool documentation must distinguish blind unguarded append from compare-and-swap edit.

## Verification

Tests must defend these observable contracts:

1. A 100,000-character note can be read to completion through cursors with no missing or duplicated Unicode content.
2. `max_chars` can change between pages.
3. A continuation after any note change returns `stale_cursor`.
4. A continuation after a move returns `not_found` and does not follow the destination.
5. `start_line` begins at the requested one-based line; invalid combinations refuse.
6. Cursor decoding rejects malformed, wrong-version, out-of-range, and mid-code-point offsets.
7. A unique exact edit changes only the requested bytes and returns both new revisions.
8. Missing and duplicate edit targets refuse without changing the note.
9. A stale edit and an edit racing a move refuse without recreating or modifying another path.
10. Two concurrent unguarded appends preserve both entries exactly once.
11. A stale guarded append changes nothing.
12. Historical large-note reads page under the same bounds.
13. Existing short `read_note` and merge-aware `write_note` behavior remains intact.

A live MCP smoke test must read a real long note in at least two pages, append a disposable entry to a disposable long note, perform a guarded exact edit, and observe the returned revisions. It must not mutate the production triage ledger during verification.

## Deferred work

If exact replacement proves insufficient, add a separate Markdown structural patch tool with explicit heading/block/frontmatter targets. Regex replacement, if ever justified, belongs in a separate tool with an expected match count and a fixed regex dialect; it must not become an optional mode of `edit_note`.
