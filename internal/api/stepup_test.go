package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/auth"
	"github.com/pkronstrom/obsidian-archivist/internal/stepup"
	"github.com/pkronstrom/obsidian-archivist/internal/vaults"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

const rfcSecretForAPITest = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

type stepUpClock struct{ now time.Time }

func (c *stepUpClock) Now() time.Time { return c.now }

// stepUpFixture is a server over three vaults: personal unprotected, work and
// private both carrying a marker.
type stepUpFixture struct {
	handler http.Handler
	root    string
	clock   *stepUpClock
	grants  *stepup.Grants

	gated         string // vault:work, vault:private -- must unlock both
	gatedReadOnly string // same posture, read scope only
	opsOnly       string // ops:work, exempt from private -- decided, not gated
	exempt        string // recorded exemption for both, no secret
	undecided     string // nothing recorded: predates the marker
}

func newStepUpFixture(t *testing.T) *stepUpFixture {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"personal", "work", "private"} {
		if err := os.MkdirAll(filepath.Join(root, "vaults", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"work", "private"} {
		dir := filepath.Join(root, ".archivist", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, vaults.StepUpMarker), nil, 0o644); err != nil {
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

	f := &stepUpFixture{
		root:          root,
		clock:         &stepUpClock{now: time.Unix(1111111109, 0)},
		gated:         "tok-gated",
		gatedReadOnly: "tok-gated-ro",
		opsOnly:       "tok-ops",
		exempt:        "tok-exempt",
		undecided:     "tok-undecided",
	}
	rw := []string{auth.ScopeRead, auth.ScopeWrite}
	set := auth.NewSetForTest(map[string]auth.Principal{
		f.gated: {
			Label: "gated", Vaults: []string{"*"}, Scopes: rw,
			TotpSecret:         rfcSecretForAPITest,
			RequiresStepUpAuth: []string{"vault:work", "vault:private"},
		},
		f.gatedReadOnly: {
			Label: "gated-ro", Vaults: []string{"*"}, Scopes: []string{auth.ScopeRead},
			TotpSecret:         rfcSecretForAPITest,
			RequiresStepUpAuth: []string{"vault:work", "vault:private"},
		},
		f.opsOnly: {
			Label: "ops", Vaults: []string{"*"}, Scopes: rw,
			TotpSecret:         rfcSecretForAPITest,
			RequiresStepUpAuth: []string{"ops:work"},
			StepUpExempt:       []string{"private"},
		},
		f.exempt: {
			Label: "exempt", Vaults: []string{"*"}, Scopes: rw,
			StepUpExempt: []string{"work", "private"},
		},
		f.undecided: {
			Label: "undecided", Vaults: []string{"*"}, Scopes: rw,
		},
	})

	f.grants = stepup.NewGrants(time.Minute, f.clock.Now)
	t.Cleanup(f.grants.Close)
	// The verifier refuses every step up to the one it was built in, so move
	// the clock on before any code is spent.
	verifier := stepup.NewVerifier(f.clock.Now)
	f.clock.now = f.clock.now.Add(stepup.Step)

	f.handler = New(reg, set, WithStepUp(f.grants, verifier))
	return f
}

func (f *stepUpFixture) as(t *testing.T, method, path string, body any, tok string) *httptest.ResponseRecorder {
	t.Helper()
	return doAs(t, f.handler, method, path, body, tok)
}

func (f *stepUpFixture) code(t *testing.T) string {
	t.Helper()
	c, err := stepup.Code(rfcSecretForAPITest, f.clock.now)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *stepUpFixture) unlock(t *testing.T, vault, tok string) *httptest.ResponseRecorder {
	t.Helper()
	res := f.as(t, "POST", "/"+vault+"/v1/unlock", map[string]any{"code": f.code(t)}, tok)
	if res.Code != http.StatusOK {
		t.Fatalf("unlock %s = %d: %s", vault, res.Code, res.Body.String())
	}
	// Every code is single-use, so move to the next window before the next one.
	f.clock.now = f.clock.now.Add(stepup.Step)
	return res
}

// expireGrants moves past the TTL and forces the expiry through, closing any
// watcher a stream is holding.
func (f *stepUpFixture) expireGrants() {
	f.clock.now = f.clock.now.Add(2 * time.Minute)
	f.grants.Held("", "")
	f.grants.Close()
}

func (f *stepUpFixture) breakMarkerLookup(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	dir := filepath.Join(f.root, ".archivist")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
}

func errorCodeOf(t *testing.T, res *httptest.ResponseRecorder) string {
	t.Helper()
	var body protocol.ErrorResponse
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the error body %q: %v", res.Body.String(), err)
	}
	return body.Error.Code
}

func TestAProtectedVaultRefusesReadsUntilUnlocked(t *testing.T) {
	s := newStepUpFixture(t)
	res := s.as(t, "GET", "/work/v1/head", nil, s.gated)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if code := errorCodeOf(t, res); code != protocol.CodeStepUpRequired {
		t.Errorf("code = %q, want %q", code, protocol.CodeStepUpRequired)
	}
}

// Absence denies. A token minted before the marker existed has no recorded
// decision, and must be refused rather than admitted.
func TestATokenThatDecidedNothingIsDenied(t *testing.T) {
	s := newStepUpFixture(t)
	res := s.as(t, "GET", "/work/v1/head", nil, s.undecided)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: this token predates the marker", res.Code)
	}
	if code := errorCodeOf(t, res); code != protocol.CodeStepUpRequired {
		t.Errorf("code = %q, want %q", code, protocol.CodeStepUpRequired)
	}
}

func TestARecordedExemptionReachesAProtectedVault(t *testing.T) {
	s := newStepUpFixture(t)
	if res := s.as(t, "GET", "/work/v1/head", nil, s.exempt); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; this token recorded an exemption", res.Code)
	}
}

func TestAnUnprotectedVaultIsNeverGated(t *testing.T) {
	s := newStepUpFixture(t)
	if res := s.as(t, "GET", "/personal/v1/head", nil, s.gated); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; personal carries no marker", res.Code)
	}
}

func TestAnOpsPostureDoesNotGateAccess(t *testing.T) {
	s := newStepUpFixture(t)
	if res := s.as(t, "GET", "/work/v1/head", nil, s.opsOnly); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; ops: gates destructive operations, not reads", res.Code)
	}
}

// A step-up prompt for a call that would be refused anyway leaks that the vault
// exists and wastes a single-use code.
func TestScopeIsCheckedBeforeStepUp(t *testing.T) {
	s := newStepUpFixture(t)
	res := s.as(t, "POST", "/work/v1/push", map[string]any{}, s.gatedReadOnly)
	if code := errorCodeOf(t, res); code != protocol.CodeForbidden {
		t.Errorf("code = %q, want %q: refuse for the missing scope, not for a code",
			code, protocol.CodeForbidden)
	}
}

// Cannot tell is not no.
func TestAnUndeterminableMarkerIsARefusal(t *testing.T) {
	s := newStepUpFixture(t)
	s.breakMarkerLookup(t)
	if res := s.as(t, "GET", "/work/v1/head", nil, s.exempt); res.Code == http.StatusOK {
		t.Fatal("a vault whose protection could not be determined was served anyway")
	}
}

func TestOnlyTheUnlockRouteIsExempt(t *testing.T) {
	s := &Server{}
	var exempt []string
	for _, rt := range s.routes() {
		if rt.SkipStepUp {
			exempt = append(exempt, rt.Method+" "+rt.Path)
		}
	}
	if len(exempt) != 1 || exempt[0] != "POST /v1/unlock" {
		t.Fatalf("routes exempt from step-up = %v, want exactly [POST /v1/unlock]", exempt)
	}
}
