package config

import "testing"

func TestEnvFallback(t *testing.T) {
	t.Setenv("VAULTSYNC_VAULT", "/srv/knowledge/test")
	t.Setenv("VAULTSYNC_TOKEN", "secret")
	c, err := Load([]string{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Vault != "/srv/knowledge/test" {
		t.Errorf("Vault = %q, want /srv/knowledge/test", c.Vault)
	}
	if c.Listen != ":8090" {
		t.Errorf("Listen = %q, want the default :8090", c.Listen)
	}
	if c.Git != "/var/lib/vaultsync/git" {
		t.Errorf("Git = %q, want the default", c.Git)
	}
}

func TestFlagBeatsEnv(t *testing.T) {
	t.Setenv("VAULTSYNC_LISTEN", ":1111")
	t.Setenv("VAULTSYNC_VAULT", "/v")
	t.Setenv("VAULTSYNC_TOKEN", "s")
	c, err := Load([]string{"-listen", ":2222"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != ":2222" {
		t.Errorf("Listen = %q, want :2222 (an explicit flag must win)", c.Listen)
	}
}

func TestMissingTokenIsAnError(t *testing.T) {
	t.Setenv("VAULTSYNC_VAULT", "/v")
	if _, err := Load([]string{}); err == nil {
		t.Fatal("want an error when the token is unset, got nil")
	}
}

func TestMissingVaultIsAnError(t *testing.T) {
	t.Setenv("VAULTSYNC_TOKEN", "s")
	if _, err := Load([]string{}); err == nil {
		t.Fatal("want an error when the vault is unset, got nil")
	}
}

func TestWatchCanBeDisabled(t *testing.T) {
	t.Setenv("VAULTSYNC_VAULT", "/v")
	t.Setenv("VAULTSYNC_TOKEN", "s")
	c, err := Load([]string{"-watch=false"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Watch {
		t.Error("Watch = true, want false")
	}
}

func TestBadDebounceIsAnError(t *testing.T) {
	t.Setenv("VAULTSYNC_VAULT", "/v")
	t.Setenv("VAULTSYNC_TOKEN", "s")
	t.Setenv("VAULTSYNC_DEBOUNCE", "not-a-duration")
	if _, err := Load([]string{}); err == nil {
		t.Fatal("want an error for an unparseable debounce, got nil")
	}
}
