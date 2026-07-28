package checks

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
)

// Cloudflare's origin-error range, which has no net/http constants. 521 web server down,
// 522/524 origin connection or response timed out, 523 origin unreachable, 525/526 origin TLS
// failure, 530 origin DNS error. All mean the edge answered us — so we were not blocked — and
// could not reach the backend, which is why they count as structural: the timeout was
// Cloudflare's from its own network, not ours from an impatient prober.
const (
	cfOriginErrMin   = 521
	cfOriginErrMax   = 526
	cfOriginDNSError = 530
)

// FailureClass is a stable, machine-groupable cause for a failed probe. The set and the
// ordering of the checks in Classify are ported from analysis/classify.awk, which was derived
// from the real error strings of a full-registry run rather than guessed at.
type FailureClass string

const (
	ClassNone FailureClass = ""

	// Structural: the endpoint is provably broken regardless of how the prober behaved.
	ClassDNSNXDomain         FailureClass = "dns_nxdomain"
	ClassConnRefused         FailureClass = "conn_refused"
	ClassNetUnreachable      FailureClass = "net_unreachable"
	ClassTLSExpired          FailureClass = "tls_expired"
	ClassTLSHostname         FailureClass = "tls_hostname"
	ClassTLSUntrusted        FailureClass = "tls_untrusted"
	ClassTLSUnrecognized     FailureClass = "tls_unrecognized_name"
	ClassHTTP404             FailureClass = "http_404"
	ClassCFOriginDown        FailureClass = "cf_origin_down"
	ClassGatewayNoBackend    FailureClass = "gateway_no_backend"
	ClassBadJSON             FailureClass = "bad_json"
	ClassEOFNoResponse       FailureClass = "eof_no_response"
	ClassMalformedAddress    FailureClass = "malformed_registry_address"
	ClassNotServedByProvider FailureClass = "not_served_by_provider"

	// Ambiguous: could be throttling, blocking, an impatient prober — or the prober's own
	// environment, which a VM run (2026-07-28) proved is a real population: a flaky local
	// systemd-resolved produced 55 dns_failure ("server misbehaving"), and a machine with no
	// IPv6 route produced 19 ENETUNREACH dials to v6 addresses. Neither said anything about
	// the endpoints. Only NXDOMAIN is an authoritative answer about the name; a SERVFAIL
	// indicts the resolver, and "network is unreachable" indicts the local routing table.
	// Caveat: "authoritative" assumes a functioning resolver — the same VM was later observed
	// returning *false* NXDOMAINs for names that resolved minutes earlier. The report warns
	// when a run's own dns_failure/vantage_no_route counts suggest its NXDOMAINs are tainted.
	ClassDNSFailure        FailureClass = "dns_failure"
	ClassVantageNoRoute    FailureClass = "vantage_no_route"
	ClassTimeout           FailureClass = "timeout"
	ClassTLSHandshakeSlow  FailureClass = "timeout_tls_handshake"
	ClassConnReset         FailureClass = "conn_reset"
	ClassTLSOther          FailureClass = "tls_other"
	ClassHTTP401           FailureClass = "http_401"
	ClassHTTP403           FailureClass = "http_403"
	ClassHTTP429           FailureClass = "http_429"
	ClassHTTP451           FailureClass = "http_451"
	ClassHTTP5xxServer     FailureClass = "http_5xx_server"
	ClassHTTP3xx           FailureClass = "http_3xx"
	ClassHTTPOther         FailureClass = "http_other"
	ClassGRPCUnavailable   FailureClass = "grpc_unavailable"
	ClassGRPCPermission    FailureClass = "grpc_permission_denied"
	ClassGRPCUnimplemented FailureClass = "grpc_unimplemented"
	ClassGRPCExhausted     FailureClass = "grpc_resource_exhausted"
	ClassGRPCInternal      FailureClass = "grpc_internal"
	ClassGRPCUnauth        FailureClass = "grpc_unauthenticated"
	ClassGRPCOther         FailureClass = "grpc_other"
	ClassBadProtocol       FailureClass = "bad_protocol"
	ClassWSSHandshake      FailureClass = "wss_handshake"
	ClassChainIDMismatch   FailureClass = "chain_id_mismatch"
	ClassOther             FailureClass = "other"
)

// structuralClasses are the classes that cannot be explained by the prober being throttled,
// blocked, or impatient. Reported separately so the conservative figure can be stated
// independently of any argument about probing aggressiveness.
var structuralClasses = map[FailureClass]bool{
	ClassDNSNXDomain:         true,
	ClassConnRefused:         true,
	ClassNetUnreachable:      true,
	ClassTLSExpired:          true,
	ClassTLSHostname:         true,
	ClassTLSUntrusted:        true,
	ClassTLSUnrecognized:     true,
	ClassHTTP404:             true,
	ClassCFOriginDown:        true,
	ClassGatewayNoBackend:    true,
	ClassBadJSON:             true,
	ClassEOFNoResponse:       true,
	ClassMalformedAddress:    true,
	ClassNotServedByProvider: true,
}

// IsStructural reports whether c means the endpoint is broken on its own terms.
func (c FailureClass) IsStructural() bool { return structuralClasses[c] }

// Classify maps a probe failure to a FailureClass.
//
// err must be the live error value, not a stringified copy — the DNS, TLS and syscall checks
// rely on errors.As and stop working once the error has been through .Error(). statusCode is 0
// when no HTTP response arrived. body is a bounded prefix of a non-2xx response body, which is
// sometimes the only place the real cause appears (see providerMessage).
//
// Ordering is significant. Causes are checked from the lowest network layer upwards, because a
// single failure is often reported by several layers at once and the innermost one is the true
// cause. The gRPC checks come last for exactly this reason: grpc-go reports NXDOMAIN as
// "code = Unavailable ... produced zero addresses", so matching on the gRPC code first would
// have mislabeled a third of all DNS failures in the 2026-07-25 run.
func Classify(err error, statusCode int, body string) FailureClass {
	if err == nil {
		return ClassNone
	}
	msg := err.Error()

	// A provider stating that it does not serve this chain outranks the HTTP status it says it
	// with — the endpoint is retired, not blocked.
	if c := providerMessage(body, msg); c != ClassNone {
		return c
	}
	if c := classifyDNS(err, msg); c != ClassNone {
		return c
	}
	if c := classifySyscall(err, msg); c != ClassNone {
		return c
	}
	if c := classifyTLS(err, msg); c != ClassNone {
		return c
	}
	// Timeout after TLS: a handshake timeout is more specific than a generic one, and
	// net.Error.Timeout() is true for both.
	if strings.Contains(msg, "TLS handshake timeout") {
		return ClassTLSHandshakeSlow
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ClassTimeout
	}
	if strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "i/o timeout") {
		return ClassTimeout
	}
	if c := classifyHTTPStatus(statusCode); c != ClassNone {
		return c
	}
	if c := classifyPayload(msg); c != ClassNone {
		return c
	}
	if c := classifyGRPC(msg); c != ClassNone {
		return c
	}
	return ClassOther
}

// providerMessage recognizes response bodies in which an operator states that it no longer
// serves the requested chain. PublicNode answers "unsupported platform" with HTTP 403 for
// chains it has dropped while leaving the DNS records in place, which is indistinguishable from
// a WAF block by status code and headers alone.
//
// Both the response body and the error text are searched, because the same message reaches us
// two ways: as an HTTP 403 body on the RPC and REST paths, and embedded in a gRPC status
// ("code = Internal desc = unsupported platform") where there is no body at all. Checking only
// the body left 30 gRPC endpoints classified as grpc_internal in the 2026-07-25 run.
//
// Deliberately narrow: every provider words this differently, and guessing wrongly would
// reclassify genuine blocks as retirements. Extend only from messages actually observed.
func providerMessage(body, msg string) FailureClass {
	if strings.Contains(strings.ToLower(body+" "+msg), "unsupported platform") {
		return ClassNotServedByProvider
	}
	return ClassNone
}

func classifyDNS(err error, msg string) FailureClass {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return ClassDNSNXDomain
		}
		return ClassDNSFailure
	}
	// grpc-go's resolver does not surface a *net.DNSError; it reports an empty resolution as
	// "produced zero addresses" wrapped in code = Unavailable. Same cause, no structured type.
	if strings.Contains(msg, "produced zero addresses") || strings.Contains(msg, "no such host") {
		return ClassDNSNXDomain
	}
	if strings.Contains(msg, "server misbehaving") || strings.Contains(msg, "Temporary failure in name resolution") {
		return ClassDNSFailure
	}
	return ClassNone
}

func classifySyscall(err error, msg string) FailureClass {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return ClassConnRefused
	case errors.Is(err, syscall.ECONNRESET):
		return ClassConnReset
	// EHOSTUNREACH is typically an ICMP answer about the destination — structural. ENETUNREACH
	// means the prober's own routing table has no route there at all (classically: dialing an
	// IPv6 address from a host without IPv6), which says nothing about the endpoint.
	case errors.Is(err, syscall.EHOSTUNREACH):
		return ClassNetUnreachable
	case errors.Is(err, syscall.ENETUNREACH):
		return ClassVantageNoRoute
	}
	// Text fallbacks, for the same reason classifyDNS needs them: grpc-go embeds the dial
	// failure as a string inside its status message ("transport: Error while dialing: dial tcp
	// ...: connect: connection refused") rather than wrapping the errno, so errors.Is has
	// nothing to find. Without these, every gRPC endpoint whose port is closed is reported as
	// generic grpc_unavailable — 84 such failures in the 2026-07-25 run.
	switch {
	case strings.Contains(msg, "connection refused"):
		return ClassConnRefused
	case strings.Contains(msg, "connection reset by peer"):
		return ClassConnReset
	case strings.Contains(msg, "no route to host"):
		return ClassNetUnreachable
	case strings.Contains(msg, "network is unreachable"):
		return ClassVantageNoRoute
	}
	return ClassNone
}

func classifyTLS(err error, msg string) FailureClass {
	var certErr *tls.CertificateVerificationError
	var invalid x509.CertificateInvalidError
	var hostErr x509.HostnameError
	var authErr x509.UnknownAuthorityError

	if errors.As(err, &hostErr) {
		return ClassTLSHostname
	}
	if errors.As(err, &authErr) {
		return ClassTLSUntrusted
	}
	if errors.As(err, &invalid) {
		if invalid.Reason == x509.Expired {
			return ClassTLSExpired
		}
		return ClassTLSOther
	}
	if errors.As(err, &certErr) {
		return ClassTLSOther
	}
	// Server-side alerts arrive as opaque errors whose text is the only discriminator.
	// "unrecognized name" means the vhost is gone even though DNS still resolves, which is a
	// structural failure rather than a generic handshake problem — hence the separate class.
	switch {
	case strings.Contains(msg, "tls: unrecognized name"):
		return ClassTLSUnrecognized
	case strings.Contains(msg, "certificate has expired"), strings.Contains(msg, "certificate is not yet valid"):
		return ClassTLSExpired
	case strings.Contains(msg, "certificate is valid for"), strings.Contains(msg, "not valid for any names"):
		return ClassTLSHostname
	case strings.Contains(msg, "signed by unknown authority"):
		return ClassTLSUntrusted
	case strings.Contains(msg, "tls: "), strings.Contains(msg, "x509:"):
		return ClassTLSOther
	}
	return ClassNone
}

func classifyHTTPStatus(code int) FailureClass {
	switch {
	case code == 0:
		return ClassNone
	case code == http.StatusUnauthorized:
		return ClassHTTP401
	case code == http.StatusForbidden:
		return ClassHTTP403
	case code == http.StatusNotFound:
		return ClassHTTP404
	case code == http.StatusTooManyRequests:
		return ClassHTTP429
	case code == http.StatusUnavailableForLegalReasons:
		return ClassHTTP451
	// Checked before the generic 5xx cases below, which would otherwise swallow them.
	case code >= cfOriginErrMin && code <= cfOriginErrMax, code == cfOriginDNSError:
		return ClassCFOriginDown
	case code == http.StatusBadGateway,
		code == http.StatusServiceUnavailable,
		code == http.StatusGatewayTimeout:
		return ClassGatewayNoBackend
	case code >= http.StatusInternalServerError:
		return ClassHTTP5xxServer
	case code >= http.StatusMultipleChoices && code < http.StatusBadRequest:
		return ClassHTTP3xx
	default:
		return ClassHTTPOther
	}
}

func classifyPayload(msg string) FailureClass {
	switch {
	case strings.HasPrefix(msg, "decode:"), strings.Contains(msg, "decode response:"):
		return ClassBadJSON
	case strings.HasPrefix(msg, "got="):
		return ClassChainIDMismatch
	// An address no client could use: missing scheme, a stray space inside the host, or a
	// single-slash "https:/host" that leaves net/http with no host to dial. A registry data
	// defect rather than a dead node, and actionable by correcting the entry.
	case strings.Contains(msg, "unsupported protocol scheme"),
		strings.Contains(msg, "in host name"),
		strings.Contains(msg, "no Host in request URL"):
		return ClassMalformedAddress
	case strings.HasSuffix(msg, ": EOF"), msg == "EOF":
		return ClassEOFNoResponse
	case strings.Contains(msg, "unexpected EOF"), strings.Contains(msg, "malformed HTTP"):
		return ClassBadProtocol
	case strings.Contains(msg, "bad handshake"), strings.Contains(msg, "websocket:"):
		return ClassWSSHandshake
	}
	return ClassNone
}

// classifyGRPC matches on the textual status name rather than calling status.FromError, so this
// file stays free of a gRPC dependency and mirrors analysis/classify.awk exactly. Reached only
// after the inner cause checks above have declined, so what remains is a genuine gRPC-level
// condition rather than a wrapped transport failure.
func classifyGRPC(msg string) FailureClass {
	switch {
	case strings.Contains(msg, "code = Unavailable"):
		return ClassGRPCUnavailable
	case strings.Contains(msg, "code = PermissionDenied"):
		return ClassGRPCPermission
	case strings.Contains(msg, "code = Unimplemented"):
		return ClassGRPCUnimplemented
	case strings.Contains(msg, "code = ResourceExhausted"):
		return ClassGRPCExhausted
	case strings.Contains(msg, "code = Internal"):
		return ClassGRPCInternal
	case strings.Contains(msg, "code = Unauthenticated"):
		return ClassGRPCUnauth
	case strings.Contains(msg, "rpc error"):
		return ClassGRPCOther
	}
	return ClassNone
}
