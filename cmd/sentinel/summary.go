package main

import (
	"fmt"
	"strings"
)

// Console summary: per-chain and total live/dead counts by endpoint type, printed after every
// probing run.

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
