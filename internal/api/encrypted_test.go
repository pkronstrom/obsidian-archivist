package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/protocol"
)

func newEncryptedVault(t *testing.T) http.Handler {
	t.Helper()
	reg, set, _ := singleVaultRegistry(t, token)
	h := New(reg, set)
	w := do(t, h, "POST", "/v1/vaults", map[string]any{"name": "work", "encrypted": true}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	return h
}

func upload(h http.Handler, vault string, content []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "/"+vault+"/v1/content/"+protocol.HashContent(content), bytes.NewReader(content))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestIndexReportsTheVaultKind(t *testing.T) {
	h := newEncryptedVault(t)
	for vault, want := range map[string]bool{"work": true, "personal": false} {
		w := do(t, h, "GET", "/"+vault+"/v1", nil, true)
		var got struct {
			Encrypted bool `json:"encrypted"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Encrypted != want {
			t.Errorf("%s: encrypted=%v, want %v", vault, got.Encrypted, want)
		}
	}
}

// The refusal has to come before the blob is written. Refusing only at push
// would leave the plaintext in the object store and in every export.
func TestEncryptedVaultRefusesPlaintextBeforeStoringIt(t *testing.T) {
	h := newEncryptedVault(t)
	plain := []byte("the client list")

	w := upload(h, "work", plain)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d %s", w.Code, w.Body)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(protocol.CodePlaintextRefused)) {
		t.Errorf("want code %s in %s", protocol.CodePlaintextRefused, w.Body)
	}
	get := do(t, h, "GET", "/work/v1/content/"+protocol.HashContent(plain), nil, true)
	if get.Code != http.StatusNotFound {
		t.Fatalf("the refused plaintext was stored anyway: %d", get.Code)
	}
}

func TestEncryptedVaultAcceptsEnvelopedContent(t *testing.T) {
	h := newEncryptedVault(t)
	if w := upload(h, "work", []byte(protocol.Envelope+"opaque")); w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
}

func TestOrdinaryVaultStillAcceptsPlaintext(t *testing.T) {
	h := newEncryptedVault(t)
	if w := upload(h, "personal", []byte("an ordinary note")); w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
}
