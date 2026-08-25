package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pkronstrom/obsidian-archivist/internal/client"
)

// maxPooledClients bounds the cache. The key is a bearer token, which is a
// value the CALLER chooses, so an unbounded map is a memory leak anyone who can
// reach the relay could trigger by presenting a new random token in a loop.
//
// The cap is generous relative to reality -- a homelab has a handful of tokens,
// not a thousand -- so eviction should never fire in normal use. When it does,
// clearing the whole map is the right response rather than evicting one entry:
// a full cache means something abnormal is happening, and a client is cheap to
// rebuild.
const maxPooledClients = 256

// Pool hands out one client per caller token.
//
// This is what makes the relay a forwarder rather than a stand-in. Before it,
// every caller reached the server as the relay's single identity, so the scopes
// on their own token were never evaluated by the authority that enforces them.
type Pool struct {
	template *client.Client

	mu       sync.Mutex
	clients  map[string]*client.Client
	servers  map[string]*mcp.Server
	defaults map[string]string
}

func NewPool(baseURL, device string) *Pool {
	return NewPoolWithProbe(client.New(baseURL, "", device))
}

// NewPoolWithProbe builds caller clients from the same client family as the
// periodic health probe. Copies keep caller tokens separate while sharing the
// server-wide compatibility verdict.
func NewPoolWithProbe(probe *client.Client) *Pool {
	return &Pool{
		template: probe,
		clients:  map[string]*client.Client{},
		servers:  map[string]*mcp.Server{},
		defaults: map[string]string{},
	}
}

// For returns the client for one caller's token, building it on first use.
//
// The client is shared across that caller's concurrent requests, which is safe:
// Client is immutable after construction and WithVault returns a copy.
func (p *Pool) For(token string) *client.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.forLocked(token)
}

// forLocked is For's body, for callers that already hold the mutex.
func (p *Pool) forLocked(token string) *client.Client {
	if c, ok := p.clients[token]; ok {
		return c
	}
	if len(p.clients) >= maxPooledClients {
		// All three maps are keyed by token and must stay in step: dropping
		// clients while keeping servers would leave a server bound to a client
		// nothing else can reach.
		p.clients = map[string]*client.Client{}
		p.servers = map[string]*mcp.Server{}
		p.defaults = map[string]string{}
	}
	c := p.template.WithToken(token)
	p.clients[token] = c
	return c
}

// Len is the number of cached clients. For tests and for a future metric.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.clients)
}

// MCPServer returns the MCP server that acts as one caller.
//
// The credential is baked into the server's tools rather than read per call,
// which is what stops a future tool from forgetting to plumb it: there is no
// per-handler credential to forget.
func (p *Pool) MCPServer(ctx context.Context, token, name, version string) (*mcp.Server, error) {
	p.mu.Lock()
	if s, ok := p.servers[token]; ok {
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()

	// The client MUST carry a vault. Without one every tool builds an
	// unqualified path -- /v1/head rather than /personal/v1/head -- which
	// matches no route and comes back as a bare 404 that names nothing. That
	// shipped: memo-ai's write_note failed with "404 page not found" for every
	// run until this was fixed. The REST surface resolves the vault per
	// request; MCP has to resolve it here, because the tools close over the
	// client rather than seeing the request.
	vault, err := p.DefaultVault(ctx, token)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.servers[token]; ok {
		return s, nil
	}
	s := NewMCPServer(p.forLocked(token).WithVia("relay-mcp").WithVault(vault), name, version)
	p.servers[token] = s
	return s, nil
}

// DefaultVault is the vault to use when a caller names none.
//
// It asks the SERVER what this token opens rather than reading a configured
// name, because under pass-through the answer differs per caller: a static
// default the caller's token does not open produces a 403 on a request they
// never scoped, which reads as a bug rather than as policy.
//
// One vault means one answer, mirroring what the plugin does at setup. More
// than one is a genuine question, so it is returned as an error naming the
// options rather than guessed at.
func (p *Pool) DefaultVault(ctx context.Context, token string) (string, error) {
	p.mu.Lock()
	v, ok := p.defaults[token]
	p.mu.Unlock()
	if ok {
		return v, nil
	}

	vaults, err := p.For(token).ListVaults(ctx)
	if err != nil {
		return "", err
	}
	switch len(vaults) {
	case 0:
		return "", errors.New("this token opens no vaults")
	case 1:
		p.mu.Lock()
		p.defaults[token] = vaults[0]
		p.mu.Unlock()
		return vaults[0], nil
	default:
		return "", fmt.Errorf("this token opens %d vaults (%s); name one with ?vault= or the vault parameter",
			len(vaults), strings.Join(vaults, ", "))
	}
}
