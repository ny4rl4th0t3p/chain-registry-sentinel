package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/registry"
)

// Version is injected at build time via -ldflags.
var Version = "dev"

type CLI struct {
	Registry    string           `help:"Path to local chain-registry clone" env:"INPUT_REGISTRY" required:""`
	Chains      string           `help:"Comma-separated chain names, or 'all'" env:"INPUT_CHAINS" default:"all"`
	Timeout     time.Duration    `help:"HTTP timeout per request" env:"INPUT_TIMEOUT" default:"30s"`
	Concurrency int              `help:"Max parallel chains" env:"INPUT_CONCURRENCY" default:"10"`
	Version     kong.VersionFlag `name:"version" help:"Print version and exit"`
}

func main() {
	var cli CLI
	kong.Parse(&cli,
		kong.Name("sentinel"),
		kong.Description("Verify chain-registry entries against on-chain reality."),
		kong.Vars{"version": Version},
	)

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
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
	if len(chains) == 0 {
		fmt.Fprintln(os.Stderr, "error: no chains found")
		os.Exit(1)
	}

	checkers := []checks.Check{
		checks.NewRPCLiveness(cli.Timeout),
		checks.NewRPCChainID(cli.Timeout),
	}

	type chainResults struct {
		chain   string
		results []checks.Result
	}

	sem := make(chan struct{}, cli.Concurrency)
	out := make(chan chainResults, len(chains))
	var wg sync.WaitGroup

	for _, chain := range chains {
		wg.Add(1)
		go func(ch registry.Chain) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), cli.Timeout*time.Duration(len(ch.RPCs)+1))
			defer cancel()

			var results []checks.Result
			for _, checker := range checkers {
				results = append(results, checker.Run(ctx, ch)...)
			}
			out <- chainResults{chain: ch.Name, results: results}
		}(chain)
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	var passed, failed, errored int
	for cr := range out {
		for _, r := range cr.results {
			switch {
			case r.Passed:
				passed++
				fmt.Printf("PASS  %-20s  %-14s  %s\n", r.Chain, r.Check, r.Endpoint)
			case r.Evidence != "" && strings.HasPrefix(r.Evidence, "fetch failed:"):
				errored++
				fmt.Printf("ERR   %-20s  %-14s  %s  %s\n", r.Chain, r.Check, r.Endpoint, r.Evidence)
			default:
				failed++
				fmt.Printf("FAIL  %-20s  %-14s  %s  %s\n", r.Chain, r.Check, r.Endpoint, r.Evidence)
			}
		}
	}

	fmt.Printf("\n%d passed, %d failed, %d errors across %d chains\n", passed, failed, errored, len(chains))

	if failed > 0 || errored > 0 {
		os.Exit(1)
	}
}
