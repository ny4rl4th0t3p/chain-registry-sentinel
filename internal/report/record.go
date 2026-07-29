// Package report turns probe results into the sentinel's primary deliverable: a JSONL record
// per endpoint per run, and the aggregate tables computed from those records.
//
// The report generator consumes records, never live probe results directly. That single
// constraint is what allows two input modes — probe now and report, or reload previous runs
// with --from and report — and what makes every table testable against fixtures instead of
// only against an hours-long live run.
package report

import (
	"net"
	"sort"
	"strings"
	"time"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/state"
)

// Record is one check against one endpoint in one run. It is the interchange format between
// probing and reporting, so it must stay self-sufficient: FailureClass is carried for grouping,
// and Evidence is carried beside it because classifications have been wrong before — without
// the raw text a bad call cannot be re-derived from the dataset without re-probing everything.
type Record struct {
	RunTS   time.Time `json:"run_ts"`
	Vantage string    `json:"vantage,omitempty"`

	// Run provenance, repeated on every record so a JSONL file is reproducible on its own:
	// which registry state was probed and how. Learned the hard way — earlier runs recorded
	// neither, and their registry commit is unrecoverable. The per-line redundancy is the
	// price of records that never depend on a sidecar file, and compresses to nothing.
	RegistryCommit string `json:"registry_commit,omitempty"`
	Concurrency    int    `json:"probe_concurrency,omitempty"`
	TimeoutMS      int64  `json:"probe_timeout_ms,omitempty"`

	Chain     string `json:"chain"`
	ChainID   string `json:"chain_id"`
	ChainType string `json:"chain_type"`
	Check     string `json:"check"`
	Endpoint  string `json:"endpoint"`
	Host      string `json:"host"`
	Domain    string `json:"domain"`
	Provider  string `json:"provider,omitempty"`

	// Order is the endpoint's 1-based position within its type's list in chain.json; 0 in
	// records written before the field existed. Most client tooling takes the first entry, so
	// order 1 is the endpoint users actually hit.
	Order int `json:"order,omitempty"`

	Passed       bool                `json:"passed"`
	Skipped      bool                `json:"skipped,omitempty"`
	FailureClass checks.FailureClass `json:"failure_class,omitempty"`
	HTTPStatus   int                 `json:"http_status,omitempty"`
	LatencyMS    int64               `json:"latency_ms,omitempty"`
	Evidence     string              `json:"evidence,omitempty"`
	CatchingUp   bool                `json:"catching_up,omitempty"`
	TxIndex      string              `json:"tx_index,omitempty"`
	// LatestBlockTime is RFC3339 (string rather than time.Time so omitempty works); set only
	// when the node reported one. Chain-death evidence: a halted chain's survivors answer with
	// this frozen.
	LatestBlockTime string `json:"latest_block_time,omitempty"`
	// MethodRestricted: live, but the gateway refused the standard probe method by name and
	// liveness came from a fallback query — usable with caveats, same family as tx_index off.
	MethodRestricted bool `json:"method_restricted,omitempty"`
	Streak           int  `json:"streak,omitempty"`
}

// liveness reports whether the record is a liveness check, the unit all endpoint counting is
// done in. Chain-ID results ride along for correctness findings but never count as endpoints.
func (r Record) liveness() bool { return strings.HasSuffix(r.Check, "_liveness") }

// RunMeta identifies one run: when and from where it probed, which registry state, and with
// what settings. Everything in it is stamped into every record.
type RunMeta struct {
	TS             time.Time
	Vantage        string
	RegistryCommit string // "" when the registry directory is not a git checkout
	Concurrency    int
	Timeout        time.Duration
}

// Build converts one run's results into records.
//
// Two kinds of skipped results reach this point and only one belongs in the dataset: a
// rate-limited liveness check carries ClassHTTP429 and is a real observation ("could not
// measure"), while a skipped chain-ID check is a pipeline artifact — the endpoint never
// answered, and its liveness record already says so. Records with no class and no pass are
// therefore dropped.
//
// stateMap may be nil (stateless run); streaks then report as zero.
func Build(results []checks.Result, stateMap map[string]state.ChainState, meta RunMeta) []Record {
	records := make([]Record, 0, len(results))
	for i := range results {
		r := &results[i]
		if r.Skipped && r.FailureClass == checks.ClassNone {
			continue
		}
		host := hostOf(r.Endpoint)
		rec := Record{
			RunTS:            meta.TS,
			Vantage:          meta.Vantage,
			RegistryCommit:   meta.RegistryCommit,
			Concurrency:      meta.Concurrency,
			TimeoutMS:        meta.Timeout.Milliseconds(),
			Chain:            r.Chain,
			ChainID:          r.ChainID,
			ChainType:        r.ChainType,
			Check:            r.Check,
			Endpoint:         r.Endpoint,
			Host:             host,
			Domain:           domainOf(host),
			Provider:         r.Provider,
			Order:            r.EndpointOrder,
			Passed:           r.Passed,
			Skipped:          r.Skipped,
			FailureClass:     r.FailureClass,
			HTTPStatus:       r.HTTPStatus,
			LatencyMS:        r.Latency.Milliseconds(),
			Evidence:         r.Evidence,
			CatchingUp:       r.CatchingUp,
			TxIndex:          r.TxIndex,
			MethodRestricted: r.MethodRestricted,
		}
		if !r.LatestBlockTime.IsZero() {
			rec.LatestBlockTime = r.LatestBlockTime.UTC().Format(time.RFC3339)
		}
		if cs, ok := stateMap[r.Chain]; ok {
			rec.Streak = cs.Endpoints[state.EndpointKey(r.Check, r.Endpoint)].ConsecutiveFailures
		}
		records = append(records, rec)
	}
	// Results arrive in channel order, which varies run to run. Sort so the written file is
	// deterministic and two runs over the same registry diff cleanly.
	sort.Slice(records, func(i, j int) bool {
		if records[i].Chain != records[j].Chain {
			return records[i].Chain < records[j].Chain
		}
		if records[i].Check != records[j].Check {
			return records[i].Check < records[j].Check
		}
		return records[i].Endpoint < records[j].Endpoint
	})
	return records
}

// hostOf extracts the host from a registry address: scheme and path stripped, port stripped.
// Registry addresses are not uniform — "https://x.example.com:443/", "grpc.example.com:9090"
// and bare "1.2.3.4:26657" all occur.
func hostOf(address string) string {
	host := address
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// EndpointDomain returns the registrable domain for a registry address — the same derivation
// records use, exported so chain-death detection groups endpoints by operator identically to
// the report. Same caveats as domainOf.
func EndpointDomain(address string) string {
	return domainOf(hostOf(address))
}

// EndpointHost returns the bare hostname for a registry address (scheme, path and port
// stripped) — usable directly in a `dig` command, which a PR body's verify line needs.
func EndpointHost(address string) string {
	return hostOf(address)
}

// domainOf reduces a host to its registrable domain using the naive last-two-labels rule.
// That is wrong for multi-part public suffixes (".com.ua", ".co.uk") — in the 2026-07-25 data
// this produced one bogus "com.ua" bucket of 9 endpoints, small enough to accept over vendoring
// a public-suffix list. IP addresses are returned whole rather than mangled into two octets.
func domainOf(host string) string {
	if net.ParseIP(host) != nil {
		return host
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return host
	}
	return labels[len(labels)-2] + "." + labels[len(labels)-1]
}
