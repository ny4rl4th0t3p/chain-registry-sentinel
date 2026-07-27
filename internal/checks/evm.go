package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// RateLimited exists for parity with the other probes: without it a 429 would count as a
	// hard failure here and could drive a removal PR, which no other check allows.
	RateLimited bool
	StatusCode  int
	Body        string
	Latency     time.Duration
}

const evmChainIDPayload = `{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`

func ProbeEVMEndpoint(
	ctx context.Context, client *http.Client, chain registry.Chain, ep registry.Endpoint,
) (probe EVMProbe) {
	probe = EVMProbe{Chain: chain, Endpoint: ep}
	// Named return plus defer so latency is recorded on every exit path, including errors.
	start := time.Now()
	defer func() { probe.Latency = time.Since(start) }()

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

	probe.StatusCode = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		probe.Body = readBodyPrefix(resp.Body)
		probe.FetchErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		probe.RateLimited = resp.StatusCode == http.StatusTooManyRequests
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

func NewEVMLiveness() *EVMLiveness { return &EVMLiveness{} }
func (*EVMLiveness) Name() string  { return "evm_liveness" }

func (p EVMProbe) outcome() livenessOutcome {
	return livenessOutcome{
		FetchErr: p.FetchErr, NetErr: p.NetErr, RateLimited: p.RateLimited,
		StatusCode: p.StatusCode, Body: p.Body, Latency: p.Latency,
	}
}

func (c *EVMLiveness) Evaluate(probe EVMProbe) Result {
	r := newResult(probe.Chain, probe.Endpoint, c.Name())
	applyLiveness(&r, probe.outcome())
	return r
}

type EVMChainID struct{}

func NewEVMChainID() *EVMChainID { return &EVMChainID{} }
func (*EVMChainID) Name() string { return "evm_chain_id" }

func (c *EVMChainID) Evaluate(probe EVMProbe) Result {
	r := newResult(probe.Chain, probe.Endpoint, c.Name())
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
		r.FailureClass = ClassChainIDMismatch
		r.Evidence = fmt.Sprintf("got=%d want=%d", probe.ChainID, expected)
	}
	return r
}
