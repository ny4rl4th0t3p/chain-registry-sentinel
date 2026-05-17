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
		if err := json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"node_info": map[string]any{"network": network},
			},
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func probeChain(t *testing.T, srv *httptest.Server) checks.EndpointProbe {
	t.Helper()
	chain := registry.Chain{
		Name:    "testchain",
		ChainID: "testchain-1",
		RPCs:    []registry.Endpoint{{Address: srv.URL, Provider: "test"}},
	}
	client := checks.NewHTTPClient(5 * time.Second)
	return checks.ProbeEndpoint(context.Background(), client, chain, chain.RPCs[0])
}

func probeDeadServer(t *testing.T) checks.EndpointProbe {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	srv.Close()
	chain := registry.Chain{Name: "testchain", ChainID: "testchain-1", RPCs: []registry.Endpoint{{Address: srv.URL}}}
	client := checks.NewHTTPClient(2 * time.Second)
	return checks.ProbeEndpoint(context.Background(), client, chain, chain.RPCs[0])
}

// RPCLiveness

func TestRPCLiveness_Pass(t *testing.T) {
	srv := httptest.NewServer(rpcStatusHandler("testchain-1"))
	defer srv.Close()

	probe := probeChain(t, srv)
	r := checks.NewRPCLiveness().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass, got evidence: %s", r.Evidence)
	}
}

func TestRPCLiveness_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	probe := probeChain(t, srv)
	r := checks.NewRPCLiveness().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for non-200 response")
	}
	if r.ConnFailed {
		t.Error("HTTP error is not a connection failure")
	}
	if r.Skipped {
		t.Error("liveness should never be skipped")
	}
}

func TestRPCLiveness_ConnectionRefused(t *testing.T) {
	probe := probeDeadServer(t)
	r := checks.NewRPCLiveness().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for connection refused")
	}
	if !r.ConnFailed {
		t.Error("want ConnFailed=true for unreachable endpoint")
	}
	if r.Evidence == "" {
		t.Error("want non-empty evidence")
	}
}

// RPCChainID

func TestRPCChainID_Match(t *testing.T) {
	srv := httptest.NewServer(rpcStatusHandler("testchain-1"))
	defer srv.Close()

	probe := probeChain(t, srv)
	r := checks.NewRPCChainID().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass, got evidence: %s", r.Evidence)
	}
}

func TestRPCChainID_Mismatch(t *testing.T) {
	srv := httptest.NewServer(rpcStatusHandler("wrongchain-99"))
	defer srv.Close()

	probe := probeChain(t, srv)
	r := checks.NewRPCChainID().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for chain ID mismatch")
	}
	if r.Evidence != "got=wrongchain-99 want=testchain-1" {
		t.Errorf("unexpected evidence: %s", r.Evidence)
	}
}

func TestRPCChainID_SkippedWhenFetchFailed(t *testing.T) {
	probe := probeDeadServer(t)
	r := checks.NewRPCChainID().Evaluate(probe)
	if !r.Skipped {
		t.Error("want skipped when endpoint unreachable")
	}
	if r.Passed {
		t.Error("skipped result should not be passed")
	}
}

func TestRPCChainID_UnwrappedNodeInfo(t *testing.T) {
	// Some nodes (e.g. Sei via Pocket Network) return node_info directly at the
	// top level without the result wrapper.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"node_info": map[string]any{"network": "testchain-1"},
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	probe := probeChain(t, srv)
	r := checks.NewRPCChainID().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass for unwrapped node_info format, got evidence: %s", r.Evidence)
	}
}

// ProbeEndpoint

func TestProbeEndpoint_SingleFetch(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		rpcStatusHandler("testchain-1")(w, r)
	}))
	defer srv.Close()

	probe := probeChain(t, srv)
	checks.NewRPCLiveness().Evaluate(probe)
	checks.NewRPCChainID().Evaluate(probe)

	if calls != 1 {
		t.Errorf("want exactly 1 HTTP call, got %d", calls)
	}
}
