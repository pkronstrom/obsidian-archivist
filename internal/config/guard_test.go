package config

import (
	"testing"
	"time"
)

// minimalEnv sets only what Load requires, so each test asserts defaults
// rather than whatever the developer's shell happens to export.
func minimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ARCHIVIST_ROOT", t.TempDir())
	t.Setenv("ARCHIVIST_GIT", t.TempDir())
	t.Setenv("ARCHIVIST_TOKEN", "t")
}

func TestGuardDefaults(t *testing.T) {
	minimalEnv(t)

	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.QuarantineWrites != 300 {
		t.Errorf("QuarantineWrites = %d, want 300", c.QuarantineWrites)
	}
	if c.QuarantinePathBytes != 100<<20 {
		t.Errorf("QuarantinePathBytes = %d, want %d", c.QuarantinePathBytes, 100<<20)
	}
	if c.QuarantineTotalBytes != 2<<30 {
		t.Errorf("QuarantineTotalBytes = %d, want %d", c.QuarantineTotalBytes, 2<<30)
	}
	if c.QuarantineWindow != 5*time.Minute {
		t.Errorf("QuarantineWindow = %v, want 5m", c.QuarantineWindow)
	}
	if c.QuarantineCooldown != 15*time.Minute {
		t.Errorf("QuarantineCooldown = %v, want 15m", c.QuarantineCooldown)
	}
	if c.ThrottleMaxDebounce != 60*time.Second {
		t.Errorf("ThrottleMaxDebounce = %v, want 60s", c.ThrottleMaxDebounce)
	}
	if c.MinFreeBytes != 20<<30 {
		t.Errorf("MinFreeBytes = %d, want %d", c.MinFreeBytes, 20<<30)
	}
	if c.NtfyURL != "" {
		t.Errorf("NtfyURL = %q, want empty", c.NtfyURL)
	}
}

func TestGuardEnvOverrides(t *testing.T) {
	minimalEnv(t)
	t.Setenv("ARCHIVIST_QUARANTINE_WRITES", "0")
	t.Setenv("ARCHIVIST_MIN_FREE_BYTES", "12345")
	t.Setenv("ARCHIVIST_QUARANTINE_WINDOW", "90s")
	t.Setenv("ARCHIVIST_NTFY_URL", "https://ntfy.example/vault")

	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.QuarantineWrites != 0 {
		t.Errorf("QuarantineWrites = %d, want 0 (disabled)", c.QuarantineWrites)
	}
	if c.MinFreeBytes != 12345 {
		t.Errorf("MinFreeBytes = %d, want 12345", c.MinFreeBytes)
	}
	if c.QuarantineWindow != 90*time.Second {
		t.Errorf("QuarantineWindow = %v, want 90s", c.QuarantineWindow)
	}
	if c.NtfyURL != "https://ntfy.example/vault" {
		t.Errorf("NtfyURL = %q", c.NtfyURL)
	}
}

// A malformed duration must be reported, not silently replaced by a default.
func TestBadDurationIsAnError(t *testing.T) {
	minimalEnv(t)
	t.Setenv("ARCHIVIST_QUARANTINE_WINDOW", "not-a-duration")

	if _, err := Load(nil); err == nil {
		t.Fatal("Load accepted a malformed ARCHIVIST_QUARANTINE_WINDOW")
	}
}
