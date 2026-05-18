package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"chain-registry-sentinel/internal/checks"
)

const currentSchemaVersion = 1

// EndpointKey returns the map key used to identify an endpoint within a ChainState.
func EndpointKey(check, address string) string {
	return check + "|" + address
}

type EndpointState struct {
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastPassed          bool      `json:"last_passed"`
	LastEvidence        string    `json:"last_evidence"`
	FirstEvidence       string    `json:"first_evidence"`
	LastChecked         time.Time `json:"last_checked"`
	FirstFailureTime    time.Time `json:"first_failure_time"`
}

type ChainState struct {
	Version        int                      `json:"version"`
	ChainID        string                   `json:"chain_id"`
	UpdatedAt      time.Time                `json:"updated_at"`
	LastPROpenedAt time.Time                `json:"last_pr_opened_at,omitempty"`
	Endpoints      map[string]EndpointState `json:"endpoints"`
}

// Load reads the state file at a path. Returns an empty ChainState when the file
// does not exist (first run). Returns an error for corrupt files or unknown schema versions.
func Load(path string) (ChainState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ChainState{Endpoints: make(map[string]EndpointState)}, nil
	}
	if err != nil {
		return ChainState{}, fmt.Errorf("load state %s: %w", path, err)
	}
	var cs ChainState
	if err := json.Unmarshal(data, &cs); err != nil {
		return ChainState{}, fmt.Errorf("load state %s: %w", path, err)
	}
	if cs.Version != currentSchemaVersion {
		return ChainState{}, fmt.Errorf("load state %s: unsupported schema version %d (want %d)", path, cs.Version, currentSchemaVersion)
	}
	if cs.Endpoints == nil {
		cs.Endpoints = make(map[string]EndpointState)
	}
	return cs, nil
}

// Save writes cs to a path atomically (temp file and rename) with 0o644 permissions.
func Save(path string, cs ChainState, now time.Time) error {
	cs.Version = currentSchemaVersion
	cs.UpdatedAt = now
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return fmt.Errorf("save state %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save state %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("save state %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("save state %s: %w", path, err)
	}
	return nil
}

// Update merges a liveness check result into the chain state. Skipped results
// must be filtered by the caller before calling Update.
func (cs *ChainState) Update(r checks.Result, now time.Time) {
	key := EndpointKey(r.Check, r.Endpoint)
	es := cs.Endpoints[key]
	es.LastChecked = now
	es.LastPassed = r.Passed
	es.LastEvidence = r.Evidence
	if r.Passed {
		es.ConsecutiveFailures = 0
		es.FirstFailureTime = time.Time{}
		es.FirstEvidence = ""
		es.LastEvidence = ""
	} else {
		if es.ConsecutiveFailures == 0 {
			es.FirstFailureTime = now
			es.FirstEvidence = r.Evidence
		}
		es.ConsecutiveFailures++
	}
	cs.Endpoints[key] = es
}

// Prune removes endpoint entries whose keys are not present in activeKeys.
func (cs *ChainState) Prune(activeKeys map[string]struct{}) {
	for key := range cs.Endpoints {
		if _, ok := activeKeys[key]; !ok {
			delete(cs.Endpoints, key)
		}
	}
}
