package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// httpGetJSON performs a GET request and decodes the JSON response body into dest.
// Returns the HTTP status code (0 for network/decode errors), the fetch error, and
// whether the error was a network-level failure.
func httpGetJSON(ctx context.Context, client *http.Client, url string, dest any) (int, error, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, err, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err, true
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode), false
	}
	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return resp.StatusCode, fmt.Errorf("decode: %w", err), false
	}
	return resp.StatusCode, nil, false
}
