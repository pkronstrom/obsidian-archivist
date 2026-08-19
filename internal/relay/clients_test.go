package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPoolReturnsTheSameClientForOneToken(t *testing.T) {
	p := NewPool("https://vault.example", "relay")
	a := p.For("tok-a")
	b := p.For("tok-a")
	if a != b {
		t.Error("two calls with the same token built two clients; the cache is not working")
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
	p := NewPool("https://vault.example", "relay")
	a := p.MCPServer("tok-a", "archivist", "test")
	b := p.MCPServer("tok-a", "archivist", "test")
	if a != b {
		t.Error("one token got two MCP servers; the eight tools are rebuilt on every call")
	}
	if a == p.MCPServer("tok-b", "archivist", "test") {
		t.Fatal("two tokens share one MCP server; a caller would act as someone else")
	}
}
