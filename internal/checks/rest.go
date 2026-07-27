package checks

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"chain-registry-sentinel/internal/registry"
)

type restNodeInfo struct {
	DefaultNodeInfo struct {
		Network string `json:"network"`
		Other   struct {
			TxIndex string `json:"tx_index"`
		} `json:"other"`
	} `json:"default_node_info"`
}

type RESTProbe struct {
	Chain       registry.Chain
	Endpoint    registry.Endpoint
	NodeInfo    *restNodeInfo
	FetchErr    error
	NetErr      bool
	RateLimited bool
	StatusCode  int
	Body        string
	Latency     time.Duration
}

//nolint:dupl // ProbeEndpoint is structurally identical but operates on different types and URL paths
func ProbeRESTEndpoint(ctx context.Context, client *http.Client, chain registry.Chain, ep registry.Endpoint) RESTProbe {
	probe := RESTProbe{Chain: chain, Endpoint: ep}
	url := strings.TrimRight(ep.Address, "/") + "/cosmos/base/tendermint/v1beta1/node_info"
	var info restNodeInfo
	res := httpGetJSON(ctx, client, url, &info)
	probe.StatusCode = res.StatusCode
	probe.Body = res.Body
	probe.Latency = res.Latency
	if res.Err != nil {
		probe.FetchErr = res.Err
		probe.NetErr = res.NetErr
		probe.RateLimited = res.StatusCode == http.StatusTooManyRequests
		return probe
	}
	probe.NodeInfo = &info
	return probe
}

type RESTLiveness struct{}

func NewRESTLiveness() *RESTLiveness { return &RESTLiveness{} }
func (*RESTLiveness) Name() string   { return "rest_liveness" }

func (p RESTProbe) outcome() livenessOutcome {
	return livenessOutcome{
		FetchErr: p.FetchErr, NetErr: p.NetErr, RateLimited: p.RateLimited,
		StatusCode: p.StatusCode, Body: p.Body, Latency: p.Latency,
	}
}

func (c *RESTLiveness) Evaluate(probe RESTProbe) Result {
	r := newResult(probe.Chain, probe.Endpoint, c.Name())
	applyLiveness(&r, probe.outcome())
	if r.Passed {
		// The REST node_info response carries tx_index but no sync_info, so CatchingUp stays
		// false here; /status via the RPC check is the only source for it.
		r.TxIndex = probe.NodeInfo.DefaultNodeInfo.Other.TxIndex
	}
	return r
}

type RESTChainID struct{}

func NewRESTChainID() *RESTChainID { return &RESTChainID{} }
func (*RESTChainID) Name() string  { return "rest_chain_id" }

func (c *RESTChainID) Evaluate(probe RESTProbe) Result {
	r := newResult(probe.Chain, probe.Endpoint, c.Name())
	if probe.FetchErr != nil {
		r.Skipped = true
		return r
	}
	got := probe.NodeInfo.DefaultNodeInfo.Network
	if got == probe.Chain.ChainID {
		r.Passed = true
	} else {
		r.FailureClass = ClassChainIDMismatch
		r.Evidence = fmt.Sprintf("got=%s want=%s", got, probe.Chain.ChainID)
	}
	return r
}
