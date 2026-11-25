package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/vaults"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// These cover paths marked .encrypted in the ORDINARY vault "personal". The
// encrypted vault "work" has its own tests in encrypted_test.go.

// newMarkedVault serves "personal" with a key check installed, which is the
// state after the first device has created the vault's key.
func newMarkedVault(t *testing.T) (http.Handler, *vaults.Registry) {
	t.Helper()
	reg, set, _ := singleVaultRegistry(t, token)
	h := New(reg, set)
	if w := do(t, h, "PUT", "/personal/v1/keycheck", map[string]string{"check": testKeyCheck}, true); w.Code != http.StatusOK {
		t.Fatalf("keycheck: %d %s", w.Code, w.Body)
	}
	return h, reg
}

func put(path string, content []byte) reconcile.Change {
	return reconcile.Change{Path: path, Op: "put", Hash: protocol.HashContent(content)}
}

// pushPersonal uploads every put's content and pushes the changes.
func pushPersonal(t *testing.T, h http.Handler, base string, contents [][]byte, changes ...reconcile.Change) protocol.PushResponse {
	t.Helper()
	for _, c := range contents {
		if w := upload(h, "personal", c); w.Code != http.StatusOK {
			t.Fatalf("upload: %d %s", w.Code, w.Body)
		}
	}
	w := do(t, h, "POST", "/personal/v1/push", protocol.PushRequest{Base: base, Device: "test", Changes: changes}, true)
	if w.Code != http.StatusOK {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	var pr protocol.PushResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pr); err != nil {
		t.Fatal(err)
	}
	return pr
}

func personalFiles(t *testing.T, h http.Handler) map[string]protocol.Entry {
	t.Helper()
	w := do(t, h, "GET", "/personal/v1/snapshot", nil, true)
	var snap protocol.SnapshotResponse
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	return snap.Files
}

func TestOrdinaryVaultTakesAKeyCheckOnce(t *testing.T) {
	h, _ := newMarkedVault(t)
	w := do(t, h, "GET", "/personal/v1", nil, true)
	if !bytes.Contains(w.Body.Bytes(), []byte(testKeyCheck)) || !bytes.Contains(w.Body.Bytes(), []byte(`"encrypted":false`)) {
		t.Fatalf("the index must report an ordinary vault with a key check: %s", w.Body)
	}
	w = do(t, h, "PUT", "/personal/v1/keycheck", map[string]string{"check": testKeyCheck}, true)
	wantCode(t, w, http.StatusConflict, protocol.CodeKeyCheckSet)
}

// The refusal is per change: one plaintext file at a marked path must not
// stop the rest of the push.
func TestMarkedPathRefusesPlaintextAndTheRestOfThePushLands(t *testing.T) {
	h, _ := newMarkedVault(t)
	plain, secret := []byte("an ordinary note"), []byte("the salary, readable")
	pr := pushPersonal(t, h, "", [][]byte{plain, secret},
		put("Note.md", plain),
		put("Salary.encrypted.md", secret),
		put("Clients.encrypted/mfrgg", secret),
	)
	if pr.Results[0].Status != protocol.StatusApplied {
		t.Errorf("the ordinary note: %+v", pr.Results[0])
	}
	for _, r := range pr.Results[1:] {
		if r.Status != protocol.StatusRefused || r.Reason != "content is not encrypted" {
			t.Errorf("want refused as not encrypted, got %+v", r)
		}
	}
	if files := personalFiles(t, h); len(files) != 1 {
		t.Errorf("want only the ordinary note stored, got %v", files)
	}
}

func TestMarkedPathAcceptsCiphertext(t *testing.T) {
	h, _ := newMarkedVault(t)
	a, b := sealed("salary"), sealed("contract")
	pr := pushPersonal(t, h, "", [][]byte{a, b},
		put("Salary.encrypted.md", a),
		put("Work/Clients.encrypted/mfrgg/mnwgs", b),
	)
	for _, r := range pr.Results {
		if r.Status != protocol.StatusApplied {
			t.Errorf("want applied, got %+v", r)
		}
	}
}

// Beneath a marked folder the client encrypts names. A readable one there
// means a client that sealed the content and forgot the name.
func TestMarkedFolderRefusesAReadableNameBeneathIt(t *testing.T) {
	h, _ := newMarkedVault(t)
	c := sealed("contract")
	pr := pushPersonal(t, h, "", [][]byte{c}, put("Clients.encrypted/Acme/Contract.md", c))
	if r := pr.Results[0]; r.Status != protocol.StatusRefused || r.Reason != "not an encrypted name" {
		t.Fatalf("want refused as not an encrypted name, got %+v", r)
	}
}

// No ciphertext may exist under a key nobody recorded.
func TestMarkedPathIsRefusedUntilTheVaultHasAKeyCheck(t *testing.T) {
	reg, set, _ := singleVaultRegistry(t, token)
	h := New(reg, set)
	c := sealed("salary")
	pr := pushPersonal(t, h, "", [][]byte{c}, put("Salary.encrypted.md", c))
	if r := pr.Results[0]; r.Status != protocol.StatusRefused || r.Reason != "the vault has no key yet" {
		t.Fatalf("want refused for want of a key, got %+v", r)
	}
}

func TestMarkedPathConflictWritesNothingAndReportsWhatWasKept(t *testing.T) {
	h, _ := newMarkedVault(t)
	v1, v2, v3 := sealed("from the laptop"), sealed("laptop again"), sealed("from the phone")
	first := pushPersonal(t, h, "", [][]byte{v1}, put("Salary.encrypted.md", v1))
	second := pushPersonal(t, h, first.Head, [][]byte{v2}, put("Salary.encrypted.md", v2))

	pr := pushPersonal(t, h, first.Head, [][]byte{v3}, put("Salary.encrypted.md", v3))
	res := pr.Results[0]
	if res.Status != protocol.StatusConflict || res.ConflictPath != "" {
		t.Fatalf("want a conflict with no copy made, got %+v", res)
	}
	if res.Hash != protocol.HashContent(v2) {
		t.Errorf("hash = %s, want what the server kept", res.Hash)
	}
	if pr.Head != second.Head {
		t.Errorf("a conflict must not move the head")
	}
	if files := personalFiles(t, h); len(files) != 1 {
		t.Errorf("want one file, got %v", files)
	}
}

// A move carries bytes unchanged, so across the boundary it would put
// plaintext at a marked path or ciphertext at a readable one.
func TestMoveCannotCrossTheMarker(t *testing.T) {
	h, _ := newMarkedVault(t)
	plain, secret := []byte("an ordinary note"), sealed("salary")
	first := pushPersonal(t, h, "", [][]byte{plain, secret},
		put("Note.md", plain),
		put("Salary.encrypted.md", secret),
		put("Clients.encrypted/mfrgg", secret),
	)
	move := func(from, to string) protocol.Result {
		t.Helper()
		pr := pushPersonal(t, h, first.Head, nil, reconcile.Change{Path: to, Op: "move", From: from})
		return pr.Results[0]
	}
	const crossing = "a move cannot cross into or out of an encrypted path"
	if r := move("Note.md", "Note.encrypted.md"); r.Status != protocol.StatusRefused || r.Reason != crossing {
		t.Errorf("plain to marked: %+v", r)
	}
	if r := move("Salary.encrypted.md", "Salary.md"); r.Status != protocol.StatusRefused || r.Reason != crossing {
		t.Errorf("marked to plain: %+v", r)
	}
	if r := move("Clients.encrypted/mfrgg", "Customers.encrypted/mfrgg"); r.Status != protocol.StatusApplied {
		t.Errorf("marked to marked: %+v", r)
	}
	files := personalFiles(t, h)
	if _, ok := files["Customers.encrypted/mfrgg"]; !ok || len(files) != 3 {
		t.Errorf("want the three files with one renamed, got %v", files)
	}
}

func TestServerSideWritesRefuseAMarkedPath(t *testing.T) {
	h, _ := newMarkedVault(t)
	c := sealed("salary")
	first := pushPersonal(t, h, "", [][]byte{c}, put("Salary.encrypted.md", c))
	for _, tc := range []struct {
		path string
		body any
	}{
		{"/personal/v1/note/append", map[string]string{"path": "Salary.encrypted.md", "content": "more"}},
		{"/personal/v1/note/edit", map[string]string{"path": "Salary.encrypted.md", "old_text": "a", "new_text": "b"}},
		{"/personal/v1/pin", map[string]string{"name": "before the raise", "path": "Salary.encrypted.md", "expectedHead": first.Head}},
	} {
		w := do(t, h, "POST", tc.path, tc.body, true)
		wantCode(t, w, http.StatusUnprocessableEntity, protocol.CodeEncryptedPath)
	}
}

// A file dropped into the directory by hand goes through the commit the
// watcher uses, and must not reach history readable.
func TestMarkedPathDoesNotCommitPlaintextFromDisk(t *testing.T) {
	h, reg := newMarkedVault(t)
	inst, err := reg.Get("personal")
	if err != nil {
		t.Fatal(err)
	}
	dir := inst.Vault.Dir()
	if err := os.MkdirAll(filepath.Join(dir, "Clients.encrypted"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"Plain.md":                  []byte("an ordinary note"),
		"Salary.encrypted.md":       []byte("dropped in by hand"),
		"Clients.encrypted/Acme.md": sealed("sealed, under a readable name"),
		"Clients.encrypted/mfrgg":   []byte("an encrypted name, plaintext inside"),
		"Clients.encrypted/mnwgs":   sealed("a real one"),
		"Bonus.encrypted.md":        sealed("a real one"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := inst.Repo.Commit("scan"); err != nil {
		t.Fatal(err)
	}
	files := personalFiles(t, h)
	for _, want := range []string{"Plain.md", "Clients.encrypted/mnwgs", "Bonus.encrypted.md"} {
		if _, ok := files[want]; !ok {
			t.Errorf("%s should have been committed", want)
		}
	}
	if len(files) != 3 {
		t.Errorf("want exactly the three acceptable files, got %v", files)
	}
}
