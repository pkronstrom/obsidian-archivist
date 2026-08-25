package main

import "testing"

func cleanRelayEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ARCHIVIST_URL", "http://server:8090")
	t.Setenv("ARCHIVIST_TOKEN", "")
	t.Setenv("ARCHIVIST_VAULT", "")
	t.Setenv("ARCHIVIST_WEBHOOKS", "")
}

func TestBackgroundTokenIsOptionalWithoutWebhooks(t *testing.T) {
	cleanRelayEnv(t)
	cfg, err := load(nil)
	if err != nil || cfg.token != "" {
		t.Fatalf("load without webhooks = (%#v, %v), want tokenless relay", cfg, err)
	}
}

func TestWebhooksRequireBackgroundTokenAndVault(t *testing.T) {
	for _, tc := range []struct{ token, vault string }{{"", "personal"}, {"arch_x", ""}} {
		t.Run("token="+tc.token+"/vault="+tc.vault, func(t *testing.T) {
			cleanRelayEnv(t)
			t.Setenv("ARCHIVIST_TOKEN", tc.token)
			t.Setenv("ARCHIVIST_VAULT", tc.vault)
			t.Setenv("ARCHIVIST_WEBHOOKS", "https://example.invalid/hook")
			if _, err := load(nil); err == nil {
				t.Fatal("webhooks must refuse incomplete background authentication")
			}
		})
	}
}
