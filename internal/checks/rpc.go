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

type rpcStatusResponse struct {
	Result struct {
		NodeInfo struct {
			Network string `json:"network"`
		} `json:"node_info"`
	} `json:"result"`
}

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// fetchStatus performs GET {address}/status and returns the parsed response.
// It normalises the address by trimming trailing slashes.
func fetchStatus(ctx context.Context, client *http.Client, address string) (*rpcStatusResponse, int, error) {
	url := strings.TrimRight(address, "/") + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var status rpcStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode response: %w", err)
	}
	return &status, resp.StatusCode, nil
}

// RPCLiveness checks that each RPC endpoint responds to /status with HTTP 200.
type RPCLiveness struct {
	client *http.Client
}

func NewRPCLiveness(timeout time.Duration) *RPCLiveness {
	return &RPCLiveness{client: newHTTPClient(timeout)}
}

func (c *RPCLiveness) Name() string { return "rpc_liveness" }

func (c *RPCLiveness) Run(ctx context.Context, chain registry.Chain) []Result {
	results := make([]Result, 0, len(chain.RPCs))
	for _, ep := range chain.RPCs {
		_, _, err := fetchStatus(ctx, c.client, ep.Address)
		r := Result{
			Chain:    chain.Name,
			Check:    c.Name(),
			Endpoint: ep.Address,
			Passed:   err == nil,
		}
		if err != nil {
			r.Evidence = err.Error()
		}
		results = append(results, r)
	}
	return results
}

// RPCChainID checks that the chain ID reported by /status matches chain.json.
type RPCChainID struct {
	client *http.Client
}

func NewRPCChainID(timeout time.Duration) *RPCChainID {
	return &RPCChainID{client: newHTTPClient(timeout)}
}

func (c *RPCChainID) Name() string { return "rpc_chain_id" }

func (c *RPCChainID) Run(ctx context.Context, chain registry.Chain) []Result {
	results := make([]Result, 0, len(chain.RPCs))
	for _, ep := range chain.RPCs {
		status, _, err := fetchStatus(ctx, c.client, ep.Address)
		r := Result{
			Chain:    chain.Name,
			Check:    c.Name(),
			Endpoint: ep.Address,
		}
		if err != nil {
			r.Passed = false
			r.Evidence = fmt.Sprintf("fetch failed: %s", err)
			results = append(results, r)
			continue
		}

		got := status.Result.NodeInfo.Network
		if got == chain.ChainID {
			r.Passed = true
		} else {
			r.Passed = false
			r.Evidence = fmt.Sprintf("got=%s want=%s", got, chain.ChainID)
		}
		results = append(results, r)
	}
	return results
}
