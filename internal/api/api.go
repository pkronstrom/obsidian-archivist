// Package api is the HTTP surface: seven routes behind a bearer token.
//
// Content is addressed by git object hash, which gives deduplication,
// resumability and idempotent uploads without any of them being designed.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/pkronstrom/vaultsync/internal/reconcile"
	"github.com/pkronstrom/vaultsync/internal/repo"
	"github.com/pkronstrom/vaultsync/internal/vault"
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
	// Method patterns (Go 1.22+) give 405 rather than 404 on a method
	// mismatch, with no router dependency.
	mux.HandleFunc("GET /v1/head", s.head)
	mux.HandleFunc("GET /v1/snapshot", s.snapshot)
	mux.HandleFunc("GET /v1/changes", s.changes)
	mux.HandleFunc("POST /v1/have", s.have)
	mux.HandleFunc("PUT /v1/content/{hash}", s.putContent)
	mux.HandleFunc("GET /v1/content/{hash}", s.getContent)
	mux.HandleFunc("POST /v1/push", s.push)
	mux.HandleFunc("GET /v1/export", s.export)

	return s.authenticate(mux)
}

// authenticate compares in constant time, so a wrong token cannot be recovered
// by timing the response.
func (s *Server) authenticate(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vaultsync"`)
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

	tmp, err := os.CreateTemp("", "vaultsync-export-*.tar")
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

	name, ctype := "vaultsync-git.tar", "application/x-tar"
	if compress {
		name, ctype = "vaultsync-git.tar.gz", "application/gzip"
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
