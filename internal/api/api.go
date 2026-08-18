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
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/internal/vaults"
	"github.com/pkronstrom/obsidian-archivist/internal/version"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

type Server struct {
	reg    *vaults.Registry
	tokens *auth.Set
}

// ctxKey carries the resolved principal from the middleware to the handlers, so
// a handler cannot forget to check the scope: by the time it runs, the check has
// already happened.
type ctxKey int

const ctxPrincipal ctxKey = iota

// New builds the whole surface: one set of routes per vault, path-qualified,
// from the SAME route table that generates the index. A route still cannot
// exist without being documented, or be documented without existing.
func New(reg *vaults.Registry, tokens *auth.Set) http.Handler {
	s := &Server{reg: reg, tokens: tokens}

	mux := http.NewServeMux()

	// Server-root routes. GET /v1/vaults is what makes a picker possible: a
	// client needs it BEFORE it knows which vault to ask for, which is exactly
	// why the vault cannot be baked into the base URL.
	mux.HandleFunc("GET /v1/vaults", s.listVaults)
	mux.HandleFunc("POST /v1/vaults", s.createVault)

	// Per-vault routes. {vault} is a single path segment, so a name containing
	// a space is addressable percent-encoded -- My%20Own%20Vault -- which the
	// plugin does automatically and a human writing curl must remember.
	mux.HandleFunc("GET /{vault}/v1", s.withVault(s.index))
	for _, rt := range s.routes() {
		if rt.handle == nil || rt.Path == "/healthz" {
			continue
		}
		mux.HandleFunc(rt.Method+" /{vault}"+rt.Path, s.withVault(rt.handle))
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
func (s *Server) withVault(h func(http.ResponseWriter, *http.Request, *vaults.Instance)) http.HandlerFunc {
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
		h(w, r, inst)
	}
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
	head, results, err := inst.Reconciler.Push(req.Base, req.Device, req.Changes)
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

	// Send the current head immediately, so a consumer that just connected can
	// orient itself without waiting for the next commit.
	if head, err := inst.Repo.Head(); err == nil && head != "" {
		writeEvent(w, map[string]any{"head": head, "count": 0, "changes": []any{}})
	}
	flusher.Flush()

	ch, stop := inst.Reconciler.Subscribe()
	defer stop()

	// Idle connections get dropped by proxies; a comment line is a valid SSE
	// keep-alive that consumers ignore.
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeEvent(w, ev)
			flusher.Flush()
		case <-ticker.C:
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

	select {
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
	revs, err := inst.Repo.History(path, limit)
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, map[string]any{"path": path, "revisions": revs})
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
	handle func(http.ResponseWriter, *http.Request, *vaults.Instance)
}

// maxWait caps how long a long-poll may hold a request open. Proxies and
// phone radios drop idle connections well before this, so a longer ceiling
// buys nothing and just hides a stuck client.
const maxWait = 120 * time.Second

func (s *Server) routes() []route {
	return []route{
		{"GET", "/v1", "this list", nil},
		{"GET", "/v1/head", "current commit hash", s.head},
		{"GET", "/v1/snapshot", "every file at head: path, hash, size", s.snapshot},
		{"GET", "/v1/changes", "what changed since ?since=<commit>; 409 if unknown", s.changes},
		{"POST", "/v1/have", "{hashes:[...]} -> {missing:[...]}", s.have},
		{"PUT", "/v1/content/{hash}", "upload content; 400 if it does not hash to {hash}", s.putContent},
		{"GET", "/v1/content/{hash}", "download content by hash", s.getContent},
		{"POST", "/v1/push", "{base,device,changes:[...]} apply a change set", s.push},
		{"GET", "/v1/events", "SSE:one per commit with changed paths, kind, size", s.events},
		{"GET", "/v1/wait", "long-poll: blocks until head moves past ?since=, or ?timeout= elapses", s.wait},
		{"GET", "/v1/history", "?path=&limit= revisions that touched a path", s.history},
		{"GET", "/v1/at/{rev}/{path...}", "a file as it was at a revision; does not restore", s.at},
		{"GET", "/v1/check", "working tree versus head", s.check},
		{"GET", "/v1/export", "consistent archive of history; ?gzip=1 to compress", s.export},
		{"GET", "/healthz", "liveness, no auth", nil},
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
	all, err := s.reg.Names()
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"vaults":    p.Visible(all),
		"canCreate": p.CanCreateVaults,
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
