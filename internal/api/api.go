// Package api is the HTTP surface: one set of vault-scoped routes per vault,
// path-qualified as /{vault}/v1/..., plus two server-root routes.
//
// Content is addressed by git object hash, which gives deduplication,
// resumability and idempotent uploads without any of them being designed.
//
// The empty-token guard that used to live in New moved to internal/auth when
// the signature changed: Load refuses a server with no credentials at all, and
// Lookup refuses the empty token whatever is in the table. Between them they
// are the only thing standing between a typo and an open vault, because with
// no check the expected header is exactly "Bearer ", which any client can send.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/auth"
	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/stepup"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/internal/vaults"
	"github.com/pkronstrom/obsidian-archivist/internal/version"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

type Server struct {
	reg      *vaults.Registry
	tokens   *auth.Set
	grants   *stepup.Grants
	verifier *stepup.Verifier
}

// Option configures the server. Variadic because step-up is additive: every
// existing caller builds a server with no protected vaults and must keep
// compiling unchanged.
type Option func(*Server)

// WithStepUp supplies the grant table and verifier.
//
// Without it the server still ENFORCES the gate -- it simply cannot open it, so
// a protected vault refuses everything. That is the correct direction for a
// build that forgot to wire this: unavailable, not unguarded.
func WithStepUp(g *stepup.Grants, v *stepup.Verifier) Option {
	return func(s *Server) { s.grants, s.verifier = g, v }
}

// stepUpState is the gate decision made at admission.
//
// Handlers that outlive the middleware -- SSE, long-poll -- must act on this
// rather than re-deriving it. Re-statting the marker inside a handler is a
// TOCTOU window: a stream opened while a vault was unprotected would otherwise
// keep running after the marker appeared, and a transient error would silently
// downgrade a gated stream to an unwatched one.
type stepUpState struct {
	gated  bool
	lapsed <-chan struct{}
}

func stepUpFrom(r *http.Request) stepUpState {
	st, _ := r.Context().Value(ctxStepUp).(stepUpState)
	return st
}

// ctxKey carries the resolved principal from the middleware to the handlers, so
// a handler cannot forget to check the scope: by the time it runs, the check has
// already happened.
type ctxKey int

const (
	ctxPrincipal ctxKey = iota
	// ctxStepUp carries the gate decision from withVault to handlers that
	// outlive it.
	ctxStepUp
)

// New builds the whole surface: one set of routes per vault, path-qualified,
// from the SAME route table that generates the index. A route still cannot
// exist without being documented, or be documented without existing.
func New(reg *vaults.Registry, tokens *auth.Set, opts ...Option) http.Handler {
	s := &Server{reg: reg, tokens: tokens}
	for _, o := range opts {
		o(s)
	}

	mux := http.NewServeMux()

	// Server-root routes. GET /v1/vaults is what makes a picker possible: a
	// client needs it BEFORE it knows which vault to ask for, which is exactly
	// why the vault cannot be baked into the base URL.
	mux.HandleFunc("GET /v1/vaults", s.listVaults)
	mux.HandleFunc("POST /v1/vaults", s.createVault)

	// Per-vault routes. {vault} is a single path segment, so a name containing
	// a space is addressable percent-encoded -- My%20Own%20Vault -- which the
	// plugin does automatically and a human writing curl must remember.
	mux.HandleFunc("GET /{vault}/v1", s.withVault(s.scopeFor("GET", "/v1"), false, s.index))
	for _, rt := range s.routes() {
		if rt.handle == nil || rt.Path == "/healthz" {
			continue
		}
		mux.HandleFunc(rt.Method+" /{vault}"+rt.Path, s.withVault(rt.Scope, rt.SkipStepUp, rt.handle))
	}

	// healthz sits OUTSIDE the auth middleware, deliberately and alone. It is
	// also unqualified: a container healthcheck has no token and no idea which
	// vaults exist.
	outer := http.NewServeMux()
	outer.HandleFunc("GET /healthz", s.healthz)
	outer.Handle("/", s.authenticate(mux))
	return outer
}

// authenticate resolves the bearer token to a principal.
//
// Constant-time comparison happens inside auth.Set.Lookup, over every entry, so
// a wrong token cannot be recovered by timing the response.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, ok := s.tokens.Lookup(token)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="archivist"`)
			fail(w, http.StatusUnauthorized, protocol.CodeUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p)))
	})
}

// withVault resolves {vault}, checks the token's scope, and hands the handler a
// request that already carries the instance.
//
// 403 for a vault outside the scope and 404 for one that does not exist. Those
// differ deliberately: a token holder learning that "work" exists is not a
// secret worth protecting here -- both vaults are the same person's -- and
// collapsing them into one status makes a misconfigured token
// indistinguishable from a typo, which is the failure people actually hit.
func (s *Server) withVault(scope string, skipStepUp bool, h func(http.ResponseWriter, *http.Request, *vaults.Instance)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("vault")
		p, _ := r.Context().Value(ctxPrincipal).(auth.Principal)

		inst, err := s.reg.Get(name)
		if err != nil {
			if vaults.IsNotFound(err) {
				fail(w, http.StatusNotFound, protocol.CodeNotFound, "no such vault: "+name)
				return
			}
			fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
			return
		}
		if !p.Opens(name) {
			fail(w, http.StatusForbidden, protocol.CodeForbidden,
				"this token does not open the vault "+name)
			return
		}
		if scope != "" && !p.Can(scope) {
			fail(w, http.StatusForbidden, protocol.CodeForbidden,
				"this token does not hold the "+scope+" scope")
			return
		}
		// Step-up LAST, so a caller who would be refused anyway is refused for
		// the reason that actually applies rather than being asked for a
		// single-use code it cannot spend.
		st, ok := s.stepUpDecision(w, r, p, name, skipStepUp)
		if !ok {
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxStepUp, st)), inst)
	}
}

// unlock exchanges a one-time code for a grant on this vault.
//
// It requires that this vault is protected AND that this token gates access to
// it. Without both, a code would be spent to create a grant nothing consults --
// and an unlock issued before a marker existed would pre-authorise the vault
// that is about to be protected.
func (s *Server) unlock(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	p, _ := r.Context().Value(ctxPrincipal).(auth.Principal)

	if !stepUpFrom(r).gated {
		fail(w, http.StatusForbidden, protocol.CodeForbidden,
			"nothing to unlock: "+inst.Name+" does not require step-up from this token")
		return
	}
	if p.TotpSecret == "" {
		fail(w, http.StatusForbidden, protocol.CodeForbidden,
			"this token has no step-up secret, so it cannot unlock anything")
		return
	}
	if s.verifier == nil || s.grants == nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal,
			"this server was built without step-up support")
		return
	}

	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, `expected {"code": "123456"}`)
		return
	}

	hash := auth.HashToken(bearer(r))
	if err := s.verifier.Check(hash, inst.Name, p.TotpSecret, body.Code); err != nil {
		var cooling stepup.ErrCoolingDown
		if errors.As(err, &cooling) {
			w.Header().Set("Retry-After", strconv.Itoa(int(cooling.Retry.Seconds())+1))
		}
		fail(w, http.StatusForbidden, protocol.CodeStepUpRequired, err.Error())
		return
	}

	until := s.grants.Grant(hash, inst.Name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"vault":     inst.Name,
		"expiresAt": until.Unix(),
	})
}

// stepUpDecision enforces the consent gate and returns what the handler needs
// to keep enforcing it. It writes the refusal itself, so the caller only has to
// return.
//
// Two halves, both required: the vault carries a marker, and this token recorded
// a decision about it. See docs/adr/0003-protected-vault-and-token-posture.md.
func (s *Server) stepUpDecision(w http.ResponseWriter, r *http.Request, p auth.Principal, vault string, skip bool) (stepUpState, bool) {
	protected, err := s.reg.Protected(vault)
	if err != nil {
		// Cannot tell is not no. Serving here would be the one failure this
		// gate exists to prevent.
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return stepUpState{}, false
	}
	if !protected {
		return stepUpState{}, true
	}

	// Absence denies. A token that recorded nothing about this vault predates
	// the marker, and treating silence as consent is exactly the fail-open that
	// ruled out making this a property of the token alone.
	if !p.StepUpDecided(vault) {
		fail(w, http.StatusForbidden, protocol.CodeStepUpRequired,
			"this token predates step-up on "+vault+
				" and has no recorded posture; re-mint it with -step-up or -no-step-up")
		return stepUpState{}, false
	}
	if !p.NeedsStepUp(auth.StepUpVault, vault) {
		return stepUpState{}, true
	}
	if skip {
		// The unlock route: gated by policy, but it is what opens the gate.
		return stepUpState{gated: true}, true
	}
	hash := auth.HashToken(bearer(r))
	if s.grants == nil {
		fail(w, http.StatusForbidden, protocol.CodeStepUpRequired,
			"this server was built without step-up support, so "+vault+" cannot be unlocked")
		return stepUpState{}, false
	}
	// One lock acquisition for both answers. Asking Held and then Watch would
	// let a re-grant land in between and hand this request the successor's
	// channel, so it would outlive the grant it was actually admitted under.
	lapsed, held := s.grants.HeldWatch(hash, vault)
	if !held {
		fail(w, http.StatusForbidden, protocol.CodeStepUpRequired,
			"this token needs an unlock code for "+vault+
				"; POST a code to /"+vault+"/v1/unlock")
		return stepUpState{}, false
	}
	return stepUpState{gated: true, lapsed: lapsed}, true
}

// stepUpStillPermits re-answers the gate for a stream that has already been
// running, and returns why it must stop.
//
// The admission-time decision is the right one for the request that carried it,
// but a long-lived stream outlives the facts it was based on. Protection can
// begin mid-stream, and the marker can become unreadable -- both of which must
// end the stream rather than be assumed benign.
func (s *Server) stepUpStillPermits(p auth.Principal, vault, presented string) error {
	protected, err := s.reg.Protected(vault)
	if err != nil {
		return err // cannot tell is not no, here as anywhere else
	}
	if !protected {
		return nil
	}
	if !p.StepUpDecided(vault) {
		return errors.New("no recorded step-up posture")
	}
	if !p.NeedsStepUp(auth.StepUpVault, vault) {
		return nil
	}
	if s.grants == nil || !s.grants.Held(auth.HashToken(presented), vault) {
		return errors.New("no active grant")
	}
	return nil
}

// bearer lifts the presented token back out of the request.
//
// The middleware resolves it to a principal and deliberately does not carry the
// secret onward; grants are keyed by its hash, so this is the one place that
// needs it again.
func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// Wire types live in the protocol package.

// ---- handlers --------------------------------------------------------------

func (s *Server) head(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	h, err := inst.Repo.Head()
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, protocol.HeadResponse{Head: h})
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	h, err := inst.Repo.Head()
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	files, err := inst.Repo.Snapshot(h)
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, protocol.SnapshotResponse{Head: h, Files: files})
}

func (s *Server) changes(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	since := r.URL.Query().Get("since")
	h, err := inst.Repo.Head()
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	entries, err := inst.Repo.Changes(since, h)
	if err != nil {
		// The cursor is from another repository, or predates a history
		// rewrite. 409 tells the client to re-bootstrap from /snapshot; a 500
		// would suggest the server was at fault and invite a retry loop.
		if repo.IsUnknownBase(err) {
			fail(w, http.StatusConflict, protocol.CodeUnknownBase, "unknown base; re-bootstrap from /v1/snapshot")
			return
		}
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	if entries == nil {
		entries = []repo.Change{}
	}
	writeJSON(w, protocol.ChangesResponse{Head: h, Entries: entries})
}

func (s *Server) have(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	var req protocol.HaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, "malformed body: "+err.Error())
		return
	}
	missing := []string{}
	for _, h := range req.Hashes {
		if !inst.Repo.HasBlob(h) {
			missing = append(missing, h)
		}
	}
	writeJSON(w, protocol.HaveResponse{Missing: missing})
}

// sanitiseVia bounds a header a caller controls before it lands in git history
// forever. Newlines would forge extra trailers; length keeps one request from
// bloating every commit.
func sanitiseVia(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
	if len(s) > 32 {
		s = s[:32]
	}
	return strings.TrimSpace(s)
}

// statusFor maps a guard's protocol code to an HTTP status. 429 tells a
// client to back off and retry, which is what a quarantine or a throttle
// wants; 507 says the server is out of room, which no retry will fix.
func statusFor(code string) int {
	switch code {
	case protocol.CodeQuarantined, protocol.CodeThrottled:
		return http.StatusTooManyRequests
	case protocol.CodeDiskLow:
		return http.StatusInsufficientStorage
	default:
		return http.StatusInternalServerError
	}
}

func noteMutationStatusFor(code string) int {
	switch code {
	case protocol.CodeMalformed, protocol.CodeNotText, protocol.CodeInvalidPath:
		return http.StatusBadRequest
	case protocol.CodeNotFound:
		return http.StatusNotFound
	case protocol.CodeStale, protocol.CodeNoMatch, protocol.CodeMultipleMatches:
		return http.StatusConflict
	case protocol.CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case protocol.CodeForbidden:
		return http.StatusForbidden
	default:
		return statusFor(code)
	}
}

func (s *Server) putContent(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	claimed := r.PathValue("hash")
	// protocol.MaxUploadBytes+1 so an oversized body is DETECTED rather than silently
	// truncated. LimitReader alone would hash the first 512 MiB, and a client
	// whose claimed hash happened to match that prefix would get a 200 for a
	// request whose tail was discarded.
	body, err := io.ReadAll(io.LimitReader(r.Body, protocol.MaxUploadBytes+1))
	if err != nil {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, err.Error())
		return
	}
	if int64(len(body)) > protocol.MaxUploadBytes {
		fail(w, http.StatusRequestEntityTooLarge, protocol.CodeTooLarge,
			fmt.Sprintf("content exceeds the %d byte limit", protocol.MaxUploadBytes))
		return
	}
	// Verify the address before storing. A store that trusts the client's hash
	// is not content-addressed, and every downstream guarantee -- dedup,
	// idempotency, the merge base -- rests on the address being true.
	actual, err := repo.HashContent(body)
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	if !strings.EqualFold(actual, claimed) {
		fail(w, http.StatusBadRequest, protocol.CodeHashMismatch,
			"content hash mismatch: claimed "+claimed+", actual "+actual)
		return
	}
	if _, err := inst.Repo.WriteBlob(body); err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, map[string]string{"hash": actual})
}

func (s *Server) getContent(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	content, err := inst.Repo.ReadBlob(r.PathValue("hash"))
	if err != nil {
		fail(w, http.StatusNotFound, protocol.CodeNotFound, "no such object")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(content)
}

func (s *Server) push(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	var req protocol.PushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, "malformed body: "+err.Error())
		return
	}
	// Delete is not a route, so the routes table cannot gate it: it is an op
	// inside the change set. Checked here, in the HTTP layer where the principal
	// already lives, so reconcile keeps knowing nothing about callers -- and
	// checked over the WHOLE set first, so a push that deletes anything is
	// refused before a single change is staged.
	//
	// A rename arrives as a del plus a put, so a token without delete cannot
	// rename either. The `move` op that fixes that is a separate change.
	p, _ := r.Context().Value(ctxPrincipal).(auth.Principal)
	if !p.Can(auth.ScopeDelete) {
		for _, c := range req.Changes {
			if c.Op == protocol.OpDel {
				fail(w, http.StatusForbidden, protocol.CodeForbidden,
					"this token does not hold the delete scope; refusing to delete "+c.Path)
				return
			}
		}
	}

	// The label comes from the principal the middleware resolved, so it names
	// the credential that was actually presented rather than whatever the
	// client claimed to be. That is the whole point: device is forgeable, this
	// is not.
	via := r.Header.Get(protocol.HeaderVia)
	if via == "" {
		via = "api"
	}
	// reconcile sanitises every trailer value on the way into the message, so
	// this only trims the header to something sensible to log.
	head, results, err := inst.Reconciler.PushWithOrigin(req.Base,
		reconcile.Origin{Device: req.Device, Token: p.Label, Via: sanitiseVia(via)},
		req.Changes)
	if err != nil {
		var pe *protocol.Error
		switch {
		// A guard refusal already names its own code; use it rather than
		// re-deriving one.
		case errors.As(err, &pe):
			fail(w, statusFor(pe.Code), pe.Code, pe.Message)
		case repo.IsUnknownBase(err):
			fail(w, http.StatusConflict, protocol.CodeUnknownBase, "unknown base; re-bootstrap from /v1/snapshot")
		// Client faults get a specific code so a client can act on them
		// rather than parsing prose.
		case errors.Is(err, vault.ErrInvalidPath):
			fail(w, http.StatusBadRequest, protocol.CodeInvalidPath, err.Error())
		case strings.Contains(err.Error(), "was never uploaded"):
			fail(w, http.StatusBadRequest, protocol.CodeMissingContent, err.Error())
		case strings.Contains(err.Error(), "appears twice"):
			fail(w, http.StatusBadRequest, protocol.CodeDuplicatePath, err.Error())
		default:
			fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		}
		return
	}
	writeJSON(w, protocol.PushResponse{Head: head, Results: results})
}

func noteMutationOrigin(r *http.Request, device string) reconcile.Origin {
	p, _ := r.Context().Value(ctxPrincipal).(auth.Principal)
	via := r.Header.Get(protocol.HeaderVia)
	if via == "" {
		via = "api"
	}
	return reconcile.Origin{
		Device: device,
		Token:  p.Label,
		Via:    sanitiseVia(via),
	}
}

func failNoteMutation(w http.ResponseWriter, err error) {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		fail(w, noteMutationStatusFor(pe.Code), pe.Code, pe.Message)
		return
	}
	fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
}

func failNoteMutationDecode(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		fail(w, http.StatusRequestEntityTooLarge, protocol.CodeTooLarge, "request body is too large")
		return
	}
	fail(w, http.StatusBadRequest, protocol.CodeMalformed, "malformed body: "+err.Error())
}

const maxNoteMutationRequestBytes = 16 << 20

func decodeOneJSON(body io.Reader, dst any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func (s *Server) appendNote(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	var req protocol.AppendNoteRequest
	if err := decodeOneJSON(http.MaxBytesReader(w, r.Body, maxNoteMutationRequestBytes), &req); err != nil {
		failNoteMutationDecode(w, err)
		return
	}
	effectivePath, head, contentRevision, err := inst.Reconciler.AppendNote(
		req.Path,
		[]byte(req.Content),
		req.ContentRevision,
		noteMutationOrigin(r, req.Device),
	)
	if err != nil {
		failNoteMutation(w, err)
		return
	}
	writeJSON(w, protocol.NoteMutationResponse{
		Path:            effectivePath,
		Status:          protocol.StatusApplied,
		Revision:        head,
		ContentRevision: contentRevision,
	})
}

func (s *Server) editNote(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	var req protocol.EditNoteRequest
	if err := decodeOneJSON(http.MaxBytesReader(w, r.Body, maxNoteMutationRequestBytes), &req); err != nil {
		failNoteMutationDecode(w, err)
		return
	}
	effectivePath, head, contentRevision, err := inst.Reconciler.EditNote(
		req.Path,
		req.ContentRevision,
		[]byte(req.OldText),
		[]byte(req.NewText),
		noteMutationOrigin(r, req.Device),
	)
	if err != nil {
		failNoteMutation(w, err)
		return
	}
	writeJSON(w, protocol.NoteMutationResponse{
		Path:            effectivePath,
		Status:          protocol.StatusApplied,
		Revision:        head,
		ContentRevision: contentRevision,
	})
}

// events is a Server-Sent Events stream of commit hashes, for anything that
// wants to react to a change rather than poll for it -- an indexer, an agent, a
// webhook bridge.
//
//	curl -N -H "Authorization: Bearer $TOKEN" https://vault.example/v1/events
//	data: {"head":"4ff143d6..."}
//
// It carries a NOTIFICATION, not the change itself. The durable feed is
// /v1/changes?since=<cursor>: git history is append-only, so a consumer that
// stores a cursor can always ask what it missed, however long it was away.
// This stream only says "something moved, go look" -- so a dropped event, a
// reconnect, or a consumer that was offline for a week all cost nothing, and
// there is no queue to persist or backlog to manage.
//
// Consumers should therefore: subscribe, and on each event call /v1/changes
// with their own cursor. Never treat the stream as the record.
func (s *Server) events(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Reverse proxies buffer by default, which would hold events until the
	// buffer fills -- defeating the point of the stream.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Subscribe BEFORE reading the head, same as wait does and for the same
	// reason: reading first leaves a window where a commit lands after the
	// read and before the subscription, and this consumer never hears about
	// it -- it would orient itself on a head that is already stale and then
	// wait for an event that has already been dropped.
	ch, stop := inst.Reconciler.Subscribe()
	defer stop()

	// Send the current head immediately, so a consumer that just connected can
	// orient itself without waiting for the next commit.
	if head, err := inst.Repo.Head(); err == nil && head != "" {
		writeEvent(w, map[string]any{"head": head, "count": 0, "changes": []any{}})
	}
	flusher.Flush()

	// Idle connections get dropped by proxies; a comment line is a valid SSE
	// keep-alive that consumers ignore.
	ticker := time.NewTicker(streamKeepalive)
	defer ticker.Stop()

	// The principal was resolved once, when the request was authenticated. A
	// stream outlives that moment by days, so without re-checking, revoking or
	// expiring a token would never stop an ALREADY OPEN stream -- and this one
	// carries the path of every change in the vault.
	//
	// The keepalive tick is where it belongs: the timer already exists, and
	// bounding the exposure to one interval is the whole requirement.
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	name := r.PathValue("vault")

	// The gate was decided at admission and carried here. Re-checking the marker
	// or the grant now would be a TOCTOU window: a stream opened while the vault
	// was unprotected would keep running after the marker appeared. A nil
	// channel blocks forever, which is exactly right for an ungated caller.
	lapsed := stepUpFrom(r).lapsed

	for {
		select {
		case <-r.Context().Done():
			return
		case <-lapsed:
			return // the consent that opened this stream has ended
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeEvent(w, ev)
			flusher.Flush()
		case <-ticker.C:
			p, ok := s.tokens.Lookup(bearer)
			if !ok || !p.Opens(name) || !p.Can(auth.ScopeRead) {
				return // revoked, expired, or narrowed since this stream opened
			}
			// The vault may have been PROTECTED since this stream opened. A
			// stream admitted while it was not has no lapse channel and would
			// otherwise keep publishing changed paths for days after the marker
			// appeared. Bounded by one tick, same as the revocation check above,
			// and for the same reason: the timer already exists.
			if s.stepUpStillPermits(p, name, bearer) != nil {
				return
			}
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
}

// history lists the revisions in which a path changed.
// wait is long-polling: it holds the request open until the head moves past
// ?since=, or ?timeout= seconds elapse, whichever comes first.
//
// This exists because /v1/events cannot be consumed by the Obsidian plugin.
// Obsidian's requestUrl is the only HTTP transport that works on both desktop
// and iOS -- it exists precisely to bypass CORS -- and it returns a complete
// response rather than a stream. Native fetch would need CORS, and the
// preflight for an Authorization header is an unauthenticated OPTIONS that this
// server answers with 401, so the request would fail before it began.
//
// Long-polling gives the same properties as SSE for a single waiter: one idle
// connection, no bytes while nothing happens, sub-second latency on a change.
// It differs only in costing one request per change rather than per stream.
//
// The client is expected to loop: call wait, sync when it reports a change,
// call wait again with the new cursor. `changed` is advisory -- the caller
// still asks /v1/changes what actually moved, so a spurious wake is harmless
// and a missed one is caught by the next poll.
func (s *Server) wait(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	since := r.URL.Query().Get("since")

	timeout := 60 * time.Second
	if v := r.URL.Query().Get("timeout"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fail(w, http.StatusBadRequest, protocol.CodeMalformed, "timeout must be a positive number of seconds")
			return
		}
		if d := time.Duration(n) * time.Second; d < maxWait {
			timeout = d
		} else {
			timeout = maxWait
		}
	}

	head, err := inst.Repo.Head()
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	// Already behind: answer at once rather than making the caller wait for a
	// change it has in fact already missed.
	if head != since {
		writeJSON(w, protocol.WaitResponse{Head: head, Changed: true})
		return
	}

	// Subscribe BEFORE re-checking head. Subscribing after would leave a window
	// in which a commit lands between the check and the subscription, and the
	// caller would then block for the full timeout on news that already exists.
	ch, stop := inst.Reconciler.Subscribe()
	defer stop()

	if head, err := inst.Repo.Head(); err == nil && head != since {
		writeJSON(w, protocol.WaitResponse{Head: head, Changed: true})
		return
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// The gate was decided at admission and carried here. Re-checking the marker
	// or the grant now would be a TOCTOU window: a stream opened while the vault
	// was unprotected would keep running after the marker appeared. A nil
	// channel blocks forever, which is exactly right for an ungated caller.
	lapsed := stepUpFrom(r).lapsed

	select {
	case <-lapsed:
		return // the consent that opened this poll has ended
	case ev, ok := <-ch:
		if !ok {
			writeJSON(w, protocol.WaitResponse{Head: since, Changed: false})
			return
		}
		writeJSON(w, protocol.WaitResponse{Head: ev.Head, Changed: true})
	case <-timer.C:
		// A timeout is a normal outcome, not an error: it means "still nothing".
		writeJSON(w, protocol.WaitResponse{Head: since, Changed: false})
	case <-r.Context().Done():
		// The caller went away. Nothing to write.
	}
}

func (s *Server) history(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	path := r.URL.Query().Get("path")
	if path == "" {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, "path is required")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	// Capped here rather than in the repo: an in-process caller may legitimately
	// want the whole history, but a request must not be able to walk every
	// commit in the vault.
	if limit > repo.MaxHistoryLimit {
		limit = repo.MaxHistoryLimit
	}
	before := r.URL.Query().Get("before")
	revs, more, err := inst.Repo.HistoryPage(path, limit, before)
	if err != nil {
		// A bad cursor is the caller's mistake, not the server's. Returning it
		// as a 500 would make a typo look like an outage.
		if errors.Is(err, repo.ErrUnknownCursor) {
			fail(w, http.StatusBadRequest, protocol.CodeMalformed, err.Error())
			return
		}
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	resp := protocol.HistoryResponse{Path: path, Revisions: revs, HasMore: more}
	if more && len(revs) > 0 {
		resp.Next = revs[len(revs)-1].Commit
	}
	writeJSON(w, resp)
}

func (s *Server) listDeleted(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	gone, err := inst.Repo.Deleted()
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, map[string]any{"deleted": gone})
}

func (s *Server) listPins(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	list, head, err := inst.Pins.List(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, map[string]any{"head": head, "pins": list})
}

func (s *Server) createPin(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	var req struct {
		Name         string `json:"name"`
		Path         string `json:"path"`
		ExpectedHead string `json:"expectedHead"`
	}
	// A pin body is three short strings. Without a bound, a write token could
	// force a large allocation and commit an enormous line -- and a name past
	// the parser's scanner limit becomes unreadable, so the pin would be
	// reported as created and then never appear in any listing.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, "malformed body")
		return
	}
	if req.Name == "" {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, "name is required")
		return
	}
	if req.ExpectedHead == "" {
		// Required, not optional: a pin is a claim about a specific tree, and
		// without the check a client whose sync had not flushed would silently
		// pin an older state than the note on the user's screen.
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, "expectedHead is required")
		return
	}
	// A pin names a FILE. Vault-wide pins were removed: a whole-vault restore
	// point implies a whole-vault restore, which has no safe gesture in a
	// plugin -- it must delete everything created since, and undoing it needs
	// a second bulk operation. Allowing a path-less pin here would mint data
	// no surface can act on. See the design note for the checkpoint idea that
	// replaces it, which belongs behind step-up auth.
	if req.Path == "" {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, "path is required")
		return
	}
	if err := vault.ValidPath(req.Path); err != nil {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, err.Error())
		return
	}

	entry, head, err := inst.Reconciler.Pin(req.ExpectedHead, req.Name, req.Path)
	switch {
	case errors.Is(err, reconcile.ErrPinName):
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, err.Error())
		return
	case errors.Is(err, reconcile.ErrPinHeadMismatch):
		// 409 with the current head, so the client can flush and retry once
		// rather than guess what it collided with.
		fail(w, http.StatusConflict, protocol.CodeStaleHead, err.Error())
		return
	case errors.Is(err, reconcile.ErrPinPathMissing):
		fail(w, http.StatusNotFound, protocol.CodeNotFound, err.Error())
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": entry.ID, "head": head, "created": entry.Created})
}

// at reads a file as it was at a revision, WITHOUT touching the working tree.
// Inspecting is much more common than restoring, and conflating the two makes
// looking at an old version a destructive act.
func (s *Server) at(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	rev, p := r.PathValue("rev"), r.PathValue("path")
	if err := vault.ValidPath(p); err != nil {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, err.Error())
		return
	}
	resolved, err := inst.Repo.Resolve(rev)
	if err != nil {
		fail(w, http.StatusNotFound, protocol.CodeNotFound, "unknown revision")
		return
	}
	content, err := inst.Repo.ReadAt(resolved, p)
	if err != nil {
		fail(w, http.StatusNotFound, protocol.CodeNotFound, "no such path at that revision")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Archivist-Revision", resolved)
	w.Write(content)
}

// check compares the working tree against HEAD.
func (s *Server) check(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	rep, err := inst.Repo.Check()
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, rep)
}

// routes is the API's own description, and the ONLY list of endpoints in the
// codebase -- the mux is built from it, so a route cannot exist without being
// documented and cannot be documented without existing. That property is worth
// more than a hand-maintained OpenAPI file, which drifts the moment someone
// adds a handler in a hurry.
type route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Does   string `json:"does"`
	// Scope is what a token must hold to call this route. It lives here rather
	// than in a middleware table because this list already IS the mux: a route
	// cannot exist without appearing here, so it cannot exist without declaring
	// what it needs.
	Scope string `json:"-"`
	// SkipStepUp exempts a route from the step-up gate. Exactly one route may
	// set it -- the one that opens the gate -- and a test asserts that. It lives
	// on this table for the same reason Scope does: the table IS the mux, so an
	// exemption cannot exist without being visible next to what it exempts.
	SkipStepUp bool `json:"-"`
	// NoScope marks a route that needs authentication but no verb. Only unlock:
	// requiring read there would stop a write-only token unlocking to write, and
	// a caller who cannot read the vault gains nothing by unlocking it. A field
	// rather than an empty Scope, so the route invariant can tell "deliberately
	// none" from "somebody forgot".
	NoScope bool `json:"-"`
	handle  func(http.ResponseWriter, *http.Request, *vaults.Instance)
}

// streamKeepalive is the SSE comment interval, and also how often an open
// stream re-checks that its token is still valid. A var so tests can shorten it.
var streamKeepalive = 25 * time.Second

// scopeFor returns the scope a route declares, or "" if there is no such route.
//
// Empty means no match, and every caller must treat that as "refuse" rather
// than "allow": a typo in a path would otherwise silently open a route.
func (s *Server) scopeFor(method, path string) string {
	for _, rt := range s.routes() {
		if rt.Method == method && rt.Path == path {
			return rt.Scope
		}
	}
	return ""
}

// maxWait caps how long a long-poll may hold a request open. Proxies and
// phone radios drop idle connections well before this, so a longer ceiling
// buys nothing and just hides a stuck client.
const maxWait = 120 * time.Second

func (s *Server) routes() []route {
	return []route{
		{Method: "GET", Path: "/v1", Does: "this list", Scope: auth.ScopeRead},
		{Method: "GET", Path: "/v1/head", Does: "current commit hash", Scope: auth.ScopeRead, handle: s.head},
		{Method: "GET", Path: "/v1/snapshot", Does: "every file at head: path, hash, size", Scope: auth.ScopeRead, handle: s.snapshot},
		{Method: "GET", Path: "/v1/changes", Does: "what changed since ?since=<commit>; 409 if unknown", Scope: auth.ScopeRead, handle: s.changes},
		{Method: "POST", Path: "/v1/have", Does: "{hashes:[...]} -> {missing:[...]}", Scope: auth.ScopeRead, handle: s.have},
		// Staging a blob is a WRITE. Nothing references it until a push, but an
		// unreferenced blob still consumes disk, and the write guards count
		// writes per path at push time -- they never see an orphan.
		{Method: "PUT", Path: "/v1/content/{hash}", Does: "upload content; 400 if it does not hash to {hash}", Scope: auth.ScopeWrite, handle: s.putContent},
		{Method: "GET", Path: "/v1/content/{hash}", Does: "download content by hash", Scope: auth.ScopeRead, handle: s.getContent},
		// Delete is an op INSIDE the change set, so this route needs write and
		// the handler additionally checks delete. See push.
		{Method: "POST", Path: "/v1/push", Does: "{base,device,changes:[...]} apply a change set", Scope: auth.ScopeWrite, handle: s.push},
		{Method: "POST", Path: "/v1/note/append", Does: "{path,content,content_revision?} atomically append text to a note", Scope: auth.ScopeWrite, handle: s.appendNote},
		{Method: "POST", Path: "/v1/note/edit", Does: "{path,content_revision,old_text,new_text} atomically replace one exact match", Scope: auth.ScopeWrite, handle: s.editNote},
		{Method: "GET", Path: "/v1/events", Does: "SSE:one per commit with changed paths, kind, size", Scope: auth.ScopeRead, handle: s.events},
		{Method: "GET", Path: "/v1/wait", Does: "long-poll: blocks until head moves past ?since=, or ?timeout= elapses", Scope: auth.ScopeRead, handle: s.wait},
		{Method: "GET", Path: "/v1/history", Does: "?path=&limit= revisions that touched a path", Scope: auth.ScopeRead, handle: s.history},
		// Pins are ordinary vault data, so their scopes are the ordinary ones:
		// creating a pin is a put to one tracked file, nothing more.
		{Method: "GET", Path: "/v1/pins", Does: "?path= named restore points; path=* for vault-wide only", Scope: auth.ScopeRead, handle: s.listPins},
		{Method: "POST", Path: "/v1/pin", Does: "{name,path?,expectedHead} name a restore point", Scope: auth.ScopeWrite, handle: s.createPin},
		// Discovery only: every version has always been in the pack, and /v1/at
		// has always been able to serve one. This answers "what did I lose",
		// which was previously answerable only by knowing the path already.
		{Method: "GET", Path: "/v1/deleted", Does: "paths history holds that head does not, with the revision to read them from", Scope: auth.ScopeRead, handle: s.listDeleted},
		{Method: "GET", Path: "/v1/at/{rev}/{path...}", Does: "a file as it was at a revision; does not restore", Scope: auth.ScopeRead, handle: s.at},
		{Method: "GET", Path: "/v1/check", Does: "working tree versus head", Scope: auth.ScopeRead, handle: s.check},
		{Method: "GET", Path: "/v1/export", Does: "consistent archive of history; ?gzip=1 to compress", Scope: auth.ScopeRead, handle: s.export},
		// The one route exempt from the gate it opens. NoScope because a caller
		// who cannot read the vault gains nothing by unlocking it, and requiring
		// read would stop a write-only token unlocking in order to write.
		{Method: "POST", Path: "/v1/unlock", Does: "{code} start a step-up grant for this vault",
			NoScope: true, SkipStepUp: true, handle: s.unlock},
		{Method: "GET", Path: "/healthz", Does: "liveness, no auth"},
	}
}

// index describes the API to whatever is calling it. Agents and humans both
// benefit, and it costs one struct rather than a specification to maintain.
func (s *Server) index(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	type doc struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Does   string `json:"does"`
	}
	out := []doc{}
	for _, rt := range s.routes() {
		out = append(out, doc{rt.Method, rt.Path, rt.Does})
	}
	writeJSON(w, map[string]any{
		"service": "archivist",
		// Which vault this process serves, so a client can tell one server from
		// another. One vault per process, so this is unambiguous.
		//
		// Without it there is no way to notice the dangerous misconfiguration:
		// point a Personal vault's plugin at the Work server and the client
		// bootstraps from whatever snapshot it is handed, merging two unrelated
		// vaults into both. Git history makes that recoverable, but only if
		// someone notices.
		"vault":    inst.Name,
		"version":  version.Version,
		"protocol": protocol.Version,
		"notes": []string{
			"All /v1 routes need Authorization: Bearer <token>.",
			"Content is addressed by git object hash: printf '%s' \"$c\" | git hash-object --stdin",
			"/v1/changes is the durable feed; /v1/events only says when to read it.",
			"Check `protocol`, not `version`: releases bump the latter constantly.",
		},
		"endpoints": out,
	})
}

// healthz is the only unauthenticated route. A container healthcheck has no
// credentials and no shell -- the image is FROM scratch -- so this exists to be
// reachable by anything that can make an HTTP request.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// Version here as well as in /v1: a monitor without a token still wants to
	// know what is running, and neither field is a secret.
	writeJSON(w, map[string]any{
		"status": "ok", "version": version.Version, "protocol": protocol.Version,
	})
}

// export streams a consistent, gzipped archive of the git directory.
//
//	curl -sf -H "Authorization: Bearer $TOKEN" https://vault.example/v1/export \
//	  | restic backup --stdin --stdin-filename vault-personal.tar.gz
//
// The archive is built into a temp file while commits are frozen, then streamed
// with the lock released -- otherwise a slow client would block every write for
// the duration of the transfer.
func (s *Server) export(w http.ResponseWriter, r *http.Request, inst *vaults.Instance) {
	// Uncompressed by default: gzip here would cost a full copy per backup
	// snapshot rather than a delta. See repo.Archive.
	compress := r.URL.Query().Get("gzip") == "1"

	// Scratch space beside the repository, NOT in /tmp.
	//
	// os.CreateTemp("") writes to /tmp, and the image is FROM scratch: there is
	// no /tmp. Every call to this endpoint returned 500 with "no such file or
	// directory" from the day it shipped -- the one endpoint whose whole job is
	// preventing a corrupt backup was dead, and nothing noticed because a
	// developer machine and CI both have /tmp.
	//
	// A tmpfs mount in compose also fixes it, and one is now in place, but a
	// deployment detail should not be what makes the binary work. The git
	// directory is writable by definition here and on the same filesystem as the
	// objects being archived.
	tmp, err := os.CreateTemp(inst.Repo.GitDir(), "archivist-export-*.tar")
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if err := inst.Reconciler.Freeze(func() error { return inst.Repo.Archive(tmp, compress) }); err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	fi, err := tmp.Stat()
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}

	name, ctype := "archivist-git.tar", "application/x-tar"
	if compress {
		name, ctype = "archivist-git.tar.gz", "application/gzip"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	io.Copy(w, tmp)
}

// ---- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent; all we can do is stop.
		return
	}
}

// fail writes the stable error envelope. Clients branch on Code; Message is for
// humans and will be reworded. Before this, the only signal was an HTTP status
// plus prose, so the plugin read every 409 as unknown-base -- correct only
// while there was exactly one thing a 409 could mean.
func fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(protocol.ErrorResponse{
		Error: protocol.Error{Code: code, Message: msg},
	})
}

// listVaults is the picker AND the authorisation check, from one fact: it
// returns exactly what the presented token opens.
//
// Rescanned behind a short cache, so a vault rsynced in appears without a
// restart -- avoiding the failure where nothing shows up and nothing says why.
func (s *Server) listVaults(w http.ResponseWriter, r *http.Request) {
	p, _ := r.Context().Value(ctxPrincipal).(auth.Principal)
	// A server-root route, so withVault never ran and the routes() table never
	// covered it. The scope check has to be here or it does not happen: without
	// it a write-only token could enumerate vault names.
	if !p.Can(auth.ScopeRead) {
		fail(w, http.StatusForbidden, protocol.CodeForbidden,
			"this token does not hold the read scope")
		return
	}
	all, err := s.reg.Names()
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	// Scopes and label are here so a CLIENT can refuse a wrong token at setup
	// rather than at first use. The Obsidian plugin needs write; handed a
	// read-only agent token it currently syncs down happily and fails on the
	// first save, which is the worst moment to find out.
	scopes := p.Scopes
	if scopes == nil {
		scopes = []string{}
	}

	// Two distinct facts, and a client needs both. protectedVaults is the
	// SERVER's policy -- which vaults carry a marker. requiresStepUpAuth is THIS
	// principal's posture. The plugin warns only where the two agree, so it
	// cannot warn about a protected vault this token is not gated on.
	//
	// A vault whose protection cannot be determined fails the whole listing
	// rather than being quietly omitted, which would read as "not protected".
	visible := p.Visible(all)
	protected := []string{}
	for _, name := range visible {
		is, err := s.reg.Protected(name)
		if err != nil {
			fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
			return
		}
		if is {
			protected = append(protected, name)
		}
	}
	posture := p.RequiresStepUpAuth
	if posture == nil {
		posture = []string{}
	}

	writeJSON(w, map[string]any{
		"vaults":             visible,
		"canCreate":          p.CanCreateVaults,
		"scopes":             scopes,
		"label":              p.Label,
		"protectedVaults":    protected,
		"requiresStepUpAuth": posture,
	})
}

// createVault makes a directory inside $ROOT/vaults and nothing else.
//
// This reverses an earlier decision, deliberately. The objection was that "an
// API that can create state outside what it was configured with is a much
// larger surface than one that reads and writes notes" -- and that reasoning
// aimed wider than what this is: a POST that can only create a directory INSIDE
// the configured root cannot create state outside it, which is the property the
// objection protected. The residual risk, a leaked token creating vaults
// endlessly, is bounded by the disk floor shipped in v0.4.0 and by
// ARCHIVIST_MAX_VAULTS.
func (s *Server) createVault(w http.ResponseWriter, r *http.Request) {
	p, _ := r.Context().Value(ctxPrincipal).(auth.Principal)
	// Two gates, and both are meant. CanCreateVaults is the capability; write is
	// the verb, because creating a vault puts directories on disk and a
	// read-only token has no business doing that whatever its capability says.
	if !p.Can(auth.ScopeWrite) {
		fail(w, http.StatusForbidden, protocol.CodeForbidden,
			"this token does not hold the write scope")
		return
	}
	if !p.CanCreateVaults {
		fail(w, http.StatusForbidden, protocol.CodeForbidden, "this token may not create vaults")
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, "malformed body: "+err.Error())
		return
	}
	if err := s.reg.Create(req.Name); err != nil {
		if vaults.ValidName(req.Name) != nil {
			fail(w, http.StatusBadRequest, protocol.CodeMalformed, err.Error())
			return
		}
		// A collision or the limit: the request was well-formed and is refused.
		fail(w, http.StatusConflict, protocol.CodeMalformed, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"vault": req.Name})
}
