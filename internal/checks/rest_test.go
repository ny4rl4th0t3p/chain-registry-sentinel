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

func restNodeInfoHandler(network string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cosmos/base/tendermint/v1beta1/node_info" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"default_node_info": map[string]any{"network": network},
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func probeREST(t *testing.T, srv *httptest.Server) checks.RESTProbe {
	t.Helper()
	chain := registry.Chain{
		Name:          "testchain",
		ChainID:       "testchain-1",
		RESTEndpoints: []registry.Endpoint{{Address: srv.URL, Provider: "test"}},
	}
	client := checks.NewHTTPClient(5 * time.Second)
	return checks.ProbeRESTEndpoint(context.Background(), client, chain, chain.RESTEndpoints[0])
}

func probeDeadREST(t *testing.T) checks.RESTProbe {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	srv.Close()
	chain := registry.Chain{Name: "testchain", ChainID: "testchain-1", RESTEndpoints: []registry.Endpoint{{Address: srv.URL}}}
	client := checks.NewHTTPClient(2 * time.Second)
	return checks.ProbeRESTEndpoint(context.Background(), client, chain, chain.RESTEndpoints[0])
}

func TestRESTLiveness_Pass(t *testing.T) {
	srv := httptest.NewServer(restNodeInfoHandler("testchain-1"))
	defer srv.Close()

	probe := probeREST(t, srv)
	r := checks.NewRESTLiveness().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass, got evidence: %s", r.Evidence)
	}
}

func TestRESTLiveness_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	probe := probeREST(t, srv)
	r := checks.NewRESTLiveness().Evaluate(probe)
	if !r.Skipped {
		t.Error("want skipped for HTTP 429")
	}
	if r.Passed {
		t.Error("skipped result should not be passed")
	}
}

func TestRESTLiveness_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	probe := probeREST(t, srv)
	r := checks.NewRESTLiveness().Evaluate(probe)
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

func TestRESTLiveness_ConnectionRefused(t *testing.T) {
	probe := probeDeadREST(t)
	r := checks.NewRESTLiveness().Evaluate(probe)
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

func TestRESTChainID_Match(t *testing.T) {
	srv := httptest.NewServer(restNodeInfoHandler("testchain-1"))
	defer srv.Close()

	probe := probeREST(t, srv)
	r := checks.NewRESTChainID().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass, got evidence: %s", r.Evidence)
	}
}

func TestRESTChainID_Mismatch(t *testing.T) {
	srv := httptest.NewServer(restNodeInfoHandler("wrongchain-99"))
	defer srv.Close()

	probe := probeREST(t, srv)
	r := checks.NewRESTChainID().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for chain ID mismatch")
	}
	if r.Evidence != "got=wrongchain-99 want=testchain-1" {
		t.Errorf("unexpected evidence: %s", r.Evidence)
	}
}

func TestRESTChainID_SkippedWhenFetchFailed(t *testing.T) {
	probe := probeDeadREST(t)
	r := checks.NewRESTChainID().Evaluate(probe)
	if !r.Skipped {
		t.Error("want skipped when endpoint unreachable")
	}
	if r.Passed {
		t.Error("skipped result should not be passed")
	}
}

func TestProbeRESTEndpoint_SingleFetch(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		restNodeInfoHandler("testchain-1")(w, r)
	}))
	defer srv.Close()

	probe := probeREST(t, srv)
	checks.NewRESTLiveness().Evaluate(probe)
	checks.NewRESTChainID().Evaluate(probe)

	if calls != 1 {
		t.Errorf("want exactly 1 HTTP call, got %d", calls)
	}
}
