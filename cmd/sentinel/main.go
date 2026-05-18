package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/github"
	"chain-registry-sentinel/internal/registry"
	"chain-registry-sentinel/internal/state"
)

// Version is injected at build time via -ldflags.
var Version = "dev"

const maxPRCeiling = 5

type CLI struct {
	Registry       string           `help:"Path to local chain-registry clone" env:"INPUT_REGISTRY" required:""`
	Chains         string           `help:"Comma-separated chain names, or 'all'" env:"INPUT_CHAINS" default:"all"`
	Timeout        time.Duration    `help:"HTTP timeout per request" env:"INPUT_TIMEOUT" default:"30s"`
	Concurrency    int              `help:"Max simultaneous endpoint probes" env:"INPUT_CONCURRENCY" default:"250"`
	StatePath      string           `help:"Directory for per-chain state files" env:"INPUT_STATE_PATH"`
	MinFailures    int              `help:"Consecutive failures before flagging an endpoint" env:"INPUT_MIN_FAILURES" default:"14"`
	DryRun         bool             `help:"Read state but do not write it or open PRs" env:"INPUT_DRY_RUN"`
	GithubToken    string           `help:"GitHub token for opening PRs" env:"INPUT_GITHUB_TOKEN"`
	GithubRepo     string           `help:"Target repo (owner/repo)" env:"INPUT_GITHUB_REPO"`
	MaxNewPRs      int              `help:"Max new PRs per run (ceiling: 5)" env:"INPUT_MAX_NEW_PRS" default:"5"`
	PRCooldownDays int              `help:"Days between PRs per chain" env:"INPUT_PR_COOLDOWN_DAYS" default:"7"`
	Verbose        bool             `short:"v" help:"Enable debug logging to stderr" env:"INPUT_VERBOSE"`
	Version        kong.VersionFlag `name:"version" help:"Print version and exit"`
}

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

func (t EndpointType) livenessCheckName() string {
	switch t {
	case TypeRPC:
		return "rpc_liveness"
	case TypeREST:
		return "rest_liveness"
	case TypeGRPCWeb:
		return "grpc_web_liveness"
	case TypeGRPC:
		return "grpc_liveness"
	case TypeEVM:
		return "evm_liveness"
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
}

func buildJobs(chains []registry.Chain) []job {
	var jobs []job
	for i := range chains {
		ch := chains[i]
		switch ch.ChainType {
		case "cosmos":
			for _, ep := range ch.RPCs {
				jobs = append(jobs, job{chain: ch, endpoint: ep, endpointType: TypeRPC})
			}
			for _, ep := range ch.RESTEndpoints {
				jobs = append(jobs, job{chain: ch, endpoint: ep, endpointType: TypeREST})
			}
			for _, ep := range ch.GRPCWebEndpoints {
				jobs = append(jobs, job{chain: ch, endpoint: ep, endpointType: TypeGRPCWeb})
			}
			for _, ep := range ch.GRPCEndpoints {
				jobs = append(jobs, job{chain: ch, endpoint: ep, endpointType: TypeGRPC})
			}
			for _, ep := range ch.WSSEndpoints {
				jobs = append(jobs, job{chain: ch, endpoint: ep, endpointType: TypeWSS})
			}
		case "eip155":
			for _, ep := range ch.EVMEndpoints {
				jobs = append(jobs, job{chain: ch, endpoint: ep, endpointType: TypeEVM})
			}
		}
	}
	return jobs
}

func runProbe(ctx context.Context, client *http.Client, j job) []checks.Result {
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
		probe := checks.ProbeGRPCEndpoint(ctx, j.chain, j.endpoint)
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
		probe := checks.ProbeWSSEndpoint(ctx, j.chain, j.endpoint)
		return []checks.Result{
			checks.NewWSSLiveness().Evaluate(probe),
			checks.NewWSSChainID().Evaluate(probe),
		}
	}
	return nil
}

func runWorkers(jobs []job, client *http.Client, timeout time.Duration, concurrency int) <-chan checks.Result {
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
				for _, r := range runProbe(ctx, client, j) {
					if !r.Skipped {
						resultCh <- r
					}
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

		label := r.Chain + "/" + r.ChainID
		switch {
		case r.Passed:
			if verbose {
				fmt.Printf("PASS  %-35s  %-14s  %s\n", label, r.Check, r.Endpoint)
			}
		case r.ConnFailed:
			fmt.Printf("ERR   %-35s  %-14s  %s  %s\n", label, r.Check, r.Endpoint, r.Evidence)
		default:
			fmt.Printf("FAIL  %-35s  %-14s  %s  %s\n", label, r.Check, r.Endpoint, r.Evidence)
		}
	}

	keys = make([]string, 0, len(perChain))
	for k := range perChain {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return perChain, keys, results
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

func loadStateMap(chains []registry.Chain, statePath string) map[string]state.ChainState {
	stateMap := make(map[string]state.ChainState, len(chains))
	for i := range chains {
		cs, err := state.Load(filepath.Join(statePath, chains[i].Name+".json"))
		if err != nil {
			slog.Warn("could not load state", "chain", chains[i].Name, "err", err)
			cs = state.ChainState{Endpoints: make(map[string]state.EndpointState)}
		}
		cs.ChainID = chains[i].ChainID
		stateMap[chains[i].Name] = cs
	}
	return stateMap
}

func updateState(
	stateMap map[string]state.ChainState,
	results []checks.Result,
	activeKeys map[string]map[string]struct{},
	threshold int,
	now time.Time,
) int {
	for _, r := range results {
		if r.Skipped || !strings.HasSuffix(r.Check, "_liveness") {
			continue
		}
		cs := stateMap[r.Chain]
		cs.Update(r, now)
		stateMap[r.Chain] = cs
	}
	flagged := 0
	for chainName, cs := range stateMap {
		cs.Prune(activeKeys[chainName])
		stateMap[chainName] = cs
		for key, ep := range cs.Endpoints {
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
	for chainName, cs := range stateMap {
		if err := state.Save(filepath.Join(statePath, chainName+".json"), cs, now); err != nil {
			slog.Warn("could not save state", "chain", chainName, "err", err)
		}
	}
}

func printSummary(perChain map[string]*chainStats, keys []string) chainStats {
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

	return totals
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
	for chainName, cs := range stateMap {
		for key, ep := range cs.Endpoints {
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
	resultCh := runWorkers(filtered, client, timeout, min(len(filtered), concurrency))
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
		slog.Info("skipping PR (cooldown)", "chain", chain.Name, "last_pr", cs.LastPROpenedAt.Format(time.RFC3339))
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
		slog.Info("PR skipped (already open or no-op)", "chain", chain.Name)
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
	maxNew := min(cli.MaxNewPRs, maxPRCeiling)
	cooldown := time.Duration(cli.PRCooldownDays) * 24 * time.Hour
	flagged := collectFlagged(stateMap, cli.MinFailures)
	if len(flagged) == 0 {
		return
	}
	passed := preflight(jobs, probeClient, cli.Timeout, cli.Concurrency, flagged)
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

func main() {
	var cli CLI
	kong.Parse(&cli,
		kong.Name("sentinel"),
		kong.Description("Verify chain-registry entries against on-chain reality."),
		kong.Vars{"version": Version},
	)

	logLevel := slog.LevelWarn
	if cli.Verbose {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

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
		os.Exit(1)
	}
	if len(chains) == 0 {
		slog.Error("no chains found in registry")
		os.Exit(1)
	}
	slog.Debug("chains loaded", "count", len(chains))

	var missingChains []string
	if len(filter) > 0 {
		loaded := make(map[string]bool, len(chains))
		for i := range chains {
			loaded[chains[i].Name] = true
		}
		for _, name := range filter {
			if !loaded[name] {
				missingChains = append(missingChains, name)
			}
		}
	}

	jobs := buildJobs(chains)
	client := checks.NewHTTPClient(cli.Timeout)

	var stateMap map[string]state.ChainState
	if cli.StatePath != "" {
		stateMap = loadStateMap(chains, cli.StatePath)
	}

	resultCh := runWorkers(jobs, client, cli.Timeout, cli.Concurrency)
	perChain, keys, results := collectResults(resultCh, cli.Verbose)

	now := time.Now().UTC()
	flagged := 0
	if cli.StatePath != "" {
		activeKeys := buildActiveLivenessKeys(jobs)
		flagged = updateState(stateMap, results, activeKeys, cli.MinFailures, now)
		if !cli.DryRun {
			saveStateMap(stateMap, cli.StatePath, now)
		}
	}

	totals := printSummary(perChain, keys)

	if flagged > 0 {
		fmt.Printf("%d endpoint(s) flagged for action\n", flagged)
		maybeOpenPRs(cli, chains, jobs, client, stateMap, now)
	}

	for _, name := range missingChains {
		slog.Warn("chain not found in registry", "chain", name, "hint", "no chain.json — may be EVM-only or unlisted")
	}

	if totals.allDead() > 0 || totals.chainIDFail > 0 {
		os.Exit(1)
	}
}
