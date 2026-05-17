package main

import (
	"context"
	"fmt"
	"os"
	"sort"
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
	Concurrency int              `help:"Max simultaneous endpoint probes" env:"INPUT_CONCURRENCY" default:"250"`
	Version     kong.VersionFlag `name:"version" help:"Print version and exit"`
}

type chainStats struct {
	endpoints   int
	live        int
	unreachable int // ERR: transport failure (DNS, timeout, connection refused)
	wrongResp   int // FAIL: endpoint responded but non-200 or bad data
	chainIDFail int
}

func (s *chainStats) dead() int { return s.unreachable + s.wrongResp }

type job struct {
	chain    registry.Chain
	endpoint registry.Endpoint
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

	var missingChains []string
	if len(filter) > 0 {
		loaded := make(map[string]bool, len(chains))
		for _, ch := range chains {
			loaded[ch.Name] = true
		}
		for _, name := range filter {
			if !loaded[name] {
				missingChains = append(missingChains, name)
			}
		}
	}

	var jobs []job
	for _, ch := range chains {
		for _, ep := range ch.RPCs {
			jobs = append(jobs, job{chain: ch, endpoint: ep})
		}
	}

	client := checks.NewHTTPClient(cli.Timeout)
	checkers := []checks.Check{
		checks.NewRPCLiveness(),
		checks.NewRPCChainID(),
	}

	jobCh := make(chan job, len(jobs))
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)

	resultCh := make(chan checks.Result, len(jobs)*len(checkers))

	var wg sync.WaitGroup
	for range cli.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				ctx, cancel := context.WithTimeout(context.Background(), cli.Timeout)
				probe := checks.ProbeEndpoint(ctx, client, j.chain, j.endpoint)
				cancel()
				for _, checker := range checkers {
					r := checker.Evaluate(probe)
					if !r.Skipped {
						resultCh <- r
					}
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	perChain := map[string]*chainStats{}

	for r := range resultCh {
		key := r.Chain + "/" + r.ChainID
		s := perChain[key]
		if s == nil {
			s = &chainStats{}
			perChain[key] = s
		}

		if r.Check == "rpc_liveness" {
			s.endpoints++
			switch {
			case r.Passed:
				s.live++
			case r.ConnFailed:
				s.unreachable++
			default:
				s.wrongResp++
			}
		} else if r.Check == "rpc_chain_id" && !r.Passed {
			s.chainIDFail++
		}

		label := r.Chain + "/" + r.ChainID
		switch {
		case r.Passed:
			fmt.Printf("PASS  %-35s  %-14s  %s\n", label, r.Check, r.Endpoint)
		case r.ConnFailed:
			fmt.Printf("ERR   %-35s  %-14s  %s  %s\n", label, r.Check, r.Endpoint, r.Evidence)
		default:
			fmt.Printf("FAIL  %-35s  %-14s  %s  %s\n", label, r.Check, r.Endpoint, r.Evidence)
		}
	}

	keys := make([]string, 0, len(perChain))
	for k := range perChain {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var totals chainStats
	for _, s := range perChain {
		totals.endpoints += s.endpoints
		totals.live += s.live
		totals.unreachable += s.unreachable
		totals.wrongResp += s.wrongResp
		totals.chainIDFail += s.chainIDFail
	}

	fmt.Printf("\n%-35s  %s\n", "chain/chain_id", "endpoints   live   dead   chain_id_mismatch")
	fmt.Printf("%s\n", strings.Repeat("─", 85))
	for _, k := range keys {
		s := perChain[k]
		fmt.Printf("%-35s  %-11d %-7d %-7d %d\n", k, s.endpoints, s.live, s.dead(), s.chainIDFail)
	}
	fmt.Printf("%s\n", strings.Repeat("─", 85))
	fmt.Printf("%-35s  %-11d %-7d %-7d %d\n", "TOTAL", totals.endpoints, totals.live, totals.dead(), totals.chainIDFail)

	fmt.Printf("\n%d endpoints: %d live, %d dead (%d unreachable, %d wrong response), %d chain ID mismatches across %d chains\n",
		totals.endpoints, totals.live, totals.dead(), totals.unreachable, totals.wrongResp, totals.chainIDFail, len(chains))

	for _, name := range missingChains {
		fmt.Printf("warning: %q not found in registry (no chain.json — may be EVM-only or unlisted)\n", name)
	}

	if totals.dead() > 0 || totals.chainIDFail > 0 {
		os.Exit(1)
	}
}
