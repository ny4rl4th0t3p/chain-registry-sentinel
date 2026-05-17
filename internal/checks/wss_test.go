package checks_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/registry"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wssNodeInfoHandler(network string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		if err != nil {
			return
		}
		resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"node_info":{"network":%q}}}`, network)
		conn.WriteMessage(websocket.TextMessage, []byte(resp))
	}
}

func probeWSS(t *testing.T, serverURL, chainID string) checks.WSSProbe {
	t.Helper()
	wsAddr := strings.Replace(serverURL, "http://", "ws://", 1)
	chain := registry.Chain{
		Name:         "testchain",
		ChainID:      chainID,
		WSSEndpoints: []registry.Endpoint{{Address: wsAddr, Provider: "test"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return checks.ProbeWSSEndpoint(ctx, chain, chain.WSSEndpoints[0])
}

func probeDeadWSS(t *testing.T, chainID string) checks.WSSProbe {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()
	chain := registry.Chain{
		Name:         "testchain",
		ChainID:      chainID,
		WSSEndpoints: []registry.Endpoint{{Address: "ws://" + addr}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return checks.ProbeWSSEndpoint(ctx, chain, chain.WSSEndpoints[0])
}

func TestWSSLiveness_Pass(t *testing.T) {
	srv := httptest.NewServer(wssNodeInfoHandler("testchain-1"))
	defer srv.Close()

	probe := probeWSS(t, srv.URL, "testchain-1")
	r := checks.NewWSSLiveness().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass, got evidence: %s", r.Evidence)
	}
}

func TestWSSLiveness_ConnectionRefused(t *testing.T) {
	probe := probeDeadWSS(t, "testchain-1")
	r := checks.NewWSSLiveness().Evaluate(probe)
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

func TestWSSLiveness_ServerRejectsUpgrade(t *testing.T) {
	// A plain HTTP server that returns 400 is not a working WSS endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not a websocket endpoint", http.StatusBadRequest)
	}))
	defer srv.Close()

	probe := probeWSS(t, srv.URL, "testchain-1")
	r := checks.NewWSSLiveness().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for server that rejects the upgrade")
	}
	if r.ConnFailed {
		t.Error("want ConnFailed=false (server responded, wrong protocol)")
	}
}

func TestWSSChainID_Match(t *testing.T) {
	srv := httptest.NewServer(wssNodeInfoHandler("testchain-1"))
	defer srv.Close()

	probe := probeWSS(t, srv.URL, "testchain-1")
	r := checks.NewWSSChainID().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass, got evidence: %s", r.Evidence)
	}
}

func TestWSSChainID_Mismatch(t *testing.T) {
	srv := httptest.NewServer(wssNodeInfoHandler("wrongchain-99"))
	defer srv.Close()

	probe := probeWSS(t, srv.URL, "testchain-1")
	r := checks.NewWSSChainID().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for chain ID mismatch")
	}
	if r.Evidence != "got=wrongchain-99 want=testchain-1" {
		t.Errorf("unexpected evidence: %s", r.Evidence)
	}
}

func TestWSSChainID_SkippedWhenFetchFailed(t *testing.T) {
	probe := probeDeadWSS(t, "testchain-1")
	r := checks.NewWSSChainID().Evaluate(probe)
	if !r.Skipped {
		t.Error("want skipped when endpoint unreachable")
	}
	if r.Passed {
		t.Error("skipped result should not be passed")
	}
}

func TestProbeWSSEndpoint_SingleFetch(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		wssNodeInfoHandler("testchain-1")(w, r)
	}))
	defer srv.Close()

	probe := probeWSS(t, srv.URL, "testchain-1")
	checks.NewWSSLiveness().Evaluate(probe)
	checks.NewWSSChainID().Evaluate(probe)

	if calls != 1 {
		t.Errorf("want exactly 1 WebSocket connection, got %d", calls)
	}
}
