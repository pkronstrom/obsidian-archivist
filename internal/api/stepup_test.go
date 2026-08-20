package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/auth"
	"github.com/pkronstrom/obsidian-archivist/internal/stepup"
	"github.com/pkronstrom/obsidian-archivist/internal/vaults"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

const rfcSecretForAPITest = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

// stepUpClock is read by request handlers on other goroutines while the test
// advances it, so it needs a lock. Without one the stream tests race.
type stepUpClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepUpClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepUpClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *stepUpClock) at() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

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
	gatedNoSecret string // gated on access but carrying no secret to unlock with
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
		gatedNoSecret: "tok-gated-nosecret",
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
		// Mint and Load both refuse this combination, so it can only arrive by
		// somebody hand-editing the file -- which is exactly why the handler
		// must still cope rather than trusting the invariant.
		f.gatedNoSecret: {
			Label: "gated-nosecret", Vaults: []string{"*"}, Scopes: rw,
			RequiresStepUpAuth: []string{"vault:work"},
		},
	})

	f.grants = stepup.NewGrants(time.Minute, f.clock.Now)
	t.Cleanup(f.grants.Close)
	// The verifier refuses every step up to its construction step plus the skew,
	// so that a restart cannot resurrect a spent code. Move past that window
	// before any code is spent here.
	verifier := stepup.NewVerifier(f.clock.Now)
	f.clock.advance((stepup.SkewSteps + 1) * stepup.Step)

	f.handler = New(reg, set, WithStepUp(f.grants, verifier))
	return f
}

func (f *stepUpFixture) as(t *testing.T, method, path string, body any, tok string) *httptest.ResponseRecorder {
	t.Helper()
	return doAs(t, f.handler, method, path, body, tok)
}

func (f *stepUpFixture) code(t *testing.T) string {
	t.Helper()
	c, err := stepup.Code(rfcSecretForAPITest, f.clock.at())
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
	f.clock.advance(stepup.Step)
	return res
}

// expireGrant advances past the TTL and forces THAT grant's expiry through.
//
// Deliberately not Close(): closing the whole table would release every watcher
// and the stream tests would prove only that shutdown ends a stream, not that an
// absolute TTL does.
func (f *stepUpFixture) expireGrant(tok, vault string) {
	f.clock.advance(2 * time.Minute)
	if f.grants.Held(auth.HashToken(tok), vault) {
		panic("the grant survived its TTL; the tests below would prove nothing")
	}
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

func TestUnlockOpensTheGate(t *testing.T) {
	s := newStepUpFixture(t)
	s.unlock(t, "work", s.gated)
	if res := s.as(t, "GET", "/work/v1/head", nil, s.gated); res.Code != http.StatusOK {
		t.Fatalf("status = %d after unlocking, want 200", res.Code)
	}
}

func TestAGrantDoesNotCoverASecondVault(t *testing.T) {
	s := newStepUpFixture(t)
	s.unlock(t, "work", s.gated)
	if res := s.as(t, "GET", "/private/v1/head", nil, s.gated); res.Code != http.StatusForbidden {
		t.Fatal("unlocking work also opened private")
	}
}

func TestUnlockRejectsAWrongCode(t *testing.T) {
	s := newStepUpFixture(t)
	res := s.as(t, "POST", "/work/v1/unlock", map[string]any{"code": "000000"}, s.gated)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if res := s.as(t, "GET", "/work/v1/head", nil, s.gated); res.Code != http.StatusForbidden {
		t.Fatal("a failed unlock still opened the gate")
	}
}

// Unlocking a vault this token has no access posture toward would spend a code
// on a grant nothing consults.
func TestUnlockIsRefusedWithoutAnAccessPosture(t *testing.T) {
	s := newStepUpFixture(t)
	res := s.as(t, "POST", "/work/v1/unlock", map[string]any{"code": s.code(t)}, s.opsOnly)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: this token gates ops, not access", res.Code)
	}
}

func TestUnlockIsRefusedOnAnUnprotectedVault(t *testing.T) {
	s := newStepUpFixture(t)
	res := s.as(t, "POST", "/personal/v1/unlock", map[string]any{"code": s.code(t)}, s.gated)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: personal carries no marker", res.Code)
	}
}

// A gated token with no secret reaches the secret check rather than exiting at
// the "nothing to unlock" branch above it. Using an exempt token here would
// return 403 for the wrong reason and never exercise this at all.
func TestUnlockIsRefusedWithNoSecret(t *testing.T) {
	s := newStepUpFixture(t)
	res := s.as(t, "POST", "/work/v1/unlock", map[string]any{"code": "000000"}, s.gatedNoSecret)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if body := res.Body.String(); !strings.Contains(body, "no step-up secret") {
		t.Errorf("refused for the wrong reason: %s", body)
	}
}

func TestUnlockReportsWhenTheGrantEnds(t *testing.T) {
	s := newStepUpFixture(t)
	before := s.clock.now.Unix()
	res := s.unlock(t, "work", s.gated)
	var got struct {
		Vault     string `json:"vault"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Vault != "work" || got.ExpiresAt <= before {
		t.Errorf("got %+v, want vault=work and a future expiresAt", got)
	}
}

func TestUnlockCooldownSetsRetryAfter(t *testing.T) {
	s := newStepUpFixture(t)
	for i := 0; i < 3; i++ {
		s.as(t, "POST", "/work/v1/unlock", map[string]any{"code": "000000"}, s.gated)
	}
	res := s.as(t, "POST", "/work/v1/unlock", map[string]any{"code": "000000"}, s.gated)
	if res.Header().Get("Retry-After") == "" {
		t.Error("a cooling-down refusal did not say when to try again")
	}
}

// Both stream tests neutralise the keepalive. The events handler re-checks the
// token on every keepalive tick and returns if it was revoked, so with the
// default 25s interval a stream ends on its own and the test passes whether or
// not the lapse arm works at all. An hour makes the lapse arm the only way out.
//
// They also assert 200 first: a test that never establishes the stream, because
// the middleware refused it, proves nothing about what happens when consent
// ends mid-stream.
func TestAnEventStreamStopsWhenItsGrantLapses(t *testing.T) {
	defer func(d time.Duration) { streamKeepalive = d }(streamKeepalive)
	streamKeepalive = time.Hour

	s := newStepUpFixture(t)
	s.unlock(t, "work", s.gated)

	srv := httptest.NewServer(s.handler)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/work/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+s.gated)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the stream never opened", res.StatusCode)
	}

	drained := make(chan struct{})
	go func() { io.ReadAll(res.Body); close(drained) }()

	// The handler has to reach its select before consent is withdrawn, or the
	// close races the registration.
	time.Sleep(150 * time.Millisecond)
	s.expireGrant(s.gated, "work")

	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream outlived its grant")
	}
}

func TestABlockedWaitStopsWhenItsGrantLapses(t *testing.T) {
	s := newStepUpFixture(t)
	s.unlock(t, "work", s.gated)

	srv := httptest.NewServer(s.handler)
	defer srv.Close()

	status := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("GET", srv.URL+"/work/v1/wait?timeout=60", nil)
		req.Header.Set("Authorization", "Bearer "+s.gated)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			status <- 0
			return
		}
		res.Body.Close()
		status <- res.StatusCode
	}()

	time.Sleep(150 * time.Millisecond) // let the long poll block
	s.expireGrant(s.gated, "work")

	select {
	case got := <-status:
		if got != http.StatusOK {
			t.Fatalf("status = %d, want 200: this poll was refused at admission "+
				"rather than ended by the lapse", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a blocked long poll outlived its grant")
	}
}

// Two different facts, and the plugin needs both: which vaults the SERVER
// protects, and what THIS token decided about them. One field cannot answer
// both, and conflating them makes the plugin warn about vaults it is not gated
// on.
func TestVaultListReportsPolicyAndPostureSeparately(t *testing.T) {
	s := newStepUpFixture(t)
	res := s.as(t, "GET", "/v1/vaults", nil, s.opsOnly)

	var got struct {
		ProtectedVaults    []string `json:"protectedVaults"`
		RequiresStepUpAuth []string `json:"requiresStepUpAuth"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.ProtectedVaults) != 2 {
		t.Errorf("protectedVaults = %v, want work and private", got.ProtectedVaults)
	}
	if len(got.RequiresStepUpAuth) != 1 || got.RequiresStepUpAuth[0] != "ops:work" {
		t.Errorf("requiresStepUpAuth = %v, want [ops:work]", got.RequiresStepUpAuth)
	}
}

// protect marks a vault after the fixture is already running, so a stream can be
// opened before protection begins.
func (f *stepUpFixture) protect(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join(f.root, ".archivist", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, vaults.StepUpMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Protection begins when the marker appears, including for streams already
// running. One admitted while the vault was open has no lapse channel, so
// without a periodic re-check it would keep publishing changed paths forever.
func TestAStreamOpenedBeforeProtectionStopsWhenTheMarkerAppears(t *testing.T) {
	defer func(d time.Duration) { streamKeepalive = d }(streamKeepalive)
	streamKeepalive = 50 * time.Millisecond

	s := newStepUpFixture(t)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()

	// personal carries no marker yet, so this is admitted ungated.
	req, _ := http.NewRequest("GET", srv.URL+"/personal/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+s.gated)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the stream never opened", res.StatusCode)
	}

	drained := make(chan struct{})
	go func() { io.ReadAll(res.Body); close(drained) }()

	s.protect(t, "personal")

	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream kept running after its vault became protected")
	}
}
