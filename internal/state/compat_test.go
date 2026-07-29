package state_test

import (
	"os"
	"path/filepath"
	"testing"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/state"
)

// v0.6.0 adds Provider, FailureClass and HTTPStatus to EndpointState without bumping
// currentSchemaVersion: optional fields are compatible in both directions, so there is no
// incompatibility for a version number to signal. See the note on EndpointState for when a bump
// is warranted.
//
// These tests are what makes that claim checkable rather than asserted. The fixture is a literal
// version-1 file as written before those fields existed, and must keep loading with its streaks
// intact. Do not regenerate it from the current struct: hand-written old-format JSON is the
// entire point.
const legacyV1State = `{
  "version": 1,
  "chain_id": "cosmoshub-4",
  "updated_at": "2026-07-01T00:00:00Z",
  "last_pr_opened_at": "2026-06-20T00:00:00Z",
  "endpoints": {
    "rpc_liveness|https://rpc.dead.example.com": {
      "consecutive_failures": 9,
      "last_passed": false,
      "last_evidence": "HTTP 404",
      "first_evidence": "no such host",
      "last_checked": "2026-07-01T00:00:00Z",
      "first_failure_time": "2026-06-22T00:00:00Z"
    },
    "rest_liveness|https://rest.live.example.com": {
      "consecutive_failures": 0,
      "last_passed": true,
      "last_checked": "2026-07-01T00:00:00Z"
    }
  }
}`

func TestLoadLegacyV1StatePreservesStreaks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cosmoshub.json")
	if err := os.WriteFile(path, []byte(legacyV1State), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cs, err := state.Load(path)
	if err != nil {
		t.Fatalf("Load() on a version-1 file must succeed, got: %v", err)
	}

	if cs.ChainID != "cosmoshub-4" {
		t.Errorf("ChainID = %q, want cosmoshub-4", cs.ChainID)
	}
	if len(cs.Endpoints) != 2 {
		t.Fatalf("len(Endpoints) = %d, want 2", len(cs.Endpoints))
	}
	if cs.LastPROpenedAt.IsZero() {
		t.Error("LastPROpenedAt was dropped; PR cooldown would reset")
	}

	dead := cs.Endpoints["rpc_liveness|https://rpc.dead.example.com"]
	if dead.ConsecutiveFailures != 9 {
		t.Errorf("ConsecutiveFailures = %d, want 9 — the streak must survive the schema change",
			dead.ConsecutiveFailures)
	}
	if dead.FirstEvidence != "no such host" {
		t.Errorf("FirstEvidence = %q, want %q", dead.FirstEvidence, "no such host")
	}
	if dead.FirstFailureTime.IsZero() {
		t.Error("FirstFailureTime was dropped")
	}

	// Fields absent from the old format must read as zero values rather than causing a failure.
	if cs.ChainDeadStreak != 0 || !cs.ChainDeadFirstTime.IsZero() || !cs.LastStatusPROpenedAt.IsZero() {
		t.Error("chain-death fields must read as zero from a legacy file")
	}
	if dead.Provider != "" {
		t.Errorf("Provider = %q, want empty for a legacy file", dead.Provider)
	}
	if dead.FailureClass != checks.ClassNone {
		t.Errorf("FailureClass = %q, want empty for a legacy file", dead.FailureClass)
	}
	if dead.HTTPStatus != 0 {
		t.Errorf("HTTPStatus = %d, want 0 for a legacy file", dead.HTTPStatus)
	}

	if live := cs.Endpoints["rest_liveness|https://rest.live.example.com"]; !live.LastPassed {
		t.Error("LastPassed = false, want true")
	}
}

// A missing file is the normal first run and must not be an error — that is what lets callers
// treat any error from Load as "this file exists and could not be understood", and refuse to
// discard streaks rather than silently resetting them.
func TestLoadMissingFileIsNotAnError(t *testing.T) {
	cs, err := state.Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load() on a missing file must succeed, got: %v", err)
	}
	if cs.Endpoints == nil {
		t.Error("Endpoints map must be initialized so callers can write to it")
	}
	if len(cs.Endpoints) != 0 {
		t.Errorf("len(Endpoints) = %d, want 0", len(cs.Endpoints))
	}
}

// Everything a caller must not mistake for empty state.
func TestLoadRejectsUnreadableState(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		// A version key that is absent or zero means a malformed file, not an old one.
		// Accepting it would let a truncated write masquerade as a fresh start.
		{"missing version", `{"chain_id":"cosmoshub-4","endpoints":{}}`},
		{"zero version", `{"version":0,"chain_id":"cosmoshub-4","endpoints":{}}`},
		// Written by a build that knows fields or meanings this one does not.
		{"newer schema", `{"version":99,"chain_id":"cosmoshub-4","endpoints":{}}`},
		{"truncated", `{"version":1,"endpoints":{`},
		{"not json", `this is not json`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cosmoshub.json")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			if _, err := state.Load(path); err == nil {
				t.Error("Load() returned nil error; unreadable state must not be mistaken for empty state")
			}
		})
	}
}

// A legacy file that is re-saved must gain the new fields without disturbing the streak, so a
// mid-upgrade run cannot lose progress.
func TestResaveLegacyV1StateKeepsStreak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cosmoshub.json")
	if err := os.WriteFile(path, []byte(legacyV1State), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cs, err := state.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	now := cs.UpdatedAt.AddDate(0, 0, 1)
	cs.Update(checks.Result{
		Chain:        "cosmoshub",
		Check:        "rpc_liveness",
		Endpoint:     "https://rpc.dead.example.com",
		Provider:     "Example Operator",
		FailureClass: checks.ClassDNSNXDomain,
		Evidence:     "no such host",
	}, now)

	if err := state.Save(path, cs, now); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := state.Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	got := reloaded.Endpoints["rpc_liveness|https://rpc.dead.example.com"]
	if got.ConsecutiveFailures != 10 {
		t.Errorf("ConsecutiveFailures = %d, want 10 (9 legacy + 1)", got.ConsecutiveFailures)
	}
	if got.Provider != "Example Operator" {
		t.Errorf("Provider = %q, want %q", got.Provider, "Example Operator")
	}
	if got.FailureClass != checks.ClassDNSNXDomain {
		t.Errorf("FailureClass = %q, want %q", got.FailureClass, checks.ClassDNSNXDomain)
	}
	// The original first-failure timestamp must not be reset by the upgrade, since PR evidence
	// quotes how long an endpoint has been failing.
	if !got.FirstFailureTime.Equal(cs.Endpoints["rpc_liveness|https://rpc.dead.example.com"].FirstFailureTime) {
		t.Error("FirstFailureTime changed across the upgrade")
	}
}
