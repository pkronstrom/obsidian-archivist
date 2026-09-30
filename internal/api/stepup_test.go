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
	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
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

// stepUpFixture is a server over three vaults. Step-up is a property of the
// token alone, so the same vault is gated for one token and open for another.
type stepUpFixture struct {
	handler http.Handler
	set     *auth.Set
	clock   *stepUpClock
	grants  *stepup.Grants

	gated         string // stepUp work, private -- must unlock both
	gatedReadOnly string // same step-up, read scope only
	plain         string // opens every vault, no step-up
	gatedNoSecret string // gated on work but carrying no secret to unlock with
	writer        string // ungated, writes commits for the wait tests
}

// principals is the fixture's token table, keyed by plaintext token.
func (f *stepUpFixture) principals() map[string]auth.Principal {
	rw := []string{auth.ScopeRead, auth.ScopeWrite}
	return map[string]auth.Principal{
		f.gated: {
			Label: "gated", Vaults: []string{"*"}, Scopes: rw,
			TotpSecret: rfcSecretForAPITest,
			StepUp:     []string{"work", "private"},
		},
		f.gatedReadOnly: {
			Label: "gated-ro", Vaults: []string{"*"}, Scopes: []string{auth.ScopeRead},
			TotpSecret: rfcSecretForAPITest,
			StepUp:     []string{"work", "private"},
		},
		f.plain: {
			Label: "plain", Vaults: []string{"*"}, Scopes: rw,
		},
		f.writer: {
			Label: "writer", Vaults: []string{"*"}, Scopes: rw,
		},
		// Mint and Load both refuse this combination, so it can only arrive by
		// somebody hand-editing the file -- which is exactly why the handler
		// must still cope rather than trusting the invariant.
		f.gatedNoSecret: {
			Label: "gated-nosecret", Vaults: []string{"*"}, Scopes: rw,
			StepUp: []string{"work"},
		},
	}
}

func newStepUpFixture(t *testing.T) *stepUpFixture {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"personal", "work", "private"} {
		if err := os.MkdirAll(filepath.Join(root, "vaults", name), 0o755); err != nil {
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
		clock:         &stepUpClock{now: time.Unix(1111111109, 0)},
		gated:         "tok-gated",
		gatedReadOnly: "tok-gated-ro",
		plain:         "tok-plain",
		gatedNoSecret: "tok-gated-nosecret",
		writer:        "tok-writer",
	}
	f.set = auth.NewSetForTest(f.principals())
	set := f.set

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
	if f.grants.Held(grantKey(tok, f.principals()[tok]), vault) {
		panic("the grant survived its TTL; the tests below would prove nothing")
	}
}

func errorCodeOf(t *testing.T, res *httptest.ResponseRecorder) string {
	t.Helper()
	var body protocol.ErrorResponse
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the error body %q: %v", res.Body.String(), err)
	}
	return body.Error.Code
}

func TestAGatedVaultRefusesReadsUntilUnlocked(t *testing.T) {
	s := newStepUpFixture(t)
	res := s.as(t, "GET", "/work/v1/head", nil, s.gated)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if code := errorCodeOf(t, res); code != protocol.CodeStepUpRequired {
		t.Errorf("code = %q, want %q", code, protocol.CodeStepUpRequired)
	}
}

// The point of making step-up a token property: gating an agent on work must
// not lock out the phone that syncs work.
func TestAnotherTokenOnTheSameVaultIsNotGated(t *testing.T) {
	s := newStepUpFixture(t)
	if res := s.as(t, "GET", "/work/v1/head", nil, s.plain); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; this token has no step-up", res.Code)
	}
}

func TestAVaultOutsideStepUpIsNeverGated(t *testing.T) {
	s := newStepUpFixture(t)
	if res := s.as(t, "GET", "/personal/v1/head", nil, s.gated); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; personal is not in this token's stepUp", res.Code)
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

// Unlocking a vault this token does not gate would spend a code on a grant
// nothing consults.
func TestUnlockIsRefusedForATokenWithoutStepUp(t *testing.T) {
	s := newStepUpFixture(t)
	res := s.as(t, "POST", "/work/v1/unlock", map[string]any{"code": s.code(t)}, s.plain)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: this token has no step-up", res.Code)
	}
}

func TestUnlockIsRefusedOnAVaultOutsideStepUp(t *testing.T) {
	s := newStepUpFixture(t)
	res := s.as(t, "POST", "/personal/v1/unlock", map[string]any{"code": s.code(t)}, s.gated)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: personal is not in this token's stepUp", res.Code)
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

	type answer struct {
		status int
		body   string
	}
	got := make(chan answer, 1)
	go func() {
		req, _ := http.NewRequest("GET", srv.URL+"/work/v1/wait?timeout=60", nil)
		req.Header.Set("Authorization", "Bearer "+s.gated)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- answer{}
			return
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		got <- answer{res.StatusCode, string(b)}
	}()

	time.Sleep(150 * time.Millisecond) // let the long poll block
	s.expireGrant(s.gated, "work")

	select {
	case a := <-got:
		// 403 step_up_required, and the "ended while waiting" one: a refusal
		// at admission would prove nothing about the lapse.
		if a.status != http.StatusForbidden || !strings.Contains(a.body, "ended while waiting") ||
			!strings.Contains(a.body, protocol.CodeStepUpRequired) {
			t.Fatalf("got %d %s, want 403 step_up_required ended while waiting", a.status, a.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a blocked long poll outlived its grant")
	}
}

// The plugin refuses, at setup, a token gated on the vault it would sync.
func TestVaultListReportsThisTokensStepUp(t *testing.T) {
	s := newStepUpFixture(t)
	for tok, want := range map[string][]string{s.gated: {"work", "private"}, s.plain: {}} {
		res := s.as(t, "GET", "/v1/vaults", nil, tok)
		var got struct {
			StepUp []string `json:"stepUp"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.StepUp == nil || strings.Join(got.StepUp, ",") != strings.Join(want, ",") {
			t.Errorf("%s: stepUp = %#v, want %v (present even when empty)", tok, got.StepUp, want)
		}
	}
}

// A token can gain a stepUp entry under the same hash when the tokens file is
// edited. A stream it opened ungated has no lapse channel, so without the
// periodic re-check it would keep publishing changed paths forever.
func TestAStreamStopsWhenItsTokenGainsStepUp(t *testing.T) {
	defer func(d time.Duration) { streamKeepalive = d }(streamKeepalive)
	streamKeepalive = 50 * time.Millisecond

	s := newStepUpFixture(t)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/work/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+s.plain)
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

	edited := s.principals()
	p := edited[s.plain]
	p.StepUp, p.TotpSecret = []string{"work"}, rfcSecretForAPITest
	edited[s.plain] = p
	s.set.Replace(auth.NewSetForTest(edited))

	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream kept running after its token gained step-up on this vault")
	}
}

// edit swaps one token's principal in place, as `token update` does.
func (f *stepUpFixture) edit(tok string, change func(*auth.Principal)) {
	all := f.principals()
	p := all[tok]
	change(&p)
	all[tok] = p
	f.set.Replace(auth.NewSetForTest(all))
}

// commitTo writes one file to vault through the API as the writer token.
func (f *stepUpFixture) commitTo(t *testing.T, vault, path, body string) {
	t.Helper()
	var head struct {
		Head string `json:"head"`
	}
	json.Unmarshal(f.as(t, "GET", "/"+vault+"/v1/head", nil, f.writer).Body.Bytes(), &head)
	hash, _ := repo.HashContent([]byte(body))
	req := httptest.NewRequest("PUT", "/"+vault+"/v1/content/"+hash, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.writer)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	res := f.as(t, "POST", "/"+vault+"/v1/push", protocol.PushRequest{
		Base: head.Head, Device: "test",
		Changes: []reconcile.Change{{Path: path, Op: "put", Hash: hash}},
	}, f.writer)
	if res.Code != http.StatusOK {
		t.Fatalf("push: %d %s", res.Code, res.Body)
	}
}

// A new TOTP secret must not inherit a grant earned with the old one: unlock,
// clear step-up, re-add it with a fresh secret, and the token needs a new code.
func TestAGrantDoesNotSurviveASecretChange(t *testing.T) {
	s := newStepUpFixture(t)
	s.unlock(t, "work", s.gated)
	s.edit(s.gated, func(p *auth.Principal) { p.TotpSecret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP" })
	if res := s.as(t, "GET", "/work/v1/head", nil, s.gated); res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: the grant was earned with a secret this token no longer has", res.Code)
	}
}

// A long poll admitted ungated must not report a commit once its token has
// gained step-up on that vault.
func TestABlockedWaitDoesNotReportNewsAfterItsTokenGainsStepUp(t *testing.T) {
	s := newStepUpFixture(t)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()

	var head struct {
		Head string `json:"head"`
	}
	json.Unmarshal(s.as(t, "GET", "/work/v1/head", nil, s.plain).Body.Bytes(), &head)

	status := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("GET", srv.URL+"/work/v1/wait?timeout=30&since="+head.Head, nil)
		req.Header.Set("Authorization", "Bearer "+s.plain)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			status <- 0
			return
		}
		res.Body.Close()
		status <- res.StatusCode
	}()

	time.Sleep(150 * time.Millisecond) // let the long poll block
	s.edit(s.plain, func(p *auth.Principal) {
		p.StepUp, p.TotpSecret = []string{"work"}, rfcSecretForAPITest
	})
	s.commitTo(t, "work", "a.md", "news\n")

	select {
	case got := <-status:
		if got != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: the poll reported a commit to a token now gated", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll never answered")
	}
}

// A stream admitted ungated has no lapse channel. If its token gains step-up and
// is unlocked before the next keepalive, it must still end: otherwise it runs on
// a grant it never watched, past that grant's absolute expiry.
func TestAnUngatedStreamEndsWhenItsTokenBecomesGatedEvenIfUnlocked(t *testing.T) {
	defer func(d time.Duration) { streamKeepalive = d }(streamKeepalive)
	streamKeepalive = 50 * time.Millisecond

	s := newStepUpFixture(t)
	srv := httptest.NewServer(s.handler)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/work/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+s.plain)
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

	// Grant FIRST, then gate, so no keepalive can ever see the token gated
	// without a grant. Only the "admitted ungated, now gated" rule can end it.
	gated := s.principals()[s.plain]
	gated.StepUp, gated.TotpSecret = []string{"work"}, rfcSecretForAPITest
	s.grants.Grant(grantKey(s.plain, gated), "work")
	s.edit(s.plain, func(p *auth.Principal) { *p = gated })

	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("an ungated stream kept running on a grant it never watched")
	}
}
