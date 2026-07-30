package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/github"
	"chain-registry-sentinel/internal/registry"
	"chain-registry-sentinel/internal/report"
	"chain-registry-sentinel/internal/state"
)

// Version is injected at build time via -ldflags.
var Version = "dev"

type CLI struct {
	Registry string        `help:"Path to local chain-registry clone (required unless --from is used)" env:"INPUT_REGISTRY"`
	Chains   string        `help:"Comma-separated chain names, or 'all'" env:"INPUT_CHAINS" default:"all"`
	Timeout  time.Duration `help:"HTTP timeout per request" env:"INPUT_TIMEOUT" default:"60s"`
	// The conservative default is deliberate: every probe fires A+AAAA DNS lookups, and at high
	// concurrency the burst overruns dnsmasq-class forwarders (default dns-forward-max: 150) —
	// home routers, container stubs, multipass VMs — which then drop queries. That surfaces as
	// dns_failure, false NXDOMAINs and v6-only dials: data that indicts the vantage, not the
	// endpoints. 16 was measured clean over the full registry; raising it is one flag for those
	// who know their resolver chain can take it.
	Concurrency          int              `help:"Max simultaneous endpoint probes" env:"INPUT_CONCURRENCY" default:"16"`
	StatePath            string           `help:"Directory for per-chain state files" env:"INPUT_STATE_PATH"`
	ResetState           bool             `help:"Start unreadable state files from scratch instead of aborting" env:"INPUT_RESET_STATE"`
	MinFailures          int              `help:"Consecutive failures before flagging an endpoint" env:"INPUT_MIN_FAILURES" default:"14"`
	ChainDeathMinRuns    int              `help:"Dead-looking runs before a status-flip PR" env:"INPUT_CHAIN_DEATH_MIN_RUNS" default:"14"`
	ChainDeathStaleAfter time.Duration    `help:"Block age that marks a chain as halted" env:"INPUT_CHAIN_DEATH_STALE_AFTER" default:"168h"`
	MaxStatusPRs         int              `help:"Max new chain status-flip PRs per run" env:"INPUT_MAX_STATUS_PRS" default:"3"`
	DryRun               bool             `help:"Read state but do not write it or open PRs" env:"INPUT_DRY_RUN"`
	GithubToken          string           `help:"GitHub token for opening PRs" env:"INPUT_GITHUB_TOKEN"`
	GithubRepo           string           `help:"Target repo (owner/repo)" env:"INPUT_GITHUB_REPO"`
	MaxEndpointPRs       int              `help:"Max new endpoint-removal PRs per run (0 disables)" env:"INPUT_MAX_ENDPOINT_PRS" default:"5"`
	MaxHashPRs           int              `help:"Max new IBC hash-fix PRs per run (0 disables)" env:"INPUT_MAX_HASH_PRS" default:"5"`
	PRCooldownDays       int              `help:"Days between PRs per chain" env:"INPUT_PR_COOLDOWN_DAYS" default:"7"`
	Report               string           `help:"Directory to write this run's JSONL records into (one file per run)" env:"INPUT_REPORT"`
	Vantage              string           `help:"Label stamped into every record to compare runs across networks" env:"INPUT_VANTAGE"`
	UserAgent            string           `help:"User-Agent for probe requests (default identifies the sentinel)" env:"INPUT_USER_AGENT"`
	From                 string           `help:"Render a report from a JSONL record file written by --report" env:"INPUT_FROM"`
	Verbose              bool             `short:"v" help:"Enable debug logging to stderr" env:"INPUT_VERBOSE"`
	Version              kong.VersionFlag `name:"version" help:"Print version and exit"`
}

// exitFatal is returned only when the sentinel could not run: an unreadable registry, no chains,
// or state files that exist but cannot be parsed.
//
// Findings deliberately exit 0. Dead endpoints are the expected steady state of a decaying
// registry, so a non-zero code for them fires on every run forever and therefore carries no
// information. It also had a cost: because findings were non-zero, the Docker entrypoint needed
// `|| true` to keep scheduled runs green, and that swallowed genuine failures — a run that
// probed nothing looked exactly like a clean one. Reserving non-zero for "could not run" removes
// the need to absorb anything.
const exitFatal = 1

type typeStats struct {
	total       int
	live        int
	unreachable int
	wrongResp   int
}

func (t *typeStats) add(other typeStats) {
	t.total += other.total
	t.live += other.live
	t.unreachable += other.unreachable
	t.wrongResp += other.wrongResp
}

func (t typeStats) format() string {
	if t.total == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", t.live, t.total)
}

type chainStats struct {
	rpc         typeStats
	rest        typeStats
	grpcWeb     typeStats
	grpc        typeStats
	evm         typeStats
	wss         typeStats
	chainIDFail int
}

func (s *chainStats) allEndpoints() int {
	return s.rpc.total + s.rest.total + s.grpcWeb.total + s.grpc.total + s.evm.total + s.wss.total
}
func (s *chainStats) allLive() int {
	return s.rpc.live + s.rest.live + s.grpcWeb.live + s.grpc.live + s.evm.live + s.wss.live
}
func (s *chainStats) allUnreachable() int {
	return s.rpc.unreachable + s.rest.unreachable + s.grpcWeb.unreachable +
		s.grpc.unreachable + s.evm.unreachable + s.wss.unreachable
}
func (s *chainStats) allWrongResp() int {
	return s.rpc.wrongResp + s.rest.wrongResp + s.grpcWeb.wrongResp +
		s.grpc.wrongResp + s.evm.wrongResp + s.wss.wrongResp
}
func (s *chainStats) allDead() int { return s.allUnreachable() + s.allWrongResp() }

type EndpointType int

const (
	TypeRPC EndpointType = iota
	TypeREST
	TypeGRPCWeb
	TypeGRPC
	TypeEVM
	TypeWSS
)

func (t EndpointType) String() string {
	switch t {
	case TypeRPC:
		return "rpc"
	case TypeREST:
		return "rest"
	case TypeGRPCWeb:
		return "grpc-web"
	case TypeGRPC:
		return "grpc"
	case TypeEVM:
		return "evm"
	case TypeWSS:
		return "wss"
	default:
		return "unknown"
	}
}

// Liveness check names, shared between probing and chain-death detection.
const (
	checkRPCLiveness  = "rpc_liveness"
	checkRESTLiveness = "rest_liveness"
	checkEVMLiveness  = "evm_liveness"
)

func (t EndpointType) livenessCheckName() string {
	switch t {
	case TypeRPC:
		return checkRPCLiveness
	case TypeREST:
		return checkRESTLiveness
	case TypeGRPCWeb:
		return "grpc_web_liveness"
	case TypeGRPC:
		return "grpc_liveness"
	case TypeEVM:
		return checkEVMLiveness
	case TypeWSS:
		return "wss_liveness"
	default:
		return ""
	}
}

type job struct {
	chain        registry.Chain
	endpoint     registry.Endpoint
	endpointType EndpointType
	// order is the endpoint's 1-based position within its type's list in chain.json. Registry
	// order is user-facing: most client tooling takes the first entry, so the report needs to
	// know which one that was.
	order int
}

func buildJobs(chains []registry.Chain) []job {
	var jobs []job
	add := func(ch registry.Chain, eps []registry.Endpoint, t EndpointType) {
		for i, ep := range eps {
			jobs = append(jobs, job{chain: ch, endpoint: ep, endpointType: t, order: i + 1})
		}
	}
	for i := range chains {
		ch := chains[i]
		switch ch.ChainType {
		case "cosmos":
			add(ch, ch.RPCs, TypeRPC)
			add(ch, ch.RESTEndpoints, TypeREST)
			add(ch, ch.GRPCWebEndpoints, TypeGRPCWeb)
			add(ch, ch.GRPCEndpoints, TypeGRPC)
			add(ch, ch.WSSEndpoints, TypeWSS)
		case "eip155":
			add(ch, ch.EVMEndpoints, TypeEVM)
		}
	}
	return jobs
}

func runProbe(ctx context.Context, client *http.Client, j job, ua string) []checks.Result {
	results := probeByType(ctx, client, j, ua)
	// Stamped here rather than inside the checks: list position is registry data the probe
	// layer never sees, and one assignment point beats six.
	for i := range results {
		results[i].EndpointOrder = j.order
	}
	return results
}

// ua reaches only the gRPC and WSS probes here: the HTTP-based checks inherit it from the
// shared client's transport.
func probeByType(ctx context.Context, client *http.Client, j job, ua string) []checks.Result {
	switch j.endpointType {
	case TypeRPC:
		probe := checks.ProbeEndpoint(ctx, client, j.chain, j.endpoint)
		return []checks.Result{
			checks.NewRPCLiveness().Evaluate(probe),
			checks.NewRPCChainID().Evaluate(probe),
		}
	case TypeREST:
		probe := checks.ProbeRESTEndpoint(ctx, client, j.chain, j.endpoint)
		return []checks.Result{
			checks.NewRESTLiveness().Evaluate(probe),
			checks.NewRESTChainID().Evaluate(probe),
		}
	case TypeGRPCWeb:
		probe := checks.ProbeGRPCWebEndpoint(ctx, client, j.chain, j.endpoint)
		return []checks.Result{
			checks.NewGRPCWebLiveness().Evaluate(probe),
			checks.NewGRPCWebChainID().Evaluate(probe),
		}
	case TypeGRPC:
		probe := checks.ProbeGRPCEndpoint(ctx, j.chain, j.endpoint, ua)
		return []checks.Result{
			checks.NewGRPCLiveness().Evaluate(probe),
			checks.NewGRPCChainID().Evaluate(probe),
		}
	case TypeEVM:
		probe := checks.ProbeEVMEndpoint(ctx, client, j.chain, j.endpoint)
		return []checks.Result{
			checks.NewEVMLiveness().Evaluate(probe),
			checks.NewEVMChainID().Evaluate(probe),
		}
	case TypeWSS:
		probe := checks.ProbeWSSEndpoint(ctx, j.chain, j.endpoint, ua)
		return []checks.Result{
			checks.NewWSSLiveness().Evaluate(probe),
			checks.NewWSSChainID().Evaluate(probe),
		}
	}
	return nil
}

func runWorkers(jobs []job, client *http.Client, timeout time.Duration, concurrency int, ua string) <-chan checks.Result {
	jobCh := make(chan job, len(jobs))
	for i := range jobs {
		jobCh <- jobs[i]
	}
	close(jobCh)

	resultCh := make(chan checks.Result, len(jobs)*2)

	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				slog.Debug("probing", "chain", j.chain.Name, "endpoint", j.endpoint.Address, "type", j.endpointType)
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				// Indexed rather than ranged by value: checks.Result is large enough that
				// copying it per iteration is wasteful (and gocritic's rangeValCopy flags it).
				// Skipped results are forwarded too — a rate-limited endpoint is a real
				// observation the report must count; collectResults keeps them out of the
				// stats, and updateState already filters them from streaks.
				probes := runProbe(ctx, client, j, ua)
				for i := range probes {
					resultCh <- probes[i]
				}
				cancel()
			}
		}()
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	return resultCh
}

func collectResults(resultCh <-chan checks.Result, verbose bool) (perChain map[string]*chainStats, keys []string, results []checks.Result) {
	perChain = map[string]*chainStats{}

	for r := range resultCh {
		results = append(results, r)
		key := r.Chain + "/" + r.ChainID
		printResultLine(&r, key, verbose)

		// Skipped results are collected for the report but kept out of the live/dead stats:
		// a rate-limited endpoint was not measured, and counting it either way is a guess.
		if r.Skipped {
			continue
		}

		s := perChain[key]
		if s == nil {
			s = &chainStats{}
			perChain[key] = s
		}

		var ts *typeStats
		switch r.Check {
		case "rpc_liveness":
			ts = &s.rpc
		case "rest_liveness":
			ts = &s.rest
		case "grpc_web_liveness":
			ts = &s.grpcWeb
		case "grpc_liveness":
			ts = &s.grpc
		case "evm_liveness":
			ts = &s.evm
		case "wss_liveness":
			ts = &s.wss
		case "rpc_chain_id", "rest_chain_id", "grpc_chain_id",
			"evm_chain_id", "grpc_web_chain_id", "wss_chain_id":
			if !r.Passed {
				s.chainIDFail++
			}
		}
		if ts != nil {
			ts.total++
			switch {
			case r.Passed:
				ts.live++
			case r.ConnFailed:
				ts.unreachable++
			default:
				ts.wrongResp++
			}
		}
	}

	keys = make([]string, 0, len(perChain))
	for k := range perChain {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return perChain, keys, results
}

// printResultLine emits the per-endpoint console line. SKIP appears only in verbose mode and
// only for skips that carry a reason (rate limiting); skipped chain-ID checks are pipeline
// artifacts and stay silent at every verbosity.
func printResultLine(r *checks.Result, key string, verbose bool) {
	switch {
	case r.Skipped:
		if verbose && r.FailureClass != checks.ClassNone {
			fmt.Printf("SKIP  %-35s  %-14s  %s  %s\n", key, r.Check, r.Endpoint, r.Evidence)
		}
	case r.Passed:
		if verbose {
			fmt.Printf("PASS  %-35s  %-14s  %s\n", key, r.Check, r.Endpoint)
		}
	case r.ConnFailed:
		fmt.Printf("ERR   %-35s  %-14s  %s  %s\n", key, r.Check, r.Endpoint, r.Evidence)
	default:
		fmt.Printf("FAIL  %-35s  %-14s  %s  %s\n", key, r.Check, r.Endpoint, r.Evidence)
	}
}

func buildActiveLivenessKeys(jobs []job) map[string]map[string]struct{} {
	active := map[string]map[string]struct{}{}
	for i := range jobs {
		j := jobs[i]
		checkName := j.endpointType.livenessCheckName()
		if checkName == "" {
			continue
		}
		if active[j.chain.Name] == nil {
			active[j.chain.Name] = map[string]struct{}{}
		}
		active[j.chain.Name][state.EndpointKey(checkName, j.endpoint.Address)] = struct{}{}
	}
	return active
}

// loadFilteredChains parses the --chains filter, loads the registry, and exits fatally when
// nothing usable is found — a run over zero chains must not look like a clean run.
func loadFilteredChains(cli CLI) (chains []registry.Chain, missing []string) {
	var filter []string
	if cli.Chains != "all" {
		for _, c := range strings.Split(cli.Chains, ",") {
			if t := strings.TrimSpace(c); t != "" {
				filter = append(filter, t)
			}
		}
	}
	chains, err := registry.LoadChains(cli.Registry, filter)
	if err != nil {
		slog.Error("failed to load chains", "err", err)
		os.Exit(exitFatal)
	}
	if len(chains) == 0 {
		slog.Error("no chains found in registry")
		os.Exit(exitFatal)
	}
	slog.Debug("chains loaded", "count", len(chains))
	return chains, missingFromRegistry(filter, chains)
}

// openPRFlows dispatches the three PR flows in order. A chain getting a status-flip PR is
// excluded from the per-endpoint flows: deleting endpoints or fixing hashes on a chain about
// to be marked killed is noise.
func openPRFlows(
	cli CLI,
	chains []registry.Chain,
	jobs []job,
	client *http.Client,
	stateMap map[string]state.ChainState,
	flagged int,
	deathCandidates []string,
	deathFacts map[string]*chainDeathFacts,
	domainLive map[string]int,
	now time.Time,
) {
	exclude := make(map[string]bool, len(deathCandidates))
	for _, name := range deathCandidates {
		exclude[name] = true
	}
	maybeOpenStatusPRs(cli, deathCandidates, deathFacts, domainLive, stateMap, now)
	if flagged > 0 {
		fmt.Printf("%d endpoint(s) flagged for action\n", flagged)
		maybeOpenPRs(cli, chains, jobs, client, stateMap, now, exclude)
	}
	maybeOpenHashPRs(cli, chains, stateMap, now, exclude)
}

// missingFromRegistry returns the --chains entries that matched no loaded chain, so they can be
// reported rather than silently ignored: a typo'd or renamed chain would otherwise look like a
// clean run that simply found nothing wrong.
func missingFromRegistry(filter []string, chains []registry.Chain) []string {
	if len(filter) == 0 {
		return nil
	}
	loaded := make(map[string]bool, len(chains))
	for i := range chains {
		loaded[chains[i].Name] = true
	}
	var missing []string
	for _, name := range filter {
		if !loaded[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// emitReport turns the run's results into records, optionally persists them as JSONL, and
// prints the aggregate report. The report always prints — it is the product, not an option;
// --report only controls whether the underlying records are kept for later --from analysis.
func emitReport(cli CLI, results []checks.Result, stateMap map[string]state.ChainState, now time.Time) {
	records := report.Build(results, stateMap, report.RunMeta{
		TS:             now,
		Vantage:        cli.Vantage,
		RegistryCommit: registryCommit(cli.Registry),
		Concurrency:    cli.Concurrency,
		Timeout:        cli.Timeout,
	})
	if len(records) == 0 {
		return
	}
	if cli.Report != "" {
		path, err := report.WriteRun(cli.Report, records)
		if err != nil {
			// The records were requested and cannot be recovered without re-probing for hours;
			// pretending the run succeeded would discard them silently.
			slog.Error("failed to write report records", "err", err)
			os.Exit(exitFatal)
		}
		fmt.Printf("\nrecords written: %s\n", path)
	}
	report.Render(os.Stdout, records)
}

// registryCommit resolves the git commit of the registry checkout being probed, so every record
// states which registry state it measured. Best-effort by design: a non-git registry directory
// or a machine without git yields "", and the field is simply omitted — an earlier canonical
// run's commit was lost exactly because nothing recorded it, but failing the run over
// provenance metadata would be backwards.
func registryCommit(registryPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", registryPath, "rev-parse", "HEAD").Output()
	if err != nil {
		slog.Debug("could not resolve registry commit", "err", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// renderFromFile is the offline mode: render the report from one previously written JSONL
// record file — no probing, no registry, no network. The file is named explicitly rather than
// discovered in a directory, so what the report describes is never ambiguous.
func renderFromFile(path string) {
	records, err := report.LoadFile(path)
	if err != nil {
		slog.Error("failed to load report records", "err", err)
		os.Exit(exitFatal)
	}
	fmt.Printf("report for run %s, vantage %s, %d records (%s)\n",
		records[0].RunTS.UTC().Format(time.RFC3339), orDefault(records[0].Vantage, "(no vantage)"),
		len(records), path)
	if records[0].RegistryCommit != "" {
		fmt.Printf("registry commit %s, probed at concurrency %d, timeout %dms\n",
			records[0].RegistryCommit, records[0].Concurrency, records[0].TimeoutMS)
	}
	report.Render(os.Stdout, records)
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// loadStateMap reads per-chain state, refusing to silently discard streaks it cannot parse.
//
// state.Load returns empty state and no error when a file is simply absent, which is the normal
// first run, so an error here means the file exists and could not be understood. Resetting those
// chains quietly would wipe streaks that take days to rebuild while the run still finished green
// — the symptom would appear much later, as PRs that never open, with nothing in the output
// pointing at the cause. Aborting is recoverable in seconds; silent loss is not.
func loadStateMap(
	chains []registry.Chain, statePath string, resetState bool,
) (map[string]state.ChainState, error) {
	stateMap := make(map[string]state.ChainState, len(chains))
	var unreadable []string
	for i := range chains {
		cs, err := state.Load(filepath.Join(statePath, chains[i].Name+".json"))
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s: %v", chains[i].Name, err))
			cs = state.ChainState{Endpoints: make(map[string]state.EndpointState)}
		}
		cs.ChainID = chains[i].ChainID
		stateMap[chains[i].Name] = cs
	}
	if len(unreadable) > 0 {
		if !resetState {
			return nil, fmt.Errorf(
				"%d state file(s) could not be read; refusing to discard accumulated streaks.\n"+
					"Fix or delete them, or pass --reset-state to start those chains fresh:\n  %s",
				len(unreadable), strings.Join(unreadable, "\n  "))
		}
		slog.Warn("discarding unreadable state files", "count", len(unreadable))
	}
	return stateMap, nil
}

func updateState(
	stateMap map[string]state.ChainState,
	results []checks.Result,
	activeKeys map[string]map[string]struct{},
	threshold int,
	now time.Time,
) int {
	for i := range results {
		r := &results[i]
		if r.Skipped || !strings.HasSuffix(r.Check, "_liveness") {
			continue
		}
		cs := stateMap[r.Chain]
		cs.Update(*r, now)
		stateMap[r.Chain] = cs
	}
	flagged := 0
	// Keys only, then explicit lookups: ChainState is too large to copy per iteration.
	for chainName := range stateMap {
		cs := stateMap[chainName]
		cs.Prune(activeKeys[chainName])
		stateMap[chainName] = cs
		// Keys only, then an explicit lookup: EndpointState is too large to copy on every
		// iteration. Do not collapse this back into `for key, ep := range`.
		for key := range cs.Endpoints {
			ep := cs.Endpoints[key]
			if ep.ConsecutiveFailures >= threshold {
				flagged++
				slog.Warn("endpoint flagged for action",
					"chain", chainName,
					"key", key,
					"consecutive_failures", ep.ConsecutiveFailures,
					"first_failure", ep.FirstFailureTime,
				)
			}
		}
	}
	return flagged
}

func saveStateMap(stateMap map[string]state.ChainState, statePath string, now time.Time) {
	// Keys only, then explicit lookups: ChainState is too large to copy per iteration.
	for chainName := range stateMap {
		if err := state.Save(filepath.Join(statePath, chainName+".json"), stateMap[chainName], now); err != nil {
			slog.Warn("could not save state", "chain", chainName, "err", err)
		}
	}
}

func printSummary(perChain map[string]*chainStats, keys []string) {
	var totals chainStats
	for _, s := range perChain {
		totals.rpc.add(s.rpc)
		totals.rest.add(s.rest)
		totals.grpcWeb.add(s.grpcWeb)
		totals.grpc.add(s.grpc)
		totals.evm.add(s.evm)
		totals.wss.add(s.wss)
		totals.chainIDFail += s.chainIDFail
	}

	nameW := len("chain/chain_id")
	for _, k := range keys {
		if len(k) > nameW {
			nameW = len(k)
		}
	}
	nameW += 2

	const numTypeCols = 6
	nameFmt := fmt.Sprintf("%%-%ds", nameW)
	const colW = "%-10s"
	rowFmt := nameFmt + "  " + colW + colW + colW + colW + colW + colW + "%d\n"
	ruler := strings.Repeat("─", nameW+2+10*numTypeCols+numTypeCols)

	fmt.Printf("\n"+nameFmt+"  "+colW+colW+colW+colW+colW+colW+"%s\n",
		"chain/chain_id", "rpc", "rest", "grpc", "grpc-web", "evm", "wss", "id_err")
	fmt.Printf("%s\n", ruler)
	for _, k := range keys {
		s := perChain[k]
		fmt.Printf(rowFmt,
			k,
			s.rpc.format(), s.rest.format(), s.grpc.format(),
			s.grpcWeb.format(), s.evm.format(), s.wss.format(),
			s.chainIDFail)
	}
	fmt.Printf("%s\n", ruler)
	fmt.Printf(rowFmt,
		"TOTAL",
		totals.rpc.format(), totals.rest.format(), totals.grpc.format(),
		totals.grpcWeb.format(), totals.evm.format(), totals.wss.format(),
		totals.chainIDFail)

	fmt.Printf("\n%d endpoints: %d live, %d dead (%d unreachable, %d wrong response),"+
		" %d chain ID mismatches across %d chains\n",
		totals.allEndpoints(), totals.allLive(), totals.allDead(),
		totals.allUnreachable(), totals.allWrongResp(),
		totals.chainIDFail, len(perChain))
}

// splitRepo splits "owner/repo" into two strings. Returns ("", ownerRepo) when malformed.
func splitRepo(ownerRepo string) (owner, repo string) {
	parts := strings.SplitN(ownerRepo, "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", ownerRepo
	}
	return parts[0], parts[1]
}

// collectFlagged builds per-chain flagged endpoint lists from stateMap.
// The state key format is "check|address".
func collectFlagged(stateMap map[string]state.ChainState, threshold int) map[string][]github.FlaggedEndpoint {
	result := make(map[string][]github.FlaggedEndpoint)
	// Keys only, then explicit lookups — see updateState: ChainState and EndpointState are both
	// too large to copy per iteration.
	for chainName := range stateMap {
		cs := stateMap[chainName]
		for key := range cs.Endpoints {
			ep := cs.Endpoints[key]
			if ep.ConsecutiveFailures < threshold {
				continue
			}
			check, address, ok := strings.Cut(key, "|")
			if !ok || strings.HasSuffix(check, "_chain_id") {
				continue
			}
			result[chainName] = append(result[chainName], github.FlaggedEndpoint{
				Check:               check,
				Address:             address,
				ConsecutiveFailures: ep.ConsecutiveFailures,
				FirstFailureTime:    ep.FirstFailureTime,
				FirstEvidence:       ep.FirstEvidence,
				LastEvidence:        ep.LastEvidence,
			})
		}
	}
	return result
}

// preflight re-probes only the flagged addresses and returns which passed per chain.
func preflight(
	allJobs []job,
	client *http.Client,
	timeout time.Duration,
	concurrency int,
	ua string,
	flagged map[string][]github.FlaggedEndpoint,
) map[string]map[string]bool {
	flaggedAddrs := make(map[string]map[string]struct{})
	for chainName, endpoints := range flagged {
		addrs := make(map[string]struct{}, len(endpoints))
		for _, ep := range endpoints {
			addrs[ep.Address] = struct{}{}
		}
		flaggedAddrs[chainName] = addrs
	}
	var filtered []job
	for i := range allJobs {
		j := allJobs[i]
		if addrs, ok := flaggedAddrs[j.chain.Name]; ok {
			if _, inSet := addrs[j.endpoint.Address]; inSet {
				filtered = append(filtered, j)
			}
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	resultCh := runWorkers(filtered, client, timeout, min(len(filtered), concurrency), ua)
	passed := make(map[string]map[string]bool)
	for r := range resultCh {
		if !strings.HasSuffix(r.Check, "_liveness") || r.Skipped {
			continue
		}
		if passed[r.Chain] == nil {
			passed[r.Chain] = make(map[string]bool)
		}
		if r.Passed {
			passed[r.Chain][r.Endpoint] = true
		}
	}
	return passed
}

// applyPreflightResults resets the failure streak for any endpoint that passed preflight.
func applyPreflightResults(
	stateMap map[string]state.ChainState,
	flagged map[string][]github.FlaggedEndpoint,
	passed map[string]map[string]bool,
	now time.Time,
) {
	for chainName, endpoints := range flagged {
		passedMap := passed[chainName]
		if len(passedMap) == 0 {
			continue
		}
		cs := stateMap[chainName]
		for _, ep := range endpoints {
			if !passedMap[ep.Address] {
				continue
			}
			key := state.EndpointKey(ep.Check, ep.Address)
			es := cs.Endpoints[key]
			es.ConsecutiveFailures = 0
			es.LastPassed = true
			es.FirstFailureTime = time.Time{}
			es.FirstEvidence = ""
			es.LastEvidence = ""
			es.LastChecked = now
			cs.Endpoints[key] = es
		}
		stateMap[chainName] = cs
	}
}

// tryOpenChainPR checks the cooldown, then opens (or dry-run logs) a PR for one chain.
// Returns true if a PR was opened (or would be in dry-run).
func tryOpenChainPR(
	ctx context.Context,
	ghClient *github.Client,
	chain registry.Chain,
	dead []github.FlaggedEndpoint,
	cs state.ChainState,
	cooldown time.Duration,
	owner, repo, registryPath string,
	now time.Time,
	dryRun bool,
) bool {
	if cooldown > 0 && !cs.LastPROpenedAt.IsZero() && now.Sub(cs.LastPROpenedAt) < cooldown {
		slog.Warn("skipping PR (cooldown)", "chain", chain.Name, "last_pr", cs.LastPROpenedAt.Format(time.RFC3339))
		return false
	}
	if dryRun {
		fmt.Printf("DRY-RUN: would open PR for %s (%d dead endpoint(s))\n", chain.Name, len(dead))
		for _, ep := range dead {
			fmt.Printf("  %s  %s  (%d consecutive failures)\n", ep.Check, ep.Address, ep.ConsecutiveFailures)
		}
		return true
	}
	req := github.PRRequest{
		Owner: owner, Repo: repo, Chain: chain, Dead: dead, RegistryPath: registryPath,
	}
	prURL, err := github.OpenChainPR(ctx, ghClient, req)
	if err != nil {
		slog.Error("failed to open PR", "chain", chain.Name, "err", err)
		return false
	}
	if prURL == "" {
		slog.Warn("PR skipped (already open or no-op)", "chain", chain.Name)
		return false
	}
	fmt.Printf("opened PR: %s\n", prURL)
	return true
}

// openPRs iterates chains in order, skips chains with nothing to do or where all
// flagged endpoints recovered in preflight, enforces the ceiling, and calls
// tryOpenChainPR. Returns the count of PRs opened (or would-be in dry-run).
func openPRs(
	ctx context.Context,
	ghClient *github.Client,
	chains []registry.Chain,
	flagged map[string][]github.FlaggedEndpoint,
	passed map[string]map[string]bool,
	stateMap map[string]state.ChainState,
	cooldown time.Duration,
	owner, repo, registryPath string,
	maxNew int,
	now time.Time,
	dryRun bool,
) int {
	opened := 0
	for i := range chains {
		if opened >= maxNew {
			break
		}
		ch := chains[i]
		dead := flagged[ch.Name]
		if len(dead) == 0 {
			continue
		}
		stillDead := dead
		if pm := passed[ch.Name]; len(pm) > 0 {
			stillDead = stillDead[:0]
			for _, ep := range dead {
				if !pm[ep.Address] {
					stillDead = append(stillDead, ep)
				}
			}
		}
		if len(stillDead) == 0 {
			continue
		}
		cs := stateMap[ch.Name]
		if tryOpenChainPR(ctx, ghClient, ch, stillDead, cs, cooldown, owner, repo, registryPath, now, dryRun) {
			opened++
			if !dryRun {
				cs.LastPROpenedAt = now
				stateMap[ch.Name] = cs
			}
		}
	}
	return opened
}

// maybeOpenPRs is the top-level gate: checks that a token and repo are set,
// runs preflight, applies resets, then opens PRs up to the configured ceiling.
func maybeOpenPRs(
	cli CLI,
	chains []registry.Chain,
	jobs []job,
	probeClient *http.Client,
	stateMap map[string]state.ChainState,
	now time.Time,
	exclude map[string]bool,
) {
	repo := cli.GithubRepo
	if repo == "" {
		repo = os.Getenv("GITHUB_REPOSITORY")
	}
	owner, repoName := splitRepo(repo)
	token := cli.GithubToken
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if !cli.DryRun {
		if token == "" {
			slog.Warn("github-token not set; skipping PR opening")
			return
		}
		if owner == "" {
			slog.Warn("github-repo not set or malformed (expected owner/repo); skipping PR opening",
				"value", strings.ReplaceAll(strings.ReplaceAll(repo, "\n", ""), "\r", ""))
			return
		}
	}
	maxNew := cli.MaxEndpointPRs
	cooldown := time.Duration(cli.PRCooldownDays) * 24 * time.Hour
	flagged := collectFlagged(stateMap, cli.MinFailures)
	for name := range exclude {
		delete(flagged, name)
	}
	if len(flagged) == 0 {
		return
	}
	passed := preflight(jobs, probeClient, cli.Timeout, cli.Concurrency, cli.UserAgent, flagged)
	applyPreflightResults(stateMap, flagged, passed, now)
	ctx := context.Background()
	var ghClient *github.Client
	if !cli.DryRun {
		ghClient = github.NewClient(token)
	}
	openPRs(ctx, ghClient, chains, flagged, passed, stateMap, cooldown, owner, repoName, cli.Registry, maxNew, now, cli.DryRun)
	if !cli.DryRun {
		saveStateMap(stateMap, cli.StatePath, now)
	}
}

// runHashChecks loads assetlist.json for every chain and returns per-chain hash mismatches.
func runHashChecks(chains []registry.Chain, registryPath string) map[string][]checks.HashMismatch {
	result := make(map[string][]checks.HashMismatch)
	for i := range chains {
		al, err := registry.LoadAssetList(registryPath, chains[i].Name)
		if err != nil {
			slog.Warn("could not load assetlist", "chain", chains[i].Name, "err", err)
			continue
		}
		if mm := checks.CheckDenomHashes(al); len(mm) > 0 {
			result[chains[i].Name] = mm
		}
	}
	slog.Debug("hash check complete", "chains_checked", len(chains), "mismatches", len(result))
	return result
}

// hashRulerWidth is the width of the IBC denom hash summary ruler.
const hashRulerWidth = 80

// printHashSummary prints a table of IBC denom hash mismatches to stdout.
func printHashSummary(chains []registry.Chain, allMismatches map[string][]checks.HashMismatch) {
	if len(allMismatches) == 0 {
		return
	}
	total := 0
	for _, mm := range allMismatches {
		total += len(mm)
	}
	ruler := strings.Repeat("─", hashRulerWidth)
	fmt.Printf("\nIBC denom hash mismatches\n%s\n", ruler)
	for i := range chains {
		for _, m := range allMismatches[chains[i].Name] {
			fmt.Printf("%-30s  %-35s  %s\n", m.ChainName, m.AssetName, m.Path)
		}
	}
	fmt.Printf("%s\n%d asset(s) across %d chain(s) with wrong IBC denom hash\n", ruler, total, len(allMismatches))
}

// tryOpenHashPR checks the cooldown, then opens (or dry-run logs) a hash-fix PR for one chain.
func tryOpenHashPR(
	ctx context.Context,
	ghClient *github.Client,
	chainName string,
	mm []checks.HashMismatch,
	cs state.ChainState,
	cooldown time.Duration,
	owner, repo, registryPath string,
	now time.Time,
	dryRun bool,
) bool {
	if cooldown > 0 && !cs.LastHashPROpenedAt.IsZero() && now.Sub(cs.LastHashPROpenedAt) < cooldown {
		slog.Warn("skipping hash PR (cooldown)", "chain", chainName, "last_pr", cs.LastHashPROpenedAt.Format(time.RFC3339))
		return false
	}
	if dryRun {
		fmt.Printf("DRY-RUN: would open hash PR for %s (%d mismatch(es))\n", chainName, len(mm))
		for _, m := range mm {
			fmt.Printf("  %s  %s -> %s\n", m.AssetName, m.Base, m.Expected)
		}
		return true
	}
	fixes := make([]github.HashFix, len(mm))
	for i, m := range mm {
		fixes[i] = github.HashFix{AssetName: m.AssetName, Base: m.Base, Expected: m.Expected, Path: m.Path}
	}
	req := github.HashPRRequest{
		Owner: owner, Repo: repo, ChainName: chainName, Fixes: fixes, RegistryPath: registryPath,
	}
	prURL, err := github.OpenHashPR(ctx, ghClient, req)
	if err != nil {
		slog.Error("failed to open hash PR", "chain", chainName, "err", err)
		return false
	}
	if prURL == "" {
		slog.Warn("hash PR skipped (already open or no-op)", "chain", chainName)
		return false
	}
	fmt.Printf("opened hash PR: %s\n", prURL)
	return true
}

// maybeOpenHashPRs runs hash checks across all chains and opens PRs for mismatches.
func maybeOpenHashPRs(
	cli CLI,
	chains []registry.Chain,
	stateMap map[string]state.ChainState,
	now time.Time,
	exclude map[string]bool,
) {
	allMismatches := runHashChecks(chains, cli.Registry)
	printHashSummary(chains, allMismatches)
	if len(allMismatches) == 0 || stateMap == nil {
		return
	}
	repo := cli.GithubRepo
	if repo == "" {
		repo = os.Getenv("GITHUB_REPOSITORY")
	}
	owner, repoName := splitRepo(repo)
	token := cli.GithubToken
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if !cli.DryRun {
		if token == "" {
			slog.Warn("github-token not set; skipping hash PR opening")
			return
		}
		if owner == "" {
			slog.Warn("github-repo not set or malformed; skipping hash PR opening",
				"value", strings.ReplaceAll(strings.ReplaceAll(repo, "\n", ""), "\r", ""))
			return
		}
	}
	cooldown := time.Duration(cli.PRCooldownDays) * 24 * time.Hour
	ctx := context.Background()
	var ghClient *github.Client
	if !cli.DryRun {
		ghClient = github.NewClient(token)
	}
	opened := 0
	for i := range chains {
		ch := chains[i]
		if exclude[ch.Name] {
			continue
		}
		mm := allMismatches[ch.Name]
		if len(mm) == 0 {
			continue
		}
		if opened >= cli.MaxHashPRs {
			slog.Warn("hash-fix PR cap reached; remaining chains wait for the next run", "cap", cli.MaxHashPRs)
			break
		}
		cs := stateMap[ch.Name]
		if tryOpenHashPR(ctx, ghClient, ch.Name, mm, cs, cooldown, owner, repoName, cli.Registry, now, cli.DryRun) {
			opened++
			if !cli.DryRun {
				cs.LastHashPROpenedAt = now
				stateMap[ch.Name] = cs
			}
		}
	}
	if !cli.DryRun {
		saveStateMap(stateMap, cli.StatePath, now)
	}
}

func main() {
	var cli CLI
	kong.Parse(&cli,
		kong.Name("sentinel"),
		kong.Description("Verify chain-registry entries against on-chain reality."),
		kong.Vars{"version": Version},
	)

	// Computed here rather than as a kong default because it embeds the build version.
	if cli.UserAgent == "" {
		cli.UserAgent = "chain-registry-sentinel/" + Version +
			" (+https://github.com/ny4rl4th0t3p/chain-registry-sentinel)"
	}

	logLevel := slog.LevelWarn
	if cli.Verbose {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	if cli.From != "" {
		if cli.Report != "" {
			slog.Error("--report cannot be combined with --from: --from renders from records that already exist")
			os.Exit(exitFatal)
		}
		renderFromFile(cli.From)
		return
	}
	if cli.Registry == "" {
		slog.Error("--registry is required unless --from is used")
		os.Exit(exitFatal)
	}

	chains, missingChains := loadFilteredChains(cli)

	jobs := buildJobs(chains)
	client := checks.NewHTTPClient(cli.Timeout, cli.UserAgent)

	var stateMap map[string]state.ChainState
	if cli.StatePath != "" {
		var err error
		stateMap, err = loadStateMap(chains, cli.StatePath, cli.ResetState)
		if err != nil {
			slog.Error("failed to load state", "err", err)
			os.Exit(exitFatal)
		}
	}

	resultCh := runWorkers(jobs, client, cli.Timeout, cli.Concurrency, cli.UserAgent)
	perChain, keys, results := collectResults(resultCh, cli.Verbose)

	now := time.Now().UTC()
	flagged := 0
	var deathCandidates []string
	var deathFacts map[string]*chainDeathFacts
	var domainLive map[string]int
	if cli.StatePath != "" {
		activeKeys := buildActiveLivenessKeys(jobs)
		flagged = updateState(stateMap, results, activeKeys, cli.MinFailures, now)
		// Before the save, so the chain-death streaks persist with the endpoint streaks.
		deathCandidates, deathFacts, domainLive = runChainDeathDetection(results, stateMap, cli.ChainDeathMinRuns, cli.ChainDeathStaleAfter, now)
		if !cli.DryRun {
			saveStateMap(stateMap, cli.StatePath, now)
		}
	}

	printSummary(perChain, keys)
	emitReport(cli, results, stateMap, now)
	openPRFlows(cli, chains, jobs, client, stateMap, flagged, deathCandidates, deathFacts, domainLive, now)

	for _, name := range missingChains {
		slog.Warn("chain not found in registry", "chain", name, "hint", "no chain.json — may be EVM-only or unlisted")
	}
}
