package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/client"
	"github.com/pkronstrom/obsidian-archivist/protocol"
)

func TestPoolReturnsTheSameClientForOneToken(t *testing.T) {
	p := NewPool("https://vault.example", "relay")
	a := p.For("tok-a")
	b := p.For("tok-a")
	if a != b {
		t.Error("two calls with the same token built two clients; the cache is not working")
	}
}

func TestPoolSharesPeriodicCompatibilityVerdict(t *testing.T) {
	var proto atomic.Int32
	proto.Store(protocol.Version)
	var pushes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			json.NewEncoder(w).Encode(protocol.HealthResponse{
				Status: "ok", Version: "test", Protocol: int(proto.Load()),
			})
		case "/personal/v1/push":
			pushes.Add(1)
			json.NewEncoder(w).Encode(protocol.PushResponse{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	probe := client.New(srv.URL, "", "relay")
	p := NewPoolWithProbe(probe)
	caller := p.For("tok").WithVault("personal")
	if err := caller.CheckCompatible(context.Background()); err != nil {
		t.Fatalf("baseline caller check: %v", err)
	}

	proto.Store(protocol.Version + 1)
	if err := probe.CheckCompatible(context.Background()); err == nil {
		t.Fatal("periodic probe accepted the incompatible server")
	}
	if _, err := caller.Push(context.Background(), "base", nil); err == nil {
		t.Fatal("a caller with an earlier cached verdict was allowed to push")
	}
	if got := pushes.Load(); got != 0 {
		t.Fatalf("incompatible server received %d push(es)", got)
	}
}

func TestPoolSeparatesClientsByToken(t *testing.T) {
	p := NewPool("https://vault.example", "relay")
	if p.For("tok-a") == p.For("tok-b") {
		t.Fatal("two different tokens share one client; every caller would use the wrong credential")
	}
}

// The whole point: the client carries the CALLER's token, not the relay's.
func TestPoolClientCarriesTheCallersToken(t *testing.T) {
	p := NewPool("https://vault.example", "relay")
	c := p.For("tok-a")
	if got := c.Token(); got != "tok-a" {
		t.Errorf("client token = %q, want the caller's own token", got)
	}
	if got := c.Device(); got != "relay" {
		t.Errorf("device = %q, want relay", got)
	}
}

// An unbounded map keyed by a value an attacker chooses is a memory leak with a
// remote trigger: every wrong guess would allocate a client forever.
func TestPoolIsBounded(t *testing.T) {
	p := NewPool("https://vault.example", "relay")
	for i := 0; i < maxPooledClients+50; i++ {
		p.For(string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}
	if n := p.Len(); n > maxPooledClients {
		t.Errorf("pool holds %d clients, cap is %d", n, maxPooledClients)
	}
}

func vaultsServer(t *testing.T, vaults []string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vaults" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if calls != nil {
			calls.Add(1)
		}
		json.NewEncoder(w).Encode(map[string]any{"vaults": vaults, "canCreate": false})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDefaultVaultResolvesWhenTheTokenOpensExactlyOne(t *testing.T) {
	srv := vaultsServer(t, []string{"personal"}, nil)
	p := NewPool(srv.URL, "relay")
	got, err := p.DefaultVault(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if got != "personal" {
		t.Errorf("default vault = %q, want personal", got)
	}
}

func TestDefaultVaultRefusesToGuessBetweenTwo(t *testing.T) {
	srv := vaultsServer(t, []string{"personal", "work"}, nil)
	p := NewPool(srv.URL, "relay")
	_, err := p.DefaultVault(context.Background(), "tok")
	if err == nil {
		t.Fatal("the pool picked one of two vaults; it must ask instead")
	}
}

func TestDefaultVaultIsCachedPerToken(t *testing.T) {
	var calls atomic.Int32
	srv := vaultsServer(t, []string{"personal"}, &calls)
	p := NewPool(srv.URL, "relay")
	for i := 0; i < 3; i++ {
		if _, err := p.DefaultVault(context.Background(), "tok"); err != nil {
			t.Fatal(err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("asked the server %d times for one token's vault list, want 1", n)
	}
}

func TestPoolCachesOneMCPServerPerToken(t *testing.T) {
	srv := vaultsServer(t, []string{"personal"}, nil)
	p := NewPool(srv.URL, "relay")
	a, err := p.MCPServer(context.Background(), "tok-a", "archivist", "test")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := p.MCPServer(context.Background(), "tok-a", "archivist", "test")
	if a != b {
		t.Error("one token got two MCP servers; the eight tools are rebuilt on every call")
	}
	c, _ := p.MCPServer(context.Background(), "tok-b", "archivist", "test")
	if a == c {
		t.Fatal("two tokens share one MCP server; a caller would act as someone else")
	}
}

// The bug this exists to stop: an MCP client with no vault builds /v1/head
// instead of /personal/v1/head, which matches no route and comes back as a bare
// 404. It shipped, and memo-ai's write_note failed on every run.
func TestMCPServerCallsAreVaultQualified(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v1/vaults" {
			json.NewEncoder(w).Encode(map[string]any{"vaults": []string{"personal"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"head": ""})
	}))
	t.Cleanup(srv.Close)

	p := NewPool(srv.URL, "relay")
	if _, err := p.MCPServer(context.Background(), "tok", "archivist", "test"); err != nil {
		t.Fatal(err)
	}
	// The client the tools close over must address a vault. Exercise it the way
	// a tool does and check where the request landed.
	if _, err := p.For("tok").WithVault("personal").Head(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if path == "/v1/head" {
			t.Fatal("an unqualified /v1/head reached the server; that is the 404")
		}
	}
	if len(paths) < 2 || paths[len(paths)-1] != "/personal/v1/head" {
		t.Errorf("paths = %v, want the last one vault-qualified", paths)
	}
}
