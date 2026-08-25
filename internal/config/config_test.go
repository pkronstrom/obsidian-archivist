package config

import (
	"strings"
	"testing"
)

func TestEnvFallback(t *testing.T) {
	// ARCHIVIST_VAULT and ARCHIVIST_GIT are gone: one process serves every
	// vault under ARCHIVIST_ROOT, and the per-vault paths are derived from it.
	t.Setenv("ARCHIVIST_ROOT", "/srv/knowledge")
	t.Setenv("ARCHIVIST_TOKENS", "/srv/knowledge/.archivist/tokens.json")
	c, err := Load([]string{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Root != "/srv/knowledge" {
		t.Errorf("Root = %q, want /srv/knowledge", c.Root)
	}
	if c.Listen != ":8090" {
		t.Errorf("Listen = %q, want the default :8090", c.Listen)
	}
}

func TestFlagBeatsEnv(t *testing.T) {
	t.Setenv("ARCHIVIST_LISTEN", ":1111")
	t.Setenv("ARCHIVIST_ROOT", "/v")
	t.Setenv("ARCHIVIST_TOKENS", "/v/.archivist/tokens.json")
	c, err := Load([]string{"-listen", ":2222"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != ":2222" {
		t.Errorf("Listen = %q, want :2222 (an explicit flag must win)", c.Listen)
	}
}

func TestTokensFileIsRequired(t *testing.T) {
	t.Setenv("ARCHIVIST_ROOT", "/v")
	t.Setenv("ARCHIVIST_TOKENS", "")
	t.Setenv("ARCHIVIST_TOKEN", "legacy-token-must-not-count")
	if _, err := Load([]string{}); err == nil || !strings.Contains(err.Error(), "ARCHIVIST_TOKENS") {
		t.Fatalf("Load without a token file = %v, want actionable refusal", err)
	}
}

func TestMissingVaultIsAnError(t *testing.T) {
	t.Setenv("ARCHIVIST_TOKENS", "/v/.archivist/tokens.json")
	if _, err := Load([]string{}); err == nil {
		t.Fatal("want an error when the vault is unset, got nil")
	}
}

func TestWatchCanBeDisabled(t *testing.T) {
	t.Setenv("ARCHIVIST_ROOT", "/v")
	t.Setenv("ARCHIVIST_TOKENS", "/v/.archivist/tokens.json")
	c, err := Load([]string{"-watch=false"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Watch {
		t.Error("Watch = true, want false")
	}
}

func TestBadDebounceIsAnError(t *testing.T) {
	t.Setenv("ARCHIVIST_ROOT", "/v")
	t.Setenv("ARCHIVIST_TOKENS", "/v/.archivist/tokens.json")
	t.Setenv("ARCHIVIST_DEBOUNCE", "not-a-duration")
	if _, err := Load([]string{}); err == nil {
		t.Fatal("want an error for an unparseable debounce, got nil")
	}
}

func TestRootIsRequired(t *testing.T) {
	t.Setenv("ARCHIVIST_ROOT", "")
	t.Setenv("ARCHIVIST_TOKENS", "/v/.archivist/tokens.json")
	if _, err := Load(nil); err == nil {
		t.Fatal("a root directory is required")
	}
}

func TestRootFromFlagAndEnvironment(t *testing.T) {
	t.Setenv("ARCHIVIST_ROOT", "/from-env")
	t.Setenv("ARCHIVIST_TOKENS", "/from-env/.archivist/tokens.json")

	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Root != "/from-env" {
		t.Errorf("Root = %q, want /from-env", c.Root)
	}

	c, err = Load([]string{"-root", "/from-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Root != "/from-flag" {
		t.Errorf("a flag must beat the environment: Root = %q", c.Root)
	}
}

func TestMaxVaultsDefaultsToFive(t *testing.T) {
	t.Setenv("ARCHIVIST_ROOT", "/data")
	t.Setenv("ARCHIVIST_TOKENS", "/data/.archivist/tokens.json")
	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxVaults != 5 {
		t.Errorf("MaxVaults = %d, want 5", c.MaxVaults)
	}
}
