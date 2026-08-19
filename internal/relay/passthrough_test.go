package relay_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"log/slog"

	"github.com/pkronstrom/obsidian-archivist/internal/api"
	"github.com/pkronstrom/obsidian-archivist/internal/auth"
	"github.com/pkronstrom/obsidian-archivist/internal/relay"
	"github.com/pkronstrom/obsidian-archivist/internal/vaults"
)

// The point of the whole change: a scope on the CALLER's token decides what a
// relay call may do. Before pass-through every caller arrived as the relay, so
// the scopes on their own token were never evaluated by anything.
func TestScopesReachThroughTheRelay(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"personal", "work"} {
		if err := os.MkdirAll(filepath.Join(root, "vaults", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := vaults.NewRegistry(vaults.Layout{Root: root}, vaults.Options{
		MaxVaults: 5,
		Log:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })

	set := auth.NewSetForTest(map[string]auth.Principal{
		"reader": {Label: "reader", Vaults: []string{"personal"},
			Scopes: []string{auth.ScopeRead}},
		"writer": {Label: "writer", Vaults: []string{"personal"},
			Scopes: []string{auth.ScopeRead, auth.ScopeWrite, auth.ScopeDelete}},
		"other": {Label: "other", Vaults: []string{"work"},
			Scopes: []string{auth.ScopeRead, auth.ScopeWrite}},
	})
	upstream := httptest.NewServer(api.New(reg, set))
	defer upstream.Close()

	pool := relay.NewPool(upstream.URL, "relay")
	relaySrv := httptest.NewServer(relay.NewHandler(pool, nil, quiet(), nil, nil))
	defer relaySrv.Close()

	put := func(tok string) int {
		req, err := http.NewRequest("PUT",
			relaySrv.URL+"/file/notes/a.md?vault=personal", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := put("reader"); code != http.StatusForbidden {
		t.Errorf("read-only token writing through the relay = %d, want 403", code)
	}
	if code := put("writer"); code == http.StatusForbidden {
		t.Error("a write-scoped token was refused through the relay")
	}
	// Vault isolation, which the relay used to make meaningless: ?vault= is
	// caller-controlled, so the SERVER has to be the one refusing.
	if code := put("other"); code != http.StatusForbidden {
		t.Errorf("a token that does not open personal reached it = %d, want 403", code)
	}
}

// Resolving the default vault asks the server, so an unknown token fails THERE.
// Reporting that as 400 told a caller with a dead credential their request was
// malformed. Observed on the first deploy of pass-through.
func TestAnUnknownTokenIs401NotABadRequest(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "vaults", "personal"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := vaults.NewRegistry(vaults.Layout{Root: root}, vaults.Options{
		MaxVaults: 5, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	set := auth.NewSetForTest(map[string]auth.Principal{
		"good": {Label: "good", Vaults: []string{"personal"}, Scopes: []string{auth.ScopeRead}},
	})
	upstream := httptest.NewServer(api.New(reg, set))
	defer upstream.Close()

	relaySrv := httptest.NewServer(
		relay.NewHandler(relay.NewPool(upstream.URL, "relay"), nil, quiet(), nil, nil))
	defer relaySrv.Close()

	// No ?vault=, so the relay must resolve the default -- which is the path
	// that used to swallow the server's status.
	req, err := http.NewRequest("GET", relaySrv.URL+"/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer nobody-minted-this")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown token = %d, want 401 forwarded from the server", resp.StatusCode)
	}
}
