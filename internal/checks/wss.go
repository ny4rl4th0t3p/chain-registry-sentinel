package checks

import (
	"context"
	"encoding/json"
	"fmt"
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
}

const wssStatusRequest = `{"jsonrpc":"2.0","method":"status","params":{},"id":1}`

// ProbeWSSEndpoint dials the WebSocket endpoint and sends a Tendermint status
// request to retrieve the chain ID. A dial failure with no HTTP response is a
// network error; a server that rejects the upgrade or doesn't speak the
// Tendermint protocol is a wrong-response failure.
func ProbeWSSEndpoint(ctx context.Context, chain registry.Chain, ep registry.Endpoint) WSSProbe {
	probe := WSSProbe{Chain: chain, Endpoint: ep}

	var timeout time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}

	dialer := websocket.Dialer{HandshakeTimeout: timeout}
	conn, resp, err := dialer.DialContext(ctx, ep.Address, nil)
	if err != nil {
		probe.FetchErr = err
		probe.NetErr = resp == nil // no HTTP response at all = transport failure
		return probe
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetReadDeadline(deadline)
		conn.SetWriteDeadline(deadline)
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

func NewWSSLiveness() *WSSLiveness  { return &WSSLiveness{} }
func (c *WSSLiveness) Name() string { return "wss_liveness" }

func (c *WSSLiveness) Evaluate(probe WSSProbe) Result {
	r := Result{Chain: probe.Chain.Name, ChainID: probe.Chain.ChainID, Check: c.Name(), Endpoint: probe.Endpoint.Address}
	if probe.FetchErr != nil {
		r.ConnFailed = probe.NetErr
		r.Evidence = probe.FetchErr.Error()
		return r
	}
	r.Passed = true
	return r
}

type WSSChainID struct{}

func NewWSSChainID() *WSSChainID   { return &WSSChainID{} }
func (c *WSSChainID) Name() string { return "wss_chain_id" }

func (c *WSSChainID) Evaluate(probe WSSProbe) Result {
	r := Result{Chain: probe.Chain.Name, ChainID: probe.Chain.ChainID, Check: c.Name(), Endpoint: probe.Endpoint.Address}
	if probe.FetchErr != nil {
		r.Skipped = true
		return r
	}
	if probe.Network == probe.Chain.ChainID {
		r.Passed = true
	} else {
		r.Evidence = fmt.Sprintf("got=%s want=%s", probe.Network, probe.Chain.ChainID)
	}
	return r
}
