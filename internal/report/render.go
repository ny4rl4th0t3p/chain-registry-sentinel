package report

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"chain-registry-sentinel/internal/checks"
)

const (
	rulerWidth = 100
	// topDomainRows bounds the domain table; the long tail of one-endpoint domains adds noise,
	// not signal, and the full detail is always in the JSONL.
	topDomainRows = 25
	// minRemedyEndpoints is the floor for the remedy taxonomy: below this a "100% dead domain"
	// is one or two endpoints, which says nothing about the operator.
	minRemedyEndpoints = 3
	nameColWidth       = 30
	chainWrapWidth     = 96
)

var ruler = strings.Repeat("─", rulerWidth)

// Render prints the full report for one run's records. Every table an operator needs is
// computed here — the target UX is "run it, read the report", with no post-processing step.
func Render(w io.Writer, records []Record) {
	agg := aggregate(records)
	renderVantageWarning(w, agg)
	renderClasses(w, agg)
	renderDomains(w, agg)
	renderRemedies(w, agg)
	renderChains(w, agg)
	renderQuality(w, agg)
}

type domainAgg struct {
	total, live int
	chains      map[string]struct{}
	classes     map[checks.FailureClass]int
}

func (d *domainAgg) dead() int { return d.total - d.live }

// dominant returns the most common failure class for the domain and its count.
func (d *domainAgg) dominant() (class checks.FailureClass, count int) {
	for c, n := range d.classes {
		if n > count || (n == count && c < class) {
			class, count = c, n
		}
	}
	return class, count
}

type chainAgg struct {
	coreTotal, coreLive int
}

type aggregates struct {
	endpoints, live int
	rateLimited     int
	chainIDMismatch int
	classes         map[checks.FailureClass]int
	domains         map[string]*domainAgg
	chains          map[string]*chainAgg
	withProvider    int
	providers       map[string]struct{}
	catchingUp      int
	txIndexOff      int
}

func (a *aggregates) dead() int { return a.endpoints - a.live }

// coreCheck reports whether the check decides chain reachability: RPC and REST for cosmos
// chains, the EVM JSON-RPC for eip155 chains. gRPC and WSS are extras — a chain with a live
// RPC but dead gRPC is degraded, not unreachable.
func coreCheck(check string) bool {
	return check == "rpc_liveness" || check == "rest_liveness" || check == "evm_liveness"
}

func aggregate(records []Record) *aggregates {
	a := &aggregates{
		classes:   map[checks.FailureClass]int{},
		domains:   map[string]*domainAgg{},
		chains:    map[string]*chainAgg{},
		providers: map[string]struct{}{},
	}
	for i := range records {
		r := &records[i]
		if !r.liveness() {
			if !r.Passed && !r.Skipped {
				a.chainIDMismatch++
			}
			continue
		}
		if r.Skipped {
			// Rate-limited: the endpoint could not be measured. Counting it as either live or
			// dead would be a guess, so it is reported as its own line instead.
			a.rateLimited++
			continue
		}

		a.endpoints++
		if r.Provider != "" {
			a.withProvider++
			a.providers[r.Provider] = struct{}{}
		}
		d := a.domains[r.Domain]
		if d == nil {
			d = &domainAgg{chains: map[string]struct{}{}, classes: map[checks.FailureClass]int{}}
			a.domains[r.Domain] = d
		}
		d.total++
		d.chains[r.Chain] = struct{}{}

		if coreCheck(r.Check) {
			c := a.chains[r.Chain]
			if c == nil {
				c = &chainAgg{}
				a.chains[r.Chain] = c
			}
			c.coreTotal++
			if r.Passed {
				c.coreLive++
			}
		}

		if r.Passed {
			a.live++
			d.live++
			if r.CatchingUp {
				a.catchingUp++
			}
			if r.TxIndex == "off" {
				a.txIndexOff++
			}
			continue
		}
		a.classes[r.FailureClass]++
		d.classes[r.FailureClass]++
	}
	return a
}

// vantageWarnPct is the share of failures attributable to the prober's own environment above
// which the whole run is flagged as untrustworthy.
const vantageWarnPct = 5.0

// renderVantageWarning is the report diagnosing its own measurement. dns_failure and
// vantage_no_route are signatures of a broken vantage — a flaky resolver, a missing IPv6 route —
// and a 2026-07-28 VM run showed a failing resolver also returns *false NXDOMAINs* (names
// flapping between resolving and not-existing across runs), so once these classes are elevated,
// even the structural figures are contaminated. A report that would be quoted must refuse to be
// quoted from a bad vantage.
func renderVantageWarning(w io.Writer, a *aggregates) {
	suspect := a.classes[checks.ClassDNSFailure] + a.classes[checks.ClassVantageNoRoute]
	if suspect == 0 || pct(suspect, a.dead()) < vantageWarnPct {
		return
	}
	section(w, "WARNING: this vantage looks unhealthy — treat every number below as suspect")
	fmt.Fprintf(w, "%d of %d failures (%.1f%%) are dns_failure or vantage_no_route: resolver failures and\n"+
		"missing routes on the machine running the probe, not information about the endpoints.\n"+
		"A failing resolver can also return false NXDOMAINs, so the structural figures are\n"+
		"contaminated too. Fix DNS and IPv6 on this machine (or change vantage) and re-run.\n",
		suspect, a.dead(), pct(suspect, a.dead()))
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(n) / float64(total)
}

func section(w io.Writer, title string) {
	fmt.Fprintf(w, "\n%s\n%s\n", title, ruler)
}

func renderClasses(w io.Writer, a *aggregates) {
	section(w, "Failure classes")
	classes := make([]checks.FailureClass, 0, len(a.classes))
	for c := range a.classes {
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool {
		if a.classes[classes[i]] != a.classes[classes[j]] {
			return a.classes[classes[i]] > a.classes[classes[j]]
		}
		return classes[i] < classes[j]
	})

	structural := 0
	for _, c := range classes {
		n := a.classes[c]
		tag := "ambiguous "
		if c.IsStructural() {
			tag = "structural"
			structural += n
		}
		fmt.Fprintf(w, "%6d  %5.1f%%  %s  %s\n", n, pct(n, a.dead()), tag, c)
	}
	fmt.Fprintf(w, "%s\n", ruler)
	fmt.Fprintf(w, "%d failures: %d structural (%.1f%%), %d ambiguous or possibly self-inflicted (%.1f%%)\n",
		a.dead(), structural, pct(structural, a.dead()), a.dead()-structural, pct(a.dead()-structural, a.dead()))
	fmt.Fprintf(w, "structural means provably broken regardless of prober behavior: DNS gone, TLS broken,\n"+
		"the server itself answering that nothing is there, or an address no client could use\n")
	if a.chainIDMismatch > 0 {
		fmt.Fprintf(w, "chain ID mismatches: %d (wrong data, not dead endpoints; counted separately)\n", a.chainIDMismatch)
	}
	if a.rateLimited > 0 {
		fmt.Fprintf(w, "rate-limited: %d endpoints could not be measured (excluded from all counts above)\n", a.rateLimited)
	}
}

// sortedDomains returns domains ordered by a key function, descending.
func sortedDomains(a *aggregates, key func(*domainAgg) int) []string {
	names := make([]string, 0, len(a.domains))
	for name := range a.domains {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		ki, kj := key(a.domains[names[i]]), key(a.domains[names[j]])
		if ki != kj {
			return ki > kj
		}
		return names[i] < names[j]
	})
	return names
}

func renderDomains(w io.Writer, a *aggregates) {
	section(w, fmt.Sprintf("Domains (top %d of %d by endpoint count)", topDomainRows, len(a.domains)))
	fmt.Fprintf(w, "%-*s %6s %6s %6s %6s %7s  %s\n",
		nameColWidth, "DOMAIN", "TOTAL", "LIVE", "DEAD", "LIVE%", "CHAINS", "DOMINANT FAILURE")
	names := sortedDomains(a, func(d *domainAgg) int { return d.total })
	for i, name := range names {
		if i >= topDomainRows {
			break
		}
		d := a.domains[name]
		domLabel := "-"
		if c, n := d.dominant(); n > 0 {
			domLabel = fmt.Sprintf("%s (%d)", c, n)
		}
		fmt.Fprintf(w, "%-*s %6d %6d %6d %5.0f%% %7d  %s\n",
			nameColWidth, name, d.total, d.live, d.dead(), pct(d.live, d.total), len(d.chains), domLabel)
	}

	renderConcentration(w, a)
}

func renderConcentration(w io.Writer, a *aggregates) {
	names := sortedDomains(a, func(d *domainAgg) int { return d.dead() })
	shares := []int{10, topDomainRows, 2 * topDomainRows}
	cum := 0
	parts := make([]string, 0, len(shares))
	for i, name := range names {
		cum += a.domains[name].dead()
		for _, threshold := range shares {
			if i+1 == threshold {
				parts = append(parts, fmt.Sprintf("top %d: %.1f%%", threshold, pct(cum, a.dead())))
			}
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(w, "\nfailure concentration across %d domains — %s of all dead endpoints\n",
			len(a.domains), strings.Join(parts, ", "))
	}
}

// remedyBucket says what a fully-dead domain's dominant failure class implies about the fix.
// This is a heuristic inference from one vantage point, not a measurement — hence the labels.
//
// Only NXDOMAIN implies "gone": it is the authoritative answer that the name does not exist.
// dns_failure (SERVFAIL) and vantage_no_route point at the prober's own resolver or routing
// table, so a domain dominated by them lands in "recheck", not "remove".
func remedyBucket(dominant checks.FailureClass) string {
	switch dominant {
	case checks.ClassDNSNXDomain:
		return "gone"
	case checks.ClassTimeout, checks.ClassConnRefused, checks.ClassConnReset,
		checks.ClassNetUnreachable, checks.ClassEOFNoResponse, checks.ClassTLSHandshakeSlow,
		checks.ClassDNSFailure, checks.ClassVantageNoRoute:
		return "unresponsive"
	default:
		return "answering"
	}
}

func renderRemedies(w io.Writer, a *aggregates) {
	section(w, "Fully dead domains — remedy taxonomy (heuristic, inferred from the dominant failure class)")
	buckets := map[string][]string{}
	for name, d := range a.domains {
		if d.live > 0 || d.total < minRemedyEndpoints {
			continue
		}
		dom, _ := d.dominant()
		buckets[remedyBucket(dom)] = append(buckets[remedyBucket(dom)], name)
	}
	for _, b := range []struct{ key, label string }{
		{"gone", "operator gone (DNS dead) — remove the entries"},
		{"answering", "server answering, endpoints broken — fix addresses or contact the operator; deletion would be wrong if the nodes exist"},
		{"unresponsive", "unresponsive (refused/timeout) — recheck across runs, then remove"},
	} {
		names := buckets[b.key]
		if len(names) == 0 {
			continue
		}
		sort.Slice(names, func(i, j int) bool { return a.domains[names[i]].total > a.domains[names[j]].total })
		fmt.Fprintf(w, "\n%s:\n", b.label)
		for _, name := range names {
			d := a.domains[name]
			dom, n := d.dominant()
			fmt.Fprintf(w, "%6d endpoints %4d chains  %-*s %s (%d)\n",
				d.total, len(d.chains), nameColWidth, name, dom, n)
		}
	}
	fmt.Fprintf(w, "\ndomains with fewer than %d endpoints are omitted; partially live domains need per-endpoint\n"+
		"pruning and are visible in the table above\n", minRemedyEndpoints)
}

func renderChains(w io.Writer, a *aggregates) {
	var unreachable []string
	for name, c := range a.chains {
		if c.coreTotal > 0 && c.coreLive == 0 {
			unreachable = append(unreachable, name)
		}
	}
	sort.Strings(unreachable)

	section(w, "Chain reachability")
	if len(unreachable) == 0 {
		fmt.Fprintf(w, "%d chains checked; every one has at least one live RPC, REST or EVM endpoint\n", len(a.chains))
		return
	}
	fmt.Fprintf(w, "%d chains checked; %d (%.1f%%) have no live RPC, REST or EVM endpoint at all —\n"+
		"unusable from registry data alone, despite carrying status \"live\":\n",
		len(a.chains), len(unreachable), pct(len(unreachable), len(a.chains)))
	line := "  "
	for _, name := range unreachable {
		if len(line)+len(name)+2 > chainWrapWidth {
			fmt.Fprintln(w, line)
			line = "  "
		}
		line += name + ", "
	}
	if strings.TrimSpace(line) != "" {
		fmt.Fprintln(w, strings.TrimSuffix(line, ", "))
	}
}

func renderQuality(w io.Writer, a *aggregates) {
	section(w, "Node quality and metadata coverage")
	fmt.Fprintf(w, "%d endpoints: %d live, %d dead\n", a.endpoints, a.live, a.dead())
	fmt.Fprintf(w, "of the live endpoints: %d still catching up (answering but behind), %d with tx_index off\n",
		a.catchingUp, a.txIndexOff)
	fmt.Fprintf(w, "provider field present on %.1f%% of endpoint entries (%d distinct providers named)\n",
		pct(a.withProvider, a.endpoints), len(a.providers))
}
