// Package api is the HTTP surface: seven routes behind a bearer token.
//
// Content is addressed by git object hash, which gives deduplication,
// resumability and idempotent uploads without any of them being designed.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/internal/version"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// maxUpload bounds a single content upload. Generous for an attachment,
// bounded enough that a broken client cannot exhaust memory.

type Server struct {
	rc    *reconcile.Reconciler
	repo  *repo.Repo
	token string
}

// New panics on an empty token rather than serving an open vault. With token ==
// "" the expected header is exactly "Bearer ", which any client can send -- an
// authentication bypass that looks like working authentication. The server
// binary rejects this in config, but nothing stops another caller (the relay,
// a test, a future embedding) from constructing one directly.
func New(rc *reconcile.Reconciler, r *repo.Repo, token string) http.Handler {
	if token == "" {
		panic("api.New: empty token would accept any request presenting 'Bearer '")
	}
	s := &Server{rc: rc, repo: r, token: token}

	mux := http.NewServeMux()
	// Method patterns (Go 1.22+) give 405 rather than 404 on a method mismatch,
	// with no router dependency.
	mux.HandleFunc("GET /v1", s.index)
	for _, rt := range s.routes() {
		if rt.handle == nil || rt.Path == "/healthz" {
			continue
		}
		mux.HandleFunc(rt.Method+" "+rt.Path, rt.handle)
	}

	// healthz sits OUTSIDE the auth middleware, deliberately and alone.
	outer := http.NewServeMux()
	outer.HandleFunc("GET /healthz", s.healthz)
	outer.Handle("/", s.authenticate(mux))
	return outer
}

// authenticate compares in constant time, so a wrong token cannot be recovered
// by timing the response.
func (s *Server) authenticate(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="archivist"`)
			fail(w, http.StatusUnauthorized, protocol.CodeUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Wire types live in the protocol package.

// ---- handlers --------------------------------------------------------------

func (s *Server) head(w http.ResponseWriter, r *http.Request) {
	h, err := s.repo.Head()
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, protocol.HeadResponse{Head: h})
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	h, err := s.repo.Head()
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	files, err := s.repo.Snapshot(h)
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, protocol.SnapshotResponse{Head: h, Files: files})
}

func (s *Server) changes(w http.ResponseWriter, r *http.Request) {
	since := r.URL.Query().Get("since")
	h, err := s.repo.Head()
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	entries, err := s.repo.Changes(since, h)
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

func (s *Server) have(w http.ResponseWriter, r *http.Request) {
	var req protocol.HaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, "malformed body: "+err.Error())
		return
	}
	missing := []string{}
	for _, h := range req.Hashes {
		if !s.repo.HasBlob(h) {
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

func (s *Server) putContent(w http.ResponseWriter, r *http.Request) {
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
	if _, err := s.repo.WriteBlob(body); err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, map[string]string{"hash": actual})
}

func (s *Server) getContent(w http.ResponseWriter, r *http.Request) {
	content, err := s.repo.ReadBlob(r.PathValue("hash"))
	if err != nil {
		fail(w, http.StatusNotFound, protocol.CodeNotFound, "no such object")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(content)
}

func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	var req protocol.PushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, "malformed body: "+err.Error())
		return
	}
	head, results, err := s.rc.Push(req.Base, req.Device, req.Changes)
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
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
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
	if head, err := s.repo.Head(); err == nil && head != "" {
		writeEvent(w, map[string]any{"head": head, "count": 0, "changes": []any{}})
	}
	flusher.Flush()

	ch, stop := s.rc.Subscribe()
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
func (s *Server) wait(w http.ResponseWriter, r *http.Request) {
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

	head, err := s.repo.Head()
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
	ch, stop := s.rc.Subscribe()
	defer stop()

	if head, err := s.repo.Head(); err == nil && head != since {
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

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
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
	revs, err := s.repo.History(path, limit)
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	writeJSON(w, map[string]any{"path": path, "revisions": revs})
}

// at reads a file as it was at a revision, WITHOUT touching the working tree.
// Inspecting is much more common than restoring, and conflating the two makes
// looking at an old version a destructive act.
func (s *Server) at(w http.ResponseWriter, r *http.Request) {
	rev, p := r.PathValue("rev"), r.PathValue("path")
	if err := vault.ValidPath(p); err != nil {
		fail(w, http.StatusBadRequest, protocol.CodeMalformed, err.Error())
		return
	}
	resolved, err := s.repo.Resolve(rev)
	if err != nil {
		fail(w, http.StatusNotFound, protocol.CodeNotFound, "unknown revision")
		return
	}
	content, err := s.repo.ReadAt(resolved, p)
	if err != nil {
		fail(w, http.StatusNotFound, protocol.CodeNotFound, "no such path at that revision")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Archivist-Revision", resolved)
	w.Write(content)
}

// check compares the working tree against HEAD.
func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	rep, err := s.repo.Check()
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
	handle http.HandlerFunc
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
		{"GET", "/healthz", "liveness, no auth", s.healthz},
	}
}

// index describes the API to whatever is calling it. Agents and humans both
// benefit, and it costs one struct rather than a specification to maintain.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
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
		"vault":    filepath.Base(s.repo.WorkTree()),
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
func (s *Server) export(w http.ResponseWriter, r *http.Request) {
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
	tmp, err := os.CreateTemp(s.repo.GitDir(), "archivist-export-*.tar")
	if err != nil {
		fail(w, http.StatusInternalServerError, protocol.CodeInternal, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if err := s.rc.Freeze(func() error { return s.repo.Archive(tmp, compress) }); err != nil {
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
