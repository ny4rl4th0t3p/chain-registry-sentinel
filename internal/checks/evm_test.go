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

func evmChainIDHandler(chainIDHex string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  chainIDHex,
		})
	}
}

func probeEVM(t *testing.T, srv *httptest.Server, chain registry.Chain) checks.EVMProbe {
	t.Helper()
	ep := registry.Endpoint{Address: srv.URL, Provider: "test"}
	client := checks.NewHTTPClient(5 * time.Second)
	return checks.ProbeEVMEndpoint(context.Background(), client, chain, ep)
}

func probeDeadEVM(t *testing.T) checks.EVMProbe {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	chain := registry.Chain{Name: "testchain", ChainID: "9001", ChainType: "eip155"}
	ep := registry.Endpoint{Address: srv.URL}
	client := checks.NewHTTPClient(2 * time.Second)
	return checks.ProbeEVMEndpoint(context.Background(), client, chain, ep)
}

func TestEVMLiveness_Pass(t *testing.T) {
	srv := httptest.NewServer(evmChainIDHandler("0x2329")) // 9001
	defer srv.Close()

	chain := registry.Chain{Name: "testchain", ChainID: "9001", ChainType: "eip155"}
	probe := probeEVM(t, srv, chain)
	r := checks.NewEVMLiveness().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass, got evidence: %s", r.Evidence)
	}
}

func TestEVMLiveness_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	chain := registry.Chain{Name: "testchain", ChainID: "9001", ChainType: "eip155"}
	probe := probeEVM(t, srv, chain)
	r := checks.NewEVMLiveness().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for non-200 response")
	}
	if r.ConnFailed {
		t.Error("HTTP error is not a connection failure")
	}
}

func TestEVMLiveness_ConnectionRefused(t *testing.T) {
	probe := probeDeadEVM(t)
	r := checks.NewEVMLiveness().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for connection refused")
	}
	if !r.ConnFailed {
		t.Error("want ConnFailed=true for unreachable endpoint")
	}
}

func TestEVMChainID_Match(t *testing.T) {
	srv := httptest.NewServer(evmChainIDHandler("0x2329")) // 9001
	defer srv.Close()

	chain := registry.Chain{Name: "testchain", ChainID: "9001", ChainType: "eip155"}
	probe := probeEVM(t, srv, chain)
	r := checks.NewEVMChainID().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass, got evidence: %s", r.Evidence)
	}
}

func TestEVMChainID_Mismatch(t *testing.T) {
	srv := httptest.NewServer(evmChainIDHandler("0x1")) // chain ID 1, not 9001
	defer srv.Close()

	chain := registry.Chain{Name: "testchain", ChainID: "9001", ChainType: "eip155"}
	probe := probeEVM(t, srv, chain)
	r := checks.NewEVMChainID().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for chain ID mismatch")
	}
	if r.Evidence != "got=1 want=9001" {
		t.Errorf("unexpected evidence: %s", r.Evidence)
	}
}

func TestEVMChainID_SkippedWhenFetchFailed(t *testing.T) {
	probe := probeDeadEVM(t)
	r := checks.NewEVMChainID().Evaluate(probe)
	if !r.Skipped {
		t.Error("want skipped when endpoint unreachable")
	}
}

func TestEVMChainID_SkippedForNonEIP155Chain(t *testing.T) {
	// A cosmos chain with EVM endpoints has no schema-defined EVM chain ID.
	srv := httptest.NewServer(evmChainIDHandler("0x2329"))
	defer srv.Close()

	chain := registry.Chain{Name: "evmos", ChainID: "evmos_9001-2", ChainType: "cosmos"}
	probe := probeEVM(t, srv, chain)
	r := checks.NewEVMChainID().Evaluate(probe)
	if !r.Skipped {
		t.Error("want skipped for non-eip155 chain type")
	}
}

func TestProbeEVMEndpoint_SingleFetch(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		evmChainIDHandler("0x2329")(w, r)
	}))
	defer srv.Close()

	chain := registry.Chain{Name: "testchain", ChainID: "9001", ChainType: "eip155"}
	probe := probeEVM(t, srv, chain)
	checks.NewEVMLiveness().Evaluate(probe)
	checks.NewEVMChainID().Evaluate(probe)

	if calls != 1 {
		t.Errorf("want exactly 1 HTTP call, got %d", calls)
	}
}
