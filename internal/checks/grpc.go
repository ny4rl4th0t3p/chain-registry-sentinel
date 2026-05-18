package checks

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"

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
}

const getNodeInfoMethod = "/cosmos.base.tendermint.v1beta1.Service/GetNodeInfo"

// ProbeGRPCEndpoint calls GetNodeInfo on a Cosmos SDK gRPC endpoint.
// The method has been stable since Cosmos SDK 0.40 (Stargate, Jan 2021).
func ProbeGRPCEndpoint(ctx context.Context, chain registry.Chain, ep registry.Endpoint) GRPCProbe {
	probe := GRPCProbe{Chain: chain, Endpoint: ep}

	target, useTLS, err := parseGRPCTarget(ep.Address)
	if err != nil {
		probe.FetchErr = err
		return probe
	}

	var creds credentials.TransportCredentials
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{})
	} else {
		creds = insecure.NewCredentials()
	}

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})),
	)
	if err != nil {
		probe.FetchErr = fmt.Errorf("dial: %w", err)
		return probe
	}
	defer conn.Close()

	var respBytes []byte
	err = conn.Invoke(ctx, getNodeInfoMethod, []byte{}, &respBytes)
	if err != nil {
		probe.FetchErr = err
		if st, ok := status.FromError(err); ok {
			switch st.Code() {
			case codes.Unavailable, codes.DeadlineExceeded:
				probe.NetErr = true
			default:
			}
			probe.RateLimited = strings.Contains(st.Message(), "429 (Too Many Requests)")
		}
		return probe
	}

	network, err := decodeGetNodeInfoNetwork(respBytes)
	if err != nil {
		probe.FetchErr = fmt.Errorf("decode response: %w", err)
		return probe
	}
	probe.Network = network
	return probe
}

// parseGRPCTarget returns a host:port dial target and whether TLS should be used.
// Handles address forms found in chain.json:
//
//	grpc.cosmos.network:9090        → cleartext
//	https://grpc.cosmos.network:443 → TLS
//	grpc.cosmos.network:443         → TLS (by port convention)
func parseGRPCTarget(address string) (target string, useTLS bool, err error) {
	if strings.Contains(address, "://") {
		u, err := url.Parse(address)
		if err != nil {
			return "", false, fmt.Errorf("parse %q: %w", address, err)
		}
		host, port := u.Hostname(), u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "9090"
			}
		}
		return net.JoinHostPort(host, port), u.Scheme == "https", nil
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		// Bare hostname with no port — assume port 443 (TLS convention for gRPC in chain-registry).
		return net.JoinHostPort(address, "443"), true, nil
	}
	return address, port == "443", nil
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

func (c *GRPCLiveness) Evaluate(probe GRPCProbe) Result {
	r := Result{Chain: probe.Chain.Name, ChainID: probe.Chain.ChainID, Check: c.Name(), Endpoint: probe.Endpoint.Address}
	if probe.RateLimited {
		r.Skipped = true
		return r
	}
	if probe.FetchErr != nil {
		r.ConnFailed = probe.NetErr
		r.Evidence = probe.FetchErr.Error()
		return r
	}
	r.Passed = true
	return r
}

type GRPCChainID struct{}

func NewGRPCChainID() *GRPCChainID { return &GRPCChainID{} }
func (*GRPCChainID) Name() string  { return "grpc_chain_id" }

func (c *GRPCChainID) Evaluate(probe GRPCProbe) Result {
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
