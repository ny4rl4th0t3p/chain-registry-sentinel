package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"chain-registry-sentinel/internal/registry"
)

type evmResponse struct {
	Result string `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type EVMProbe struct {
	Chain    registry.Chain
	Endpoint registry.Endpoint
	ChainID  int64 // 0 if fetch failed or unparseable
	FetchErr error
	NetErr   bool
}

const evmChainIDPayload = `{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`

func ProbeEVMEndpoint(ctx context.Context, client *http.Client, chain registry.Chain, ep registry.Endpoint) EVMProbe {
	probe := EVMProbe{Chain: chain, Endpoint: ep}

	url := strings.TrimRight(ep.Address, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(evmChainIDPayload))
	if err != nil {
		probe.FetchErr = err
		return probe
	}
	req.Header.Set("Content-Type", "application/json")

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

	var evmResp evmResponse
	if err := json.NewDecoder(resp.Body).Decode(&evmResp); err != nil {
		probe.FetchErr = fmt.Errorf("decode: %w", err)
		return probe
	}
	if evmResp.Error != nil {
		probe.FetchErr = fmt.Errorf("json-rpc error: %s", evmResp.Error.Message)
		return probe
	}

	hex := strings.TrimPrefix(evmResp.Result, "0x")
	id, err := strconv.ParseInt(hex, 16, 64)
	if err != nil {
		probe.FetchErr = fmt.Errorf("parse chain id %q: %w", evmResp.Result, err)
		return probe
	}
	probe.ChainID = id
	return probe
}

type EVMLiveness struct{}

func NewEVMLiveness() *EVMLiveness  { return &EVMLiveness{} }
func (c *EVMLiveness) Name() string { return "evm_liveness" }

func (c *EVMLiveness) Evaluate(probe EVMProbe) Result {
	r := Result{Chain: probe.Chain.Name, ChainID: probe.Chain.ChainID, Check: c.Name(), Endpoint: probe.Endpoint.Address}
	if probe.FetchErr != nil {
		r.ConnFailed = probe.NetErr
		r.Evidence = probe.FetchErr.Error()
		return r
	}
	r.Passed = true
	return r
}

type EVMChainID struct{}

func NewEVMChainID() *EVMChainID   { return &EVMChainID{} }
func (c *EVMChainID) Name() string { return "evm_chain_id" }

func (c *EVMChainID) Evaluate(probe EVMProbe) Result {
	r := Result{Chain: probe.Chain.Name, ChainID: probe.Chain.ChainID, Check: c.Name(), Endpoint: probe.Endpoint.Address}
	if probe.FetchErr != nil {
		r.Skipped = true
		return r
	}
	// chain_id is only a decimal EVM chain ID for eip155 chains per the schema.
	if probe.Chain.ChainType != "eip155" {
		r.Skipped = true
		return r
	}
	expected, err := strconv.ParseInt(probe.Chain.ChainID, 10, 64)
	if err != nil {
		r.Skipped = true
		return r
	}
	if probe.ChainID == expected {
		r.Passed = true
	} else {
		r.Evidence = fmt.Sprintf("got=%d want=%d", probe.ChainID, expected)
	}
	return r
}
