package pins

import (
	"strings"
	"testing"
	"time"
)

// One corrupt line must cost that pin and nothing else. This is the whole
// reason the file is JSONL rather than a JSON array: an array fails to parse
// in its entirety, so a single bad merge would lose every pin at once.
func TestParseSkipsCorruptLinesOnly(t *testing.T) {
	in := strings.Join([]string{
		`{"id":"a1","name":"before reorg","created":"2026-08-22T10:00:00Z"}`,
		`{"id":"b2","name":"trunc`,
		``,
		`not json at all`,
		`{"id":"c3","name":"sent to anna","path":"Note.md","created":"2026-08-22T11:00:00Z"}`,
		// An entry without an id cannot be resolved to a commit, so it is not
		// a pin -- dropping it beats listing something unopenable.
		`{"name":"no id","created":"2026-08-22T12:00:00Z"}`,
	}, "\n")

	got := Parse([]byte(in))
	if len(got) != 2 {
		t.Fatalf("Parse kept %d entries, want 2: %+v", len(got), got)
	}
	if got[0].ID != "a1" || got[1].ID != "c3" {
		t.Errorf("wrong survivors: %+v", got)
	}
	if got[1].Path != "Note.md" {
		t.Errorf("path lost: %+v", got[1])
	}
}

// Append must touch exactly one line. A JSON array would also rewrite the
// previous entry's comma and the closing bracket, so two independent pins
// would collide structurally even though they do not overlap in meaning.
func TestAppendAddsExactlyOneLine(t *testing.T) {
	first, err := Append(nil, Entry{ID: "a1", Name: "one", Created: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Append(first, Entry{ID: "b2", Name: "two", Created: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(second), string(first)) {
		t.Fatal("append rewrote earlier content instead of adding to it")
	}
	if n := strings.Count(strings.TrimRight(string(second), "\n"), "\n"); n != 1 {
		t.Fatalf("second file has %d newlines between entries, want 1", n)
	}
	if len(Parse(second)) != 2 {
		t.Fatal("appended file does not parse back to two entries")
	}
}

// A file missing its trailing newline (hand edit, truncated write) must not
// merge two entries into one unparseable line.
func TestAppendRepairsMissingTrailingNewline(t *testing.T) {
	existing := []byte(`{"id":"a1","name":"one","created":"2026-08-22T10:00:00Z"}`)
	out, err := Append(existing, Entry{ID: "b2", Name: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if got := Parse(out); len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %q", len(got), out)
	}
}

func TestNewIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id := NewID()
		if id == "" || seen[id] {
			t.Fatalf("bad id %q at %d", id, i)
		}
		seen[id] = true
	}
}
