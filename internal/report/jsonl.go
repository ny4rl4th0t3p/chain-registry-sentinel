package report

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// scanBufSize bounds a single JSONL line. Evidence is capped upstream (readBodyPrefix), so real
// lines are far smaller; this is headroom, not an expectation.
const (
	scanBufSize  = 1024 * 1024
	scanInitSize = 64 * 1024
)

// WriteRun writes one run's records as a single JSONL file in dir and returns its path.
//
// One file per run rather than one appended file, so a run is atomic — written to a temp file
// and renamed, like state.Save — and a bad run can be deleted without rewriting anything. The
// name is derived from the run timestamp and vantage; two full runs within the same second do
// not happen in practice (a full-registry pass takes time).
func WriteRun(dir string, records []Record) (string, error) {
	if len(records) == 0 {
		return "", errors.New("write report: no records")
	}
	// 0o700 matches state.Save. The record files themselves are created at 0o600 by CreateTemp
	// and keep that mode through the rename — owner-only is tighter than these public probe
	// results need, but nothing else reads them and loosening would be a decision, not a default.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}
	name := records[0].RunTS.UTC().Format("20060102T150405Z") + "-" + sanitizeVantage(records[0].Vantage) + ".jsonl"
	path := filepath.Join(dir, name)

	tmp, err := os.CreateTemp(dir, name+".tmp")
	if err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}
	defer os.Remove(tmp.Name())

	w := bufio.NewWriter(tmp)
	enc := json.NewEncoder(w)
	for i := range records {
		if err := enc.Encode(records[i]); err != nil {
			_ = tmp.Close()
			return "", fmt.Errorf("write report: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write report: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}
	return path, nil
}

// LoadFile reads one run's records from a single, explicitly named JSONL file.
//
// A file, not a directory: an earlier directory mode picked the newest run automatically, and
// that implicit selection loaded the wrong file in practice on the first multi-file directory
// it met. A report that will be quoted must be traceable to exactly one named input.
func LoadFile(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("load report: %w", err)
	}
	defer f.Close()

	var records []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, scanInitSize), scanBufSize)
	line := 0
	for sc.Scan() {
		line++
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			// A corrupt line means a truncated or hand-edited file. Refuse rather than skip:
			// silently dropping records would skew every table computed from them.
			return nil, fmt.Errorf("load report: %s:%d: %w", path, line, err)
		}
		records = append(records, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("load report: %s: %w", path, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("load report: %s: no records", path)
	}

	// A file written by WriteRun holds exactly one run. More than one means files were
	// concatenated by hand; rendering the mix would double-count endpoints, so refuse.
	runs := map[string]struct{}{}
	for i := range records {
		runs[records[i].RunTS.UTC().Format(time.RFC3339)+"|"+records[i].Vantage] = struct{}{}
	}
	if len(runs) > 1 {
		keys := make([]string, 0, len(runs))
		for k := range runs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return nil, fmt.Errorf("load report: %s contains %d distinct runs (%s); a report renders exactly one run",
			path, len(runs), strings.Join(keys, ", "))
	}
	return records, nil
}

// sanitizeVantage makes a vantage label safe for a filename.
func sanitizeVantage(v string) string {
	if v == "" {
		return "run"
	}
	return strings.Map(func(c rune) rune {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			return c
		default:
			return '-'
		}
	}, v)
}
