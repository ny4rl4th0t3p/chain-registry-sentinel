package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"chain-registry-sentinel/internal/github"
	"chain-registry-sentinel/internal/registry"
	"chain-registry-sentinel/internal/state"
)

// Endpoint-removal PR flow: collect flagged endpoints from state, re-probe them once
// (preflight), and open one PR per chain proposing to delete the ones still dead. Also home of
// openPRFlows, the dispatcher that orders the three PR flows and keeps them off dying chains.

// openPRFlows dispatches the three PR flows in order. A chain that looks dead this run —
// matured status candidate or not — is excluded from the per-endpoint flows: an endpoint or
// hash PR on a dying chain is noise, and one opened during the maturing window would sit next
// to the status-flip PR that later supersedes it.
func openPRFlows(
	cli CLI,
	chains []registry.Chain,
	jobs []job,
	client *http.Client,
	stateMap map[string]state.ChainState,
	flagged int,
	deathCandidates []string,
	looksDead map[string]bool,
	deathFacts map[string]*chainDeathFacts,
	domainLive map[string]int,
	now time.Time,
) {
	maybeOpenStatusPRs(cli, deathCandidates, deathFacts, domainLive, stateMap, now)
	if flagged > 0 {
		fmt.Printf("%d endpoint(s) flagged for action\n", flagged)
		maybeOpenPRs(cli, chains, jobs, client, stateMap, now, looksDead)
	}
	maybeOpenHashPRs(cli, chains, stateMap, now, looksDead)
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

// preflight re-probes only the flagged (check, address) pairs and returns which passed per
// chain, keyed by state.EndpointKey. Pair-keyed on purpose: 28 registry addresses are
// declared under multiple api categories (Pocket-style gateways serve RPC and REST on one
// URL), and an address-keyed rescue would let an alive RPC forever shield its dead REST twin
// from removal.
func preflight(
	allJobs []job,
	client *http.Client,
	timeout time.Duration,
	concurrency int,
	ua string,
	flagged map[string][]github.FlaggedEndpoint,
) map[string]map[string]bool {
	flaggedKeys := make(map[string]map[string]struct{})
	for chainName, endpoints := range flagged {
		keys := make(map[string]struct{}, len(endpoints))
		for _, ep := range endpoints {
			keys[state.EndpointKey(ep.Check, ep.Address)] = struct{}{}
		}
		flaggedKeys[chainName] = keys
	}
	var filtered []job
	for i := range allJobs {
		j := allJobs[i]
		if keys, ok := flaggedKeys[j.chain.Name]; ok {
			jobKey := state.EndpointKey(j.endpointType.livenessCheckName(), j.endpoint.Address)
			if _, inSet := keys[jobKey]; inSet {
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
			passed[r.Chain][state.EndpointKey(r.Check, r.Endpoint)] = true
		}
	}
	return passed
}

// applyPreflightResults resets the failure streak for any (check, address) pair that passed
// preflight — the same pair the streak is keyed by, so a pass on one protocol cannot reset a
// sibling protocol's streak on the same address.
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
			if !passedMap[state.EndpointKey(ep.Check, ep.Address)] {
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
				if !pm[state.EndpointKey(ep.Check, ep.Address)] {
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
