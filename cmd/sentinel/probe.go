package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/registry"
	"chain-registry-sentinel/internal/state"
)

// Probing: from registry entries to check results. Jobs enumerate every declared endpoint,
// workers probe them concurrently, collectResults folds the stream into per-chain stats.

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
	for i := range chains {
		ch := chains[i]
		// The registry contains literal duplicate entries — the same address twice in one
		// list (10 across the registry when measured, 2026-08-01). One job per occurrence
		// would double-count the streak: state keys on (check, address), so a duplicated
		// endpoint matures at min-failures/2 runs, which is how a premature PR shipped once.
		// First occurrence wins and keeps its true list position for `order`.
		seen := map[string]struct{}{}
		add := func(eps []registry.Endpoint, t EndpointType) {
			for i, ep := range eps {
				key := t.String() + "|" + ep.Address
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				jobs = append(jobs, job{chain: ch, endpoint: ep, endpointType: t, order: i + 1})
			}
		}
		switch ch.ChainType {
		case "cosmos":
			add(ch.RPCs, TypeRPC)
			add(ch.RESTEndpoints, TypeREST)
			add(ch.GRPCWebEndpoints, TypeGRPCWeb)
			add(ch.GRPCEndpoints, TypeGRPC)
			add(ch.WSSEndpoints, TypeWSS)
			// Cosmos chains declare EVM JSON-RPC lists too (evmos-style chains, 46 of them at
			// last count). Probing them matters beyond coverage: evm_liveness is a core check in
			// chain-death detection, so a chain surviving only through its EVM side must be seen
			// or the abandoned signature can fire on a serving chain.
			add(ch.EVMEndpoints, TypeEVM)
		case "eip155":
			add(ch.EVMEndpoints, TypeEVM)
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
