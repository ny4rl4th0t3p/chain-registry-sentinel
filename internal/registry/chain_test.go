package registry_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"chain-registry-sentinel/internal/registry"
)

func writeChainJSON(t *testing.T, dir, chainName, chainID string, rpcs []registry.Endpoint) {
	t.Helper()
	writeChainFull(t, dir, chainName, chainID, "cosmos", "live", rpcs, nil, nil, nil)
}

func writeChainFull(t *testing.T, dir, chainName, chainID, chainType, status string,
	rpcs, rest, grpcWeb, evm []registry.Endpoint,
) {
	t.Helper()
	chainDir := filepath.Join(dir, chainName)
	if err := os.MkdirAll(chainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{
		"chain_name": chainName,
		"chain_id":   chainID,
		"chain_type": chainType,
		"status":     status,
		"apis": map[string]any{
			"rpc":              rpcs,
			"rest":             rest,
			"grpc-web":         grpcWeb,
			"evm-http-jsonrpc": evm,
		},
	})
	if err := os.WriteFile(filepath.Join(chainDir, "chain.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadChains_All(t *testing.T) {
	dir := t.TempDir()
	writeChainJSON(t, dir, "cosmoshub", "cosmoshub-4", []registry.Endpoint{
		{Address: "https://rpc.cosmos.network", Provider: "acme"},
	})
	writeChainJSON(t, dir, "osmosis", "osmosis-1", nil)

	chains, err := registry.LoadChains(dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chains) != 2 {
		t.Fatalf("want 2 chains, got %d", len(chains))
	}
}

func TestLoadChains_Filter(t *testing.T) {
	dir := t.TempDir()
	writeChainJSON(t, dir, "cosmoshub", "cosmoshub-4", nil)
	writeChainJSON(t, dir, "osmosis", "osmosis-1", nil)
	writeChainJSON(t, dir, "juno", "juno-1", nil)

	chains, err := registry.LoadChains(dir, []string{"cosmoshub", "juno"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chains) != 2 {
		t.Fatalf("want 2 chains, got %d", len(chains))
	}
	names := map[string]bool{}
	for _, c := range chains {
		names[c.Name] = true
	}
	if !names["cosmoshub"] || !names["juno"] {
		t.Errorf("unexpected chains: %v", names)
	}
}

func TestLoadChains_SkipsNonLiveChains(t *testing.T) {
	dir := t.TempDir()
	writeChainJSON(t, dir, "cosmoshub", "cosmoshub-4", nil) // live
	writeChainFull(t, dir, "upcoming-chain", "upcoming-1", "cosmos", "upcoming", nil, nil, nil, nil)
	writeChainFull(t, dir, "killed-chain", "killed-1", "cosmos", "killed", nil, nil, nil, nil)

	chains, err := registry.LoadChains(dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chains) != 1 {
		t.Fatalf("want 1 live chain, got %d", len(chains))
	}
	if chains[0].Name != "cosmoshub" {
		t.Errorf("want cosmoshub, got %s", chains[0].Name)
	}
}

func TestLoadChains_SkipsUnderscoreAndDotDirs(t *testing.T) {
	dir := t.TempDir()
	writeChainJSON(t, dir, "cosmoshub", "cosmoshub-4", nil)

	for _, skip := range []string{"_IBC", "_non-cosmos", ".hidden"} {
		d := filepath.Join(dir, skip)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "chain.json"), []byte(`{"chain_name":"x","chain_id":"x-1","chain_type":"cosmos","status":"live"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	chains, err := registry.LoadChains(dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chains) != 1 {
		t.Fatalf("want 1 chain, got %d: %+v", len(chains), chains)
	}
}

func TestLoadChains_SkipsDirWithNoChainJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeChainJSON(t, dir, "cosmoshub", "cosmoshub-4", nil)

	chains, err := registry.LoadChains(dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chains) != 1 {
		t.Fatalf("want 1 chain, got %d", len(chains))
	}
}

func TestLoadChains_RPCEndpoints(t *testing.T) {
	dir := t.TempDir()
	writeChainJSON(t, dir, "cosmoshub", "cosmoshub-4", []registry.Endpoint{
		{Address: "https://rpc1.example.com", Provider: "p1"},
		{Address: "https://rpc2.example.com", Provider: "p2"},
	})

	chains, err := registry.LoadChains(dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chains[0].RPCs) != 2 {
		t.Fatalf("want 2 RPCs, got %d", len(chains[0].RPCs))
	}
	if chains[0].ChainID != "cosmoshub-4" {
		t.Errorf("want chain_id cosmoshub-4, got %s", chains[0].ChainID)
	}
}

func TestLoadChains_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	chainDir := filepath.Join(dir, "badchain")
	if err := os.MkdirAll(chainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chainDir, "chain.json"), []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := registry.LoadChains(dir, nil)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestLoadChains_AllEndpointTypes(t *testing.T) {
	dir := t.TempDir()
	writeChainFull(t, dir, "cosmoshub", "cosmoshub-4", "cosmos", "live",
		[]registry.Endpoint{{Address: "https://rpc.example.com", Provider: "p1"}},
		[]registry.Endpoint{{Address: "https://rest.example.com", Provider: "p1"}},
		[]registry.Endpoint{{Address: "https://grpc.example.com:443", Provider: "p1"}},
		nil,
	)

	chains, err := registry.LoadChains(dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c := chains[0]
	if len(c.RPCs) != 1 {
		t.Errorf("want 1 RPC, got %d", len(c.RPCs))
	}
	if len(c.RESTEndpoints) != 1 {
		t.Errorf("want 1 REST, got %d", len(c.RESTEndpoints))
	}
	if len(c.GRPCWebEndpoints) != 1 {
		t.Errorf("want 1 gRPC-web, got %d", len(c.GRPCWebEndpoints))
	}
	if c.ChainType != "cosmos" {
		t.Errorf("want chain_type cosmos, got %q", c.ChainType)
	}
}

func TestLoadChains_EIP155Chain(t *testing.T) {
	dir := t.TempDir()
	writeChainFull(t, dir, "ethereum", "1", "eip155", "live",
		nil, nil, nil,
		[]registry.Endpoint{{Address: "https://rpc.ethereum.org", Provider: "test"}},
	)

	chains, err := registry.LoadChains(dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c := chains[0]
	if len(c.EVMEndpoints) != 1 {
		t.Errorf("want 1 EVM endpoint, got %d", len(c.EVMEndpoints))
	}
	if c.ChainType != "eip155" {
		t.Errorf("want chain_type eip155, got %s", c.ChainType)
	}
	if c.ChainID != "1" {
		t.Errorf("want chain_id 1, got %s", c.ChainID)
	}
}
