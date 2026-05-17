package checks

import (
	"context"
	"encoding/json"
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

	var info restNodeInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		probe.FetchErr = fmt.Errorf("decode: %w", err)
		return probe
	}

	probe.NodeInfo = &info
	return probe
}

type RESTLiveness struct{}

func NewRESTLiveness() *RESTLiveness { return &RESTLiveness{} }
func (c *RESTLiveness) Name() string { return "rest_liveness" }

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

func NewRESTChainID() *RESTChainID  { return &RESTChainID{} }
func (c *RESTChainID) Name() string { return "rest_chain_id" }

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
