package merge

import (
	"bytes"
	"strings"
	"testing"
)

// The twelve cases the vendored library was validated against, encoded so a
// re-vendoring cannot silently regress. Each was checked against
// `git merge-file` before adoption: 12 agree, 0 disagree on the verdict.
func TestVerdictsMatchGitMergeFile(t *testing.T) {
	cases := []struct {
		name             string
		base, ours, thrs string
		wantConflict     bool
	}{
		{"disjoint-edits", "one\ntwo\nthree\n", "ONE\ntwo\nthree\n", "one\ntwo\nTHREE\n", false},
		{"same-line-conflict", "one\n", "ours\n", "theirs\n", true},
		{"identical-change-both-sides", "one\ntwo\n", "ONE\ntwo\n", "ONE\ntwo\n", false},
		{"insert-same-place-differently", "a\nb\n", "a\nX\nb\n", "a\nY\nb\n", true},
		{"delete-vs-edit", "a\nb\nc\n", "a\nc\n", "a\nB\nc\n", true},
		{"append-both-ends", "a\n", "a\nours\n", "a\ntheirs\n", true},
		{"only-one-side-changed", "a\nb\n", "a\nb\n", "a\nB\n", false},
		{"no-trailing-newline", "a\nb", "a\nB", "a\nb", false},
		{"empty-base", "", "ours\n", "theirs\n", true},
		{"unicode-and-emoji", "Ä\nö\n", "Ä\nÖ\n", "Ä\nö\n🎉\n", true},
		{"large-disjoint",
			strings.Repeat("line\n", 500) + "end\n",
			"HEAD\n" + strings.Repeat("line\n", 500) + "end\n",
			strings.Repeat("line\n", 500) + "end\nTAIL\n", false},
		{"whitespace-only-change", "a\nb\n", "a \nb\n", "a\nb \n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, conflict, err := Merge([]byte(c.base), []byte(c.ours), []byte(c.thrs))
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			if conflict != c.wantConflict {
				t.Errorf("conflict = %v, want %v", conflict, c.wantConflict)
			}
		})
	}
}

// The vendored library drops the trailing newline; git merge-file keeps it.
// Without this fix every merged note loses its final newline, which then shows
// up as a spurious one-line diff on the very next sync.
func TestPreservesTrailingNewline(t *testing.T) {
	got, conflict, err := Merge(
		[]byte("one\ntwo\nthree\n"),
		[]byte("ONE\ntwo\nthree\n"),
		[]byte("one\ntwo\nTHREE\n"),
	)
	if err != nil || conflict {
		t.Fatalf("conflict=%v err=%v", conflict, err)
	}
	if string(got) != "ONE\ntwo\nTHREE\n" {
		t.Errorf("merged = %q, want the trailing newline preserved", got)
	}
}

// The mirror of the above: if the inputs had no trailing newline, we must not
// invent one.
func TestDoesNotInventATrailingNewline(t *testing.T) {
	got, conflict, err := Merge([]byte("a\nb"), []byte("a\nB"), []byte("a\nb"))
	if err != nil || conflict {
		t.Fatalf("conflict=%v err=%v", conflict, err)
	}
	if strings.HasSuffix(string(got), "\n") {
		t.Errorf("merged = %q, want no trailing newline added", got)
	}
}

func TestConflictProducesMarkers(t *testing.T) {
	got, conflict, err := Merge([]byte("one\n"), []byte("ours\n"), []byte("theirs\n"))
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !conflict {
		t.Fatal("conflict = false, want true")
	}
	for _, marker := range []string{"<<<<<<<", "=======", ">>>>>>>"} {
		if !bytes.Contains(got, []byte(marker)) {
			t.Errorf("output lacks %q marker: %q", marker, got)
		}
	}
}

func TestIdenticalSidesMergeToThemselves(t *testing.T) {
	same := []byte("unchanged\n")
	got, conflict, err := Merge(same, same, same)
	if err != nil || conflict {
		t.Fatalf("conflict=%v err=%v", conflict, err)
	}
	if string(got) != string(same) {
		t.Errorf("merged = %q, want %q", got, same)
	}
}

func TestIsBinaryDetectsNulBytes(t *testing.T) {
	if !IsBinary([]byte("PK\x03\x04\x00\x00some zip")) {
		t.Error("IsBinary = false for content with NUL bytes")
	}
	if IsBinary([]byte("# A perfectly ordinary note\n\nwith ä and 🎉\n")) {
		t.Error("IsBinary = true for UTF-8 text")
	}
	if IsBinary(nil) {
		t.Error("IsBinary = true for empty content")
	}
}
