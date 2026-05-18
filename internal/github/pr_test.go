package github_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chain-registry-sentinel/internal/github"
	"chain-registry-sentinel/internal/registry"
)

// chainJSON builds a chain.json []byte with the provided apis map.
func chainJSON(t *testing.T, apis map[string]any) []byte {
	t.Helper()
	doc := map[string]any{
		"chain_name": "testchain",
		"chain_id":   "testchain-1",
		"extra":      "preserved",
		"apis":       apis,
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("chainJSON: %v", err)
	}
	return append(b, '\n')
}

func writeChainJSON(t *testing.T, dir string, apis map[string]any) {
	t.Helper()
	chainDir := filepath.Join(dir, "testchain")
	if err := os.MkdirAll(chainDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(chainDir, "chain.json"), chainJSON(t, apis), 0o600); err != nil {
		t.Fatalf("write chain.json: %v", err)
	}
}

func dead(check, address string) github.FlaggedEndpoint {
	return github.FlaggedEndpoint{
		Check:               check,
		Address:             address,
		ConsecutiveFailures: 14,
		FirstFailureTime:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		FirstEvidence:       "connection refused",
		LastEvidence:        "connection refused",
	}
}

func TestEditChainJSON_removeOneRPC(t *testing.T) {
	dir := t.TempDir()
	writeChainJSON(t, dir, map[string]any{
		"rpc": []any{
			map[string]any{"address": "https://dead.example.com", "provider": "bad"},
			map[string]any{"address": "https://live.example.com", "provider": "good"},
		},
	})
	out, err := github.EditChainJSON(dir, "testchain", []github.FlaggedEndpoint{
		dead("rpc_liveness", "https://dead.example.com"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil {
		t.Fatal("want non-nil output (something was removed)")
	}
	if strings.Contains(string(out), "dead.example.com") {
		t.Error("dead endpoint should be removed")
	}
	if !strings.Contains(string(out), "live.example.com") {
		t.Error("live endpoint should be preserved")
	}
}

func TestEditChainJSON_lastEndpointInCategory(t *testing.T) {
	dir := t.TempDir()
	writeChainJSON(t, dir, map[string]any{
		"grpc": []any{
			map[string]any{"address": "dead.example.com:9090", "provider": "bad"},
		},
		"rpc": []any{
			map[string]any{"address": "https://live.example.com", "provider": "good"},
		},
	})
	out, err := github.EditChainJSON(dir, "testchain", []github.FlaggedEndpoint{
		dead("grpc_liveness", "dead.example.com:9090"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil {
		t.Fatal("want non-nil output")
	}
	// empty array must marshal as [] not null
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	apis := parsed["apis"].(map[string]any)
	grpc, ok := apis["grpc"]
	if !ok {
		t.Fatal("grpc key must be present")
	}
	arr, ok := grpc.([]any)
	if !ok || len(arr) != 0 {
		t.Errorf("grpc should be empty array, got %v", grpc)
	}
}

func TestEditChainJSON_addressNotInFile(t *testing.T) {
	dir := t.TempDir()
	writeChainJSON(t, dir, map[string]any{
		"rpc": []any{
			map[string]any{"address": "https://live.example.com", "provider": "ok"},
		},
	})
	out, err := github.EditChainJSON(dir, "testchain", []github.FlaggedEndpoint{
		dead("rpc_liveness", "https://notinthere.example.com"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Error("want nil (no-op) when address not in file")
	}
}

func TestEditChainJSON_unknownFieldsPreserved(t *testing.T) {
	dir := t.TempDir()
	writeChainJSON(t, dir, map[string]any{
		"rpc": []any{
			map[string]any{"address": "https://dead.example.com", "provider": "bad"},
		},
	})
	out, err := github.EditChainJSON(dir, "testchain", []github.FlaggedEndpoint{
		dead("rpc_liveness", "https://dead.example.com"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil {
		t.Fatal("want non-nil output")
	}
	if !strings.Contains(string(out), `"extra"`) {
		t.Error("unknown top-level fields should be preserved")
	}
}

func TestEditChainJSON_multipleTypesSimultaneously(t *testing.T) {
	dir := t.TempDir()
	writeChainJSON(t, dir, map[string]any{
		"rpc": []any{
			map[string]any{"address": "https://dead-rpc.example.com", "provider": "bad"},
			map[string]any{"address": "https://live-rpc.example.com", "provider": "ok"},
		},
		"grpc": []any{
			map[string]any{"address": "dead-grpc.example.com:9090", "provider": "bad"},
		},
	})
	out, err := github.EditChainJSON(dir, "testchain", []github.FlaggedEndpoint{
		dead("rpc_liveness", "https://dead-rpc.example.com"),
		dead("grpc_liveness", "dead-grpc.example.com:9090"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil {
		t.Fatal("want non-nil output")
	}
	if strings.Contains(string(out), "dead-rpc.example.com") {
		t.Error("dead RPC should be removed")
	}
	if strings.Contains(string(out), "dead-grpc.example.com") {
		t.Error("dead gRPC should be removed")
	}
	if !strings.Contains(string(out), "live-rpc.example.com") {
		t.Error("live RPC should be preserved")
	}
}

func TestBuildPRBody(t *testing.T) {
	chain := registry.Chain{Name: "cosmoshub", ChainID: "cosmoshub-4"}
	endpoints := []github.FlaggedEndpoint{
		dead("rpc_liveness", "https://dead.example.com"),
		dead("grpc_liveness", "dead.example.com:9090"),
	}
	body := github.BuildPRBody(chain, endpoints)
	if body == "" {
		t.Fatal("want non-empty body")
	}
	if !strings.Contains(body, "cosmoshub") {
		t.Error("body should contain chain name")
	}
	if !strings.Contains(body, "| Check |") {
		t.Error("body should contain table header")
	}
	for _, ep := range endpoints {
		if !strings.Contains(body, ep.Address) {
			t.Errorf("body should contain address %q", ep.Address)
		}
	}
}
