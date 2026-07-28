package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/state"
)

func TestHostOfAndDomainOf(t *testing.T) {
	tests := []struct {
		address, host, domain string
	}{
		{"https://rpc.cosmos.example.com:443/", "rpc.cosmos.example.com", "example.com"},
		{"https://rpc.example.com/status", "rpc.example.com", "example.com"},
		{"grpc.osmosis.example.io:9090", "grpc.osmosis.example.io", "example.io"},
		{"http://185.245.182.192:46657", "185.245.182.192", "185.245.182.192"},
		{"wss://ws.example.network", "ws.example.network", "example.network"},
		{"localhost", "localhost", "localhost"},
		// The documented last-two-labels limitation: multi-part public suffixes collapse.
		{"https://rpc.host.com.ua", "rpc.host.com.ua", "com.ua"},
	}
	for _, tt := range tests {
		if got := hostOf(tt.address); got != tt.host {
			t.Errorf("hostOf(%q) = %q, want %q", tt.address, got, tt.host)
		}
		if got := domainOf(tt.host); got != tt.domain {
			t.Errorf("domainOf(%q) = %q, want %q", tt.host, got, tt.domain)
		}
	}
}

// fixtureResults builds one run shaped like the real findings: a vanished operator, an
// answering-but-broken operator, a partially rotten domain, a healthy domain with quality
// caveats, one rate-limited endpoint and one chain-ID mismatch.
func fixtureResults() []checks.Result {
	res := func(chain, chainType, check, endpoint, provider string, passed bool) checks.Result {
		return checks.Result{
			Chain: chain, ChainID: chain + "-1", ChainType: chainType,
			Check: check, Endpoint: endpoint, Provider: provider, Passed: passed,
		}
	}
	fail := func(chain, chainType, check, endpoint, provider string, class checks.FailureClass) checks.Result {
		r := res(chain, chainType, check, endpoint, provider, false)
		r.FailureClass = class
		r.Evidence = "evidence for " + endpoint
		return r
	}

	live1 := res("alivechain", "cosmos", "rpc_liveness", "https://rpc.healthy.example", "", true)
	live1.CatchingUp = true
	live2 := res("alivechain", "cosmos", "rest_liveness", "https://rest.healthy.example", "", true)
	live2.TxIndex = "off"

	rateLimited := fail("alivechain", "cosmos", "grpc_web_liveness", "https://gw.healthy.example", "", checks.ClassHTTP429)
	rateLimited.Skipped = true

	skippedChainID := checks.Result{
		Chain: "deadchain", ChainType: "cosmos", Check: "rpc_chain_id",
		Endpoint: "https://rpc-1.gonecorp.example", Skipped: true,
	}
	mismatch := fail("alivechain", "cosmos", "rpc_chain_id", "https://rpc.healthy.example", "", checks.ClassChainIDMismatch)

	results := []checks.Result{
		// gonecorp.example: 3 endpoints, all NXDOMAIN, 2 chains -> "operator gone"
		fail("deadchain", "cosmos", "rpc_liveness", "https://rpc-1.gonecorp.example", "Gonecorp", checks.ClassDNSNXDomain),
		fail("deadchain", "cosmos", "rest_liveness", "https://rest.gonecorp.example", "Gonecorp", checks.ClassDNSNXDomain),
		fail("flakychain", "cosmos", "grpc_liveness", "grpc.gonecorp.example:9090", "Gonecorp", checks.ClassDNSNXDomain),
		// wrongpath.example: 4 endpoints, all 404, 3 chains -> "answering but broken"
		fail("deadchain", "cosmos", "rpc_liveness", "https://rpc-2.wrongpath.example", "", checks.ClassHTTP404),
		fail("deadchain", "cosmos", "rest_liveness", "https://lcd.wrongpath.example", "", checks.ClassHTTP404),
		fail("evmchain", "eip155", "evm_liveness", "https://evm.wrongpath.example", "", checks.ClassHTTP404),
		fail("flakychain", "cosmos", "rest_liveness", "https://api.wrongpath.example", "", checks.ClassHTTP404),
		// flaky.example: 1 live + 1 timeout -> partial rot, keeps flakychain reachable
		res("flakychain", "cosmos", "rpc_liveness", "https://rpc-1.flaky.example", "", true),
		fail("flakychain", "cosmos", "rpc_liveness", "https://rpc-2.flaky.example", "", checks.ClassTimeout),
		// healthy.example: live with quality caveats
		live1,
		live2,
		rateLimited,
		skippedChainID,
		mismatch,
	}

	// Stamp registry order on the first-listed core endpoint of each chain, as runProbe does:
	// deadchain and evmchain lead with a dead endpoint, alivechain and flakychain with a live
	// one — so the first-listed metric reads 2 of 4.
	firstListed := map[string]bool{
		"https://rpc-1.gonecorp.example": true, // deadchain, dead
		"https://rpc.healthy.example":    true, // alivechain, live
		"https://rpc-1.flaky.example":    true, // flakychain, live
		"https://evm.wrongpath.example":  true, // evmchain, dead
	}
	for i := range results {
		if firstListed[results[i].Endpoint] && strings.HasSuffix(results[i].Check, "_liveness") {
			results[i].EndpointOrder = 1
		}
	}
	return results
}

var runTS = time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)

func TestBuildFiltersSortsAndJoinsStreaks(t *testing.T) {
	stateMap := map[string]state.ChainState{
		"deadchain": {Endpoints: map[string]state.EndpointState{
			state.EndpointKey("rpc_liveness", "https://rpc-1.gonecorp.example"): {ConsecutiveFailures: 7},
		}},
	}
	records := Build(fixtureResults(), stateMap, runTS, "test")

	// 14 results, minus the skipped chain-ID pipeline artifact; the skipped-but-classified
	// rate-limited record stays.
	if len(records) != 13 {
		t.Fatalf("len(records) = %d, want 13", len(records))
	}
	for i := 1; i < len(records); i++ {
		a, b := records[i-1], records[i]
		if a.Chain > b.Chain || (a.Chain == b.Chain && a.Check > b.Check) {
			t.Fatalf("records not sorted at %d: %s/%s after %s/%s", i, b.Chain, b.Check, a.Chain, a.Check)
		}
	}
	var found bool
	for i := range records {
		r := &records[i]
		if r.Check == "rpc_chain_id" && r.Skipped {
			t.Error("skipped chain-ID record survived Build; it carries no information")
		}
		if r.Endpoint == "https://rpc-1.gonecorp.example" && r.Check == "rpc_liveness" {
			found = true
			if r.Streak != 7 {
				t.Errorf("Streak = %d, want 7 from state", r.Streak)
			}
			if r.Domain != "gonecorp.example" {
				t.Errorf("Domain = %q, want gonecorp.example", r.Domain)
			}
			if r.Evidence == "" {
				t.Error("Evidence must ride along with FailureClass in every record")
			}
		}
	}
	if !found {
		t.Fatal("expected record for rpc-1.gonecorp.example not built")
	}
}

func TestWriteRunLoadFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	records := Build(fixtureResults(), nil, runTS, "vantage/one")

	path, err := WriteRun(dir, records)
	if err != nil {
		t.Fatalf("WriteRun: %v", err)
	}
	if !strings.HasSuffix(path, "20260728T100000Z-vantage-one.jsonl") {
		t.Errorf("unexpected file name %q; vantage must be sanitized and timestamp stable", path)
	}

	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !reflect.DeepEqual(records, loaded) {
		t.Errorf("round trip mismatch: wrote %d records, loaded %d", len(records), len(loaded))
	}
}

// A file written by WriteRun holds exactly one run; a hand-concatenated file does not, and
// rendering the mix would double-count every endpoint. LoadFile must refuse, not pick.
func TestLoadFileRejectsMixedRuns(t *testing.T) {
	dir := t.TempDir()
	older := Build(fixtureResults(), nil, runTS.Add(-24*time.Hour), "old")
	newer := Build(fixtureResults(), nil, runTS, "new")

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, rec := range append(older, newer...) {
		if err := enc.Encode(rec); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	path := filepath.Join(dir, "concatenated.jsonl")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if _, err := LoadFile(path); err == nil {
		t.Fatal("LoadFile accepted a file containing two runs")
	} else if !strings.Contains(err.Error(), "2 distinct runs") {
		t.Errorf("error should name the run count, got: %v", err)
	}
}

func TestRenderComputesTheHeadlineNumbers(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, Build(fixtureResults(), nil, runTS, "test"))
	out := buf.String()

	// 11 measured endpoints: 3 live, 8 dead. Structural = 3 dns + 4 http_404; timeout is
	// ambiguous. Every assertion below is a number the report is quoted on.
	for _, want := range []string{
		"11 endpoints: 3 live, 8 dead",
		"8 failures: 7 structural (87.5%), 1 ambiguous or possibly self-inflicted (12.5%)",
		"chain ID mismatches: 1",
		"rate-limited: 1 endpoints could not be measured",
		// deadchain (rpc+rest all dead) and evmchain (evm dead) of 4 chains; flakychain is
		// reachable through its one live RPC.
		"2 (50.0%) have no live RPC, REST or EVM endpoint",
		"deadchain, evmchain",
		// remedy buckets, keyed by dominant class
		"operator gone (DNS dead)",
		"gonecorp.example",
		"server answering, endpoints broken",
		"wrongpath.example",
		// quality and coverage: 3 gonecorp records carry a provider, of 11 measured
		"1 still catching up (answering but behind), 1 with tx_index off",
		"provider field present on 27.3% of endpoint entries (1 distinct providers named)",
		// deadchain and evmchain lead with a dead endpoint; alivechain and flakychain do not
		"first-listed endpoint (what most tooling defaults to): dead on 2 of 4 chains (50.0%)",
		// alivechain (2 endpoints, healthy.example) and evmchain (1, wrongpath.example) sit on
		// one domain each; deadchain and flakychain span several
		"2 chains depend on a single registrable domain",
		"2 endpoint(s), all on healthy.example",
		"1 endpoint(s), all on wrongpath.example",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n--- full output ---\n%s", want, out)
		}
	}

	// flaky.example has a live endpoint, so it must never appear from the remedy taxonomy
	// onward — everything after that section heading concerns fully-dead domains or chains.
	idx := strings.Index(out, "Fully dead domains")
	if idx < 0 {
		t.Fatal("remedy taxonomy section missing from report")
	}
	if strings.Contains(out[idx:], "flaky.example") {
		t.Error("partially live domain listed in the fully-dead remedy taxonomy")
	}

	// The fixture's failures are all endpoint-side, so the report must not question itself.
	if strings.Contains(out, "WARNING") {
		t.Error("vantage warning shown for a run with no vantage-side failure classes")
	}
	// Every fixture chain lists at least one core endpoint, so the no-core line must not appear.
	if strings.Contains(out, "list no RPC, REST or EVM endpoint") {
		t.Error("no-core-endpoint line rendered although every chain has core endpoints")
	}
}

// A chain listing only gRPC/WSS endpoints has nothing standard tooling can use, and is excluded
// from the unreachable count (nothing core was probed). It must be named, not silently dropped —
// idep and imversed are real occurrences in the registry.
func TestRenderNamesChainsWithoutCoreEndpoints(t *testing.T) {
	results := []checks.Result{
		{
			Chain: "grpconly", ChainID: "grpconly-1", ChainType: "cosmos",
			Check: "grpc_liveness", Endpoint: "grpc.solo.example:443",
			FailureClass: checks.ClassDNSNXDomain, Evidence: "no such host",
		},
		{
			Chain: "normal", ChainID: "normal-1", ChainType: "cosmos",
			Check: "rpc_liveness", Endpoint: "https://rpc.normal.example", Passed: true,
		},
	}
	var buf bytes.Buffer
	Render(&buf, Build(results, nil, runTS, "test"))
	out := buf.String()

	if !strings.Contains(out, "1 chain(s) list no RPC, REST or EVM endpoint at all") {
		t.Errorf("missing no-core-endpoint line\n--- full output ---\n%s", out)
	}
	if !strings.Contains(out, "grpconly") {
		t.Error("the no-core chain must be named")
	}
}

// Records written before the order field existed must not produce a first-listed line at all —
// rendering "dead on 0 of 0 chains" from data that simply lacks the field would be a lie.
func TestRenderSkipsFirstListedWithoutOrderData(t *testing.T) {
	results := fixtureResults()
	for i := range results {
		results[i].EndpointOrder = 0
	}
	var buf bytes.Buffer
	Render(&buf, Build(results, nil, runTS, "test"))
	if strings.Contains(buf.String(), "first-listed endpoint") {
		t.Error("first-listed section rendered from records that predate the order field")
	}
}

// A run dominated by resolver failures and missing routes must lead with a warning: those
// classes describe the probing machine, and a failing resolver also produces false NXDOMAINs,
// so nothing in such a run is quotable.
func TestRenderWarnsOnUnhealthyVantage(t *testing.T) {
	results := fixtureResults()
	for i := range results {
		if !results[i].Passed && !results[i].Skipped && strings.HasSuffix(results[i].Check, "_liveness") {
			results[i].FailureClass = checks.ClassDNSFailure
		}
	}
	var buf bytes.Buffer
	Render(&buf, Build(results, nil, runTS, "test"))
	out := buf.String()

	if !strings.Contains(out, "WARNING: this vantage looks unhealthy") {
		t.Error("expected vantage warning when failures are dominated by dns_failure")
	}
	if !strings.Contains(out, "8 of 8 failures (100.0%)") {
		t.Errorf("warning should quantify the suspect share\n--- full output ---\n%s", out)
	}
}
