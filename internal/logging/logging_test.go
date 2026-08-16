package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLevelParsing(t *testing.T) {
	for _, s := range []string{"debug", "info", "", "warn", "error", "ERROR"} {
		if _, err := Level(s); err != nil {
			t.Errorf("Level(%q): %v", s, err)
		}
	}
	if _, err := Level("chatty"); err == nil {
		t.Error("want an error for an unknown level")
	}
}

// The point of rotation is a bounded footprint. Assert that, not the mechanics.
func TestRotationKeepsBoundedHistory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v.log")
	r, err := newRotator(p, 200, 2) // rotate at 200 bytes, keep 2 old files
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	line := strings.Repeat("x", 90) + "\n"
	for i := 0; i < 40; i++ {
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) > 3 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("kept %d files (%v), want at most 3: the live one plus 2", len(entries), names)
	}
	var total int64
	for _, e := range entries {
		fi, _ := e.Info()
		total += fi.Size()
	}
	// 40 lines of 91 bytes is ~3.6 KB written; bounded storage must be far less.
	if total > 1000 {
		t.Errorf("total on disk = %d bytes after writing ~3600; rotation is not bounding it", total)
	}
}
