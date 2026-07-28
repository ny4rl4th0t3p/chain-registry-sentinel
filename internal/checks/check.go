package checks

import (
	"time"

	"chain-registry-sentinel/internal/registry"
)

type Result struct {
	Chain      string // chain_name, used for grouping
	ChainID    string // chain_id, used for display
	ChainType  string // "cosmos" or "eip155"; decides which checks are meaningful for a chain
	Check      string
	Endpoint   string
	Provider   string // registry-declared operator; "" when the entry omits it
	Passed     bool
	Skipped    bool
	ConnFailed bool // true when the endpoint was unreachable (network/DNS/TLS/timeout)
	Evidence   string

	// FailureClass is derived from the live error value, before it is flattened into
	// Evidence — see Classify, which relies on errors.As and cannot work from a string.
	FailureClass FailureClass
	HTTPStatus   int           // 0 when no HTTP response arrived
	Latency      time.Duration // time to complete the probe; 0 when not measured

	// Node quality, read from /status on a successful probe. Neither field ever affects
	// Passed or a failure streak: a node that is catching up is a real node, and proposing
	// its removal because it is behind would be wrong. Reporting only.
	CatchingUp bool   // sync_info.catching_up; false when the field is absent
	TxIndex    string // node_info.other.tx_index: "on", "off", or "" when absent
}

// newResult seeds a Result from the registry entry under test. Centralized so Provider —
// which every check must carry through for provider-level analysis to work — cannot be
// wired up in one Evaluate and forgotten in another.
func newResult(chain registry.Chain, ep registry.Endpoint, check string) Result {
	return Result{
		Chain:     chain.Name,
		ChainID:   chain.ChainID,
		ChainType: chain.ChainType,
		Check:     check,
		Endpoint:  ep.Address,
		Provider:  ep.Provider,
	}
}

// livenessOutcome is the part of a probe that every liveness check interprets identically:
// whether the endpoint answered, and what went wrong if it did not. Each probe type exposes its
// own via an outcome() method, since the probe structs are otherwise unrelated.
type livenessOutcome struct {
	FetchErr    error
	NetErr      bool
	RateLimited bool
	StatusCode  int
	Body        string
	Latency     time.Duration
}

// applyLiveness writes the shared liveness verdict into r.
//
// Extracted because five checks had byte-identical bodies. With one copy, a change to how
// throttling or classification is recorded cannot land in four checks and be forgotten in the
// fifth — which is exactly the bug class that let Provider go unpropagated for a whole release.
func applyLiveness(r *Result, o livenessOutcome) {
	r.HTTPStatus = o.StatusCode
	r.Latency = o.Latency
	switch {
	case o.RateLimited:
		// Skipped is what keeps throttling out of failure streaks, so a rate-limited endpoint
		// can never drive a removal PR. The class is still recorded so the report can count
		// endpoints that could not be measured rather than dropping them silently.
		r.Skipped = true
		r.FailureClass = ClassHTTP429
		r.Evidence = errText(o.FetchErr)
	case o.FetchErr != nil:
		r.ConnFailed = o.NetErr
		r.FailureClass = Classify(o.FetchErr, o.StatusCode, o.Body)
		r.Evidence = errText(o.FetchErr)
		if o.Body != "" {
			r.Evidence += ": " + o.Body
		}
	default:
		r.Passed = true
	}
}

// errText tolerates a nil error. RateLimited is only ever set alongside a non-nil FetchErr, but
// the previous per-check code would have panicked if that invariant ever broke.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type EndpointProbe struct {
	Chain       registry.Chain
	Endpoint    registry.Endpoint
	Status      *rpcStatus
	FetchErr    error
	NetErr      bool // true when FetchErr came from a transport failure, not an HTTP-level error
	RateLimited bool // true when the server responded with HTTP 429
	StatusCode  int  // 0 when no HTTP response arrived
	Body        string
	Latency     time.Duration
}

func (p EndpointProbe) outcome() livenessOutcome {
	return livenessOutcome{
		FetchErr: p.FetchErr, NetErr: p.NetErr, RateLimited: p.RateLimited,
		StatusCode: p.StatusCode, Body: p.Body, Latency: p.Latency,
	}
}

type Check interface {
	Name() string
	Evaluate(probe EndpointProbe) Result
}
