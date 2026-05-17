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
	chainDir := filepath.Join(dir, chainName)
	if err := os.MkdirAll(chainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{
		"chain_name": chainName,
		"chain_id":   chainID,
		"apis":       map[string]any{"rpc": rpcs},
	})
	if err := os.WriteFile(filepath.Join(chainDir, "chain.json"), data, 0o644); err != nil {
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

func TestLoadChains_SkipsUnderscoreAndDotDirs(t *testing.T) {
	dir := t.TempDir()
	writeChainJSON(t, dir, "cosmoshub", "cosmoshub-4", nil)

	// these should be ignored
	for _, skip := range []string{"_IBC", "_non-cosmos", ".hidden"} {
		d := filepath.Join(dir, skip)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "chain.json"), []byte(`{"chain_name":"x","chain_id":"x-1"}`), 0o644)
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
	os.MkdirAll(filepath.Join(dir, "empty-dir"), 0o755)
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
	os.MkdirAll(chainDir, 0o755)
	os.WriteFile(filepath.Join(chainDir, "chain.json"), []byte(`not json`), 0o644)

	_, err := registry.LoadChains(dir, nil)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}
