// Package client is a Go client for an Archivist server.
//
// It is the second implementation of the wire contract, after the TypeScript
// plugin, and that is most of its value: everything the two must agree on lives
// in protocol/, so a disagreement is a compile error rather than a bug found in
// production six weeks later.
//
// It deliberately imports nothing beyond the standard library and protocol/.
// No go-git, no filesystem watching: a gateway that carries a git
// implementation it never calls is three times the size for nothing.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkronstrom/obsidian-archivist/protocol"
)

// Error carries the server's stable code. Callers branch on Code via IsCode;
// Message is for humans and will be reworded.
type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string {
	return fmt.Sprintf("archivist: %s (%s, HTTP %d)", e.Message, e.Code, e.Status)
}

// IsCode reports whether err is a server error with the given stable code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

type Client struct {
	base   string
	vault  string
	token  string
	device string
	http   *http.Client

	// compat caches the last compatibility verdict. A startup-only check is not
	// enough: the relay is explicitly allowed to start while the server is down,
	// and the server can be redeployed under a running relay. Either way the
	// relay would otherwise keep writing to a server whose protocol it has never
	// agreed with.
	//
	// A POINTER, so WithVault's copy shares it rather than duplicating a mutex.
	// Sharing is also the correct semantics: protocol compatibility is a
	// property of the SERVER, and every vault-scoped copy talks to the same one,
	// so one check answers for all of them. (`go vet` catches the copy-a-lock
	// form; the sharing is the reason to prefer this shape anyway.)
	compat *compatCache
}

type compatCache struct {
	mu  sync.Mutex
	err error
	set bool
}

// New returns a client. device names this caller in commit messages and
// conflict filenames, so make it recognisable.
func New(baseURL, token, device string) *Client {
	return &Client{
		base:   strings.TrimRight(baseURL, "/"),
		token:  token,
		device: device,
		// No global timeout: the events stream is long-lived by design, and a
		// client-wide deadline would sever it. Per-call deadlines come from the
		// context the caller passes.
		http:   &http.Client{},
		compat: &compatCache{},
	}
}

func (c *Client) Device() string { return c.device }

// WithVault returns a copy of the client addressing a different vault.
//
// A copy rather than a mutation, so the relay can hold one client per vault
// over one connection pool without any of them racing on a shared field.
func (c *Client) WithVault(name string) *Client {
	cp := *c
	cp.vault = name
	return &cp
}

func (c *Client) Vault() string { return c.vault }

// qualify is the ONE place a request path is built. Every call goes through
// do(), and do() calls this -- which is what made changing the addressing
// scheme a one-line change per layer rather than the "21 call sites" it looks
// like from the outside.
//
// url.PathEscape, not raw concatenation: a vault called "My Own Vault" is legal
// and must arrive as My%20Own%20Vault.
func (c *Client) qualify(path string) string {
	if c.vault == "" {
		return c.base + path
	}
	return c.base + "/" + url.PathEscape(c.vault) + path
}

// atRoot is this client with no vault, for the SERVER-ROOT routes: /healthz and
// /v1/vaults. Both exist above the vault namespace -- a healthcheck has no idea
// which vaults there are, and a picker needs the list before it can choose one
// -- so qualifying them asks for a path that does not exist.
//
// A copy with an empty vault rather than a flag on do(), so there is still
// exactly one way a path gets qualified and no branch inside it to get
// backwards. The compat cache is a pointer, so the copy shares it.
func (c *Client) atRoot() *Client {
	unqualified := *c
	unqualified.vault = ""
	return &unqualified
}

func (c *Client) doRoot(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	return c.atRoot().do(ctx, method, path, body, contentType)
}

// ListVaults asks what this token opens. A SERVER-root route: a caller needs it
// before it knows which vault to address, so it cannot itself be qualified.
func (c *Client) ListVaults(ctx context.Context) ([]string, error) {
	resp, err := c.doRoot(ctx, "GET", "/v1/vaults", nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Vaults []string `json:"vaults"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Vaults, nil
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.qualify(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp, nil
}

// decodeError prefers the server's envelope over the status code. A status
// alone is ambiguous -- 409 used to be assumed to mean unknown-base, which held
// only while exactly one thing produced a 409.
func decodeError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	var env protocol.ErrorResponse
	if json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
		return &Error{Code: env.Error.Code, Message: env.Error.Message, Status: resp.StatusCode}
	}

	// No envelope: an older server, or something in front of it.
	code := protocol.CodeInternal
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		code = protocol.CodeUnauthorized
	case http.StatusNotFound:
		code = protocol.CodeNotFound
	case http.StatusConflict:
		code = protocol.CodeUnknownBase
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return &Error{
		Code:    code,
		Message: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, msg),
		Status:  resp.StatusCode,
	}
}

func getJSON[T any](ctx context.Context, c *Client, path string) (T, error) {
	var out T
	resp, err := c.do(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}

func postJSON[T any](ctx context.Context, c *Client, path string, in any) (T, error) {
	var out T
	b, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	resp, err := c.do(ctx, http.MethodPost, path, bytes.NewReader(b), "application/json")
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}

// ---- the endpoints ---------------------------------------------------------

func (c *Client) Index(ctx context.Context) (protocol.IndexResponse, error) {
	return getJSON[protocol.IndexResponse](ctx, c, "/v1")
}

// CheckCompatible refuses a server speaking a protocol this client does not.
//
// It compares `protocol`, never `version`: the build number changes every
// release and would make every upgrade look like a breaking change. A newer
// server protocol is refused because we cannot know what changed; an older one
// is refused because we may send fields it will ignore silently.
func (c *Client) CheckCompatible(ctx context.Context) error {
	idx, err := c.Index(ctx)
	if err != nil {
		// Unreachable is not incompatible. Leave the verdict unset so the next
		// call retries rather than caching a network blip as a protocol verdict.
		return err
	}
	var verdict error
	if idx.Protocol != protocol.Version {
		verdict = fmt.Errorf(
			"archivist: server speaks protocol %d, this client speaks %d (server build %s)",
			idx.Protocol, protocol.Version, idx.Version)
	}
	c.compat.mu.Lock()
	c.compat.err, c.compat.set = verdict, true
	c.compat.mu.Unlock()
	return verdict
}

// ensureCompatible gates mutations. If the protocol has never been confirmed --
// the server was down at startup, say -- it is confirmed now, once. If it was
// confirmed INCOMPATIBLE, the mutation is refused rather than sent to a server
// that may interpret the fields differently.
//
// A server that is merely unreachable does not block anything: the request goes
// out and fails on its own terms, with its own error.
func (c *Client) ensureCompatible(ctx context.Context) error {
	c.compat.mu.Lock()
	set, verdict := c.compat.set, c.compat.err
	c.compat.mu.Unlock()
	if set {
		return verdict
	}
	if err := c.CheckCompatible(ctx); err != nil {
		c.compat.mu.Lock()
		set, verdict = c.compat.set, c.compat.err
		c.compat.mu.Unlock()
		if set {
			return verdict // a real protocol mismatch
		}
		return nil // unreachable; let the actual call report it
	}
	return nil
}

func (c *Client) Head(ctx context.Context) (string, error) {
	r, err := getJSON[protocol.HeadResponse](ctx, c, "/v1/head")
	return r.Head, err
}

func (c *Client) Snapshot(ctx context.Context) (protocol.SnapshotResponse, error) {
	return getJSON[protocol.SnapshotResponse](ctx, c, "/v1/snapshot")
}

func (c *Client) Changes(ctx context.Context, since string) (protocol.ChangesResponse, error) {
	return getJSON[protocol.ChangesResponse](ctx, c, "/v1/changes?since="+url.QueryEscape(since))
}

// Missing returns the subset the server does not already hold.
func (c *Client) Missing(ctx context.Context, hashes []string) ([]string, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	r, err := postJSON[protocol.HaveResponse](ctx, c, "/v1/have", protocol.HaveRequest{Hashes: hashes})
	return r.Missing, err
}

func (c *Client) PutContent(ctx context.Context, hash string, content []byte) error {
	if int64(len(content)) > protocol.MaxUploadBytes {
		return &Error{
			Code:    protocol.CodeTooLarge,
			Message: fmt.Sprintf("content is %d bytes, limit is %d", len(content), protocol.MaxUploadBytes),
			Status:  http.StatusRequestEntityTooLarge,
		}
	}
	resp, err := c.do(ctx, http.MethodPut, "/v1/content/"+hash,
		bytes.NewReader(content), "application/octet-stream")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) GetContent(ctx context.Context, hash string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/content/"+hash, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Push applies a change set against base.
//
// It is NOT retried on an ambiguous network failure, deliberately. A replayed
// push is not idempotent: a delete already applied comes back "refused", and a
// put that merged once can merge again. Reporting an uncertain outcome honestly
// is better than a retry that quietly does something different.
func (c *Client) Push(ctx context.Context, base string, changes []protocol.Change) (protocol.PushResponse, error) {
	// Gate the only call that changes the vault. Reads against a mismatched
	// protocol are merely wrong; writes are durable.
	if err := c.ensureCompatible(ctx); err != nil {
		return protocol.PushResponse{}, err
	}
	return postJSON[protocol.PushResponse](ctx, c, "/v1/push", protocol.PushRequest{
		Base: base, Device: c.device, Changes: changes,
	})
}

func (c *Client) History(ctx context.Context, path string, limit int) ([]protocol.Revision, error) {
	q := "/v1/history?path=" + url.QueryEscape(path)
	if limit > 0 {
		q += "&limit=" + strconv.Itoa(limit)
	}
	r, err := getJSON[protocol.HistoryResponse](ctx, c, q)
	return r.Revisions, err
}

// ReadAt returns a path's content as it was at a revision, without restoring
// it. "head" or "" means the current version.
func (c *Client) ReadAt(ctx context.Context, rev, path string) ([]byte, error) {
	if rev == "" {
		rev = "head"
	}
	resp, err := c.do(ctx, http.MethodGet, "/v1/at/"+url.PathEscape(rev)+"/"+escapePath(path), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *Client) Check(ctx context.Context) (protocol.CheckResponse, error) {
	return getJSON[protocol.CheckResponse](ctx, c, "/v1/check")
}

// escapePath escapes each segment but keeps the slashes: the route is a
// wildcard, so an encoded slash would not match it.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// ---- convenience -----------------------------------------------------------

// Read returns a path's current content.
func (c *Client) Read(ctx context.Context, path string) ([]byte, error) {
	return c.ReadAt(ctx, "head", path)
}

// ReadForEdit reads a note AND the revision it was read at, for a caller that
// intends to write it back.
//
// The revision is taken first and the content is read AT that revision, so the
// pair is a consistent snapshot: reading content and then asking for head could
// return a revision newer than the bytes in hand, which is the same lost-update
// bug in a subtler form.
func (c *Client) ReadForEdit(ctx context.Context, path string) (content []byte, base string, err error) {
	base, err = c.Head(ctx)
	if err != nil {
		return nil, "", err
	}
	content, err = c.ReadAt(ctx, base, path)
	if err != nil {
		return nil, "", err
	}
	return content, base, nil
}

// WriteAt stores content as an edit derived from `base`.
//
// The base is what makes the system's merge machinery work, and it MUST be the
// revision the caller's content was derived from -- not the current head.
// Pushing against current head tells the server "nothing has changed since I
// looked", so a concurrent edit is fast-forwarded over and silently lost
// instead of merged. Passing an empty base is a blind overwrite; see Write.
func (c *Client) WriteAt(ctx context.Context, path string, content []byte, base string) (protocol.Result, error) {
	var zero protocol.Result

	if base == "" {
		var err error
		if base, err = c.Head(ctx); err != nil {
			return zero, err
		}
	}
	hash := protocol.HashContent(content)

	missing, err := c.Missing(ctx, []string{hash})
	if err != nil {
		return zero, err
	}
	if len(missing) > 0 {
		if err := c.PutContent(ctx, hash, content); err != nil {
			return zero, err
		}
	}

	resp, err := c.Push(ctx, base, []protocol.Change{{
		Path: path, Op: protocol.OpPut, Hash: hash, Size: int64(len(content)),
	}})
	if err != nil {
		return zero, err
	}
	if len(resp.Results) == 0 {
		return zero, fmt.Errorf("archivist: push of %s returned no result", path)
	}
	return resp.Results[0], nil
}

// Write stores content with NO base, which is a blind overwrite: whatever the
// note currently holds is replaced, and a concurrent edit is lost rather than
// merged.
//
// That is correct only for content that does not derive from a previous read --
// creating a note, or replacing one wholesale. Anything that read the note
// first must use WriteAt with the revision from ReadForEdit.
func (c *Client) Write(ctx context.Context, path string, content []byte) (protocol.Result, error) {
	return c.WriteAt(ctx, path, content, "")
}

// Delete removes a path. It cannot be done without a base: a caller with no
// base has no idea what exists, so the server refuses such deletions outright.
func (c *Client) Delete(ctx context.Context, path string) (protocol.Result, error) {
	return c.DeleteAt(ctx, path, "")
}

// DeleteAt removes a path as of `base`. As with WriteAt, the base is what lets
// the server notice that the note changed after the caller decided to delete
// it. An empty base means "delete whatever is there now".
func (c *Client) DeleteAt(ctx context.Context, path, base string) (protocol.Result, error) {
	var zero protocol.Result
	if base == "" {
		var err error
		if base, err = c.Head(ctx); err != nil {
			return zero, err
		}
	}
	resp, err := c.Push(ctx, base, []protocol.Change{{Path: path, Op: protocol.OpDel}})
	if err != nil {
		return zero, err
	}
	if len(resp.Results) == 0 {
		return zero, fmt.Errorf("archivist: delete of %s returned no result", path)
	}
	return resp.Results[0], nil
}

// List returns the current files, optionally filtered by prefix.
//
// The server has no prefix filter, so this fetches the whole manifest and
// filters here. Fine at vault scale; if it ever is not, the fix belongs on the
// server and this signature does not change.
func (c *Client) List(ctx context.Context, prefix string) (map[string]protocol.Entry, error) {
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if prefix == "" {
		return snap.Files, nil
	}
	out := make(map[string]protocol.Entry)
	for p, e := range snap.Files {
		if strings.HasPrefix(p, prefix) {
			out[p] = e
		}
	}
	return out, nil
}

// ---- events ----------------------------------------------------------------

// Events streams commits until ctx is cancelled or the connection drops.
//
// It does NOT reconnect: that is the caller's policy, because the right
// behaviour differs between a webhook fan-out (reconnect forever) and a
// one-shot tool (fail). Whatever is missed while disconnected is recovered from
// Changes with a cursor -- which is why the stream is allowed to be lossy.
func (c *Client) Events(ctx context.Context) (<-chan protocol.Event, <-chan error, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/events", nil, "")
	if err != nil {
		return nil, nil, err
	}

	events := make(chan protocol.Event)
	errs := make(chan error, 1)

	go func() {
		defer resp.Body.Close()
		defer close(events)
		defer close(errs)

		dec := newSSEDecoder(resp.Body)
		for {
			data, err := dec.next()
			if err != nil {
				if ctx.Err() == nil {
					errs <- err
				}
				return
			}
			var ev protocol.Event
			if json.Unmarshal(data, &ev) != nil {
				continue // a comment, a keep-alive, or something we do not know
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	return events, errs, nil
}

// Ping is a cheap liveness check that does not need a token.
func (c *Client) Ping(ctx context.Context) (protocol.HealthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// c.atRoot(): /healthz is unqualified and unauthenticated, so asking for it
	// under a vault prefix is a 404 that reads as "the server is down".
	return getJSON[protocol.HealthResponse](ctx, c.atRoot(), "/healthz")
}
