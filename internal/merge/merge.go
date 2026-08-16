// Package merge is three-way text merging.
//
// The algorithm is vendored under diff3/ (see its PROVENANCE.md). This file is
// the boundary that corrects for its one known defect, so the vendored files
// stay a clean copy of upstream and re-vendoring is a plain overwrite.
package merge

import (
	"bytes"
	"io"

	"github.com/pkronstrom/vaultsync/internal/merge/diff3"
)

// Merge performs a three-way merge of ours and theirs against base.
//
// On conflict the returned content carries ordinary git-style conflict markers
// and conflict is true. Callers decide what to do with that -- this package
// takes no position.
func Merge(base, ours, theirs []byte) (result []byte, conflict bool, err error) {
	r, err := diff3.Merge(
		bytes.NewReader(ours),
		bytes.NewReader(base),
		bytes.NewReader(theirs),
		true, // exclude false conflicts: identical edits on both sides are not a conflict
		"ours", "theirs",
	)
	if err != nil {
		return nil, false, err
	}
	out, err := io.ReadAll(r.Result)
	if err != nil {
		return nil, false, err
	}
	return fixTrailingNewline(out, ours, theirs), r.Conflicts, nil
}

// fixTrailingNewline restores a final newline the merge dropped.
//
// The vendored diff3 works line-wise and does not re-emit a trailing newline;
// `git merge-file` does. Left alone, every merged note would lose its final
// newline -- which then appears as a spurious one-line diff on the next sync,
// forever, on every merged file.
//
// The rule is conservative in both directions: restore a newline only when a
// LIVING side had one -- base is history and does not get a vote -- and never
// invent one for content that did not.
func fixTrailingNewline(out, ours, theirs []byte) []byte {
	if len(out) == 0 || bytes.HasSuffix(out, []byte("\n")) {
		return out
	}
	// Ours and theirs are what the user actually has; base is history. If
	// either living side ended with a newline, the merged file should too.
	if endsWithNewline(ours) || endsWithNewline(theirs) {
		return append(out, '\n')
	}
	return out
}

func endsWithNewline(b []byte) bool {
	return len(b) > 0 && b[len(b)-1] == '\n'
}

// IsBinary reports whether content should be treated as binary and therefore
// never merged.
//
// The NUL-byte heuristic is what git itself uses. It is not perfect, but the
// failure mode is benign: a misjudged text file is kept-both instead of merged,
// which loses no data and is visible to the user.
func IsBinary(b []byte) bool {
	const sniff = 8000
	if len(b) > sniff {
		b = b[:sniff]
	}
	return bytes.IndexByte(b, 0) >= 0
}
