package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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
	Verbose     bool             `short:"v" help:"Enable debug logging to stderr" env:"INPUT_VERBOSE"`
	Version     kong.VersionFlag `name:"version" help:"Print version and exit"`
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

func collectResults(resultCh <-chan checks.Result, verbose bool) (perChain map[string]*chainStats, keys []string) {
	perChain = map[string]*chainStats{}

	for r := range resultCh {
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
	return perChain, keys
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

	client := checks.NewHTTPClient(cli.Timeout)
	resultCh := runWorkers(buildJobs(chains), client, cli.Timeout, cli.Concurrency)
	perChain, keys := collectResults(resultCh, cli.Verbose)
	totals := printSummary(perChain, keys)

	for _, name := range missingChains {
		slog.Warn("chain not found in registry", "chain", name, "hint", "no chain.json — may be EVM-only or unlisted")
	}

	if totals.allDead() > 0 || totals.chainIDFail > 0 {
		os.Exit(1)
	}
}
