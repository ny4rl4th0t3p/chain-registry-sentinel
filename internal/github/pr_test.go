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

// A dead verdict on one protocol must not delete the same address's entry under another
// category: 28 registry addresses are listed under both rpc and rest (Pocket-style gateways
// serving both on one URL), and address-keyed deletion would remove the live sibling too.
func TestEditChainJSON_categoryScoped(t *testing.T) {
	dir := t.TempDir()
	shared := "https://gateway.example.com"
	writeChainJSON(t, dir, map[string]any{
		"rpc":  []any{map[string]any{"address": shared, "provider": "gw"}},
		"rest": []any{map[string]any{"address": shared, "provider": "gw"}},
	})
	out, err := github.EditChainJSON(dir, "testchain", []github.FlaggedEndpoint{
		dead("rest_liveness", shared),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil {
		t.Fatal("want non-nil output (the rest entry should be removed)")
	}
	var doc struct {
		APIs struct {
			RPC  []map[string]any `json:"rpc"`
			REST []map[string]any `json:"rest"`
		} `json:"apis"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(doc.APIs.RPC) != 1 || doc.APIs.RPC[0]["address"] != shared {
		t.Errorf("rpc entry must survive a rest-only removal, got %v", doc.APIs.RPC)
	}
	if len(doc.APIs.REST) != 0 {
		t.Errorf("rest entry should be removed, got %v", doc.APIs.REST)
	}
}

// EVM entries live under apis."evm-http-jsonrpc"; the editor removes matching (category,
// address) pairs, and since v0.8.1 probes those entries on cosmos chains, so dead ones reach
// the removal flow.
func TestEditChainJSON_removeEVMEndpoint(t *testing.T) {
	dir := t.TempDir()
	writeChainJSON(t, dir, map[string]any{
		"evm-http-jsonrpc": []any{
			map[string]any{"address": "https://dead-evm.example.com", "provider": "bad"},
			map[string]any{"address": "https://live-evm.example.com", "provider": "good"},
		},
	})
	out, err := github.EditChainJSON(dir, "testchain", []github.FlaggedEndpoint{
		dead("evm_liveness", "https://dead-evm.example.com"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil {
		t.Fatal("want non-nil output (something was removed)")
	}
	if strings.Contains(string(out), "dead-evm.example.com") {
		t.Error("dead EVM endpoint should be removed")
	}
	if !strings.Contains(string(out), "live-evm.example.com") {
		t.Error("live EVM endpoint should be preserved")
	}
	if !strings.Contains(string(out), "evm-http-jsonrpc") {
		t.Error("the category key must survive the edit")
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

// The verification commands must mirror how the probe dialed (checks.ParseGRPCTarget), or a
// maintainer's copy-paste fails for its own reasons and falsely "confirms" a dead endpoint:
// schemes stripped, bare hosts get the :443 the probe dials, and the TLS mode matches.
func TestBuildPRBody_GRPCVerifyCommandsMirrorProbe(t *testing.T) {
	chain := registry.Chain{Name: "testchain", ChainID: "test-1"}
	body := github.BuildPRBody(chain, []github.FlaggedEndpoint{
		dead("grpc_liveness", "https://grpc.tls.example:443"),
		dead("grpc_liveness", "http://grpc.plain.example:9090"),
		dead("grpc_liveness", "grpc.bare.example"),
		dead("grpc_liveness", "grpc.nonstd.example:9220"),
	})
	for _, want := range []string{
		"grpcurl grpc.tls.example:443 cosmos.base",           // scheme stripped, TLS stated by operator
		"grpcurl -plaintext grpc.plain.example:9090 cosmos.", // scheme stripped, plaintext stated
		"grpcurl grpc.bare.example:443 cosmos.",              // bare host gets the :443 the probe dials
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
	if !strings.Contains(body, "grpcurl grpc.nonstd.example:9220 cosmos") ||
		!strings.Contains(body, "retry with -plaintext") {
		t.Error("nonstandard port must show the TLS-first command with the plaintext retry note")
	}
	if strings.Contains(body, "grpcurl https://") || strings.Contains(body, "grpcurl -plaintext http") {
		t.Error("a scheme must never reach a grpcurl command")
	}
}

func writeAssetListJSON(t *testing.T, dir, chainName string, assets []any) {
	t.Helper()
	chainDir := filepath.Join(dir, chainName)
	if err := os.MkdirAll(chainDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	doc := map[string]any{"chain_name": chainName, "assets": assets}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(chainDir, "assetlist.json"), append(b, '\n'), 0o600); err != nil {
		t.Fatalf("write assetlist.json: %v", err)
	}
}

func TestEditAssetListJSON_fixOne(t *testing.T) {
	dir := t.TempDir()
	writeAssetListJSON(t, dir, "osmosis", []any{
		map[string]any{
			"name": "Cosmos Hub Atom",
			"base": "ibc/WRONGHASH",
			"denom_units": []any{
				map[string]any{"denom": "ibc/WRONGHASH", "exponent": 0},
				map[string]any{"denom": "atom", "exponent": 6},
			},
			"traces": []any{
				map[string]any{
					"type":  "ibc",
					"chain": map[string]any{"path": "transfer/channel-0/uatom"},
				},
			},
		},
	})
	fixes := []github.HashFix{{
		AssetName: "Cosmos Hub Atom",
		Base:      "ibc/WRONGHASH",
		Expected:  "ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2",
		Path:      "transfer/channel-0/uatom",
	}}
	out, err := github.EditAssetListJSON(dir, "osmosis", fixes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil {
		t.Fatal("want non-nil output")
	}
	if strings.Contains(string(out), "ibc/WRONGHASH") {
		t.Error("wrong hash should be replaced everywhere, including denom_units")
	}
	if got := strings.Count(string(out), "27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2"); got != 2 {
		t.Errorf("correct hash should appear in base and denom_units, got %d occurrence(s)", got)
	}
	if !strings.Contains(string(out), `"denom": "atom"`) {
		t.Error("unrelated denom_units entry should be untouched")
	}
	if !strings.Contains(string(out), "transfer/channel-0/uatom") {
		t.Error("trace path should be preserved")
	}
}

func TestEditAssetListJSON_duplicateBaseLeavesLegitAssetUntouched(t *testing.T) {
	dir := t.TempDir()
	// Asset one legitimately owns the hash; asset two copy-pasted it from
	// asset one's IBC connection while declaring a different trace path.
	// Only asset two may be rewritten.
	const atomHash = "ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2"
	writeAssetListJSON(t, dir, "zigchain", []any{
		map[string]any{
			"name": "Cosmos Hub Atom",
			"base": atomHash,
			"denom_units": []any{
				map[string]any{"denom": atomHash, "exponent": 0},
			},
			"traces": []any{
				map[string]any{
					"type":  "ibc",
					"chain": map[string]any{"path": "transfer/channel-0/uatom"},
				},
			},
		},
		map[string]any{
			"name": "Noble USDC",
			"base": atomHash,
			"denom_units": []any{
				map[string]any{"denom": atomHash, "exponent": 0},
			},
			"traces": []any{
				map[string]any{
					"type":  "ibc",
					"chain": map[string]any{"path": "transfer/channel-2/uusdc"},
				},
			},
		},
	})
	fixes := []github.HashFix{{
		AssetName: "Noble USDC",
		Base:      atomHash,
		Expected:  "ibc/USDCEXPECTED",
		Path:      "transfer/channel-2/uusdc",
	}}
	out, err := github.EditAssetListJSON(dir, "zigchain", fixes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil {
		t.Fatal("want non-nil output")
	}
	if got := strings.Count(string(out), atomHash); got != 2 {
		t.Errorf("legit asset's base and denom_units must keep the hash, want 2 occurrences, got %d", got)
	}
	if got := strings.Count(string(out), "ibc/USDCEXPECTED"); got != 2 {
		t.Errorf("flagged asset's base and denom_units must be rewritten, want 2 occurrences, got %d", got)
	}
}

func TestEditAssetListJSON_noOp(t *testing.T) {
	dir := t.TempDir()
	writeAssetListJSON(t, dir, "osmosis", []any{
		map[string]any{"name": "Atom", "base": "ibc/CORRECT"},
	})
	fixes := []github.HashFix{{
		AssetName: "Atom",
		Base:      "ibc/NOTPRESENT",
		Expected:  "ibc/SOMETHING",
		Path:      "transfer/channel-0/uatom",
	}}
	out, err := github.EditAssetListJSON(dir, "osmosis", fixes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Error("want nil (no-op) when base not in file")
	}
}

func TestBuildHashMismatchPRBody(t *testing.T) {
	fixes := []github.HashFix{
		{AssetName: "Atom", Base: "ibc/WRONG", Expected: "ibc/CORRECT", Path: "transfer/channel-0/uatom"},
	}
	body := github.BuildHashMismatchPRBody("osmosis", fixes)
	if body == "" {
		t.Fatal("want non-empty body")
	}
	if !strings.Contains(body, "osmosis") {
		t.Error("body should contain chain name")
	}
	if !strings.Contains(body, "| Asset |") {
		t.Error("body should contain table header")
	}
	if !strings.Contains(body, "ibc/WRONG") {
		t.Error("body should contain declared hash")
	}
	if !strings.Contains(body, "ibc/CORRECT") {
		t.Error("body should contain expected hash")
	}
}

// writeStatusChainJSON writes a chain.json fixture into a temp registry and returns the
// registry path. Field order and unknown fields matter: EditChainStatus must be a surgical
// one-field flip, since the PR diff is the product.
func writeStatusChainJSON(t *testing.T, status string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "deadchain"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `{
  "$schema": "../chain.schema.json",
  "chain_name": "deadchain",
  "status": "` + status + `",
  "chain_id": "deadchain-1",
  "extra": "preserved"
}
`
	if err := os.WriteFile(filepath.Join(dir, "deadchain", "chain.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir
}

func TestEditChainStatus_FlipsLiveToKilled(t *testing.T) {
	dir := writeStatusChainJSON(t, "live")
	out, err := github.EditChainStatus(dir, "deadchain")
	if err != nil {
		t.Fatalf("EditChainStatus: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"status": "killed"`) {
		t.Errorf("status not flipped:\n%s", s)
	}
	if strings.Contains(s, `"live"`) {
		t.Errorf("live still present:\n%s", s)
	}
	if !strings.Contains(s, `"$schema": "../chain.schema.json"`) || !strings.Contains(s, `"extra": "preserved"`) {
		t.Errorf("surrounding fields disturbed:\n%s", s)
	}
	if !strings.HasPrefix(s, "{\n  \"$schema\"") {
		t.Errorf("field order not preserved:\n%s", s)
	}
}

// The registry may have been fixed between detection and PR; flipping anything other than
// live→killed is never correct, so a non-live status is a no-op, not an error.
func TestEditChainStatus_NoOpWhenNotLive(t *testing.T) {
	dir := writeStatusChainJSON(t, "killed")
	out, err := github.EditChainStatus(dir, "deadchain")
	if err != nil {
		t.Fatalf("EditChainStatus: %v", err)
	}
	if out != nil {
		t.Errorf("want nil (no-op) when status is not live, got:\n%s", out)
	}
}

func TestBuildStatusPRBody(t *testing.T) {
	ev := github.StatusPREvidence{
		Streak:          14,
		FirstSeen:       time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		EndpointsProbed: 53,
		ClassLines:      []string{"| `rpc` | 0/20 live — dns_nxdomain (18), timeout (2) |"},
		Withdrawn: []github.WithdrawnOperator{
			{Domain: "polkachu.com", LiveElsewhere: 108, DeadHere: 3},
		},
		NewestBlockTime: time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC),
		SampleHost:      "evmos-rpc.polkachu.com",
	}
	body := github.BuildStatusPRBody("evmos", ev)
	for _, want := range []string{
		"14 consecutive sentinel runs",
		"2026-07-14",
		"53 declared endpoints",
		"dns_nxdomain (18)",
		"polkachu.com",
		"2026-05-18T12:00:00Z",
		"dig +short evmos-rpc.polkachu.com",
		"close this PR",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestBuildSupersededComment(t *testing.T) {
	body := github.BuildSupersededComment("evmos", 41)
	for _, want := range []string{
		"#41",
		"`evmos`",
		"`killed`",
		"can be closed",
		"never closes PRs",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q\n--- comment ---\n%s", want, body)
		}
	}
}
