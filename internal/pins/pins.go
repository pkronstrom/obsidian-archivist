// Package pins stores named restore points as lines in an ordinary tracked
// vault file.
//
// Three designs preceded this one and each failed on durability or machinery.
// A store of commit hashes does not survive Prune: it rewrites history and
// keeps only the head pair of its translation map, so a hash naming any older
// commit is dangling afterwards. Named empty commits fixed identity but needed
// AllowEmptyCommits, trailer sanitising and a full-log scan to list.
//
// A tracked file needs none of that. It is live at HEAD so prune never drops
// it, push/merge/history/at all apply unchanged, and listing is one read. The
// entries deliberately carry NO commit hash -- a pin's snapshot is the tree of
// the commit that first introduced its id, which is derived rather than
// stored, and therefore cannot dangle.
package pins

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// File is the canonical path, in the vault root.
//
// Not under .obsidian: config sync refuses everything there at the default
// level and the plugin's scan does not descend into it, so a pulled file would
// be invisible locally and get re-sent as a DELETION. The root also means a
// conflict copy syncs like any other note, which is what makes a losing side
// recoverable at all.
const File = "pins.jsonl"

// Entry is one pin. Immutable once written, except by removal: first
// appearance is what identifies a pin's snapshot, so rewriting a line would
// silently move the snapshot it names.
type Entry struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Path    string    `json:"path,omitempty"` // empty means a vault-wide pin
	Created time.Time `json:"created"`
}

// NewID returns an opaque identifier. Server-generated and random rather than
// derived from the name or the commit: names repeat across files, and a commit
// hash is exactly what must not be stored.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a timestamp still yields a
		// usable id rather than an empty one.
		return hex.EncodeToString([]byte(time.Now().UTC().Format("20060102150405.000000")))
	}
	return hex.EncodeToString(b[:])
}

// Parse reads entries, skipping lines it cannot decode.
//
// Skipping rather than failing is the reason for JSONL: one corrupt line -- a
// bad merge, a hand edit, a truncated write -- costs that pin instead of the
// whole file. A JSON array would fail to parse in its entirety.
func Parse(b []byte) []Entry {
	out := []Entry{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil || e.ID == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Append returns the file content with one entry added.
//
// Appending a single line is what keeps concurrent pins mergeable: a JSON
// array would touch the previous line's comma and the closing bracket too, so
// two independent additions would collide structurally even though they do not
// overlap in meaning.
func Append(existing []byte, e Entry) ([]byte, error) {
	line, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	out := existing
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	out = append(out, line...)
	out = append(out, '\n')
	return out, nil
}
