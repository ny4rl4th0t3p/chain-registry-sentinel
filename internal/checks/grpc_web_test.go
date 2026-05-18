package checks_test

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/registry"
)

// grpcWebNodeInfoHandler returns an HTTP handler that responds to gRPC-web
// GetNodeInfo requests with a properly framed protobuf response.
func grpcWebNodeInfoHandler(network string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		body := buildGetNodeInfoResponse(network)
		bodyLen := len(body)
		if bodyLen > math.MaxUint32 {
			http.Error(w, "response body too large", http.StatusInternalServerError)
			return
		}
		frame := make([]byte, 5+bodyLen)
		frame[0] = 0x00 // data frame flag
		frame[1] = byte(bodyLen >> 24)
		frame[2] = byte(bodyLen >> 16)
		frame[3] = byte(bodyLen >> 8)
		frame[4] = byte(bodyLen)
		copy(frame[5:], body)
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(frame); err != nil {
			return
		}
	}
}

func probeGRPCWeb(t *testing.T, serverURL string) checks.GRPCWebProbe {
	t.Helper()
	chain := registry.Chain{
		Name:             "testchain",
		ChainID:          "testchain-1",
		GRPCWebEndpoints: []registry.Endpoint{{Address: serverURL, Provider: "test"}},
	}
	client := checks.NewHTTPClient(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return checks.ProbeGRPCWebEndpoint(ctx, client, chain, chain.GRPCWebEndpoints[0])
}

func probeDeadGRPCWeb(t *testing.T) checks.GRPCWebProbe {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	srv.Close()
	chain := registry.Chain{Name: "testchain", ChainID: "testchain-1", GRPCWebEndpoints: []registry.Endpoint{{Address: srv.URL}}}
	client := checks.NewHTTPClient(2 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return checks.ProbeGRPCWebEndpoint(ctx, client, chain, chain.GRPCWebEndpoints[0])
}

func TestGRPCWebLiveness_Pass(t *testing.T) {
	srv := httptest.NewServer(grpcWebNodeInfoHandler("testchain-1"))
	defer srv.Close()

	probe := probeGRPCWeb(t, srv.URL)
	r := checks.NewGRPCWebLiveness().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass, got evidence: %s", r.Evidence)
	}
}

func TestGRPCWebLiveness_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	probe := probeGRPCWeb(t, srv.URL)
	r := checks.NewGRPCWebLiveness().Evaluate(probe)
	if !r.Skipped {
		t.Error("want skipped for HTTP 429")
	}
	if r.Passed {
		t.Error("skipped result should not be passed")
	}
}

func TestGRPCWebLiveness_ConnectionRefused(t *testing.T) {
	probe := probeDeadGRPCWeb(t)
	r := checks.NewGRPCWebLiveness().Evaluate(probe)
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

func TestGRPCWebLiveness_NonGRPCWebServer(t *testing.T) {
	// A plain HTTP server returning 404 is not a working gRPC-web endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	probe := probeGRPCWeb(t, srv.URL)
	r := checks.NewGRPCWebLiveness().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for non-gRPC-web server")
	}
	if r.ConnFailed {
		t.Error("want ConnFailed=false (server responded, wrong protocol)")
	}
}

func TestGRPCWebChainID_Match(t *testing.T) {
	srv := httptest.NewServer(grpcWebNodeInfoHandler("testchain-1"))
	defer srv.Close()

	probe := probeGRPCWeb(t, srv.URL)
	r := checks.NewGRPCWebChainID().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass, got evidence: %s", r.Evidence)
	}
}

func TestGRPCWebChainID_Mismatch(t *testing.T) {
	srv := httptest.NewServer(grpcWebNodeInfoHandler("wrongchain-99"))
	defer srv.Close()

	probe := probeGRPCWeb(t, srv.URL)
	r := checks.NewGRPCWebChainID().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for chain ID mismatch")
	}
	if r.Evidence != "got=wrongchain-99 want=testchain-1" {
		t.Errorf("unexpected evidence: %s", r.Evidence)
	}
}

func TestGRPCWebChainID_SkippedWhenFetchFailed(t *testing.T) {
	probe := probeDeadGRPCWeb(t)
	r := checks.NewGRPCWebChainID().Evaluate(probe)
	if !r.Skipped {
		t.Error("want skipped when endpoint unreachable")
	}
	if r.Passed {
		t.Error("skipped result should not be passed")
	}
}

func TestProbeGRPCWebEndpoint_SingleFetch(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		grpcWebNodeInfoHandler("testchain-1")(w, r)
	}))
	defer srv.Close()

	probe := probeGRPCWeb(t, srv.URL)
	checks.NewGRPCWebLiveness().Evaluate(probe)
	checks.NewGRPCWebChainID().Evaluate(probe)

	if calls != 1 {
		t.Errorf("want exactly 1 HTTP request, got %d", calls)
	}
}
