package state_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/state"
)

var testNow = time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

func makeState(chainID string) state.ChainState {
	return state.ChainState{
		ChainID:   chainID,
		Endpoints: make(map[string]state.EndpointState),
	}
}

func failResult(endpoint, evidence string) checks.Result {
	return checks.Result{Chain: "testchain", ChainID: "testchain-1", Check: "rpc_liveness", Endpoint: endpoint, Evidence: evidence}
}

func passResult(endpoint string) checks.Result {
	return checks.Result{Chain: "testchain", ChainID: "testchain-1", Check: "rpc_liveness", Endpoint: endpoint, Passed: true}
}

// Load tests

func TestLoad_FileNotExist(t *testing.T) {
	cs, err := state.Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("want nil error, got %v", err)
	}
	if cs.Endpoints == nil {
		t.Error("want non-nil Endpoints map")
	}
}

func TestLoad_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cosmoshub.json")
	cs := makeState("cosmoshub-4")
	cs.Update(failResult("https://rpc.cosmos.network", "connection refused"), testNow)
	if err := state.Save(path, cs, testNow); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := state.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ChainID != "cosmoshub-4" {
		t.Errorf("chain_id: got %q want %q", got.ChainID, "cosmoshub-4")
	}
	key := state.EndpointKey("rpc_liveness", "https://rpc.cosmos.network")
	ep, ok := got.Endpoints[key]
	if !ok {
		t.Fatalf("endpoint key %q not found", key)
	}
	if ep.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures: got %d want 1", ep.ConsecutiveFailures)
	}
}

func TestLoad_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := state.Load(path)
	if err == nil {
		t.Error("want error for corrupt JSON, got nil")
	}
}

func TestLoad_NullEndpoints(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "null.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"chain_id":"x","endpoints":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := state.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cs.Endpoints == nil {
		t.Error("want non-nil Endpoints after null in JSON")
	}
}

func TestLoad_UnknownVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"chain_id":"x","endpoints":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := state.Load(path)
	if err == nil {
		t.Error("want error for unknown schema version, got nil")
	}
}

// Save tests

func TestSave_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "cosmoshub.json")
	if err := state.Save(path, makeState("cosmoshub-4"), testNow); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestSave_SetsUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := state.Save(path, makeState("x"), testNow); err != nil {
		t.Fatal(err)
	}
	cs, err := state.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cs.UpdatedAt.Equal(testNow) {
		t.Errorf("updated_at: got %v want %v", cs.UpdatedAt, testNow)
	}
}

func TestSave_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := state.Save(path, makeState("x"), testNow); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("file permissions: got %o want 0o644", got)
	}
}

func TestSave_NoTmpFileLeft(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := state.Save(path, makeState("x"), testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("want .tmp file to be removed after save")
	}
}

func TestSave_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	cs := makeState("cosmoshub-4")
	cs.Update(failResult("https://rpc.example.com", "timeout"), testNow)
	cs.Update(failResult("https://rpc.example.com", "timeout"), testNow.Add(24*time.Hour))
	if err := state.Save(path, cs, testNow); err != nil {
		t.Fatal(err)
	}
	got, err := state.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	key := state.EndpointKey("rpc_liveness", "https://rpc.example.com")
	ep := got.Endpoints[key]
	if ep.ConsecutiveFailures != 2 {
		t.Errorf("consecutive_failures: got %d want 2", ep.ConsecutiveFailures)
	}
	if !ep.FirstFailureTime.Equal(testNow) {
		t.Errorf("first_failure_time: got %v want %v", ep.FirstFailureTime, testNow)
	}
}

// Update tests

func TestUpdate_FirstFailure(t *testing.T) {
	cs := makeState("x")
	cs.Update(failResult("https://rpc.example.com", "refused"), testNow)
	ep := cs.Endpoints[state.EndpointKey("rpc_liveness", "https://rpc.example.com")]
	if ep.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures: got %d want 1", ep.ConsecutiveFailures)
	}
	if !ep.FirstFailureTime.Equal(testNow) {
		t.Errorf("first_failure_time: got %v want %v", ep.FirstFailureTime, testNow)
	}
	if ep.LastPassed {
		t.Error("want last_passed=false")
	}
	if ep.LastEvidence != "refused" {
		t.Errorf("last_evidence: got %q want %q", ep.LastEvidence, "refused")
	}
	if ep.FirstEvidence != "refused" {
		t.Errorf("first_evidence: got %q want %q (should match last_evidence on first failure)", ep.FirstEvidence, "refused")
	}
}

func TestUpdate_SecondFailure(t *testing.T) {
	cs := makeState("x")
	ep := "https://rpc.example.com"
	cs.Update(failResult(ep, "refused"), testNow)
	later := testNow.Add(24 * time.Hour)
	cs.Update(failResult(ep, "timeout"), later)
	got := cs.Endpoints[state.EndpointKey("rpc_liveness", ep)]
	if got.ConsecutiveFailures != 2 {
		t.Errorf("consecutive_failures: got %d want 2", got.ConsecutiveFailures)
	}
	if !got.FirstFailureTime.Equal(testNow) {
		t.Error("first_failure_time should not change during streak")
	}
	if got.FirstEvidence != "refused" {
		t.Errorf("first_failure_evidence: got %q want %q (should not change during streak)", got.FirstEvidence, "refused")
	}
	if got.LastEvidence != "timeout" {
		t.Errorf("last_evidence: got %q want %q", got.LastEvidence, "timeout")
	}
	if !got.LastChecked.Equal(later) {
		t.Errorf("last_checked: got %v want %v", got.LastChecked, later)
	}
}

func TestUpdate_PassAfterFailure(t *testing.T) {
	cs := makeState("x")
	ep := "https://rpc.example.com"
	cs.Update(failResult(ep, "refused"), testNow)
	cs.Update(failResult(ep, "refused"), testNow.Add(24*time.Hour))
	cs.Update(passResult(ep), testNow.Add(48*time.Hour))
	got := cs.Endpoints[state.EndpointKey("rpc_liveness", ep)]
	if got.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures: got %d want 0", got.ConsecutiveFailures)
	}
	if !got.FirstFailureTime.IsZero() {
		t.Error("first_failure_time should be zero after pass")
	}
	if got.LastEvidence != "" {
		t.Errorf("last_evidence should be cleared, got %q", got.LastEvidence)
	}
	if got.FirstEvidence != "" {
		t.Errorf("first_failure_evidence should be cleared, got %q", got.FirstEvidence)
	}
	if !got.LastPassed {
		t.Error("want last_passed=true")
	}
}

func TestUpdate_FailAfterPass(t *testing.T) {
	cs := makeState("x")
	ep := "https://rpc.example.com"
	cs.Update(passResult(ep), testNow)
	later := testNow.Add(24 * time.Hour)
	cs.Update(failResult(ep, "refused"), later)
	got := cs.Endpoints[state.EndpointKey("rpc_liveness", ep)]
	if got.ConsecutiveFailures != 1 {
		t.Errorf("consecutive_failures: got %d want 1", got.ConsecutiveFailures)
	}
	if !got.FirstFailureTime.Equal(later) {
		t.Errorf("first_failure_time: got %v want %v (new streak)", got.FirstFailureTime, later)
	}
}

// Prune tests

func TestPrune_RemovesStaleKey(t *testing.T) {
	cs := makeState("x")
	cs.Update(failResult("https://old.example.com", "refused"), testNow)
	cs.Update(failResult("https://new.example.com", "refused"), testNow)
	active := map[string]struct{}{
		state.EndpointKey("rpc_liveness", "https://new.example.com"): {},
	}
	cs.Prune(active)
	if _, ok := cs.Endpoints[state.EndpointKey("rpc_liveness", "https://old.example.com")]; ok {
		t.Error("stale key should have been pruned")
	}
	if _, ok := cs.Endpoints[state.EndpointKey("rpc_liveness", "https://new.example.com")]; !ok {
		t.Error("active key should be kept")
	}
}

func TestPrune_KeepsAllActiveKeys(t *testing.T) {
	cs := makeState("x")
	key := state.EndpointKey("rpc_liveness", "https://rpc.example.com")
	cs.Update(failResult("https://rpc.example.com", "refused"), testNow)
	cs.Prune(map[string]struct{}{key: {}})
	if _, ok := cs.Endpoints[key]; !ok {
		t.Error("active key should not be pruned")
	}
}

func TestPrune_EmptyActiveKeys(t *testing.T) {
	cs := makeState("x")
	cs.Update(failResult("https://rpc.example.com", "refused"), testNow)
	cs.Prune(map[string]struct{}{})
	if len(cs.Endpoints) != 0 {
		t.Errorf("want empty endpoints after pruning all, got %d", len(cs.Endpoints))
	}
}

// EndpointKey test

func TestEndpointKey_Format(t *testing.T) {
	got := state.EndpointKey("rpc_liveness", "https://rpc.cosmos.network")
	want := "rpc_liveness|https://rpc.cosmos.network"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
