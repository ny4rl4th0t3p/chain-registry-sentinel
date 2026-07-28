package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// NewHTTPClient returns the shared probe client. A non-empty ua is set as the User-Agent on
// every request at the transport level — one place instead of every call site. Identifying
// ourselves is scanner manners: it gives operators a specific string to allowlist (or contact)
// instead of a generic Go client to block, and it cannot change any finding — the User-Agent
// hypothesis was tested against the largest suspect providers and disconfirmed.
//
// The gRPC and WSS probes do not go through this client; they carry the UA themselves.
func NewHTTPClient(timeout time.Duration, ua string) *http.Client {
	c := &http.Client{Timeout: timeout}
	if ua != "" {
		c.Transport = uaTransport{base: http.DefaultTransport, ua: ua}
	}
	return c
}

type uaTransport struct {
	base http.RoundTripper
	ua   string
}

// RoundTrip clones the request before mutating it, as the RoundTripper contract requires.
func (t uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", t.ua)
	}
	return t.base.RoundTrip(req)
}

// maxBodyPrefix bounds how much of a non-2xx response body is retained: enough for a short
// operator message, small enough that a hostile or misconfigured endpoint cannot flood memory
// or the report.
const maxBodyPrefix = 512

// httpResult is the outcome of one HTTP attempt.
//
// Body is populated only for non-2xx responses, where it is frequently the only place the real
// cause appears. PublicNode answers 403 with "unsupported platform" for chains it has dropped —
// a retired endpoint, not a block — and that is indistinguishable from a WAF rejection by
// status code and headers alone. Discarding the body cost a wrong conclusion once already.
type httpResult struct {
	StatusCode int
	Body       string
	Latency    time.Duration
	Err        error
	NetErr     bool // Err came from a transport failure rather than an HTTP-level error
}

// httpGetJSON performs a GET request and decodes the JSON response body into dest.
func httpGetJSON(ctx context.Context, client *http.Client, url string, dest any) (res httpResult) {
	start := time.Now()
	// Named return plus defer so latency is recorded on every exit path, including errors.
	defer func() { res.Latency = time.Since(start) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		res.Err = err
		return res
	}
	resp, err := client.Do(req)
	if err != nil {
		// Returned unwrapped: Classify needs the *url.Error chain intact to reach the
		// underlying *net.DNSError, x509 error, or syscall errno.
		res.Err = err
		res.NetErr = true
		return res
	}
	defer resp.Body.Close()

	res.StatusCode = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		res.Body = readBodyPrefix(resp.Body)
		res.Err = fmt.Errorf("HTTP %d", resp.StatusCode)
		return res
	}
	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		res.Err = fmt.Errorf("decode: %w", err)
		return res
	}
	return res
}

// readBodyPrefix reads at most maxBodyPrefix bytes and flattens the result to a single line,
// so an operator message can be classified and printed without corrupting log or report output.
func readBodyPrefix(r io.Reader) string {
	buf, err := io.ReadAll(io.LimitReader(r, maxBodyPrefix))
	if err != nil && len(buf) == 0 {
		return ""
	}
	flattened := strings.Map(func(c rune) rune {
		switch {
		case c == '\n', c == '\r', c == '\t':
			return ' '
		case unicode.IsControl(c):
			return -1
		default:
			return c
		}
	}, string(buf))
	return strings.TrimSpace(flattened)
}
