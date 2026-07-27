package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"chain-registry-sentinel/internal/registry"
)

type WSSProbe struct {
	Chain    registry.Chain
	Endpoint registry.Endpoint
	Network  string
	FetchErr error
	NetErr   bool
	// A rejected upgrade still carries an HTTP response, so status, body and rate limiting are
	// all observable here just as they are for the plain HTTP checks.
	RateLimited bool
	StatusCode  int
	Body        string
	Latency     time.Duration
}

const wssStatusRequest = `{"jsonrpc":"2.0","method":"status","params":{},"id":1}`

// ProbeWSSEndpoint dials the WebSocket endpoint and sends a Tendermint status
// request to retrieve the chain ID. A dial failure with no HTTP response is a
// network error; a server that rejects the upgrade or doesn't speak the
// Tendermint protocol is a wrong-response failure.
func ProbeWSSEndpoint(ctx context.Context, chain registry.Chain, ep registry.Endpoint) (probe WSSProbe) {
	probe = WSSProbe{Chain: chain, Endpoint: ep}
	// Named return plus defer so latency is recorded on every exit path, including errors.
	start := time.Now()
	defer func() { probe.Latency = time.Since(start) }()

	var timeout time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}

	dialer := websocket.Dialer{HandshakeTimeout: timeout}
	conn, resp, err := dialer.DialContext(ctx, ep.Address, nil)
	if resp != nil {
		defer resp.Body.Close()
		probe.StatusCode = resp.StatusCode
	}
	if err != nil {
		probe.FetchErr = err
		probe.NetErr = resp == nil // no HTTP response at all = transport failure
		if resp != nil {
			probe.Body = readBodyPrefix(resp.Body)
			probe.RateLimited = resp.StatusCode == http.StatusTooManyRequests
		}
		return probe
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			probe.FetchErr = fmt.Errorf("set deadline: %w", err)
			return probe
		}
		if err := conn.SetWriteDeadline(deadline); err != nil {
			probe.FetchErr = fmt.Errorf("set deadline: %w", err)
			return probe
		}
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(wssStatusRequest)); err != nil {
		probe.FetchErr = fmt.Errorf("write: %w", err)
		return probe
	}

	_, msg, err := conn.ReadMessage()
	if err != nil {
		probe.FetchErr = fmt.Errorf("read: %w", err)
		return probe
	}

	network, err := parseWSSStatusNetwork(msg)
	if err != nil {
		probe.FetchErr = fmt.Errorf("parse status: %w", err)
		return probe
	}
	probe.Network = network
	return probe
}

func parseWSSStatusNetwork(data []byte) (string, error) {
	var resp struct {
		Result struct {
			NodeInfo struct {
				Network string `json:"network"`
			} `json:"node_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	if resp.Result.NodeInfo.Network == "" {
		return "", fmt.Errorf("network field empty or missing")
	}
	return resp.Result.NodeInfo.Network, nil
}

type WSSLiveness struct{}

func NewWSSLiveness() *WSSLiveness { return &WSSLiveness{} }
func (*WSSLiveness) Name() string  { return "wss_liveness" }

func (p WSSProbe) outcome() livenessOutcome {
	return livenessOutcome{
		FetchErr: p.FetchErr, NetErr: p.NetErr, RateLimited: p.RateLimited,
		StatusCode: p.StatusCode, Body: p.Body, Latency: p.Latency,
	}
}

func (c *WSSLiveness) Evaluate(probe WSSProbe) Result {
	r := newResult(probe.Chain, probe.Endpoint, c.Name())
	applyLiveness(&r, probe.outcome())
	return r
}

type WSSChainID struct{}

func NewWSSChainID() *WSSChainID { return &WSSChainID{} }
func (*WSSChainID) Name() string { return "wss_chain_id" }

func (c *WSSChainID) Evaluate(probe WSSProbe) Result {
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
