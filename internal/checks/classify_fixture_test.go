package checks

import (
	"errors"
	"testing"
)

// Every string below was observed in a real full-registry run (2026-07-25, 4161 endpoints,
// 2904 failures), extracted one or two per class from run-gentle.log. This is the durable
// counterpart to TestClassifyAgainstRunLog: that test needs a gitignored 609 KB log and skips
// everywhere else, whereas these cases run in CI forever.
//
// The value is provenance. Three real bugs in Classify were found by looking at data rather than
// by reasoning about it, and each is pinned below:
//
//   - gRPC reports NXDOMAIN as "produced zero addresses" with no *net.DNSError to unwrap.
//   - gRPC reports a closed port as text inside its status message, with no errno to unwrap.
//   - A provider's retirement notice ("unsupported platform") arrives as an HTTP body on the
//     RPC/REST paths but as a gRPC status message on the gRPC path.
//
// All three share one shape: grpc-go flattens the real cause into a string, so structured
// unwrapping silently misses it. When adding a class, add an observed sample here too.
//
// httpStatus is what the live probe would have supplied from the response; 0 means no HTTP
// response arrived. Bodies are empty because the run predates body capture — the two
// not_served_by_provider cases below are therefore both from the gRPC path.
func TestClassifyObservedEvidence(t *testing.T) {
	tests := []struct {
		evidence   string
		httpStatus int
		want       FailureClass
	}{
		// DNS — 41.5% of all failures, and the single most undeniable class.
		{`dial tcp: lookup jackal-rpc.agoranodes.com on 127.0.0.53:53: no such host`, 0, ClassDNSNXDomain},
		{`rpc error: code = Unavailable desc = name resolver error: produced zero addresses`, 0, ClassDNSNXDomain},

		// TCP. The gRPC-wrapped form has no errno to unwrap; 84 failures were mislabeled
		// grpc_unavailable before the text fallback existed.
		{`Get "http://37.60.240.43:46657/status": dial tcp 37.60.240.43:46657: connect: connection refused`, 0, ClassConnRefused},
		{`Get "https://rpc-orai.blockval.io/status": dial tcp 46.4.23.251:443: connect: connection refused`, 0, ClassConnRefused},
		{`Get "https://rpc.planq.indonode.net/status": read tcp 10.69.10.65:49708->167.235.102.45:443: read: connection reset by peer`, 0, ClassConnReset},
		{`Get "https://rpc.cosmos.dragonstake.io/status": dial tcp 141.95.202.13:443: connect: no route to host`, 0, ClassNetUnreachable},

		// Timeouts.
		{`Get "https://rpc.planq.network/status": context deadline exceeded`, 0, ClassTimeout},
		{`rpc error: code = DeadlineExceeded desc = context deadline exceeded`, 0, ClassTimeout},
		{`Get "https://fetch-rpc.cosmosrescue.com/status": net/http: TLS handshake timeout`, 0, ClassTLSHandshakeSlow},

		// TLS. Note tls_expired's message says "has expired or is not yet valid" — matching on
		// "certificate has expired" alone is enough and must not be narrowed further.
		{`Get "https://rpc-acre.synergynodes.com/status": tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2026-07-25T09:11:16Z is after 2026-07-18T10:32:09Z`, 0, ClassTLSExpired},
		{`Get "https://m-aura.rpc.utsa.tech/status": tls: failed to verify certificate: x509: certificate is valid for grafana.utsa.tech, not m-aura.rpc.utsa.tech`, 0, ClassTLSHostname},
		{`Get "https://elys.rpc.quasarstaking.ai:443/status": tls: failed to verify certificate: x509: certificate signed by unknown authority`, 0, ClassTLSUntrusted},
		// DNS resolves and the server answers, but the vhost is gone: structural, not a generic
		// handshake problem.
		{`Get "https://saga.rpc.kjnodes.com/status": remote error: tls: unrecognized name`, 0, ClassTLSUnrecognized},
		{`Get "https://rpc.sunrise.nodestake.org/status": remote error: tls: internal error`, 0, ClassTLSOther},

		// HTTP status.
		{`HTTP 401`, 401, ClassHTTP401},
		{`HTTP 403`, 403, ClassHTTP403},
		{`HTTP 404`, 404, ClassHTTP404},
		{`HTTP 400`, 400, ClassHTTPOther},
		{`HTTP 500`, 500, ClassHTTP5xxServer},
		{`HTTP 502`, 502, ClassGatewayNoBackend},
		// Cloudflare answered, so the prober was not blocked, and reports the origin as gone.
		{`HTTP 521`, 521, ClassCFOriginDown},

		// Provider retirement. Arrives as a gRPC status here rather than an HTTP body, which is
		// why providerMessage searches the error text as well.
		{`rpc error: code = Internal desc = unsupported platform`, 0, ClassNotServedByProvider},

		// gRPC conditions that are genuinely gRPC-level, reached only once the inner-cause
		// checks decline.
		{`rpc error: code = Unavailable desc = unavailable`, 0, ClassGRPCUnavailable},
		{`rpc error: code = Unavailable desc = connection error: desc = "error reading server preface: EOF"`, 0, ClassGRPCUnavailable},
		{`rpc error: code = PermissionDenied desc = unexpected HTTP status code received from server: 403 (Forbidden); transport: received unexpected content-type "text/html"`, 0, ClassGRPCPermission},
		{`rpc error: code = Unauthenticated desc = unexpected HTTP status code received from server: 401 (Unauthorized); transport: received unexpected content-type "text/html"`, 0, ClassGRPCUnauth},
		{`rpc error: code = Unimplemented desc = `, 0, ClassGRPCUnimplemented},
		{`rpc error: code = Unknown desc = unexpected HTTP status code received from server: 0 (); malformed header: missing HTTP content-type`, 0, ClassGRPCOther},

		// Registry data defects: no client could ever use these addresses. Note the single slash
		// in "https:/pylons-rpc" — a typo in the registry, not a dead node.
		{`Get "rpc.nolus.network/status": unsupported protocol scheme ""`, 0, ClassMalformedAddress},
		{`Get "https:/pylons-rpc.noders.services/status": http: no Host in request URL`, 0, ClassMalformedAddress},

		// Payload.
		{`decode: EOF`, 0, ClassBadJSON},
		{`Get "https://jblfg.org/status": EOF`, 0, ClassEOFNoResponse},
		{`websocket: bad handshake`, 0, ClassWSSHandshake},
		{`got= want=cosmoshub-4`, 0, ClassChainIDMismatch},
		{`got=axone-1 want=secret-4`, 0, ClassChainIDMismatch},
	}

	for _, tt := range tests {
		t.Run(string(tt.want)+"/"+truncate(tt.evidence), func(t *testing.T) {
			if got := Classify(errors.New(tt.evidence), tt.httpStatus, ""); got != tt.want {
				t.Errorf("Classify(%q, %d) = %q, want %q", tt.evidence, tt.httpStatus, got, tt.want)
			}
		})
	}
}

func truncate(s string) string {
	const maxLen = 40
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
