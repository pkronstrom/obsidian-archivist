package relay

import (
	"context"
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
	pool *Pool
	// bg is the relay's OWN client, used only where there is no caller: the
	// healthz upstream probe. Never used on a caller's behalf -- every request
	// is served with the credential its caller presented.
	bg    *client.Client
	log   *slog.Logger
	mcp   http.Handler
	hooks *Webhooks
}

// ctxKey carries the per-caller client from the middleware to the handlers.
type ctxKey int

const ctxClient ctxKey = iota

func NewHandler(pool *Pool, bg *client.Client, log *slog.Logger, mcpHandler http.Handler, hooks *Webhooks) http.Handler {
	h := &Handler{pool: pool, bg: bg, log: log, mcp: mcpHandler, hooks: hooks}

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

// authenticate lifts the caller's bearer and attaches a client carrying it.
//
// It deliberately does NOT check whether the token is valid: only the server
// knows, and asking would mean the relay holding a table of credentials --
// which is the thing this design removes. The relay forwards, and passes the
// server's verdict back; relayError already preserves the upstream status.
//
// An empty bearer is the single case it can answer alone, because forwarding it
// would present "Bearer " upstream -- the exact shape that looks like
// authentication and is not.
func (h *Handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="archivist-relay"`)
			writeError(w, http.StatusUnauthorized, protocol.CodeUnauthorized, "unauthorized")
			return
		}
		ctx := context.WithValue(r.Context(), ctxClient, h.pool.For(token))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// vaultFor picks the client for one request: the CALLER's, addressing the vault
// they named, or the single vault their own token opens.
//
//	curl -T note.md ".../file/notes/idea.md?vault=work"
//
// ?vault= stops being a bypass here. It is still caller-controlled, but the
// credential is now theirs too, so naming a vault they cannot open earns a 403
// from the server rather than reaching it on the relay's authority.
func (h *Handler) vaultFor(r *http.Request) (*client.Client, error) {
	c, _ := r.Context().Value(ctxClient).(*client.Client)
	if c == nil {
		return nil, errors.New("no credential on this request")
	}
	if name := r.URL.Query().Get("vault"); name != "" {
		return c.WithVault(name), nil
	}
	name, err := h.pool.DefaultVault(r.Context(), c.Token())
	if err != nil {
		return nil, err
	}
	return c.WithVault(name), nil
}

// vaultForError reports a failure to pick the caller's vault.
//
// Resolving the default asks the SERVER what this token opens, so the failure
// is often the server's verdict on the token itself. Flattening that to 400
// told a caller with a dead credential their request was malformed, which sent
// them looking in the wrong place -- observed on the first deploy of
// pass-through. An upstream answer is forwarded with its own status; only a
// genuine ambiguity ("your token opens three vaults") is a 400.
func (h *Handler) vaultForError(w http.ResponseWriter, err error) {
	var e *client.Error
	if errors.As(err, &e) {
		h.relayError(w, err)
		return
	}
	writeError(w, http.StatusBadRequest, protocol.CodeMalformed, err.Error())
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
	c, err := h.vaultFor(r)
	if err != nil {
		h.vaultForError(w, err)
		return
	}
	p := r.PathValue("path")
	if err := checkPath(p); err != nil {
		writeError(w, http.StatusBadRequest, protocol.CodeInvalidPath, err.Error())
		return
	}
	body, base, err := c.ReadForEdit(r.Context(), p)
	if err != nil {
		h.relayError(w, err)
		return
	}
	// The ETag is the vault revision this content was read at. Send it back as
	// If-Match on the write and a concurrent edit is merged instead of lost.
	w.Header().Set("ETag", `"`+base+`"`)
	w.Header().Set("Content-Type", contentType(p))
	w.Write(body)
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request) {
	c, err := h.vaultFor(r)
	if err != nil {
		h.vaultForError(w, err)
		return
	}
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

	base, err := ifMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, protocol.CodeMalformed, err.Error())
		return
	}
	res, err := c.WriteAt(r.Context(), p, body, base)
	if err != nil {
		h.relayError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus(res))
	json.NewEncoder(w).Encode(res)
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	c, err := h.vaultFor(r)
	if err != nil {
		h.vaultForError(w, err)
		return
	}
	p := r.PathValue("path")
	if err := checkPath(p); err != nil {
		writeError(w, http.StatusBadRequest, protocol.CodeInvalidPath, err.Error())
		return
	}
	base, err := ifMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, protocol.CodeMalformed, err.Error())
		return
	}
	res, err := c.DeleteAt(r.Context(), p, base)
	if err != nil {
		h.relayError(w, err)
		return
	}
	// A refused delete used to come back 200: a caller checking only the status
	// would report the note as removed while it is still in the vault.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus(res))
	json.NewEncoder(w).Encode(res)
}

// ifMatch pulls the vault revision out of If-Match. Absent means a blind
// overwrite, which is the right default for `curl -T newfile.md` but wrong for
// anything that read the note first -- hence the ETag on GET.
//
// Quotes are optional here: the header is specified with them, every real
// client sends them, and a hand-written curl call usually does not.
func ifMatch(r *http.Request) (string, error) {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if v == "" {
		return "", nil
	}
	// RFC 9110's wildcard means "if the resource exists", which is not a
	// revision and cannot be honoured as one. Silently treating it as an absent
	// base turns a request that ASKED to be conditional into a blind overwrite
	// -- the exact failure this header exists to prevent -- so refuse instead.
	if v == "*" {
		return "", errors.New(
			"If-Match: * is not supported; send the ETag returned by GET")
	}
	return strings.Trim(v, `"`), nil
}

// httpStatus maps a push outcome onto a status code. A caller that reads only
// the status line still learns that its change did not land as sent -- and a
// refused delete must not look like a successful one.
func httpStatus(res protocol.Result) int {
	switch res.Status {
	case protocol.StatusConflict:
		return http.StatusConflict
	case protocol.StatusRefused:
		return http.StatusConflict
	default:
		return http.StatusOK
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	c, err := h.vaultFor(r)
	if err != nil {
		h.vaultForError(w, err)
		return
	}
	files, err := c.List(r.Context(), r.URL.Query().Get("prefix"))
	if err != nil {
		h.relayError(w, err)
		return
	}
	writeJSON(w, map[string]any{"count": len(files), "files": files})
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	endpoints := []protocol.Endpoint{
		{Method: "GET", Path: "/file/{path}", Does: "read a note; the ETag is its revision"},
		{Method: "PUT", Path: "/file/{path}", Does: "create or replace a note; send If-Match: <ETag> to merge instead of overwrite; 409 if it conflicted"},
		{Method: "DELETE", Path: "/file/{path}", Does: "delete a note; If-Match is honoured"},
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
	// healthz is unauthenticated, so there is no caller credential to probe
	// with. It uses the relay's own background token -- the same one the
	// compatibility checker and the webhook stream use. Without one configured
	// the relay can still report its own liveness, which is what a container
	// healthcheck is actually asking.
	if h.bg == nil {
		out["upstream"] = "not probed: the relay has no background credential"
		writeJSON(w, out)
		return
	}
	if up, err := h.bg.Ping(r.Context()); err != nil {
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
		delivered, failed, dropped := h.hooks.Stats()
		out["webhooks"] = map[string]int{
			"delivered": delivered, "failed": failed, "dropped": dropped}
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
