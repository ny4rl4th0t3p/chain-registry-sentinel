package checks

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"chain-registry-sentinel/internal/registry"
)

// rpcNodeInfo is the node_info payload, which carries both the chain ID (network) and whether
// the node indexes transactions.
type rpcNodeInfo struct {
	Network string `json:"network"`
	Other   struct {
		TxIndex string `json:"tx_index"`
	} `json:"other"`
}

type rpcStatus struct {
	Result struct {
		NodeInfo rpcNodeInfo `json:"node_info"`
		SyncInfo struct {
			CatchingUp bool `json:"catching_up"`
		} `json:"sync_info"`
	} `json:"result"`
	// Some nodes (e.g. Sei via Pocket Network) omit the result wrapper and
	// return node_info directly at the top level.
	DirectNodeInfo rpcNodeInfo `json:"node_info"`
}

// nodeInfo returns whichever of the two shapes carried a chain ID.
func (s *rpcStatus) nodeInfo() rpcNodeInfo {
	if s.Result.NodeInfo.Network != "" {
		return s.Result.NodeInfo
	}
	return s.DirectNodeInfo
}

// ProbeEndpoint fetches /status once for an endpoint. Both checks share this result.
//
//nolint:dupl // same structure as ProbeRESTEndpoint but different types and URL path
func ProbeEndpoint(ctx context.Context, client *http.Client, chain registry.Chain, ep registry.Endpoint) EndpointProbe {
	probe := EndpointProbe{Chain: chain, Endpoint: ep}
	url := strings.TrimRight(ep.Address, "/") + "/status"
	var status rpcStatus
	res := httpGetJSON(ctx, client, url, &status)
	probe.StatusCode = res.StatusCode
	probe.Body = res.Body
	probe.Latency = res.Latency
	if res.Err != nil {
		probe.FetchErr = res.Err
		probe.NetErr = res.NetErr
		probe.RateLimited = res.StatusCode == http.StatusTooManyRequests
		return probe
	}
	probe.Status = &status
	return probe
}

// RPCLiveness passes when the endpoint responds to /status with HTTP 200.
type RPCLiveness struct{}

func NewRPCLiveness() *RPCLiveness { return &RPCLiveness{} }
func (*RPCLiveness) Name() string  { return "rpc_liveness" }

func (c *RPCLiveness) Evaluate(probe EndpointProbe) Result {
	r := newResult(probe.Chain, probe.Endpoint, c.Name())
	applyLiveness(&r, probe.outcome())
	if r.Passed {
		// Only meaningful once the node answered, and guarded on r.Passed so probe.Status
		// cannot be nil here.
		r.CatchingUp = probe.Status.Result.SyncInfo.CatchingUp
		r.TxIndex = probe.Status.nodeInfo().Other.TxIndex
	}
	return r
}

// RPCChainID passes when the chain ID in /status matches chain.json.
// Skipped when the endpoint was unreachable — liveness already reported that.
type RPCChainID struct{}

func NewRPCChainID() *RPCChainID { return &RPCChainID{} }
func (*RPCChainID) Name() string { return "rpc_chain_id" }

func (c *RPCChainID) Evaluate(probe EndpointProbe) Result {
	r := newResult(probe.Chain, probe.Endpoint, c.Name())
	if probe.FetchErr != nil {
		r.Skipped = true
		return r
	}
	got := probe.Status.nodeInfo().Network
	if got == probe.Chain.ChainID {
		r.Passed = true
	} else {
		r.FailureClass = ClassChainIDMismatch
		r.Evidence = fmt.Sprintf("got=%s want=%s", got, probe.Chain.ChainID)
	}
	return r
}
