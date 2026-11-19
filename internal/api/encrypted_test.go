package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/vaults"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

var testKeyCheck = strings.Repeat("ab", 32)

// newEncryptedVault serves an ordinary vault "personal" and an encrypted
// vault "work" with no key check installed.
func newEncryptedVault(t *testing.T) (http.Handler, *vaults.Registry) {
	t.Helper()
	reg, set, _ := singleVaultRegistry(t, token)
	h := New(reg, set)
	w := do(t, h, "POST", "/v1/vaults", map[string]any{"name": "work", "encrypted": true}, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	return h, reg
}

func newKeyedVault(t *testing.T) (http.Handler, *vaults.Registry) {
	t.Helper()
	h, reg := newEncryptedVault(t)
	if w := do(t, h, "PUT", "/work/v1/keycheck", map[string]string{"check": testKeyCheck}, true); w.Code != http.StatusOK {
		t.Fatalf("keycheck: %d %s", w.Code, w.Body)
	}
	return h, reg
}

func sealed(s string) []byte { return []byte(protocol.Envelope + s) }

func upload(h http.Handler, vault string, content []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "/"+vault+"/v1/content/"+protocol.HashContent(content), bytes.NewReader(content))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// pushTo uploads content and pushes it to path in the encrypted vault.
func pushTo(t *testing.T, h http.Handler, base, path string, content []byte) protocol.PushResponse {
	t.Helper()
	if w := upload(h, "work", content); w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	w := do(t, h, "POST", "/work/v1/push", protocol.PushRequest{
		Base: base, Device: "test",
		Changes: []reconcile.Change{{Path: path, Op: "put", Hash: protocol.HashContent(content)}},
	}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	var pr protocol.PushResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pr); err != nil {
		t.Fatal(err)
	}
	return pr
}

func snapshotPaths(t *testing.T, h http.Handler) map[string]protocol.Entry {
	t.Helper()
	w := do(t, h, "GET", "/work/v1/snapshot", nil, true)
	var snap protocol.SnapshotResponse
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	return snap.Files
}

func wantCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status || !bytes.Contains(w.Body.Bytes(), []byte(`"`+code+`"`)) {
		t.Fatalf("want %d %s, got %d %s", status, code, w.Code, w.Body)
	}
}

func TestIndexReportsTheVaultKindAndKeyCheck(t *testing.T) {
	h, _ := newKeyedVault(t)
	type index struct {
		Encrypted bool   `json:"encrypted"`
		KeyCheck  string `json:"keyCheck"`
	}
	for vault, want := range map[string]index{"work": {true, testKeyCheck}, "personal": {false, ""}} {
		w := do(t, h, "GET", "/"+vault+"/v1", nil, true)
		var got index
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", vault, got, want)
		}
	}
}

// The refusal has to come before the blob is written. Refusing only at push
// would leave the plaintext in the object store and in every export.
func TestEncryptedVaultRefusesPlaintextBeforeStoringIt(t *testing.T) {
	h, _ := newKeyedVault(t)
	plain := []byte("the client list")

	wantCode(t, upload(h, "work", plain), http.StatusUnprocessableEntity, protocol.CodePlaintextRefused)
	get := do(t, h, "GET", "/work/v1/content/"+protocol.HashContent(plain), nil, true)
	if get.Code != http.StatusNotFound {
		t.Fatalf("the refused plaintext was stored anyway: %d", get.Code)
	}
}

func TestOrdinaryVaultStillAcceptsPlaintext(t *testing.T) {
	h, _ := newEncryptedVault(t)
	if w := upload(h, "personal", []byte("an ordinary note")); w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
}

// No ciphertext may exist under a key nobody recorded.
func TestEncryptedVaultTakesNoContentBeforeItsKeyCheck(t *testing.T) {
	h, _ := newEncryptedVault(t)
	wantCode(t, upload(h, "work", sealed("x")), http.StatusConflict, protocol.CodeNoKey)

	if w := do(t, h, "PUT", "/work/v1/keycheck", map[string]string{"check": testKeyCheck}, true); w.Code != http.StatusOK {
		t.Fatalf("keycheck: %d %s", w.Code, w.Body)
	}
	if w := upload(h, "work", sealed("x")); w.Code != http.StatusOK {
		t.Fatalf("upload after the key check: %d %s", w.Code, w.Body)
	}
}

// Two devices pairing at once must not both install a key.
func TestKeyCheckCanBeInstalledOnlyOnce(t *testing.T) {
	h, _ := newKeyedVault(t)
	other := strings.Repeat("cd", 32)
	w := do(t, h, "PUT", "/work/v1/keycheck", map[string]string{"check": other}, true)
	wantCode(t, w, http.StatusConflict, protocol.CodeKeyCheckSet)
}

func TestKeyCheckRefusesMalformedValuesAndOrdinaryVaults(t *testing.T) {
	h, _ := newEncryptedVault(t)
	w := do(t, h, "PUT", "/work/v1/keycheck", map[string]string{"check": "short"}, true)
	wantCode(t, w, http.StatusBadRequest, protocol.CodeMalformed)
	w = do(t, h, "PUT", "/personal/v1/keycheck", map[string]string{"check": testKeyCheck}, true)
	wantCode(t, w, http.StatusUnprocessableEntity, protocol.CodeMalformed)
}

// The server's own path rules match plaintext names, so the allowlist is the
// only thing between a client that forgot to encrypt a name and history.
func TestEncryptedVaultRefusesAReadableName(t *testing.T) {
	h, _ := newKeyedVault(t)
	pr := pushTo(t, h, "", "Clients/Acme.md", sealed("x"))
	if len(pr.Results) != 1 || pr.Results[0].Status != protocol.StatusRefused {
		t.Fatalf("want refused, got %+v", pr.Results)
	}
	if files := snapshotPaths(t, h); len(files) != 0 {
		t.Fatalf("a readable name reached the vault: %v", files)
	}

	pr = pushTo(t, h, "", "mnwgszlooq/mfrw2zi", sealed("x"))
	if pr.Results[0].Status != protocol.StatusApplied {
		t.Fatalf("an encrypted name must apply, got %+v", pr.Results)
	}
}

// The server cannot merge ciphertext or name a copy the client could decrypt,
// so it keeps its own version, writes nothing, and reports what it kept.
func TestEncryptedConflictWritesNothingAndReportsWhatWasKept(t *testing.T) {
	h, _ := newKeyedVault(t)
	first := pushTo(t, h, "", "mfrgg", sealed("from the laptop"))
	second := pushTo(t, h, first.Head, "mfrgg", sealed("laptop again"))

	// A device still on the first head now pushes its own edit.
	pr := pushTo(t, h, first.Head, "mfrgg", sealed("from the phone"))
	res := pr.Results[0]
	if res.Status != protocol.StatusConflict {
		t.Fatalf("want conflict, got %+v", res)
	}
	if res.ConflictPath != "" || res.ConflictHash != "" {
		t.Errorf("the server must not make a conflict copy: %+v", res)
	}
	if want := protocol.HashContent(sealed("laptop again")); res.Hash != want {
		t.Errorf("hash = %s, want what the server kept, %s", res.Hash, want)
	}
	if pr.Head != second.Head {
		t.Errorf("a conflict must not move the head: %s -> %s", second.Head, pr.Head)
	}
	if files := snapshotPaths(t, h); len(files) != 1 {
		t.Errorf("want one file, got %v", files)
	}
}

func TestEncryptedVaultClosesRoutesThatHandlePlaintext(t *testing.T) {
	h, _ := newKeyedVault(t)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/work/v1/note/append", map[string]string{"path": "a.md", "content": "x"}},
		{"POST", "/work/v1/note/edit", map[string]string{"path": "a.md"}},
		{"POST", "/work/v1/pin", map[string]string{"name": "before the audit"}},
		{"GET", "/work/v1/pins", nil},
	} {
		w := do(t, h, c.method, c.path, c.body, true)
		if w.Code != http.StatusUnprocessableEntity || !bytes.Contains(w.Body.Bytes(), []byte(protocol.CodeEncryptedVault)) {
			t.Errorf("%s %s: want 422 %s, got %d %s", c.method, c.path, protocol.CodeEncryptedVault, w.Code, w.Body)
		}
	}
}

// A file dropped into the directory by hand goes through the same commit the
// watcher uses. Plaintext must not enter history that way either.
func TestEncryptedVaultDoesNotCommitPlaintextFromDisk(t *testing.T) {
	h, reg := newKeyedVault(t)
	inst, err := reg.Get("work")
	if err != nil {
		t.Fatal(err)
	}
	dir := inst.Vault.Dir()
	for name, content := range map[string][]byte{
		"Notes.md": []byte("dropped in by hand"),
		"mfrgg":    []byte("an encrypted name, plaintext inside"),
		"mnwgs":    sealed("a real one"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := inst.Repo.Commit("scan"); err != nil {
		t.Fatal(err)
	}
	files := snapshotPaths(t, h)
	if _, ok := files["mnwgs"]; !ok || len(files) != 1 {
		t.Fatalf("want only the encrypted file committed, got %v", files)
	}
}
