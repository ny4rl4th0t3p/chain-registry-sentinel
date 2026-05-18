package github

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"chain-registry-sentinel/internal/registry"
)

const (
	labelSentinel  = "sentinel"
	colorSentinel  = "0052cc"
	labelAutomated = "automated"
	colorAutomated = "e4e669"
)

// FlaggedEndpoint describes one endpoint that has crossed the minimum failure count.
type FlaggedEndpoint struct {
	Check               string
	Address             string
	ConsecutiveFailures int
	FirstFailureTime    time.Time
	FirstEvidence       string
	LastEvidence        string
}

// PRRequest contains everything needed to open a PR for one chain.
type PRRequest struct {
	Owner        string
	Repo         string
	BaseBranch   string // empty → resolved via DefaultBranch
	Chain        registry.Chain
	Dead         []FlaggedEndpoint
	RegistryPath string
}

// EditChainJSON reads {registryPath}/{chainName}/chain.json, surgically removes
// the dead addresses from all apis subarrays, and returns the modified bytes.
// Returns nil, nil when nothing was removed (no-op signal).
// The file's original formatting and key order are preserved.
func EditChainJSON(registryPath, chainName string, dead []FlaggedEndpoint) ([]byte, error) {
	path := filepath.Join(registryPath, chainName, "chain.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("EditChainJSON: %w", err)
	}

	deadAddrs := make(map[string]struct{}, len(dead))
	for _, ep := range dead {
		deadAddrs[ep.Address] = struct{}{}
	}

	// Collect indices to remove per api category.
	toDelete := make(map[string][]int)
	gjson.GetBytes(data, "apis").ForEach(func(category, endpoints gjson.Result) bool {
		endpoints.ForEach(func(idx, ep gjson.Result) bool {
			if _, isDead := deadAddrs[ep.Get("address").String()]; isDead {
				cat := category.String()
				toDelete[cat] = append(toDelete[cat], int(idx.Int()))
			}
			return true
		})
		return true
	})

	if len(toDelete) == 0 {
		return nil, nil
	}

	// Delete highest indices first to keep lower indices stable.
	for cat, indices := range toDelete {
		sort.Sort(sort.Reverse(sort.IntSlice(indices)))
		for _, i := range indices {
			data, err = sjson.DeleteBytes(data, fmt.Sprintf("apis.%s.%d", cat, i))
			if err != nil {
				return nil, fmt.Errorf("EditChainJSON: %w", err)
			}
		}
	}

	return data, nil
}

// BuildPRBody renders the PR description as GitHub-flavored markdown.
func BuildPRBody(chain registry.Chain, dead []FlaggedEndpoint) string {
	var sb strings.Builder
	sb.WriteString("## Dead endpoints\n\n")
	sb.WriteString("| Check | Address | First failed | Consecutive failures | First evidence | Latest evidence |\n")
	sb.WriteString("|-------|---------|-------------|---------------------|----------------|------------------|\n")
	for _, ep := range dead {
		firstFailed := ep.FirstFailureTime.UTC().Format("2006-01-02")
		fmt.Fprintf(&sb, "| `%s` | `%s` | %s | %d | %s | %s |\n",
			ep.Check, ep.Address, firstFailed, ep.ConsecutiveFailures,
			escapeMarkdown(ep.FirstEvidence), escapeMarkdown(ep.LastEvidence))
	}
	sb.WriteString("\n## Verification\n\nRun these commands to confirm before closing:\n\n")
	for _, ep := range dead {
		fmt.Fprintf(&sb, "**%s** — `%s`:\n```\n%s\n```\n\n", ep.Check, ep.Address, verifyCmd(ep))
	}
	sb.WriteString("---\n\n")
	fmt.Fprintf(&sb,
		"> This PR was opened automatically by chain-registry-sentinel CI for chain `%s` (`%s`).\n",
		chain.Name, chain.ChainID)
	sb.WriteString("> If these endpoints have recovered or this is a false positive, close this PR with a note — ")
	sb.WriteString("the sentinel will re-evaluate on the next run.\n")
	return sb.String()
}

// verifyCmd returns a shell snippet to manually check an endpoint.
func verifyCmd(ep FlaggedEndpoint) string {
	switch ep.Check {
	case "rpc_liveness":
		return fmt.Sprintf("curl -s '%s/status' | jq .result.node_info.network", ep.Address)
	case "rest_liveness":
		return fmt.Sprintf("curl -s '%s/cosmos/base/tendermint/v1beta1/node_info' | jq .default_node_info.network", ep.Address)
	case "grpc_web_liveness":
		return fmt.Sprintf("curl -s -H 'Content-Type: application/grpc-web+proto' '%s'", ep.Address)
	case "grpc_liveness":
		flag := "-plaintext"
		if strings.HasSuffix(ep.Address, ":443") || !strings.Contains(ep.Address, ":") {
			flag = ""
		}
		return fmt.Sprintf("grpcurl %s %s cosmos.base.tendermint.v1beta1.Service/GetNodeInfo", flag, ep.Address)
	case "evm_liveness":
		return fmt.Sprintf(
			"curl -s -X POST -H 'Content-Type: application/json' -d '{\"jsonrpc\":\"2.0\",\"method\":\"eth_chainId\",\"params\":[],\"id\":1}' '%s'",
			ep.Address)
	case "wss_liveness":
		return fmt.Sprintf("websocat '%s'", ep.Address)
	default:
		return fmt.Sprintf("curl -s '%s'", ep.Address)
	}
}

func escapeMarkdown(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

// prepareCommit returns the HEAD SHA of baseBranch and the blob SHA of filePath.
func prepareCommit(
	ctx context.Context,
	c *Client,
	owner, repo, baseBranch, filePath string,
) (baseSHA, blobSHA string, err error) {
	baseSHA, err = c.branchSHA(ctx, owner, repo, baseBranch)
	if err != nil {
		return "", "", fmt.Errorf("prepareCommit: %w", err)
	}
	_, blobSHA, err = c.GetFileSHA(ctx, owner, repo, filePath, baseBranch)
	if err != nil {
		return "", "", fmt.Errorf("prepareCommit: %w", err)
	}
	return baseSHA, blobSHA, nil
}

// OpenChainPR opens a PR to remove dead endpoints from a chain's chain.json.
// Returns ("", nil) when a PR is already open or the edit is a no-op.
func OpenChainPR(ctx context.Context, client *Client, req PRRequest) (string, error) {
	baseBranch := req.BaseBranch
	if baseBranch == "" {
		var err error
		baseBranch, err = client.DefaultBranch(ctx, req.Owner, req.Repo)
		if err != nil {
			return "", fmt.Errorf("OpenChainPR: %w", err)
		}
	}
	open, err := client.HasOpenPR(ctx, req.Owner, req.Repo, req.Chain.Name)
	if err != nil {
		return "", fmt.Errorf("OpenChainPR: %w", err)
	}
	if open {
		return "", nil
	}
	n, err := client.NextBranchN(ctx, req.Owner, req.Repo, req.Chain.Name)
	if err != nil {
		return "", fmt.Errorf("OpenChainPR: %w", err)
	}
	branch := fmt.Sprintf("sentinel/%s-%d", req.Chain.Name, n)
	filePath := req.Chain.Name + "/chain.json"
	baseSHA, blobSHA, err := prepareCommit(ctx, client, req.Owner, req.Repo, baseBranch, filePath)
	if err != nil {
		return "", fmt.Errorf("OpenChainPR: %w", err)
	}
	content, err := EditChainJSON(req.RegistryPath, req.Chain.Name, req.Dead)
	if err != nil {
		return "", fmt.Errorf("OpenChainPR: %w", err)
	}
	if content == nil {
		return "", nil
	}
	if err := client.CreateBranch(ctx, req.Owner, req.Repo, branch, baseSHA); err != nil {
		return "", fmt.Errorf("OpenChainPR: %w", err)
	}
	commitMsg := "sentinel: remove dead endpoints from " + req.Chain.Name + "/chain.json"
	if err := client.CommitFile(ctx, req.Owner, req.Repo, filePath, branch, commitMsg, blobSHA, content); err != nil {
		return "", fmt.Errorf("OpenChainPR: %w", err)
	}
	if err := client.EnsureLabel(ctx, req.Owner, req.Repo, labelSentinel, colorSentinel); err != nil {
		return "", fmt.Errorf("OpenChainPR: %w", err)
	}
	if err := client.EnsureLabel(ctx, req.Owner, req.Repo, labelAutomated, colorAutomated); err != nil {
		return "", fmt.Errorf("OpenChainPR: %w", err)
	}
	title := "[sentinel] remove dead endpoints: " + req.Chain.Name
	body := BuildPRBody(req.Chain, req.Dead)
	prNum, prURL, err := client.CreatePR(ctx, req.Owner, req.Repo, title, body, branch, baseBranch)
	if err != nil {
		return "", fmt.Errorf("OpenChainPR: %w", err)
	}
	if err := client.AddLabels(ctx, req.Owner, req.Repo, prNum, []string{labelSentinel, labelAutomated}); err != nil {
		return "", fmt.Errorf("OpenChainPR: %w", err)
	}
	return prURL, nil
}
