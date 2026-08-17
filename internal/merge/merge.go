// Package merge is three-way text merging.
//
// The algorithm is vendored under diff3/ (see its PROVENANCE.md). This file is
// the boundary that corrects for its one known defect, so the vendored files
// stay a clean copy of upstream and re-vendoring is a plain overwrite.
package merge

import (
	"bytes"
	"io"

	"github.com/pkronstrom/obsidian-archivist/internal/merge/diff3"
)

// Merge performs a three-way merge of ours and theirs against base.
//
// On conflict the returned content carries ordinary git-style conflict markers
// and conflict is true. Callers decide what to do with that -- this package
// takes no position.
func Merge(base, ours, theirs []byte) (result []byte, conflict bool, err error) {
	return MergeLabelled(base, ours, theirs, "ours", "theirs")
}

// MergeLabelled is Merge with the names that appear in the conflict markers.
//
// The labels matter more than they look. "ours" and "theirs" are meaningless to
// someone opening a conflicted note on a phone a day later -- they cannot tell
// which side is which. "server" and the device name say it outright.
func MergeLabelled(base, ours, theirs []byte, ourLabel, theirLabel string) (result []byte, conflict bool, err error) {
	r, err := diff3.Merge(
		bytes.NewReader(ours),
		bytes.NewReader(base),
		bytes.NewReader(theirs),
		true, // exclude false conflicts: identical edits on both sides are not a conflict
		ourLabel, theirLabel,
	)
	if err != nil {
		return nil, false, err
	}
	out, err := io.ReadAll(r.Result)
	if err != nil {
		return nil, false, err
	}
	if r.Conflicts {
		out = gitWidthMarkers(out, ourLabel, theirLabel)
	}
	return fixTrailingNewline(out, ours, theirs), r.Conflicts, nil
}

// gitWidthMarkers rewrites the vendored diff3's nine-character markers to git's
// seven. Nothing recognises nine: not git, not a merge tool, not an editor's
// conflict highlighting.
//
// Done here rather than in diff3.go because that file is vendored verbatim and
// its provenance note says so -- keeping it pristine means the next upstream
// comparison is a clean diff.
//
// Only the exact lines diff3 emits are rewritten, label included, so a note that
// happens to contain a row of angle brackets is left alone.
func gitWidthMarkers(out []byte, ourLabel, theirLabel string) []byte {
	for _, r := range []struct{ from, to string }{
		{"<<<<<<<<< " + ourLabel, "<<<<<<< " + ourLabel},
		{"=========", "======="},
		{">>>>>>>>> " + theirLabel, ">>>>>>> " + theirLabel},
	} {
		out = bytes.ReplaceAll(out,
			append([]byte("\n"), r.from...), append([]byte("\n"), r.to...))
		// The first marker can also open the file, with no preceding newline.
		if bytes.HasPrefix(out, []byte(r.from)) {
			out = append([]byte(r.to), out[len(r.from):]...)
		}
	}
	return out
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
