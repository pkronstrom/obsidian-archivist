package relay

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/pkronstrom/obsidian-archivist/internal/client"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// maxBody bounds a single upload through the relay. Lower than the server's
// limit on purpose: this surface is for scripts and automations writing notes,
// and anything approaching the server's ceiling should be talking to the server.
const maxBody = 32 << 20

// Handler is the relay's own HTTP surface: a plain file API for scripts and
// other containers, plus MCP if it is enabled.
//
// This exists because writing one note through the sync protocol takes four
// calls and a git-hash implementation. Here it is `curl -T`. The relay does the
// sequence internally, which is also why there is deliberately no equivalent
// endpoint on the server: one write path lives there, in one place, and this is
// ergonomics rather than a second way in.
type Handler struct {
	client *client.Client
	token  string
	log    *slog.Logger
	mcp    http.Handler
	hooks  *Webhooks
}

func NewHandler(c *client.Client, token string, log *slog.Logger, mcpHandler http.Handler, hooks *Webhooks) http.Handler {
	h := &Handler{client: c, token: token, log: log, mcp: mcpHandler, hooks: hooks}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /file/{path...}", h.read)
	mux.HandleFunc("PUT /file/{path...}", h.write)
	mux.HandleFunc("DELETE /file/{path...}", h.remove)
	mux.HandleFunc("GET /list", h.list)
	// "GET /{$}" matches ONLY "/", not everything under it. A bare "GET /"
	// is a catch-all that conflicts with "/mcp" -- ServeMux panics at
	// registration, so with MCP enabled the process died on startup. The tests
	// never saw it because they all passed a nil MCP handler, i.e. they never
	// built the configuration that actually ships.
	mux.HandleFunc("GET /{$}", h.index)
	if h.mcp != nil {
		// The MCP endpoint sits behind the same auth as everything else.
		// Both forms: clients differ on whether they append a slash.
		mux.Handle("/mcp", h.mcp)
		mux.Handle("/mcp/", h.mcp)
	} else {
		mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, protocol.CodeNotFound,
				"MCP is disabled on this relay; start it without -mcp=false")
		})
	}

	outer := http.NewServeMux()
	outer.HandleFunc("GET /healthz", h.healthz) // unauthenticated, like the server's
	outer.Handle("/", h.authenticate(mux))
	return outer
}

// authenticate uses the RELAY's token, which is deliberately separate from the
// server's. The relay may be reachable by callers that should not be handed a
// credential for the vault itself, and rotating one should not disturb the
// other.
func (h *Handler) authenticate(next http.Handler) http.Handler {
	want := "Bearer " + h.token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtleCompare(r.Header.Get("Authorization"), want) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="archivist-relay"`)
		writeError(w, http.StatusUnauthorized, protocol.CodeUnauthorized, "unauthorized")
	})
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if err := checkPath(p); err != nil {
		writeError(w, http.StatusBadRequest, protocol.CodeInvalidPath, err.Error())
		return
	}
	body, err := h.client.Read(r.Context(), p)
	if err != nil {
		h.relayError(w, err)
		return
	}
	w.Header().Set("Content-Type", contentType(p))
	w.Write(body)
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if err := checkPath(p); err != nil {
		writeError(w, http.StatusBadRequest, protocol.CodeInvalidPath, err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, protocol.CodeMalformed, err.Error())
		return
	}
	if int64(len(body)) > maxBody {
		writeError(w, http.StatusRequestEntityTooLarge, protocol.CodeTooLarge,
			"body exceeds the relay's limit; write large attachments to the server directly")
		return
	}

	res, err := h.client.Write(r.Context(), p, body)
	if err != nil {
		h.relayError(w, err)
		return
	}
	// 200 for a clean write, 409 for a conflict: a caller that ignores the body
	// still learns something went sideways from the status alone.
	status := http.StatusOK
	if res.Status == protocol.StatusConflict {
		status = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(res)
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if err := checkPath(p); err != nil {
		writeError(w, http.StatusBadRequest, protocol.CodeInvalidPath, err.Error())
		return
	}
	res, err := h.client.Delete(r.Context(), p)
	if err != nil {
		h.relayError(w, err)
		return
	}
	writeJSON(w, res)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	files, err := h.client.List(r.Context(), r.URL.Query().Get("prefix"))
	if err != nil {
		h.relayError(w, err)
		return
	}
	writeJSON(w, map[string]any{"count": len(files), "files": files})
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	endpoints := []protocol.Endpoint{
		{Method: "GET", Path: "/file/{path}", Does: "read a note"},
		{Method: "PUT", Path: "/file/{path}", Does: "create or replace a note; 409 if it conflicted"},
		{Method: "DELETE", Path: "/file/{path}", Does: "delete a note"},
		{Method: "GET", Path: "/list", Does: "list files, ?prefix= to scope"},
		{Method: "GET", Path: "/healthz", Does: "liveness and upstream reachability, no auth"},
	}
	if h.mcp != nil {
		endpoints = append(endpoints, protocol.Endpoint{
			Method: "POST", Path: "/mcp", Does: "MCP over streamable HTTP",
		})
	}
	writeJSON(w, map[string]any{
		"service":  "archivist-relay",
		"protocol": protocol.Version,
		"notes": []string{
			"All routes except /healthz need Authorization: Bearer <relay token>.",
			"A PUT may return status 'merged' or 'conflict': the stored content is then " +
				"not what you sent. Check the body, not just the HTTP status.",
			"This is ergonomics over the server's sync protocol; the server remains the authority.",
		},
		"endpoints": endpoints,
	})
}

// healthz reports the relay AND whether it can reach the server, because a
// relay that is up but cut off from the vault is not usefully healthy.
func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"status": "ok", "protocol": protocol.Version}
	if up, err := h.client.Ping(r.Context()); err != nil {
		out["status"] = "degraded"
		out["upstream"] = "unreachable: " + err.Error()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(out)
		return
	} else {
		out["upstream"] = map[string]any{"version": up.Version, "protocol": up.Protocol}
	}
	if h.hooks != nil {
		delivered, failed := h.hooks.Stats()
		out["webhooks"] = map[string]int{"delivered": delivered, "failed": failed}
	}
	writeJSON(w, out)
}

// relayError forwards the server's own code and an appropriate status, so a
// caller sees why something failed rather than a blanket 502.
func (h *Handler) relayError(w http.ResponseWriter, err error) {
	var e *client.Error
	if errors.As(err, &e) {
		status := e.Status
		if status < 400 {
			status = http.StatusBadGateway
		}
		writeError(w, status, e.Code, e.Message)
		return
	}
	h.log.Error("relay: upstream call failed", "err", err)
	writeError(w, http.StatusBadGateway, protocol.CodeInternal, err.Error())
}

// checkPath rejects anything that is not a plain relative path. The server
// validates too, but a relay that forwards rubbish upstream is a bad neighbour
// and produces confusing errors far from their cause.
func checkPath(p string) error {
	if p == "" {
		return errors.New("path is required")
	}
	if strings.HasPrefix(p, "/") {
		return errors.New("path must be relative")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("path must not contain empty or dot segments: " + p)
		}
	}
	return nil
}

func contentType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".txt":
		return "text/markdown; charset=utf-8"
	case ".json":
		return "application/json"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".pdf":
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(protocol.ErrorResponse{
		Error: protocol.Error{Code: code, Message: msg},
	})
}

// subtleCompare is a constant-time equality check, so a wrong token cannot be
// recovered by timing the response.
func subtleCompare(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
