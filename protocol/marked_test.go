package protocol

import (
	"encoding/json"
	"os"
	"testing"
)

// The same table is read by test/unit/marked-paths.test.mjs. The rule exists
// twice, in Go and in TypeScript, and a path the two disagree about is one
// the plugin sends in plaintext and the server stores.
func TestMarkedPathsMatchTheSharedTable(t *testing.T) {
	raw, err := os.ReadFile("testdata/marked-paths.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Path   string `json:"path"`
		Marked bool   `json:"marked"`
		Valid  bool   `json:"valid"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the table is empty")
	}
	for _, c := range cases {
		if got := EncryptedPath(c.Path); got != c.Marked {
			t.Errorf("EncryptedPath(%q) = %v, want %v", c.Path, got, c.Marked)
		}
		if got := ValidMarkedPath(c.Path); got != c.Valid {
			t.Errorf("ValidMarkedPath(%q) = %v, want %v", c.Path, got, c.Valid)
		}
	}
}
