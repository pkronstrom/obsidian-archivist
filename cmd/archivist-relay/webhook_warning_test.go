package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Webhooks run on the relay's own background token with no caller present, so
// they are outside the gate entirely. That must be said out loud, once, to
// whoever configured them.
func TestWebhookTargetsOnAProtectedVaultWarnAtStartup(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	warnIfProtected(log, "work", []string{"work"}, []string{"https://example.invalid/hook"})
	if !strings.Contains(logged.String(), "step-up") {
		t.Errorf("no warning was logged:\n%s", logged.String())
	}
}

func TestNoWarningWithoutTargets(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	warnIfProtected(log, "work", []string{"work"}, nil)
	if logged.Len() != 0 {
		t.Errorf("warned with no webhook targets configured:\n%s", logged.String())
	}
}

// An unprotected vault has no gate for webhooks to sit outside of.
func TestNoWarningWhenTheVaultIsNotProtected(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	warnIfProtected(log, "personal", []string{"work"}, []string{"https://example.invalid/hook"})
	if logged.Len() != 0 {
		t.Errorf("warned about an unprotected vault:\n%s", logged.String())
	}
}
