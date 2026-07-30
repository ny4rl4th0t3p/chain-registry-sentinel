package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/github"
	"chain-registry-sentinel/internal/registry"
	"chain-registry-sentinel/internal/state"
)

// IBC denom hash-fix PR flow: verify every assetlist's ibc/... base hashes against
// SHA256(path), print the mismatch table, and open one fix PR per chain.

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
