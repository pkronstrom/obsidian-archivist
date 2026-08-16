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
	"strconv"
	"strings"
	"time"

	"github.com/pkronstrom/obsidian-archivist/internal/reconcile"
	"github.com/pkronstrom/obsidian-archivist/internal/repo"
	"github.com/pkronstrom/obsidian-archivist/internal/vault"
	"github.com/pkronstrom/obsidian-archivist/internal/version"
)

// maxUpload bounds a single content upload. Generous for an attachment,
// bounded enough that a broken client cannot exhaust memory.
const maxUpload = 512 << 20 // 512 MiB

type Server struct {
	rc    *reconcile.Reconciler
	repo  *repo.Repo
	token string
}

func New(rc *reconcile.Reconciler, r *repo.Repo, token string) http.Handler {
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
			httpError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- responses -------------------------------------------------------------

type headResponse struct {
	Head string `json:"head"`
}

type snapshotResponse struct {
	Head  string                `json:"head"`
	Files map[string]repo.Entry `json:"files"`
}

type changesResponse struct {
	Head    string        `json:"head"`
	Entries []repo.Change `json:"entries"`
}

type haveRequest struct {
	Hashes []string `json:"hashes"`
}

type haveResponse struct {
	Missing []string `json:"missing"`
}

type pushRequest struct {
	Base    string             `json:"base"`
	Device  string             `json:"device"`
	Changes []reconcile.Change `json:"changes"`
}

type pushResponse struct {
	Head    string             `json:"head"`
	Results []reconcile.Result `json:"results"`
}

// ---- handlers --------------------------------------------------------------

func (s *Server) head(w http.ResponseWriter, r *http.Request) {
	h, err := s.repo.Head()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, headResponse{Head: h})
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	h, err := s.repo.Head()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	files, err := s.repo.Snapshot(h)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, snapshotResponse{Head: h, Files: files})
}

func (s *Server) changes(w http.ResponseWriter, r *http.Request) {
	since := r.URL.Query().Get("since")
	h, err := s.repo.Head()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	entries, err := s.repo.Changes(since, h)
	if err != nil {
		// The cursor is from another repository, or predates a history
		// rewrite. 409 tells the client to re-bootstrap from /snapshot; a 500
		// would suggest the server was at fault and invite a retry loop.
		if repo.IsUnknownBase(err) {
			httpError(w, http.StatusConflict, "unknown base; re-bootstrap from /v1/snapshot")
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		entries = []repo.Change{}
	}
	writeJSON(w, changesResponse{Head: h, Entries: entries})
}

func (s *Server) have(w http.ResponseWriter, r *http.Request) {
	var req haveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed body: "+err.Error())
		return
	}
	missing := []string{}
	for _, h := range req.Hashes {
		if !s.repo.HasBlob(h) {
			missing = append(missing, h)
		}
	}
	writeJSON(w, haveResponse{Missing: missing})
}

func (s *Server) putContent(w http.ResponseWriter, r *http.Request) {
	claimed := r.PathValue("hash")
	body, err := io.ReadAll(io.LimitReader(r.Body, maxUpload))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Verify the address before storing. A store that trusts the client's hash
	// is not content-addressed, and every downstream guarantee -- dedup,
	// idempotency, the merge base -- rests on the address being true.
	actual, err := repo.HashContent(body)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !strings.EqualFold(actual, claimed) {
		httpError(w, http.StatusBadRequest,
			"content hash mismatch: claimed "+claimed+", actual "+actual)
		return
	}
	if _, err := s.repo.WriteBlob(body); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"hash": actual})
}

func (s *Server) getContent(w http.ResponseWriter, r *http.Request) {
	content, err := s.repo.ReadBlob(r.PathValue("hash"))
	if err != nil {
		httpError(w, http.StatusNotFound, "no such object")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(content)
}

func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	var req pushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed body: "+err.Error())
		return
	}
	head, results, err := s.rc.Push(req.Base, req.Device, req.Changes)
	if err != nil {
		switch {
		case repo.IsUnknownBase(err):
			httpError(w, http.StatusConflict, "unknown base; re-bootstrap from /v1/snapshot")
		case isClientFault(err):
			httpError(w, http.StatusBadRequest, err.Error())
		default:
			httpError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, pushResponse{Head: head, Results: results})
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
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
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
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		httpError(w, http.StatusBadRequest, "path is required")
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
		httpError(w, http.StatusInternalServerError, err.Error())
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
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	resolved, err := s.repo.Resolve(rev)
	if err != nil {
		httpError(w, http.StatusNotFound, "unknown revision")
		return
	}
	content, err := s.repo.ReadAt(resolved, p)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such path at that revision")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Vaultsync-Revision", resolved)
	w.Write(content)
}

// check compares the working tree against HEAD.
func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	rep, err := s.repo.Check()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
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
		"service":  "archivist",
		"version":  version.Version,
		"protocol": version.Protocol,
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
		"status": "ok", "version": version.Version, "protocol": version.Protocol,
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

	tmp, err := os.CreateTemp("", "archivist-export-*.tar")
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if err := s.rc.Freeze(func() error { return s.repo.Archive(tmp, compress) }); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	fi, err := tmp.Stat()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
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

// isClientFault distinguishes a bad request from a server failure. A path
// outside the vault, or content that was never uploaded, are both the client's
// mistakes and must not be reported as 500 -- that would hide a broken client
// behind an apparent server bug.
func isClientFault(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, vault.ErrInvalidPath) {
		return true
	}
	return strings.Contains(err.Error(), "was never uploaded")
}

// ---- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent; all we can do is stop.
		return
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
