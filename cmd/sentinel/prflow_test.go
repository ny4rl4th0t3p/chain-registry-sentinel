package main

import (
	"testing"
	"time"

	"chain-registry-sentinel/internal/github"
	"chain-registry-sentinel/internal/state"
)

// A preflight pass on one protocol must not rescue the same address under another protocol:
// streaks, rescue, and removal all key on the (check, address) pair. Before v0.8.3 the rescue
// was address-keyed, so an alive RPC would forever shield its dead REST twin from removal on
// the 28 registry addresses declared under multiple categories.
func TestApplyPreflightResultsIsPairKeyed(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	addr := "https://gateway.example.com"
	rpcKey := state.EndpointKey("rpc_liveness", addr)
	restKey := state.EndpointKey("rest_liveness", addr)
	stateMap := map[string]state.ChainState{"gwchain": {
		Endpoints: map[string]state.EndpointState{
			rpcKey:  {ConsecutiveFailures: 5},
			restKey: {ConsecutiveFailures: 5},
		},
	}}
	flagged := map[string][]github.FlaggedEndpoint{"gwchain": {
		{Check: "rpc_liveness", Address: addr},
		{Check: "rest_liveness", Address: addr},
	}}
	passed := map[string]map[string]bool{"gwchain": {rpcKey: true}}

	applyPreflightResults(stateMap, flagged, passed, now)

	if got := stateMap["gwchain"].Endpoints[rpcKey].ConsecutiveFailures; got != 0 {
		t.Errorf("rpc streak must reset on its own pass, got %d", got)
	}
	if got := stateMap["gwchain"].Endpoints[restKey].ConsecutiveFailures; got != 5 {
		t.Errorf("rest streak must NOT be rescued by the rpc pass, got %d", got)
	}
}
