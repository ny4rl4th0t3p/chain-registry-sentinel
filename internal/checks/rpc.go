package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"chain-registry-sentinel/internal/registry"
)

type rpcStatus struct {
	Result struct {
		NodeInfo struct {
			Network string `json:"network"`
		} `json:"node_info"`
	} `json:"result"`
	// Some nodes (e.g. Sei via Pocket Network) omit the result wrapper and
	// return node_info directly at the top level.
	DirectNodeInfo struct {
		Network string `json:"network"`
	} `json:"node_info"`
}

func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// ProbeEndpoint fetches /status once for an endpoint. Both checks share this result.
func ProbeEndpoint(ctx context.Context, client *http.Client, chain registry.Chain, ep registry.Endpoint) EndpointProbe {
	probe := EndpointProbe{Chain: chain, Endpoint: ep}

	url := strings.TrimRight(ep.Address, "/") + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		probe.FetchErr = err
		return probe
	}

	resp, err := client.Do(req)
	if err != nil {
		probe.FetchErr = err
		probe.NetErr = true
		return probe
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		probe.FetchErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		return probe
	}

	var status rpcStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		probe.FetchErr = fmt.Errorf("decode: %w", err)
		return probe
	}

	probe.Status = &status
	return probe
}

// RPCLiveness passes when the endpoint responds to /status with HTTP 200.
type RPCLiveness struct{}

func NewRPCLiveness() *RPCLiveness  { return &RPCLiveness{} }
func (c *RPCLiveness) Name() string { return "rpc_liveness" }

func (c *RPCLiveness) Evaluate(probe EndpointProbe) Result {
	r := Result{Chain: probe.Chain.Name, ChainID: probe.Chain.ChainID, Check: c.Name(), Endpoint: probe.Endpoint.Address}
	if probe.FetchErr != nil {
		r.ConnFailed = probe.NetErr
		r.Evidence = probe.FetchErr.Error()
		return r
	}
	r.Passed = true
	return r
}

// RPCChainID passes when the chain ID in /status matches chain.json.
// Skipped when the endpoint was unreachable — liveness already reported that.
type RPCChainID struct{}

func NewRPCChainID() *RPCChainID   { return &RPCChainID{} }
func (c *RPCChainID) Name() string { return "rpc_chain_id" }

func (c *RPCChainID) Evaluate(probe EndpointProbe) Result {
	r := Result{Chain: probe.Chain.Name, ChainID: probe.Chain.ChainID, Check: c.Name(), Endpoint: probe.Endpoint.Address}
	if probe.FetchErr != nil {
		r.Skipped = true
		return r
	}
	got := probe.Status.Result.NodeInfo.Network
	if got == "" {
		got = probe.Status.DirectNodeInfo.Network
	}
	if got == probe.Chain.ChainID {
		r.Passed = true
	} else {
		r.Evidence = fmt.Sprintf("got=%s want=%s", got, probe.Chain.ChainID)
	}
	return r
}
