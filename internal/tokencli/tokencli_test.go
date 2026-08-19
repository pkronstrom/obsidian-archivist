package tokencli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/auth"
)

func printedToken(t *testing.T, out string) string {
	t.Helper()
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, auth.TokenPrefix) {
			return f
		}
	}
	t.Fatalf("no token was printed:\n%s", out)
	return ""
}

func TestAddMintsATokenAndPrintsItOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	var out bytes.Buffer

	err := Run([]string{"add", "-tokens", path, "-label", "mac",
		"-vaults", "personal", "-scopes", "read,write"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	token := printedToken(t, out.String())

	set, err := auth.Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := set.Lookup(token)
	if !ok {
		t.Fatal("the printed token does not resolve against the file it was written to")
	}
	if p.Label != "mac" || !p.Can(auth.ScopeWrite) || p.Can(auth.ScopeDelete) {
		t.Errorf("wrong principal: %+v", p)
	}
}

func TestAddAppendsRatherThanReplacing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	var out bytes.Buffer
	if err := Run([]string{"add", "-tokens", path, "-label", "one",
		"-vaults", "personal", "-scopes", "read"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"add", "-tokens", path, "-label", "two",
		"-vaults", "personal", "-scopes", "read"}, &out); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Tokens map[string]json.RawMessage `json:"tokens"`
	}
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Tokens) != 2 {
		t.Errorf("file holds %d tokens after two adds, want 2", len(f.Tokens))
	}
}

func TestAddRejectsAnUnknownScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	var out bytes.Buffer
	err := Run([]string{"add", "-tokens", path, "-label", "x",
		"-vaults", "personal", "-scopes", "read,admin"}, &out)
	if err == nil {
		t.Fatal("an unknown scope was accepted; the typo would silently grant nothing")
	}
	if !strings.Contains(err.Error(), "admin") {
		t.Errorf("the error must name the bad scope, got: %v", err)
	}
}

func TestListShowsLabelsAndHashesButNoSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	var mint bytes.Buffer
	if err := Run([]string{"add", "-tokens", path, "-label", "mac",
		"-vaults", "personal", "-scopes", "read"}, &mint); err != nil {
		t.Fatal(err)
	}
	token := printedToken(t, mint.String())

	var out bytes.Buffer
	if err := Run([]string{"list", "-tokens", path}, &out); err != nil {
		t.Fatal(err)
	}
	listing := out.String()
	if !strings.Contains(listing, "mac") {
		t.Errorf("listing does not name the token:\n%s", listing)
	}
	if strings.Contains(listing, token) {
		t.Error("listing printed the secret; it must only ever be shown at mint time")
	}
}

func TestRevokeRemovesByHashPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	var mint bytes.Buffer
	if err := Run([]string{"add", "-tokens", path, "-label", "gone",
		"-vaults", "personal", "-scopes", "read"}, &mint); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"add", "-tokens", path, "-label", "stays",
		"-vaults", "personal", "-scopes", "read"}, &mint); err != nil {
		t.Fatal(err)
	}

	set, _ := auth.Load(path, "")
	var target string
	for hash, p := range set.Entries() {
		if p.Label == "gone" {
			target = hash
		}
	}

	var out bytes.Buffer
	if err := Run([]string{"revoke", "-tokens", path, target[:12]}, &out); err != nil {
		t.Fatal(err)
	}

	after, err := auth.Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range after.Entries() {
		if p.Label == "gone" {
			t.Error("the revoked token is still in the file")
		}
	}
	if len(after.Entries()) != 1 {
		t.Errorf("%d tokens remain, want 1", len(after.Entries()))
	}
}

func TestRevokeRefusesAnAmbiguousPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	var mint bytes.Buffer
	for _, label := range []string{"a", "b"} {
		if err := Run([]string{"add", "-tokens", path, "-label", label,
			"-vaults", "personal", "-scopes", "read"}, &mint); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	// The empty prefix matches everything.
	err := Run([]string{"revoke", "-tokens", path, ""}, &out)
	if err == nil {
		t.Fatal("an ambiguous prefix revoked something; it must refuse instead")
	}
}

func TestHandlesOnlyClaimsToken(t *testing.T) {
	if !Handles("token") {
		t.Error("token is this package's subcommand")
	}
	if Handles("history") || Handles("") {
		t.Error("this package must not claim other subcommands")
	}
}
