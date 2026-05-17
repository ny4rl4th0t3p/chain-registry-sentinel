package checks_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"google.golang.org/protobuf/encoding/protowire"

	"chain-registry-sentinel/internal/checks"
	"chain-registry-sentinel/internal/registry"
)

// testRawCodec mirrors rawCodec in grpc.go so the test gRPC server uses the
// same raw-bytes codec as the client. Registered as "proto" to match the
// Content-Type the client sends.
type testRawCodec struct{}

func (testRawCodec) Name() string { return "proto" }
func (testRawCodec) Marshal(v any) ([]byte, error) {
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	return nil, fmt.Errorf("testRawCodec: expected []byte, got %T", v)
}
func (testRawCodec) Unmarshal(data []byte, v any) error {
	if b, ok := v.(*[]byte); ok {
		*b = append((*b)[:0], data...)
		return nil
	}
	return fmt.Errorf("testRawCodec: expected *[]byte, got %T", v)
}

func init() {
	// Replace the default proto codec for the duration of this test binary.
	// Safe here because all gRPC in checks_test uses raw bytes.
	encoding.RegisterCodec(testRawCodec{})
}

// buildGetNodeInfoResponse encodes a minimal GetNodeInfoResponse with
// DefaultNodeInfo.network set to network.
//
//	GetNodeInfoResponse.default_node_info = field 1
//	DefaultNodeInfo.network              = field 4
func buildGetNodeInfoResponse(network string) []byte {
	nodeInfo := protowire.AppendTag(nil, 4, protowire.BytesType)
	nodeInfo = protowire.AppendString(nodeInfo, network)
	resp := protowire.AppendTag(nil, 1, protowire.BytesType)
	resp = protowire.AppendBytes(resp, nodeInfo)
	return resp
}

// grpcNodeInfoIface is the handler type for the test service descriptor.
type grpcNodeInfoIface interface {
	handleGetNodeInfo(ctx context.Context, req []byte) ([]byte, error)
}

type testGRPCServer struct{ network string }

func (s *testGRPCServer) handleGetNodeInfo(_ context.Context, _ []byte) ([]byte, error) {
	return buildGetNodeInfoResponse(s.network), nil
}

var testServiceDesc = grpc.ServiceDesc{
	ServiceName: "cosmos.base.tendermint.v1beta1.Service",
	HandlerType: (*grpcNodeInfoIface)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "GetNodeInfo",
		Handler: func(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			var req []byte
			if err := dec(&req); err != nil {
				return nil, err
			}
			return srv.(grpcNodeInfoIface).handleGetNodeInfo(ctx, req)
		},
	}},
	Streams: []grpc.StreamDesc{},
}

func startTestGRPCServer(t *testing.T, network string) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	srv.RegisterService(&testServiceDesc, &testGRPCServer{network: network})
	go srv.Serve(lis)
	t.Cleanup(srv.GracefulStop)
	return lis.Addr().String()
}

func probeGRPC(t *testing.T, addr, chainID string) checks.GRPCProbe {
	t.Helper()
	chain := registry.Chain{
		Name:          "testchain",
		ChainID:       chainID,
		GRPCEndpoints: []registry.Endpoint{{Address: addr, Provider: "test"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return checks.ProbeGRPCEndpoint(ctx, chain, chain.GRPCEndpoints[0])
}

func probeDeadGRPC(t *testing.T, chainID string) checks.GRPCProbe {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()
	chain := registry.Chain{Name: "testchain", ChainID: chainID, GRPCEndpoints: []registry.Endpoint{{Address: addr}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return checks.ProbeGRPCEndpoint(ctx, chain, chain.GRPCEndpoints[0])
}

func TestGRPCLiveness_Pass(t *testing.T) {
	addr := startTestGRPCServer(t, "testchain-1")
	probe := probeGRPC(t, addr, "testchain-1")
	r := checks.NewGRPCLiveness().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass, got evidence: %s", r.Evidence)
	}
}

func TestGRPCLiveness_ConnectionRefused(t *testing.T) {
	probe := probeDeadGRPC(t, "testchain-1")
	r := checks.NewGRPCLiveness().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for connection refused")
	}
	if !r.ConnFailed {
		t.Error("want ConnFailed=true for unreachable endpoint")
	}
	if r.Evidence == "" {
		t.Error("want non-empty evidence")
	}
}

func TestGRPCChainID_Match(t *testing.T) {
	addr := startTestGRPCServer(t, "testchain-1")
	probe := probeGRPC(t, addr, "testchain-1")
	r := checks.NewGRPCChainID().Evaluate(probe)
	if !r.Passed {
		t.Errorf("want pass, got evidence: %s", r.Evidence)
	}
}

func TestGRPCChainID_Mismatch(t *testing.T) {
	addr := startTestGRPCServer(t, "wrongchain-99")
	probe := probeGRPC(t, addr, "testchain-1")
	r := checks.NewGRPCChainID().Evaluate(probe)
	if r.Passed {
		t.Error("want fail for chain ID mismatch")
	}
	if r.Evidence != "got=wrongchain-99 want=testchain-1" {
		t.Errorf("unexpected evidence: %s", r.Evidence)
	}
}

func TestGRPCChainID_SkippedWhenFetchFailed(t *testing.T) {
	probe := probeDeadGRPC(t, "testchain-1")
	r := checks.NewGRPCChainID().Evaluate(probe)
	if !r.Skipped {
		t.Error("want skipped when endpoint unreachable")
	}
	if r.Passed {
		t.Error("skipped result should not be passed")
	}
}

func TestProbeGRPCEndpoint_SingleFetch(t *testing.T) {
	calls := 0
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	counting := &countingGRPCServer{network: "testchain-1", calls: &calls}
	srv := grpc.NewServer()
	srv.RegisterService(&testServiceDesc, counting)
	go srv.Serve(lis)
	t.Cleanup(srv.GracefulStop)

	probe := probeGRPC(t, lis.Addr().String(), "testchain-1")
	checks.NewGRPCLiveness().Evaluate(probe)
	checks.NewGRPCChainID().Evaluate(probe)

	if calls != 1 {
		t.Errorf("want exactly 1 gRPC call, got %d", calls)
	}
}

type countingGRPCServer struct {
	network string
	calls   *int
}

func (s *countingGRPCServer) handleGetNodeInfo(_ context.Context, _ []byte) ([]byte, error) {
	*s.calls++
	return buildGetNodeInfoResponse(s.network), nil
}
