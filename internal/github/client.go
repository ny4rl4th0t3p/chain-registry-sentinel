package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const defaultBaseURL = "https://api.github.com"

// Client is a minimal GitHub REST API client.
type Client struct {
	token   string
	baseURL string // "https://api.github.com"; overridable via WithBaseURL for tests
	http    *http.Client
}

// NewClient returns a Client authenticated with a token.
func NewClient(token string) *Client {
	return &Client{token: token, baseURL: defaultBaseURL, http: &http.Client{}}
}

// WithBaseURL returns a shallow copy of c with a different base URL (tests only).
func (c *Client) WithBaseURL(u string) *Client {
	cp := *c
	cp.baseURL = u
	return &cp
}

// do execute one GitHub API call. reqBody nil → no request body.
// respDest nil → response body discarded. Returns the HTTP status code.
func (c *Client) do(ctx context.Context, method, path string, reqBody, respDest any) (int, error) {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return 0, fmt.Errorf("github.do: marshal: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, fmt.Errorf("github.do: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("github.do: %w", err)
	}
	defer resp.Body.Close()
	if respDest != nil && resp.StatusCode < http.StatusMultipleChoices {
		if err := json.NewDecoder(resp.Body).Decode(respDest); err != nil {
			return resp.StatusCode, fmt.Errorf("github.do: decode: %w", err)
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode, nil
}

// DefaultBranch returns the default branch name for a repository.
func (c *Client) DefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	var dest struct {
		DefaultBranch string `json:"default_branch"`
	}
	status, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s", owner, repo), nil, &dest)
	if err != nil {
		return "", fmt.Errorf("github.DefaultBranch: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("github.DefaultBranch: unexpected status %d", status)
	}
	return dest.DefaultBranch, nil
}

// branchSHA returns the HEAD commit SHA of a branch.
func (c *Client) branchSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	var dest struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	path := fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, repo, branch)
	status, err := c.do(ctx, http.MethodGet, path, nil, &dest)
	if err != nil {
		return "", fmt.Errorf("github.branchSHA: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("github.branchSHA: unexpected status %d", status)
	}
	return dest.Object.SHA, nil
}

// NextBranchN returns the next integer suffix N for branches named sentinel/{chain}-N.
// Finds existing refs via matching-refs and returns max(N)+1, or 1 if none exist.
func (c *Client) NextBranchN(ctx context.Context, owner, repo, chain string) (int, error) {
	path := fmt.Sprintf("/repos/%s/%s/git/matching-refs/heads/sentinel/%s-", owner, repo, chain)
	var refs []struct {
		Ref string `json:"ref"`
	}
	status, err := c.do(ctx, http.MethodGet, path, nil, &refs)
	if err != nil {
		return 0, fmt.Errorf("github.NextBranchN: %w", err)
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("github.NextBranchN: unexpected status %d", status)
	}
	prefix := "refs/heads/sentinel/" + chain + "-"
	maxN := 0
	for _, r := range refs {
		suffix := strings.TrimPrefix(r.Ref, prefix)
		n, err := strconv.Atoi(suffix)
		if err != nil {
			continue
		}
		if n > maxN {
			maxN = n
		}
	}
	return maxN + 1, nil
}

// HasOpenPR returns true if there is already an open PR for the given chain.
// Searches for open PRs with the sentinel label and the standard PR title.
func (c *Client) HasOpenPR(ctx context.Context, owner, repo, chain string) (bool, error) {
	title := fmt.Sprintf("[sentinel] remove dead endpoints: %s", chain)
	q := fmt.Sprintf(`repo:%s/%s is:pr is:open label:sentinel %q in:title`, owner, repo, title)
	var dest struct {
		TotalCount int `json:"total_count"`
	}
	status, err := c.do(ctx, http.MethodGet, "/search/issues?q="+url.QueryEscape(q), nil, &dest)
	if err != nil {
		return false, fmt.Errorf("github.HasOpenPR: %w", err)
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("github.HasOpenPR: unexpected status %d", status)
	}
	return dest.TotalCount > 0, nil
}

// GetFileSHA returns the decoded content and blob SHA of a file at the given branch.
func (c *Client) GetFileSHA(ctx context.Context, owner, repo, path, branch string) (content []byte, blobSHA string, err error) {
	var dest struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	apiPath := fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", owner, repo, path, branch)
	status, err := c.do(ctx, http.MethodGet, apiPath, nil, &dest)
	if err != nil {
		return nil, "", fmt.Errorf("github.GetFileSHA: %w", err)
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("github.GetFileSHA: unexpected status %d", status)
	}
	content, err = base64.StdEncoding.DecodeString(strings.ReplaceAll(dest.Content, "\n", ""))
	if err != nil {
		return nil, "", fmt.Errorf("github.GetFileSHA: decode content: %w", err)
	}
	return content, dest.SHA, nil
}

// CreateBranch creates a new branch from the given SHA.
func (c *Client) CreateBranch(ctx context.Context, owner, repo, branch, fromSHA string) error {
	body := map[string]string{"ref": "refs/heads/" + branch, "sha": fromSHA}
	status, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo), body, nil)
	if err != nil {
		return fmt.Errorf("github.CreateBranch: %w", err)
	}
	if status != http.StatusCreated {
		return fmt.Errorf("github.CreateBranch: unexpected status %d", status)
	}
	return nil
}

// CommitFile creates or updates a file on an existing branch.
// blobSHA is the current blob SHA of the file (required by the GitHub API to detect conflicts).
func (c *Client) CommitFile(
	ctx context.Context,
	owner, repo, path, branch, message, blobSHA string,
	content []byte,
) error {
	body := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  branch,
		"sha":     blobSHA,
	}
	status, err := c.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, path), body, nil)
	if err != nil {
		return fmt.Errorf("github.CommitFile: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("github.CommitFile: unexpected status %d", status)
	}
	return nil
}

// CreatePR opens a new pull request and returns its number and HTML URL.
func (c *Client) CreatePR(
	ctx context.Context,
	owner, repo, title, body, head, base string,
) (number int, htmlURL string, err error) {
	reqBody := map[string]string{
		"title": title,
		"body":  body,
		"head":  head,
		"base":  base,
	}
	var dest struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	status, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", owner, repo), reqBody, &dest)
	if err != nil {
		return 0, "", fmt.Errorf("github.CreatePR: %w", err)
	}
	if status != http.StatusCreated {
		return 0, "", fmt.Errorf("github.CreatePR: unexpected status %d", status)
	}
	return dest.Number, dest.HTMLURL, nil
}

// EnsureLabel creates the label if it does not already exist. A 200 response
// means it exists; a 404 triggers a POST to create it.
func (c *Client) EnsureLabel(ctx context.Context, owner, repo, name, color string) error {
	status, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/labels/%s", owner, repo, name), nil, nil)
	if err != nil {
		return fmt.Errorf("github.EnsureLabel: %w", err)
	}
	if status == http.StatusOK {
		return nil
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("github.EnsureLabel: unexpected status %d", status)
	}
	body := map[string]string{"name": name, "color": color}
	status, err = c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/labels", owner, repo), body, nil)
	if err != nil {
		return fmt.Errorf("github.EnsureLabel: %w", err)
	}
	if status != http.StatusCreated {
		return fmt.Errorf("github.EnsureLabel: unexpected status %d", status)
	}
	return nil
}

// AddLabels applies labels to an issue or PR by number.
func (c *Client) AddLabels(ctx context.Context, owner, repo string, prNumber int, labels []string) error {
	body := map[string][]string{"labels": labels}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels", owner, repo, prNumber)
	status, err := c.do(ctx, http.MethodPost, path, body, nil)
	if err != nil {
		return fmt.Errorf("github.AddLabels: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("github.AddLabels: unexpected status %d", status)
	}
	return nil
}
