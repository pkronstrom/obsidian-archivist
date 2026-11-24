package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/protocol"
)

func getAs(h http.Handler, client string, auth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/personal/v1", nil)
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if client != "" {
		req.Header.Set(protocol.HeaderClient, client)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestIndexListsFeatures(t *testing.T) {
	h, _, _ := newServer(t)
	var idx struct {
		Features []string `json:"features"`
	}
	if err := json.Unmarshal(getAs(h, "", true).Body.Bytes(), &idx); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{protocol.FeatureEncryptedVaults, protocol.FeatureEncryptedFiles} {
		found := false
		for _, f := range idx.Features {
			found = found || f == want
		}
		if !found {
			t.Errorf("index features = %v, missing %q", idx.Features, want)
		}
	}
}

// No minimum ships set: the switch exists for the release that needs it.
func TestAnyPluginVersionIsAcceptedByDefault(t *testing.T) {
	h, _, _ := newServer(t)
	if got := getAs(h, "plugin/0.0.1", true).Code; got != http.StatusOK {
		t.Fatalf("plugin/0.0.1 with no minimum = %d, want 200", got)
	}
}

func TestPluginBelowTheMinimumIsTurnedAway(t *testing.T) {
	reg, set, _ := singleVaultRegistry(t, token)
	h := New(reg, set, WithMinPlugin("0.21.0"))

	w := getAs(h, "plugin/0.20.3", true)
	if w.Code != http.StatusUpgradeRequired {
		t.Fatalf("plugin/0.20.3 = %d, want 426", w.Code)
	}
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != protocol.CodeClientTooOld {
		t.Errorf("code = %q, want %q", body.Error.Code, protocol.CodeClientTooOld)
	}
	// The message is what an old plugin shows its user, so it has to name
	// both versions.
	for _, want := range []string{"0.21.0", "0.20.3"} {
		if !strings.Contains(body.Error.Message, want) {
			t.Errorf("message %q does not mention %s", body.Error.Message, want)
		}
	}

	// Everything the minimum does not describe is let through.
	for _, client := range []string{"plugin/0.21.0", "plugin/1.0.0", "plugin/dev", "relay/v0.1.0", "curl", ""} {
		if got := getAs(h, client, true).Code; got != http.StatusOK {
			t.Errorf("client %q = %d, want 200", client, got)
		}
	}

	// A caller without a token learns nothing about versions.
	if got := getAs(h, "plugin/0.20.3", false).Code; got != http.StatusUnauthorized {
		t.Errorf("old plugin without a token = %d, want 401", got)
	}
}

func TestEachClientIsLoggedOnce(t *testing.T) {
	reg, set, _ := singleVaultRegistry(t, token)
	var buf bytes.Buffer
	h := New(reg, set, WithLog(slog.New(slog.NewTextHandler(&buf, nil))))

	for range 3 {
		getAs(h, "plugin/0.21.0", true)
	}
	getAs(h, "plugin/0.22.0", true)
	getAs(h, "", true)

	if got := strings.Count(buf.String(), "client seen"); got != 2 {
		t.Fatalf("logged %d client lines, want 2:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "client=plugin/0.21.0") || !strings.Contains(buf.String(), "token=test") {
		t.Errorf("log line names neither the client nor the token:\n%s", buf.String())
	}
}
