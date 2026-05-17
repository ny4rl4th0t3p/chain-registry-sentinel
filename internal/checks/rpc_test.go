package checks_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/registry"
)

func rpcStatusHandler(network string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"node_info": map[string]any{"network": network},
			},
		})
	}
}

func chainWith(t *testing.T, address string) registry.Chain {
	t.Helper()
	return registry.Chain{
		Name:    "testchain",
		ChainID: "testchain-1",
		RPCs:    []registry.Endpoint{{Address: address, Provider: "test"}},
	}
}

// RPCLiveness

func TestRPCLiveness_Pass(t *testing.T) {
	srv := httptest.NewServer(rpcStatusHandler("testchain-1"))
	defer srv.Close()

	results := checks.NewRPCLiveness(5*time.Second).Run(context.Background(), chainWith(t, srv.URL))
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if !results[0].Passed {
		t.Errorf("want pass, got evidence: %s", results[0].Evidence)
	}
}

func TestRPCLiveness_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	results := checks.NewRPCLiveness(5*time.Second).Run(context.Background(), chainWith(t, srv.URL))
	if results[0].Passed {
		t.Error("want fail for non-200 response")
	}
}

func TestRPCLiveness_ConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // close immediately so the port is unreachable

	results := checks.NewRPCLiveness(2*time.Second).Run(context.Background(), chainWith(t, srv.URL))
	if results[0].Passed {
		t.Error("want fail for connection refused")
	}
	if results[0].Evidence == "" {
		t.Error("want non-empty evidence")
	}
}

func TestRPCLiveness_MultipleEndpoints(t *testing.T) {
	good := httptest.NewServer(rpcStatusHandler("testchain-1"))
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	chain := registry.Chain{
		Name:    "testchain",
		ChainID: "testchain-1",
		RPCs: []registry.Endpoint{
			{Address: good.URL},
			{Address: bad.URL},
		},
	}
	results := checks.NewRPCLiveness(5*time.Second).Run(context.Background(), chain)
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if !results[0].Passed {
		t.Error("first endpoint should pass")
	}
	if results[1].Passed {
		t.Error("second endpoint should fail")
	}
}

// RPCChainID

func TestRPCChainID_Match(t *testing.T) {
	srv := httptest.NewServer(rpcStatusHandler("testchain-1"))
	defer srv.Close()

	results := checks.NewRPCChainID(5*time.Second).Run(context.Background(), chainWith(t, srv.URL))
	if !results[0].Passed {
		t.Errorf("want pass, got evidence: %s", results[0].Evidence)
	}
}

func TestRPCChainID_Mismatch(t *testing.T) {
	srv := httptest.NewServer(rpcStatusHandler("wrongchain-99"))
	defer srv.Close()

	results := checks.NewRPCChainID(5*time.Second).Run(context.Background(), chainWith(t, srv.URL))
	if results[0].Passed {
		t.Error("want fail for chain ID mismatch")
	}
	if results[0].Evidence != "got=wrongchain-99 want=testchain-1" {
		t.Errorf("unexpected evidence: %s", results[0].Evidence)
	}
}

func TestRPCChainID_FetchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	results := checks.NewRPCChainID(2*time.Second).Run(context.Background(), chainWith(t, srv.URL))
	if results[0].Passed {
		t.Error("want fail when fetch fails")
	}
	if results[0].Evidence == "" {
		t.Error("want non-empty evidence")
	}
}

func TestRPCChainID_NoEndpoints(t *testing.T) {
	chain := registry.Chain{Name: "empty", ChainID: "empty-1", RPCs: nil}
	results := checks.NewRPCChainID(5*time.Second).Run(context.Background(), chain)
	if len(results) != 0 {
		t.Errorf("want 0 results for chain with no RPCs, got %d", len(results))
	}
}
