package checks

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"chain-registry-sentinel/internal/registry"
)

type restNodeInfo struct {
	DefaultNodeInfo struct {
		Network string `json:"network"`
	} `json:"default_node_info"`
}

type RESTProbe struct {
	Chain    registry.Chain
	Endpoint registry.Endpoint
	NodeInfo *restNodeInfo
	FetchErr error
	NetErr   bool
}

func ProbeRESTEndpoint(ctx context.Context, client *http.Client, chain registry.Chain, ep registry.Endpoint) RESTProbe {
	probe := RESTProbe{Chain: chain, Endpoint: ep}
	url := strings.TrimRight(ep.Address, "/") + "/cosmos/base/tendermint/v1beta1/node_info"
	var info restNodeInfo
	fetchErr, netErr := httpGetJSON(ctx, client, url, &info)
	if fetchErr != nil {
		probe.FetchErr = fetchErr
		probe.NetErr = netErr
		return probe
	}
	probe.NodeInfo = &info
	return probe
}

type RESTLiveness struct{}

func NewRESTLiveness() *RESTLiveness { return &RESTLiveness{} }
func (*RESTLiveness) Name() string   { return "rest_liveness" }

func (c *RESTLiveness) Evaluate(probe RESTProbe) Result {
	r := Result{Chain: probe.Chain.Name, ChainID: probe.Chain.ChainID, Check: c.Name(), Endpoint: probe.Endpoint.Address}
	if probe.FetchErr != nil {
		r.ConnFailed = probe.NetErr
		r.Evidence = probe.FetchErr.Error()
		return r
	}
	r.Passed = true
	return r
}

type RESTChainID struct{}

func NewRESTChainID() *RESTChainID { return &RESTChainID{} }
func (*RESTChainID) Name() string  { return "rest_chain_id" }

func (c *RESTChainID) Evaluate(probe RESTProbe) Result {
	r := Result{Chain: probe.Chain.Name, ChainID: probe.Chain.ChainID, Check: c.Name(), Endpoint: probe.Endpoint.Address}
	if probe.FetchErr != nil {
		r.Skipped = true
		return r
	}
	got := probe.NodeInfo.DefaultNodeInfo.Network
	if got == probe.Chain.ChainID {
		r.Passed = true
	} else {
		r.Evidence = fmt.Sprintf("got=%s want=%s", got, probe.Chain.ChainID)
	}
	return r
}
