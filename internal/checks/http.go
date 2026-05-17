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
// Returns the fetch error (if any) and whether it was a network-level failure.
func httpGetJSON(ctx context.Context, client *http.Client, url string, dest any) (error, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return err, true
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode), false
	}
	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return fmt.Errorf("decode: %w", err), false
	}
	return nil, false
}
