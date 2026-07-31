package main

import (
	"testing"
	"time"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/state"
)

var deathNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

// testStale mirrors the --chain-death-stale-after default.
const testStale = 7 * 24 * time.Hour

func livenessResult(chain, check, endpoint string, passed bool, class checks.FailureClass) checks.Result {
	return checks.Result{Chain: chain, Check: check, Endpoint: endpoint, Passed: passed, FailureClass: class}
}

// deadChainResults models the Evmos pattern: every endpoint dead, three of the operators
// demonstrably alive on another chain.
func deadChainResults() []checks.Result {
	return []checks.Result{
		// The dead chain: 3 rpc + 1 rest, three distinct operators.
		livenessResult("deadchain", "rpc_liveness", "https://dead-rpc.opa.example", false, checks.ClassDNSNXDomain),
		livenessResult("deadchain", "rpc_liveness", "https://dead-rpc.opb.example", false, checks.ClassDNSNXDomain),
		livenessResult("deadchain", "rpc_liveness", "https://dead-rpc.opc.example", false, checks.ClassTimeout),
		livenessResult("deadchain", "rest_liveness", "https://dead-rest.opa.example", false, checks.ClassDNSNXDomain),
		// The same operators, alive on a healthy chain.
		livenessResult("alivechain", "rpc_liveness", "https://rpc.opa.example", true, checks.ClassNone),
		livenessResult("alivechain", "rest_liveness", "https://rest.opb.example", true, checks.ClassNone),
		livenessResult("alivechain", "grpc_liveness", "grpc.opc.example:443", true, checks.ClassNone),
	}
}

func stateFor(chains ...string) map[string]state.ChainState {
	m := make(map[string]state.ChainState, len(chains))
	for _, c := range chains {
		m[c] = state.ChainState{Endpoints: map[string]state.EndpointState{}}
	}
	return m
}

func TestChainDeathStreakAdvancesAndMatures(t *testing.T) {
	stateMap := stateFor("deadchain", "alivechain")
	results := deadChainResults()

	// Run 1: streak starts, no candidate yet — but the chain already counts as looking dead,
	// which keeps the per-endpoint PR flows away during the maturing window.
	candidates, looksDead, _, _ := runChainDeathDetection(results, stateMap, 2, testStale, deathNow)
	if len(candidates) != 0 {
		t.Fatalf("run 1: no candidate expected, got %v", candidates)
	}
	if !looksDead["deadchain"] || looksDead["alivechain"] {
		t.Fatalf("run 1: looksDead must hold deadchain only, got %v", looksDead)
	}
	if cs := stateMap["deadchain"]; cs.ChainDeadStreak != 1 || cs.ChainDeadFirstTime.IsZero() {
		t.Fatalf("run 1: streak = %d, firstTime zero=%v; want 1, false", cs.ChainDeadStreak, cs.ChainDeadFirstTime.IsZero())
	}
	if cs := stateMap["alivechain"]; cs.ChainDeadStreak != 0 {
		t.Fatalf("alivechain must not accumulate a streak, got %d", cs.ChainDeadStreak)
	}

	// Run 2: matures.
	candidates, _, facts, domainLive := runChainDeathDetection(results, stateMap, 2, testStale, deathNow.Add(24*time.Hour))
	if len(candidates) != 1 || candidates[0] != "deadchain" {
		t.Fatalf("run 2: want [deadchain], got %v", candidates)
	}
	// The evidence must name the withdrawn operators.
	ev := buildStatusEvidence(facts["deadchain"], domainLive, stateMap["deadchain"])
	if ev.Streak != 2 || ev.EndpointsProbed != 4 {
		t.Errorf("evidence streak=%d probed=%d, want 2, 4", ev.Streak, ev.EndpointsProbed)
	}
	if len(ev.Withdrawn) != 3 {
		t.Errorf("want 3 withdrawn operators, got %d: %+v", len(ev.Withdrawn), ev.Withdrawn)
	}
}

func TestChainDeathStreakResetsOnRecovery(t *testing.T) {
	stateMap := stateFor("deadchain", "alivechain")
	cs := stateMap["deadchain"]
	cs.ChainDeadStreak = 5
	cs.ChainDeadFirstTime = deathNow.Add(-5 * 24 * time.Hour)
	stateMap["deadchain"] = cs

	results := deadChainResults()
	// One RPC recovers.
	results[0].Passed = true
	results[0].FailureClass = checks.ClassNone

	if candidates, _, _, _ := runChainDeathDetection(results, stateMap, 2, testStale, deathNow); len(candidates) != 0 {
		t.Fatalf("recovered chain must not be a candidate, got %v", candidates)
	}
	if cs := stateMap["deadchain"]; cs.ChainDeadStreak != 0 || !cs.ChainDeadFirstTime.IsZero() {
		t.Errorf("streak must reset on recovery, got streak=%d", cs.ChainDeadStreak)
	}
}

// Fewer than minWithdrawnOperators alive elsewhere: "all dead" cannot be told apart from "all
// its operators died" — no streak movement toward death.
func TestChainDeathRequiresWithdrawalSignature(t *testing.T) {
	stateMap := stateFor("deadchain")
	results := []checks.Result{
		livenessResult("deadchain", "rpc_liveness", "https://rpc.gone1.example", false, checks.ClassDNSNXDomain),
		livenessResult("deadchain", "rest_liveness", "https://rest.gone2.example", false, checks.ClassDNSNXDomain),
	}
	runChainDeathDetection(results, stateMap, 1, testStale, deathNow)
	if cs := stateMap["deadchain"]; cs.ChainDeadStreak != 0 {
		t.Errorf("no withdrawal signature → no death streak, got %d", cs.ChainDeadStreak)
	}
}

// Guard 1: a suspect vantage freezes every streak — no advance AND no reset, because a broken
// resolver manufactures "every chain unreachable" in one run, and one bad vantage day must not
// destroy weeks of accumulated evidence either way.
func TestChainDeathFrozenOnSuspectVantage(t *testing.T) {
	stateMap := stateFor("deadchain", "alivechain")
	cs := stateMap["deadchain"]
	cs.ChainDeadStreak = 7
	stateMap["deadchain"] = cs

	results := deadChainResults()
	for i := range results {
		if !results[i].Passed {
			results[i].FailureClass = checks.ClassDNSFailure // resolver-side signature
		}
	}
	candidates, looksDead, _, _ := runChainDeathDetection(results, stateMap, 1, testStale, deathNow)
	if candidates != nil {
		t.Fatalf("suspect vantage must yield no candidates, got %v", candidates)
	}
	if looksDead != nil {
		t.Fatalf("suspect vantage must not mark chains as looking dead, got %v", looksDead)
	}
	if cs := stateMap["deadchain"]; cs.ChainDeadStreak != 7 {
		t.Errorf("streak must be frozen on suspect vantage, got %d (want 7)", cs.ChainDeadStreak)
	}
}

// Guard 2: no core checks ran for a chain → its streak must not move in either direction.
// Pre-empts the future --checks selector repeating the endpoint-pruning trap.
func TestChainDeathUntouchedWithoutCoreChecks(t *testing.T) {
	stateMap := stateFor("grpconly")
	cs := stateMap["grpconly"]
	cs.ChainDeadStreak = 3
	stateMap["grpconly"] = cs

	results := []checks.Result{
		livenessResult("grpconly", "grpc_liveness", "grpc.solo.example:443", false, checks.ClassDNSNXDomain),
	}
	runChainDeathDetection(results, stateMap, 1, testStale, deathNow)
	if cs := stateMap["grpconly"]; cs.ChainDeadStreak != 3 {
		t.Errorf("no core checks → streak untouched, got %d (want 3)", cs.ChainDeadStreak)
	}
}

// Small-chain adaptation: a 2-operator chain needs BOTH operators withdrawn (min(3,N), floor
// 2), so genuinely abandoned small chains are detectable without lowering the bar for large
// ones.
func TestChainDeathSmallChainNeedsAllOperators(t *testing.T) {
	small := func() []checks.Result {
		return []checks.Result{
			livenessResult("smallchain", "rpc_liveness", "https://rpc.opa.example", false, checks.ClassDNSNXDomain),
			livenessResult("smallchain", "rest_liveness", "https://rest.opb.example", false, checks.ClassDNSNXDomain),
			// Both operators alive on another chain.
			livenessResult("alivechain", "rpc_liveness", "https://a.opa.example", true, checks.ClassNone),
			livenessResult("alivechain", "rest_liveness", "https://b.opb.example", true, checks.ClassNone),
		}
	}
	stateMap := stateFor("smallchain", "alivechain")
	runChainDeathDetection(small(), stateMap, 1, testStale, deathNow)
	if cs := stateMap["smallchain"]; cs.ChainDeadStreak != 1 {
		t.Errorf("2-operator chain with both withdrawn must look dead, streak = %d", cs.ChainDeadStreak)
	}

	// Only one of the two alive elsewhere: not every witness testifies — no streak.
	results := small()
	results[3] = livenessResult("alivechain", "rest_liveness", "https://b.other.example", true, checks.ClassNone)
	stateMap = stateFor("smallchain", "alivechain")
	runChainDeathDetection(results, stateMap, 1, testStale, deathNow)
	if cs := stateMap["smallchain"]; cs.ChainDeadStreak != 0 {
		t.Errorf("1 of 2 witnesses is not enough, streak = %d", cs.ChainDeadStreak)
	}
}

// A single-operator chain is undetectable via the abandonment path by design: one witness is
// an anecdote, and a wrong kill PR on a live chain is the worst outcome this feature has.
func TestChainDeathSingleOperatorChainNeverAbandonedCandidate(t *testing.T) {
	stateMap := stateFor("solochain", "alivechain")
	results := []checks.Result{
		livenessResult("solochain", "rpc_liveness", "https://rpc.only.example", false, checks.ClassDNSNXDomain),
		livenessResult("alivechain", "rpc_liveness", "https://a.only.example", true, checks.ClassNone),
	}
	runChainDeathDetection(results, stateMap, 1, testStale, deathNow)
	if cs := stateMap["solochain"]; cs.ChainDeadStreak != 0 {
		t.Errorf("single-operator chain must not accumulate an abandonment streak, got %d", cs.ChainDeadStreak)
	}
}

// The disaster-recovery scenario: a chain halted for 10 days while its operator works on
// recovery must not accumulate a death streak when --chain-death-stale-after is raised above
// the outage length. The flag exists precisely so a known, managed outage never starts the
// clock toward a status-flip PR.
func TestChainDeathStaleAfterIsConfigurable(t *testing.T) {
	stateMap := stateFor("recovering")
	r := livenessResult("recovering", "rpc_liveness", "https://rpc.recovering.example", true, checks.ClassNone)
	r.LatestBlockTime = deathNow.Add(-10 * 24 * time.Hour)

	// Default 7d threshold: 10 days stale → dead-looking.
	runChainDeathDetection([]checks.Result{r}, stateMap, 1, testStale, deathNow)
	if cs := stateMap["recovering"]; cs.ChainDeadStreak != 1 {
		t.Fatalf("at 7d threshold a 10-day halt should look dead, streak = %d", cs.ChainDeadStreak)
	}

	// Raised to 14 days for the recovery window: same chain, no streak.
	stateMap = stateFor("recovering")
	runChainDeathDetection([]checks.Result{r}, stateMap, 1, 14*24*time.Hour, deathNow)
	if cs := stateMap["recovering"]; cs.ChainDeadStreak != 0 {
		t.Errorf("at 14d threshold a 10-day halt must not look dead, streak = %d", cs.ChainDeadStreak)
	}
}

// Path 2: a halted chain answers — every synced RPC reports a block time frozen past the
// staleness cutoff. Reachability is not the question; block production is.
func TestChainDeathDetectsHaltedChain(t *testing.T) {
	stateMap := stateFor("haltedchain")
	stale := deathNow.Add(-30 * 24 * time.Hour)
	r1 := livenessResult("haltedchain", "rpc_liveness", "https://rpc-1.halted.example", true, checks.ClassNone)
	r1.LatestBlockTime = stale
	r2 := livenessResult("haltedchain", "rpc_liveness", "https://rpc-2.halted.example", true, checks.ClassNone)
	r2.LatestBlockTime = stale.Add(time.Hour)

	candidates, _, facts, _ := runChainDeathDetection([]checks.Result{r1, r2}, stateMap, 1, testStale, deathNow)
	if len(candidates) != 1 || candidates[0] != "haltedchain" {
		t.Fatalf("want [haltedchain], got %v", candidates)
	}
	if got := facts["haltedchain"].newestBlock; !got.Equal(stale.Add(time.Hour)) {
		t.Errorf("newestBlock = %v, want the freshest stale time", got)
	}

	// One fresh node breaks the halted signature: the chain advances somewhere.
	r2.LatestBlockTime = deathNow.Add(-time.Minute)
	stateMap = stateFor("haltedchain")
	if candidates, _, _, _ := runChainDeathDetection([]checks.Result{r1, r2}, stateMap, 1, testStale, deathNow); len(candidates) != 0 {
		t.Fatalf("one fresh node must clear the halted signature, got %v", candidates)
	}
}

// The v0.8.1 regression pair: evm_liveness is a core check, so a chain whose Cosmos-side
// endpoints all died but whose EVM side still answers is NOT abandoned — one live core
// endpoint blocks the signature. The same chain with the EVM side dead too is a candidate.
// Before EVM endpoints were probed on cosmos chains, the live-EVM case was invisible and the
// chain could be proposed as killed while it demonstrably served.
func TestChainDeathEVMEndpointDefendsChain(t *testing.T) {
	alive := append(deadChainResults(),
		livenessResult("deadchain", "evm_liveness", "https://evm.opd.example", true, checks.ClassNone))
	stateMap := stateFor("deadchain", "alivechain")
	if candidates, _, _, _ := runChainDeathDetection(alive, stateMap, 1, testStale, deathNow); len(candidates) != 0 {
		t.Fatalf("live EVM endpoint must block abandonment, got %v", candidates)
	}
	if cs := stateMap["deadchain"]; cs.ChainDeadStreak != 0 {
		t.Errorf("streak must not advance while the EVM side answers, got %d", cs.ChainDeadStreak)
	}

	dead := append(deadChainResults(),
		livenessResult("deadchain", "evm_liveness", "https://evm.opd.example", false, checks.ClassConnRefused))
	stateMap = stateFor("deadchain", "alivechain")
	if candidates, _, _, _ := runChainDeathDetection(dead, stateMap, 1, testStale, deathNow); len(candidates) != 1 || candidates[0] != "deadchain" {
		t.Fatalf("dead EVM side must restore the candidacy, got %v", candidates)
	}
}
