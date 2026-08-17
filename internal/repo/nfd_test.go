package repo

import (
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

// Real vault filenames that were silently not tracked on the server.
func TestNonASCIIFilenamesAreTracked(t *testing.T) {
	cases := []struct{ name, path string }{
		{"NFD a-umlaut (macOS form)", "Oirepäiväkirja.md"},
		{"NFC a-umlaut (Linux form)", "Oirepäiväkirja.md"},
		{"emoji", "star \U0001F31F note.md"},
		{"double spaces", "a  b  c.md"},
		{"plain ascii control", "plain.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			work := filepath.Join(base, "vault")
			if err := os.MkdirAll(work, 0o755); err != nil {
				t.Fatal(err)
			}
			r, err := Open(work, filepath.Join(base, "git"))
			if err != nil {
				t.Fatal(err)
			}
			r.SetSyncable(func(string) bool { return true })

			if err := os.WriteFile(filepath.Join(work, c.path), []byte("body\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			head, err := r.Commit("add")
			if err != nil {
				t.Fatalf("commit: %v", err)
			}
			files, err := r.Snapshot(head)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := files[c.path]; !ok {
				got := make([]string, 0, len(files))
				for p := range files {
					got = append(got, p)
				}
				t.Errorf("%q NOT tracked (valid utf8: %v). tracked instead: %q",
					c.path, utf8.ValidString(c.path), got)
			}
		})
	}
}
