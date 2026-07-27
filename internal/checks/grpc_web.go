package checks

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"chain-registry-sentinel/internal/registry"
)

const grpcWebGetNodeInfoPath = "/cosmos.base.tendermint.v1beta1.Service/GetNodeInfo"

type GRPCWebProbe struct {
	Chain       registry.Chain
	Endpoint    registry.Endpoint
	Network     string
	FetchErr    error
	NetErr      bool
	RateLimited bool
	StatusCode  int
	Body        string
	Latency     time.Duration
}

// ProbeGRPCWebEndpoint calls GetNodeInfo via the gRPC-web wire protocol.
// It sends a 5-byte length-prefixed frame with an empty body (GetNodeInfo
// takes no parameters) and decodes the response using the same protowire
// path as the native gRPC probe.
func ProbeGRPCWebEndpoint(
	ctx context.Context, client *http.Client, chain registry.Chain, ep registry.Endpoint,
) (probe GRPCWebProbe) {
	probe = GRPCWebProbe{Chain: chain, Endpoint: ep}
	// Named return plus defer so latency is recorded on every exit path, including errors.
	start := time.Now()
	defer func() { probe.Latency = time.Since(start) }()

	// gRPC-web data frame: 1 flag byte (0x00 = data) + 4-byte big-endian length + body.
	// Empty request body → length = 0.
	frame := []byte{0x00, 0x00, 0x00, 0x00, 0x00}
	url := strings.TrimRight(ep.Address, "/") + grpcWebGetNodeInfoPath

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(frame))
	if err != nil {
		probe.FetchErr = err
		return probe
	}
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	req.Header.Set("Accept", "application/grpc-web+proto")
	req.Header.Set("X-Grpc-Web", "1")

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

	// Read the first gRPC-web frame header (5 bytes: 1 flag + 4 length).
	var header [5]byte
	if _, err := io.ReadFull(resp.Body, header[:]); err != nil {
		probe.FetchErr = fmt.Errorf("read frame header: %w", err)
		return probe
	}
	if header[0]&0x80 != 0 {
		probe.FetchErr = fmt.Errorf("expected data frame, got trailers")
		return probe
	}
	length := binary.BigEndian.Uint32(header[1:5])
	body := make([]byte, length)
	if _, err := io.ReadFull(resp.Body, body); err != nil {
		probe.FetchErr = fmt.Errorf("read frame body: %w", err)
		return probe
	}

	network, err := decodeGetNodeInfoNetwork(body)
	if err != nil {
		probe.FetchErr = fmt.Errorf("decode response: %w", err)
		return probe
	}
	probe.Network = network
	return probe
}

type GRPCWebLiveness struct{}

func NewGRPCWebLiveness() *GRPCWebLiveness { return &GRPCWebLiveness{} }
func (*GRPCWebLiveness) Name() string      { return "grpc_web_liveness" }

func (p GRPCWebProbe) outcome() livenessOutcome {
	return livenessOutcome{
		FetchErr: p.FetchErr, NetErr: p.NetErr, RateLimited: p.RateLimited,
		StatusCode: p.StatusCode, Body: p.Body, Latency: p.Latency,
	}
}

func (c *GRPCWebLiveness) Evaluate(probe GRPCWebProbe) Result {
	r := newResult(probe.Chain, probe.Endpoint, c.Name())
	applyLiveness(&r, probe.outcome())
	return r
}

type GRPCWebChainID struct{}

func NewGRPCWebChainID() *GRPCWebChainID { return &GRPCWebChainID{} }
func (*GRPCWebChainID) Name() string     { return "grpc_web_chain_id" }

func (c *GRPCWebChainID) Evaluate(probe GRPCWebProbe) Result {
	r := newResult(probe.Chain, probe.Endpoint, c.Name())
	if probe.FetchErr != nil {
		r.Skipped = true
		return r
	}
	if probe.Network == probe.Chain.ChainID {
		r.Passed = true
	} else {
		r.FailureClass = ClassChainIDMismatch
		r.Evidence = fmt.Sprintf("got=%s want=%s", probe.Network, probe.Chain.ChainID)
	}
	return r
}
