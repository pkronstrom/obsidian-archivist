package relay_test

import (
	"bufio"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pkronstrom/obsidian-archivist/internal/relay"
)

// A hostile client does not use Go's http.Client and will not normalise the
// path for us. Speak raw HTTP to find out what the relay actually does.
func TestRawUnnormalisedPathsCannotEscape(t *testing.T) {
	c := liveClient(t)
	srv := httptest.NewServer(relay.NewHandler(relay.NewPool(c.BaseURL(), "relay"), c, quiet(), nil, nil))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	for _, raw := range []string{
		"/file/../escape.md",
		"/file/a/../../b.md",
		"/file/./x.md",
		"/file//double.md",
	} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		body := "pwned"
		fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer relay-token\r\n"+
			"Content-Length: %d\r\nConnection: close\r\n\r\n%s", raw, len(body), body)
		status, _ := bufio.NewReader(conn).ReadString('\n')
		conn.Close()
		t.Logf("raw PUT %-24s -> %s", raw, strings.TrimSpace(status))
	}

	// Whatever the statuses were, nothing may exist outside the vault.
	files, err := c.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for p := range files {
		if strings.Contains(p, "..") || strings.HasPrefix(p, "/") {
			t.Errorf("an escaping path was created: %q", p)
		}
		t.Logf("created: %q", p)
	}
}
