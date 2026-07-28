package checks_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/registry"
)

// The User-Agent is applied at the transport level so no probe call site can forget it. This
// test pins the one non-obvious property: it must reach the wire, not just sit on the client.
// Identifying the scanner is what gives operators a specific string to allowlist instead of a
// generic Go client to block.
func TestHTTPClientSendsUserAgent(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.UserAgent()
		w.Write([]byte(`{"result":{"node_info":{"network":"test-1"}}}`)) //nolint:errcheck // test server
	}))
	defer srv.Close()

	const ua = "chain-registry-sentinel/test (+https://example.com)"
	client := checks.NewHTTPClient(5*time.Second, ua)
	chain := registry.Chain{Name: "test", ChainID: "test-1", ChainType: "cosmos"}

	probe := checks.ProbeEndpoint(context.Background(), client, chain, registry.Endpoint{Address: srv.URL})
	if probe.FetchErr != nil {
		t.Fatalf("probe failed: %v", probe.FetchErr)
	}
	if sent := <-got; sent != ua {
		t.Errorf("server saw User-Agent %q, want %q", sent, ua)
	}
}

// An empty ua must leave the client untouched: Go's default identifies itself as
// Go-http-client, and tests that construct the client with "" rely on unchanged behavior.
func TestHTTPClientEmptyUserAgentKeepsDefault(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.UserAgent()
		w.Write([]byte(`{}`)) //nolint:errcheck // test server
	}))
	defer srv.Close()

	client := checks.NewHTTPClient(5*time.Second, "")
	chain := registry.Chain{Name: "test", ChainID: "test-1", ChainType: "cosmos"}
	checks.ProbeEndpoint(context.Background(), client, chain, registry.Endpoint{Address: srv.URL})

	if sent := <-got; sent == "" || sent[:3] != "Go-" {
		t.Errorf("expected Go's default User-Agent, got %q", sent)
	}
}
