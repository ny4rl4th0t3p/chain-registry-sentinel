package checks

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"

	"chain-registry-sentinel/internal/registry"
)

// rawCodec passes []byte through grpc's codec layer unchanged.
// Name "proto" ensures Content-Type: application/grpc+proto, which real nodes expect.
type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }

func (rawCodec) Marshal(v any) ([]byte, error) {
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	return nil, fmt.Errorf("rawCodec: expected []byte, got %T", v)
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	if b, ok := v.(*[]byte); ok {
		*b = append((*b)[:0], data...)
		return nil
	}
	return fmt.Errorf("rawCodec: expected *[]byte, got %T", v)
}

type GRPCProbe struct {
	Chain       registry.Chain
	Endpoint    registry.Endpoint
	Network     string
	FetchErr    error
	NetErr      bool
	RateLimited bool
	Latency     time.Duration
	// CanaryBlocked: the gateway refused GetNodeInfo by name but answered the fallback query —
	// a live, usable service whose blocklist happens to include our canary (observed at
	// PublicNode). Liveness passes; the chain-ID check must skip, since no method that
	// answers can tell us the network name.
	CanaryBlocked bool
}

const (
	getNodeInfoMethod = "/cosmos.base.tendermint.v1beta1.Service/GetNodeInfo"
	// totalSupplyMethod is the liveness fallback when a gateway blocklists GetNodeInfo by name:
	// stable since Stargate, present on every Cosmos SDK chain, and an empty request is valid.
	// It cannot yield the network name, so a fallback-confirmed endpoint skips the chain-ID check.
	totalSupplyMethod = "/cosmos.bank.v1beta1.Query/TotalSupply"
)

// ProbeGRPCEndpoint checks a Cosmos SDK gRPC endpoint, trying each transport mode
// parseGRPCTarget allows, in order. Both correction mechanisms here were found by manually
// grpcurl-ing endpoints the probe had counted dead (see .claude/manual-verification.md):
// TLS on nonstandard ports is real, and at least one gateway blocklists our canary method.
//
// ua, when non-empty, is set via grpc.WithUserAgent; grpc-go appends its own "grpc-go/x.y"
// suffix, which is expected.
func ProbeGRPCEndpoint(ctx context.Context, chain registry.Chain, ep registry.Endpoint, ua string) (probe GRPCProbe) {
	probe = GRPCProbe{Chain: chain, Endpoint: ep}
	// Named return plus defer so latency is recorded on every exit path, including errors.
	start := time.Now()
	defer func() { probe.Latency = time.Since(start) }()

	target, modes, err := parseGRPCTarget(ep.Address)
	if err != nil {
		probe.FetchErr = err
		return probe
	}

	var kept error
	for _, useTLS := range modes {
		network, canaryBlocked, err := invokeGRPC(ctx, target, useTLS, ua)
		if err == nil {
			probe.Network = network
			probe.CanaryBlocked = canaryBlocked
			return probe
		}
		// Keep the first mode's error for classification — except when it is the
		// TLS-at-a-plaintext-server artifact, which describes our dialing, not the endpoint.
		if kept == nil || strings.Contains(kept.Error(), "first record does not look like a TLS handshake") {
			kept = err
		}
		// Only a transport-level failure justifies trying the other mode; an application-level
		// answer means the channel worked and this IS the endpoint's response.
		if st, ok := status.FromError(err); !ok || st.Code() != codes.Unavailable {
			break
		}
	}

	probe.FetchErr = kept
	if st, ok := status.FromError(kept); ok {
		switch st.Code() {
		case codes.Unavailable, codes.DeadlineExceeded:
			probe.NetErr = true
		default:
		}
		probe.RateLimited = strings.Contains(st.Message(), "429 (Too Many Requests)")
	}
	return probe
}

// invokeGRPC dials target in one transport mode and runs the liveness canary, falling back to
// a second query method when a gateway refuses the canary at the application level.
func invokeGRPC(ctx context.Context, target string, useTLS bool, ua string) (network string, canaryBlocked bool, err error) {
	var creds credentials.TransportCredentials
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{})
	} else {
		creds = insecure.NewCredentials()
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})),
	}
	if ua != "" {
		opts = append(opts, grpc.WithUserAgent(ua))
	}
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return "", false, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	var respBytes []byte
	err = conn.Invoke(ctx, getNodeInfoMethod, []byte{}, &respBytes)
	if err == nil {
		network, derr := decodeGetNodeInfoNetwork(respBytes)
		if derr != nil {
			return "", false, fmt.Errorf("decode response: %w", derr)
		}
		return network, false, nil
	}
	if !grpcAppLevelRefusal(err) {
		return "", false, err
	}
	// The gateway spoke well-formed gRPC and refused the canary by name (PublicNode's shape).
	// Confirm with an unrelated query before calling it live — "articulate" alone could also
	// be a non-Cosmos gRPC server answering Unimplemented.
	var supplyBytes []byte
	if err2 := conn.Invoke(ctx, totalSupplyMethod, []byte{}, &supplyBytes); err2 == nil {
		return "", true, nil
	}
	// Both methods refused: keep the canary's error, which is what classification understands.
	return "", false, err
}

// grpcAppLevelRefusal reports whether err is a well-formed gRPC status from a working channel,
// as opposed to a transport failure or an HTTP gateway splatting HTML into the stream. The
// text/html shape (AutoStake) means no gRPC service exists behind the gateway — falling back
// there would waste a call on a dead endpoint.
func grpcAppLevelRefusal(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return false
	default:
	}
	msg := st.Message()
	return !strings.Contains(msg, "unexpected HTTP status code") &&
		!strings.Contains(msg, `content-type "text/html"`)
}

// parseGRPCTarget returns a host:port dial target and the transport modes to attempt, in
// order. Address forms found in chain.json:
//
//	https://grpc.cosmos.network:443 → TLS only (operator stated the scheme)
//	http://grpc.cosmos.network:9090 → plaintext only (operator stated the scheme)
//	grpc.cosmos.network:443         → TLS only (port convention; zero counterexamples in 1,139 probes)
//	grpc.cosmos.network             → TLS only (bare host, assume :443)
//	grpc.cosmos.network:9090        → TLS first, then plaintext
//
// The last rule is the 2026-07-28 correction: nonstandard ports carry no scheme information —
// operators terminate TLS on :9090/:2083 as readily as they serve plaintext there, and a
// plaintext dial at a TLS server dies as a silent timeout (the server waits for a ClientHello
// that never comes), which counted at least one live endpoint dead. TLS is attempted first
// because the failure costs are asymmetric: TLS at a plaintext server fails in one round trip,
// plaintext at a TLS server burns the whole timeout.
func parseGRPCTarget(address string) (target string, modes []bool, err error) {
	tlsOnly, plainOnly, tlsFirst := []bool{true}, []bool{false}, []bool{true, false}

	if strings.Contains(address, "://") {
		u, err := url.Parse(address)
		if err != nil {
			return "", nil, fmt.Errorf("parse %q: %w", address, err)
		}
		host, port := u.Hostname(), u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "9090"
			}
		}
		if u.Scheme == "https" {
			return net.JoinHostPort(host, port), tlsOnly, nil
		}
		return net.JoinHostPort(host, port), plainOnly, nil
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		// Bare hostname with no port — assume port 443 (TLS convention for gRPC in chain-registry).
		return net.JoinHostPort(address, "443"), tlsOnly, nil
	}
	if port == "443" {
		return address, tlsOnly, nil
	}
	return address, tlsFirst, nil
}

// decodeGetNodeInfoNetwork extracts default_node_info.network from a raw
// GetNodeInfoResponse protobuf payload using field numbers:
//
//	GetNodeInfoResponse.default_node_info = field 1 (embedded message)
//	DefaultNodeInfo.network              = field 4 (string)
func decodeGetNodeInfoNetwork(data []byte) (string, error) {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return "", protowire.ParseError(n)
		}
		data = data[n:]
		if num == 1 && typ == protowire.BytesType {
			embedded, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return "", protowire.ParseError(n)
			}
			return decodeDefaultNodeInfoNetwork(embedded)
		}
		n = protowire.ConsumeFieldValue(num, typ, data)
		if n < 0 {
			return "", protowire.ParseError(n)
		}
		data = data[n:]
	}
	return "", fmt.Errorf("default_node_info not found in GetNodeInfoResponse")
}

func decodeDefaultNodeInfoNetwork(data []byte) (string, error) {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return "", protowire.ParseError(n)
		}
		data = data[n:]
		if num == 4 && typ == protowire.BytesType {
			s, n := protowire.ConsumeString(data)
			if n < 0 {
				return "", protowire.ParseError(n)
			}
			return s, nil
		}
		n = protowire.ConsumeFieldValue(num, typ, data)
		if n < 0 {
			return "", protowire.ParseError(n)
		}
		data = data[n:]
	}
	return "", fmt.Errorf("network field not found in DefaultNodeInfo")
}

type GRPCLiveness struct{}

func NewGRPCLiveness() *GRPCLiveness { return &GRPCLiveness{} }
func (*GRPCLiveness) Name() string   { return "grpc_liveness" }

// outcome leaves StatusCode and Body zero: gRPC folds any HTTP status and body into the status
// message, which Classify reads from the error text once the transport-level checks decline.
func (p GRPCProbe) outcome() livenessOutcome {
	return livenessOutcome{
		FetchErr: p.FetchErr, NetErr: p.NetErr, RateLimited: p.RateLimited, Latency: p.Latency,
	}
}

func (c *GRPCLiveness) Evaluate(probe GRPCProbe) Result {
	r := newResult(probe.Chain, probe.Endpoint, c.Name())
	applyLiveness(&r, probe.outcome())
	r.MethodRestricted = probe.CanaryBlocked
	return r
}

type GRPCChainID struct{}

func NewGRPCChainID() *GRPCChainID { return &GRPCChainID{} }
func (*GRPCChainID) Name() string  { return "grpc_chain_id" }

func (c *GRPCChainID) Evaluate(probe GRPCProbe) Result {
	r := newResult(probe.Chain, probe.Endpoint, c.Name())
	// CanaryBlocked: liveness was confirmed via the fallback query, which cannot yield the
	// network name — comparing "" against the chain ID would report a false mismatch.
	if probe.FetchErr != nil || probe.CanaryBlocked {
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
