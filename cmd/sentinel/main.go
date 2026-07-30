package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/kong"

	"chain-registry-sentinel/internal/checks"
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
	var looksDead map[string]bool
	var deathFacts map[string]*chainDeathFacts
	var domainLive map[string]int
	if cli.StatePath != "" {
		activeKeys := buildActiveLivenessKeys(jobs)
		flagged = updateState(stateMap, results, activeKeys, cli.MinFailures, now)
		// Before the save, so the chain-death streaks persist with the endpoint streaks.
		deathCandidates, looksDead, deathFacts, domainLive = runChainDeathDetection(
			results, stateMap, cli.ChainDeathMinRuns, cli.ChainDeathStaleAfter, now)
		if !cli.DryRun {
			saveStateMap(stateMap, cli.StatePath, now)
		}
	}

	printSummary(perChain, keys)
	emitReport(cli, results, stateMap, now)
	openPRFlows(cli, chains, jobs, client, stateMap, flagged, deathCandidates, looksDead, deathFacts, domainLive, now)

	for _, name := range missingChains {
		slog.Warn("chain not found in registry", "chain", name, "hint", "no chain.json — may be EVM-only or unlisted")
	}
}
