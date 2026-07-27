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

	// Diagnostics carried alongside the streak so a state directory is analyzable on its own,
	// without re-deriving causes from evidence strings.
	//
	// currentSchemaVersion deliberately stays at 1. Adding optional fields is compatible in both
	// directions — new code reads an old file as zero values, and old code ignores fields it does
	// not know, which is encoding/json's default — so there is no incompatibility for a version
	// number to signal. Because Load compares versions for exact equality, a bump would
	// manufacture one and discard every existing state file to convey nothing.
	//
	// Bump only for a genuinely breaking change: a renamed or retyped field, a changed meaning
	// for an existing field (e.g. ConsecutiveFailures counting days rather than runs), or a new
	// endpoint key format.
	Provider     string              `json:"provider,omitempty"`
	FailureClass checks.FailureClass `json:"failure_class,omitempty"`
	HTTPStatus   int                 `json:"http_status,omitempty"`
}

type ChainState struct {
	Version            int                      `json:"version"`
	ChainID            string                   `json:"chain_id"`
	UpdatedAt          time.Time                `json:"updated_at"`
	LastPROpenedAt     time.Time                `json:"last_pr_opened_at,omitempty"`
	LastHashPROpenedAt time.Time                `json:"last_hash_pr_opened_at,omitempty"`
	Endpoints          map[string]EndpointState `json:"endpoints"`
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
	// Older and newer are different risks, so they are not collapsed into one check.
	//
	// A version above ours was written by a build that knows fields or meanings this one does
	// not, so reading it could silently misinterpret data — refuse.
	//
	// A version below ours is readable: every change so far has been additive, so absent fields
	// unmarshal as zero values. When a genuinely breaking change lands, migration goes here
	// rather than in a rejection.
	//
	// Zero means the version key was missing or unparseable, which is a malformed file rather
	// than an old one. Accepting it would let a truncated write masquerade as empty state.
	if cs.Version < 1 {
		return ChainState{}, fmt.Errorf("load state %s: missing or invalid schema version", path)
	}
	if cs.Version > currentSchemaVersion {
		return ChainState{}, fmt.Errorf(
			"load state %s: written by a newer sentinel (schema v%d, this build understands v%d)",
			path, cs.Version, currentSchemaVersion)
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
	// Provider describes the registry entry, not the failure, so it is kept in both branches.
	es.Provider = r.Provider
	es.FailureClass = r.FailureClass
	es.HTTPStatus = r.HTTPStatus
	if r.Passed {
		es.ConsecutiveFailures = 0
		es.FirstFailureTime = time.Time{}
		es.FirstEvidence = ""
		es.LastEvidence = ""
		es.FailureClass = ""
		es.HTTPStatus = 0
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
