package checks

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
)

// wrapped builds the error shape net/http actually produces: *url.Error around *net.OpError
// around the real cause. Classify must unwrap through both.
func wrapped(cause error) error {
	return &url.Error{
		Op:  "Get",
		URL: "https://rpc.example.com/status",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: cause},
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
		body string
		want FailureClass
	}{
		{name: "nil error", err: nil, want: ClassNone},

		// DNS — structured, through two layers of wrapping.
		{
			name: "dns not found",
			err:  wrapped(&net.DNSError{Err: "no such host", Name: "rpc.example.com", IsNotFound: true}),
			want: ClassDNSNXDomain,
		},
		{
			name: "dns other failure",
			err:  wrapped(&net.DNSError{Err: "server misbehaving", Name: "rpc.example.com"}),
			want: ClassDNSFailure,
		},

		// The regression that matters most: grpc-go reports NXDOMAIN as a Unavailable status with
		// "produced zero addresses" and no *net.DNSError anywhere. Matching the gRPC code before
		// the inner cause mislabeled 410 of 459 such failures as generic gRPC unavailability in
		// the 2026-07-25 run. Do not reorder Classify so that this returns grpc_unavailable.
		{
			name: "grpc wraps dns nxdomain",
			err:  errors.New(`rpc error: code = Unavailable desc = name resolver error: produced zero addresses`),
			want: ClassDNSNXDomain,
		},
		{
			name: "grpc unavailable with no recognized inner cause",
			err:  errors.New(`rpc error: code = Unavailable desc = unavailable`),
			want: ClassGRPCUnavailable,
		},
		{
			name: "grpc permission denied",
			err:  errors.New(`rpc error: code = PermissionDenied desc = 403 (Forbidden)`),
			want: ClassGRPCPermission,
		},
		// The same trap as the DNS case above, found by the oracle test rather than by
		// inspection: gRPC reports a closed port as text inside its status message, with no
		// errno to unwrap, so errors.Is cannot see it. 84 failures in the 2026-07-25 run were
		// reported as generic grpc_unavailable before the text fallback was added.
		{
			name: "grpc wraps connection refused",
			err: errors.New(`rpc error: code = Unavailable desc = connection error: desc = ` +
				`"transport: Error while dialing: dial tcp 10.0.0.1:9090: connect: connection refused"`),
			want: ClassConnRefused,
		},
		{
			name: "plain text connection refused with no wrapped errno",
			err:  errors.New(`Get "https://rpc.example.com/status": dial tcp 10.0.0.1:443: connect: connection refused`),
			want: ClassConnRefused,
		},

		// TCP.
		{
			name: "connection refused",
			err:  wrapped(os.NewSyscallError("connect", syscall.ECONNREFUSED)),
			want: ClassConnRefused,
		},
		{
			name: "connection reset",
			err:  wrapped(os.NewSyscallError("read", syscall.ECONNRESET)),
			want: ClassConnReset,
		},
		{
			name: "host unreachable",
			err:  wrapped(os.NewSyscallError("connect", syscall.EHOSTUNREACH)),
			want: ClassNetUnreachable,
		},
		{
			// ENETUNREACH is the prober's own routing table (classically a v6 dial from a
			// v4-only host), not the endpoint — it must never count as structural.
			name: "network unreachable is the vantage, not the endpoint",
			err:  wrapped(os.NewSyscallError("connect", syscall.ENETUNREACH)),
			want: ClassVantageNoRoute,
		},

		// Timeouts. context.DeadlineExceeded satisfies net.Error with Timeout() == true.
		{name: "timeout", err: wrapped(context.DeadlineExceeded), want: ClassTimeout},
		{
			name: "tls handshake timeout is more specific than a plain timeout",
			err:  errors.New(`net/http: TLS handshake timeout`),
			want: ClassTLSHandshakeSlow,
		},

		// TLS / x509 — structured where Go provides a type.
		{
			name: "certificate expired",
			err:  wrapped(x509.CertificateInvalidError{Cert: &x509.Certificate{}, Reason: x509.Expired}),
			want: ClassTLSExpired,
		},
		{
			name: "certificate hostname mismatch",
			err: wrapped(x509.HostnameError{
				Certificate: &x509.Certificate{DNSNames: []string{"other.example.com"}},
				Host:        "rpc.example.com",
			}),
			want: ClassTLSHostname,
		},
		{
			name: "unknown authority",
			err:  wrapped(x509.UnknownAuthorityError{Cert: &x509.Certificate{}}),
			want: ClassTLSUntrusted,
		},
		{
			// DNS resolves and the server answers, but the vhost is gone. Structural, so it must
			// not collapse into tls_other.
			name: "tls unrecognized name",
			err:  errors.New(`remote error: tls: unrecognized name`),
			want: ClassTLSUnrecognized,
		},
		{
			name: "generic tls handshake failure",
			err:  errors.New(`remote error: tls: handshake failure`),
			want: ClassTLSOther,
		},

		// HTTP status.
		{name: "404", err: errors.New("HTTP 404"), code: 404, want: ClassHTTP404},
		{name: "403", err: errors.New("HTTP 403"), code: 403, want: ClassHTTP403},
		{name: "429", err: errors.New("HTTP 429"), code: 429, want: ClassHTTP429},
		{name: "500 plain server error", err: errors.New("HTTP 500"), code: 500, want: ClassHTTP5xxServer},
		{name: "502 gateway", err: errors.New("HTTP 502"), code: 502, want: ClassGatewayNoBackend},
		{name: "503 gateway", err: errors.New("HTTP 503"), code: 503, want: ClassGatewayNoBackend},
		// Cloudflare origin codes: the edge answered, so we were not blocked, and it reports the
		// backend as gone.
		{name: "521 cloudflare origin", err: errors.New("HTTP 521"), code: 521, want: ClassCFOriginDown},
		{name: "526 cloudflare origin", err: errors.New("HTTP 526"), code: 526, want: ClassCFOriginDown},
		{name: "530 cloudflare origin", err: errors.New("HTTP 530"), code: 530, want: ClassCFOriginDown},

		// Provider retirement notices outrank the status they arrive with. PublicNode answers
		// 403 "unsupported platform" for chains it has dropped, which is a retired endpoint
		// rather than a block — and is indistinguishable from a WAF by status and headers alone.
		{
			name: "unsupported platform body beats http 403",
			err:  errors.New("HTTP 403"),
			code: 403,
			body: "unsupported platform\n",
			want: ClassNotServedByProvider,
		},
		{
			name: "ordinary 403 body stays http_403",
			err:  errors.New("HTTP 403"),
			code: 403,
			body: "Forbidden",
			want: ClassHTTP403,
		},

		// Payload and registry-data defects.
		{name: "bad json", err: errors.New("decode: invalid character '<'"), want: ClassBadJSON},
		{name: "chain id mismatch", err: errors.New("got=foo-1 want=bar-1"), want: ClassChainIDMismatch},
		{
			name: "address missing scheme",
			err:  errors.New(`Get "rpc.nolus.network/status": unsupported protocol scheme ""`),
			want: ClassMalformedAddress,
		},
		{
			name: "address with a space in the host",
			err:  errors.New(`parse "https://evmos-grpc.example.info ": invalid character " " in host name`),
			want: ClassMalformedAddress,
		},
		{
			name: "server closed without responding",
			err:  errors.New(`Get "https://rpc.example.com/status": EOF`),
			want: ClassEOFNoResponse,
		},

		{name: "unrecognized", err: errors.New("something entirely new"), want: ClassOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err, tt.code, tt.body); got != tt.want {
				t.Errorf("Classify() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFailureClassIsStructural(t *testing.T) {
	structural := []FailureClass{
		ClassDNSNXDomain, ClassConnRefused, ClassHTTP404, ClassTLSExpired,
		ClassTLSUnrecognized, ClassCFOriginDown, ClassGatewayNoBackend,
		ClassNotServedByProvider, ClassMalformedAddress,
	}
	for _, c := range structural {
		if !c.IsStructural() {
			t.Errorf("%q should be structural", c)
		}
	}

	// These can all be explained by the prober being throttled, blocked or impatient, so they
	// must stay out of the conservative figure.
	ambiguous := []FailureClass{
		ClassTimeout, ClassHTTP403, ClassHTTP429, ClassConnReset,
		ClassGRPCPermission, ClassTLSOther, ClassNone,
		// Proven vantage-side by the 2026-07-28 VM run: a flaky local resolver and a missing
		// IPv6 route produced these en masse for endpoints that were perfectly healthy.
		ClassDNSFailure, ClassVantageNoRoute,
	}
	for _, c := range ambiguous {
		if c.IsStructural() {
			t.Errorf("%q should not be structural", c)
		}
	}
}
