package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/github"
	"chain-registry-sentinel/internal/report"
	"chain-registry-sentinel/internal/state"
)

// Chain-death detection: a chain, not just its endpoints, can be dead — and the correct PR is
// then a one-line status flip, not dozens of endpoint deletions. Two machine-checkable
// signatures, both required to persist across --chain-death-min-runs runs:
//
//   - unreachable + withdrawal: zero live core endpoints while healthy operators (alive on
//     other chains) return nothing for this one — deliberate abandonment, the Evmos pattern;
//   - halted: every answering, synced RPC reports a latest_block_time frozen past
//     blockStaleAfter — the survivors are answering about a chain that stopped advancing.
//
// minWithdrawnOperators below this, "all dead" cannot be distinguished from "all its
// operators happened to die" — the withdrawal signature needs independent witnesses.
// Mirrors the constant in internal/report; keep the two in agreement.
//
// The other two dials are flags, not constants: --chain-death-stale-after (how old the newest
// block must be before an answering chain counts as halted — an operator running a long
// disaster recovery raises it so a known outage never starts a death streak) and
// --max-status-prs (per-run cap on these rare, high-visibility PRs — opening fifteen at once
// would be terrible optics even if all fifteen were right).
const minWithdrawnOperators = 3

type checkTally struct {
	total, live int
	classes     map[checks.FailureClass]int
}

type chainDeathFacts struct {
	endpointsProbed     int
	coreTotal, coreLive int
	deadDomains         map[string]int // registrable domain -> dead endpoints on this chain
	answeringRPC        int            // RPCs that answered and are not catching up
	staleRPC            int            // of those, latest_block_time older than blockStaleAfter
	newestBlock         time.Time
	perCheck            map[string]*checkTally
	sampleHost          string // a dead hostname for the PR's verify line; prefers a DNS-dead one
	sampleIsDNS         bool
}

// vantageSuspect mirrors the report's vantage-health warning: when this run's failures are
// dominated by resolver/routing classes, the measurement indicts the prober's environment. A
// suspect run must neither advance nor reset chain-death streaks — the DNS-overload incident
// showed a broken resolver manufactures "every chain unreachable" in a single run.
func vantageSuspect(results []checks.Result) bool {
	failed, suspect := 0, 0
	for i := range results {
		r := &results[i]
		if !strings.HasSuffix(r.Check, "_liveness") || r.Skipped || r.Passed {
			continue
		}
		failed++
		if r.FailureClass == checks.ClassDNSFailure || r.FailureClass == checks.ClassVantageNoRoute {
			suspect++
		}
	}
	return failed > 0 && 100*float64(suspect)/float64(failed) >= report.VantageWarnPct
}

// buildChainDeathFacts distills one run's results into per-chain death evidence plus the
// cross-registry domain liveness map the withdrawal signature compares against.
func buildChainDeathFacts(
	results []checks.Result, now time.Time, staleAfter time.Duration,
) (facts map[string]*chainDeathFacts, domainLive map[string]int) {
	facts = map[string]*chainDeathFacts{}
	domainLive = map[string]int{}
	for i := range results {
		r := &results[i]
		if !strings.HasSuffix(r.Check, "_liveness") || r.Skipped {
			continue
		}
		f := facts[r.Chain]
		if f == nil {
			f = &chainDeathFacts{deadDomains: map[string]int{}, perCheck: map[string]*checkTally{}}
			facts[r.Chain] = f
		}
		f.endpointsProbed++
		tally := f.perCheck[r.Check]
		if tally == nil {
			tally = &checkTally{classes: map[checks.FailureClass]int{}}
			f.perCheck[r.Check] = tally
		}
		tally.total++

		core := r.Check == checkRPCLiveness || r.Check == checkRESTLiveness || r.Check == checkEVMLiveness
		if core {
			f.coreTotal++
		}
		dom := report.EndpointDomain(r.Endpoint)

		if r.Passed {
			tally.live++
			domainLive[dom]++
			if core {
				f.coreLive++
			}
			if r.Check == checkRPCLiveness && !r.CatchingUp {
				f.answeringRPC++
				if !r.LatestBlockTime.IsZero() {
					if now.Sub(r.LatestBlockTime) > staleAfter {
						f.staleRPC++
					}
					if r.LatestBlockTime.After(f.newestBlock) {
						f.newestBlock = r.LatestBlockTime
					}
				}
			}
			continue
		}
		f.deadDomains[dom]++
		tally.classes[r.FailureClass]++
		if dns := r.FailureClass == checks.ClassDNSNXDomain; f.sampleHost == "" || (dns && !f.sampleIsDNS) {
			f.sampleHost = report.EndpointHost(r.Endpoint)
			f.sampleIsDNS = dns
		}
	}
	return facts, domainLive
}

// chainLooksDead applies both detection paths to one chain's facts.
//
// The withdrawal threshold adapts to chain size: what matters is the fraction of available
// witnesses that testify, not the absolute count. A 30-operator chain needs
// minWithdrawnOperators of them alive elsewhere; a 2-operator chain needs both — two
// independent healthy operators deliberately dropping the same chain is still strong. The
// floor is 2 because one witness is an anecdote (a single stale record while the chain lives
// on infrastructure the sentinel cannot see would produce a wrong kill PR), so
// single-operator chains are undetectable via this path by design — the halted path covers
// them whenever anything still answers.
func chainLooksDead(f *chainDeathFacts, domainLive map[string]int) bool {
	withdrawn := 0
	for d := range f.deadDomains {
		if domainLive[d] > 0 {
			withdrawn++
		}
	}
	needed := minWithdrawnOperators
	if n := len(f.deadDomains); n < needed {
		needed = n
	}
	if needed < 2 {
		needed = 2
	}
	unreachable := f.coreTotal > 0 && f.coreLive == 0 && withdrawn >= needed
	halted := f.answeringRPC > 0 && f.staleRPC == f.answeringRPC
	return unreachable || halted
}

// runChainDeathDetection updates per-chain death streaks in state and returns the chains whose
// streak has reached minRuns, ready for a status PR. looksDead holds every chain that looks
// dead this run, matured or not — the per-endpoint PR flows exclude those, because a chain in
// the maturing window would otherwise get an endpoint-removal PR that deletes every core
// endpoint: a death certificate filed under the wrong title. It refuses to move any streak
// when the vantage is suspect (guard 1), and per chain when no core check ran (guard 2 — the
// same trap as endpoint-state pruning under a future --checks selector, pre-empted here).
func runChainDeathDetection(
	results []checks.Result,
	stateMap map[string]state.ChainState,
	minRuns int,
	staleAfter time.Duration,
	now time.Time,
) (candidates []string, looksDead map[string]bool, facts map[string]*chainDeathFacts, domainLive map[string]int) {
	if stateMap == nil {
		return nil, nil, nil, nil
	}
	if vantageSuspect(results) {
		slog.Warn("vantage looks unhealthy; chain-death streaks frozen this run")
		return nil, nil, nil, nil
	}
	facts, domainLive = buildChainDeathFacts(results, now, staleAfter)
	looksDead = make(map[string]bool)
	for name, f := range facts {
		if f.coreTotal == 0 {
			continue
		}
		dead := chainLooksDead(f, domainLive)
		if dead {
			looksDead[name] = true
		}
		cs, ok := stateMap[name]
		if !ok {
			continue
		}
		if dead {
			if cs.ChainDeadStreak == 0 {
				cs.ChainDeadFirstTime = now
			}
			cs.ChainDeadStreak++
		} else {
			cs.ChainDeadStreak = 0
			cs.ChainDeadFirstTime = time.Time{}
		}
		stateMap[name] = cs
		if cs.ChainDeadStreak >= minRuns {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	return candidates, looksDead, facts, domainLive
}

// buildStatusEvidence assembles the PR body's evidence from this run's facts and the streak.
// Everything here was gathered by probing; nothing requires human research.
func buildStatusEvidence(f *chainDeathFacts, domainLive map[string]int, cs state.ChainState) github.StatusPREvidence {
	ev := github.StatusPREvidence{
		Streak:          cs.ChainDeadStreak,
		FirstSeen:       cs.ChainDeadFirstTime,
		EndpointsProbed: f.endpointsProbed,
		NewestBlockTime: f.newestBlock,
		SampleHost:      f.sampleHost,
	}
	for _, check := range []string{
		checkRPCLiveness, checkRESTLiveness, "grpc_liveness", "grpc_web_liveness", checkEVMLiveness, "wss_liveness",
	} {
		t := f.perCheck[check]
		if t == nil {
			continue
		}
		ev.ClassLines = append(ev.ClassLines, fmt.Sprintf("| `%s` | %d/%d live%s |",
			strings.TrimSuffix(check, "_liveness"), t.live, t.total, dominantClasses(t.classes)))
	}
	for d, deadHere := range f.deadDomains {
		if live := domainLive[d]; live > 0 {
			ev.Withdrawn = append(ev.Withdrawn, github.WithdrawnOperator{
				Domain: d, LiveElsewhere: live, DeadHere: deadHere,
			})
		}
	}
	sort.Slice(ev.Withdrawn, func(i, j int) bool {
		if ev.Withdrawn[i].DeadHere != ev.Withdrawn[j].DeadHere {
			return ev.Withdrawn[i].DeadHere > ev.Withdrawn[j].DeadHere
		}
		return ev.Withdrawn[i].Domain < ev.Withdrawn[j].Domain
	})
	return ev
}

// dominantClasses renders up to the two most common failure classes as " — a (n), b (m)".
func dominantClasses(classes map[checks.FailureClass]int) string {
	type cc struct {
		c checks.FailureClass
		n int
	}
	var list []cc
	for c, n := range classes {
		list = append(list, cc{c, n})
	}
	if len(list) == 0 {
		return ""
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].c < list[j].c
	})
	if len(list) > 2 {
		list = list[:2]
	}
	parts := make([]string, 0, len(list))
	for _, e := range list {
		parts = append(parts, fmt.Sprintf("%s (%d)", e.c, e.n))
	}
	return " — " + strings.Join(parts, ", ")
}

// maybeOpenStatusPRs opens status-flip PRs for matured chain-death candidates, mirroring the
// guards of the other PR flows: token and repo required, per-chain cooldown, open-PR check
// inside OpenStatusPR, a per-run cap, and dry-run printing instead of acting.
func maybeOpenStatusPRs(
	cli CLI,
	candidates []string,
	facts map[string]*chainDeathFacts,
	domainLive map[string]int,
	stateMap map[string]state.ChainState,
	now time.Time,
) {
	if len(candidates) == 0 || stateMap == nil {
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
			slog.Warn("github-token not set; skipping status PR opening")
			return
		}
		if owner == "" {
			slog.Warn("github-repo not set or malformed; skipping status PR opening")
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
	for _, name := range candidates {
		if opened >= cli.MaxStatusPRs {
			slog.Warn("status PR cap reached; remaining candidates wait for the next run",
				"cap", cli.MaxStatusPRs, "remaining", len(candidates)-opened)
			break
		}
		cs := stateMap[name]
		if cooldown > 0 && !cs.LastStatusPROpenedAt.IsZero() && now.Sub(cs.LastStatusPROpenedAt) < cooldown {
			slog.Warn("skipping status PR (cooldown)", "chain", name,
				"last_pr", cs.LastStatusPROpenedAt.Format(time.RFC3339))
			continue
		}
		if cli.DryRun {
			fmt.Printf("DRY-RUN: would open status PR marking %s as killed (dead for %d runs)\n",
				name, cs.ChainDeadStreak)
			opened++
			continue
		}
		prURL, err := github.OpenStatusPR(ctx, ghClient, github.StatusPRRequest{
			Owner:        owner,
			Repo:         repoName,
			ChainName:    name,
			RegistryPath: cli.Registry,
			Evidence:     buildStatusEvidence(facts[name], domainLive, cs),
		})
		if err != nil {
			slog.Warn("could not open status PR", "chain", name, "err", err)
			continue
		}
		if prURL == "" {
			slog.Warn("status PR skipped (already open or no-op)", "chain", name)
			continue
		}
		fmt.Printf("opened status PR: %s\n", prURL)
		cs.LastStatusPROpenedAt = now
		stateMap[name] = cs
		opened++
	}
	if !cli.DryRun {
		saveStateMap(stateMap, cli.StatePath, now)
	}
}
