package checks

import "chain-registry-sentinel/internal/registry"

type Result struct {
	Chain      string // chain_name, used for grouping
	ChainID    string // chain_id, used for display
	Check      string
	Endpoint   string
	Passed     bool
	Skipped    bool
	ConnFailed bool // true when the endpoint was unreachable (network/DNS/TLS/timeout)
	Evidence   string
}

type EndpointProbe struct {
	Chain    registry.Chain
	Endpoint registry.Endpoint
	Status   *rpcStatus
	FetchErr error
	NetErr   bool // true when FetchErr came from a transport failure, not an HTTP-level error
}

type Check interface {
	Name() string
	Evaluate(probe EndpointProbe) Result
}
