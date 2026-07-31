package main

import (
	"testing"

	"chain-registry-sentinel/internal/registry"
)

// Cosmos chains declare EVM JSON-RPC lists too (the evmos-style chains); since v0.8.1 those
// are probed. The eip155 branch keeps probing EVM endpoints only.
func TestBuildJobsCosmosIncludesEVMEndpoints(t *testing.T) {
	chains := []registry.Chain{
		{
			Name:      "evmoslike",
			ChainType: "cosmos",
			RPCs:      []registry.Endpoint{{Address: "https://rpc.example"}},
			EVMEndpoints: []registry.Endpoint{
				{Address: "https://evm.example"},
				{Address: "https://evm2.example"},
			},
		},
		{
			Name:         "purevm",
			ChainType:    "eip155",
			RPCs:         []registry.Endpoint{{Address: "https://must-not-probe.example"}},
			EVMEndpoints: []registry.Endpoint{{Address: "https://eth.example"}},
		},
	}
	jobs := buildJobs(chains)

	counts := map[string]map[EndpointType]int{}
	for i := range jobs {
		j := &jobs[i]
		if counts[j.chain.Name] == nil {
			counts[j.chain.Name] = map[EndpointType]int{}
		}
		counts[j.chain.Name][j.endpointType]++
	}
	if got := counts["evmoslike"][TypeEVM]; got != 2 {
		t.Errorf("cosmos chain EVM jobs = %d, want 2", got)
	}
	if got := counts["evmoslike"][TypeRPC]; got != 1 {
		t.Errorf("cosmos chain RPC jobs = %d, want 1", got)
	}
	if got := counts["purevm"][TypeEVM]; got != 1 {
		t.Errorf("eip155 chain EVM jobs = %d, want 1", got)
	}
	if got := len(counts["purevm"]); got != 1 {
		t.Errorf("eip155 chain must build only EVM jobs, got %v", counts["purevm"])
	}
}
