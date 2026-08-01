package github

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"chain-registry-sentinel/internal/checks"
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

// checkCategory maps a liveness check name to the apis key its entries live under. Removal
// must be scoped to the (category, address) pair, never the bare address: 28 registry
// addresses are declared under multiple categories (Pocket Network's gateways serve RPC and
// REST on one URL), and address-keyed deletion would remove a live sibling along with the
// dead entry.
var checkCategory = map[string]string{
	"rpc_liveness":      "rpc",
	"rest_liveness":     "rest",
	"grpc_liveness":     "grpc",
	"grpc_web_liveness": "grpc-web",
	"evm_liveness":      "evm-http-jsonrpc",
	"wss_liveness":      "wss",
}

// EditChainJSON reads {registryPath}/{chainName}/chain.json, surgically removes
// the dead (category, address) pairs from the apis subarrays, and returns the modified bytes.
// Returns nil, nil when nothing was removed (no-op signal).
// The file's original formatting and key order are preserved.
func EditChainJSON(registryPath, chainName string, dead []FlaggedEndpoint) ([]byte, error) {
	path := filepath.Join(registryPath, chainName, "chain.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("EditChainJSON: %w", err)
	}

	deadPairs := make(map[string]struct{}, len(dead))
	for _, ep := range dead {
		cat, known := checkCategory[ep.Check]
		if !known {
			// An unmapped check name must not fall back to any broader match: refusing to
			// delete is recoverable, deleting the wrong entry is not.
			continue
		}
		deadPairs[cat+"|"+ep.Address] = struct{}{}
	}

	// Collect indices to remove per api category.
	toDelete := make(map[string][]int)
	gjson.GetBytes(data, "apis").ForEach(func(category, endpoints gjson.Result) bool {
		endpoints.ForEach(func(idx, ep gjson.Result) bool {
			if _, isDead := deadPairs[category.String()+"|"+ep.Get("address").String()]; isDead {
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
	sb.WriteString("| Check | Address | First seen failing | Consecutive failures | First evidence | Latest evidence |\n")
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
		return fmt.Sprintf("printf '\\x00\\x00\\x00\\x00\\x00' | curl -s -X POST"+
			" -H 'Content-Type: application/grpc-web+proto' -H 'X-Grpc-Web: 1' --data-binary @-"+
			" '%s/cosmos.base.tendermint.v1beta1.Service/GetNodeInfo' | strings | head", ep.Address)
	case "grpc_liveness":
		return grpcVerifyCmd(ep.Address)
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

// grpcVerifyMethod is grpcurl's method form: no leading slash, unlike the wire path the probe
// invokes.
const grpcVerifyMethod = "cosmos.base.tendermint.v1beta1.Service/GetNodeInfo"

// grpcVerifyCmd derives the grpcurl line from the same target-resolution rules the probe
// dialed with (checks.ParseGRPCTarget) — scheme stripped, default port applied, and the TLS
// mode the probe actually attempted. A command derived from any other rule can fail for its
// own reasons and falsely "confirm" a dead endpoint.
func grpcVerifyCmd(address string) string {
	target, modes, err := checks.ParseGRPCTarget(address)
	if err != nil {
		// Malformed in the registry; grpcurl cannot dial it either — show it as recorded.
		return fmt.Sprintf("grpcurl %s %s", address, grpcVerifyMethod)
	}
	switch {
	case len(modes) == 2:
		// Nonstandard port: the probe tries TLS first, then plaintext. One line cannot do
		// both, so mirror the order and say so.
		return fmt.Sprintf("grpcurl %s %s   # TLS first, as probed; if the dial fails, retry with -plaintext",
			target, grpcVerifyMethod)
	case modes[0]:
		return fmt.Sprintf("grpcurl %s %s", target, grpcVerifyMethod)
	default:
		return fmt.Sprintf("grpcurl -plaintext %s %s", target, grpcVerifyMethod)
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
	title := "[sentinel] remove dead endpoints: " + req.Chain.Name
	open, err := client.HasOpenPR(ctx, req.Owner, req.Repo, title)
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

// HashFix describes one IBC asset whose base hash needs to be corrected.
type HashFix struct {
	AssetName string
	Base      string // declared (wrong) hash: "ibc/WRONG"
	Expected  string // correct hash: "ibc/SHA256(path)"
	Path      string
}

// HashPRRequest contains everything needed to open a hash-fix PR for one chain.
type HashPRRequest struct {
	Owner        string
	Repo         string
	BaseBranch   string // empty → resolved via DefaultBranch
	ChainName    string
	Fixes        []HashFix
	RegistryPath string
}

// EditAssetListJSON reads {registryPath}/{chainName}/assetlist.json and surgically
// updates the base field of assets whose current base matches a fix's Base field,
// along with any denom_units entries carrying the same wrong hash.
// Returns nil, nil when nothing was changed (no-op signal).
func EditAssetListJSON(registryPath, chainName string, fixes []HashFix) ([]byte, error) {
	p := filepath.Join(registryPath, chainName, "assetlist.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("EditAssetListJSON: %w", err)
	}
	// Key on base AND trace path: a wrong hash can collide with another
	// asset's correct base (e.g. a denom copy-pasted from a different IBC
	// connection), and that asset must not be touched.
	type fixKey struct{ base, path string }
	fixMap := make(map[fixKey]string, len(fixes))
	for _, f := range fixes {
		fixMap[fixKey{f.Base, f.Path}] = f.Expected
	}
	type indexFix struct {
		idx      int
		expected string
		units    []int // denom_units entries that carry the wrong hash
	}
	var toFix []indexFix
	gjson.GetBytes(data, "assets").ForEach(func(idx, asset gjson.Result) bool {
		base := asset.Get("base").String()
		expected, ok := fixMap[fixKey{base, lastTracePath(asset.Get("traces"))}]
		if !ok {
			return true
		}
		f := indexFix{idx: int(idx.Int()), expected: expected}
		asset.Get("denom_units").ForEach(func(uidx, unit gjson.Result) bool {
			if unit.Get("denom").String() == base {
				f.units = append(f.units, int(uidx.Int()))
			}
			return true
		})
		toFix = append(toFix, f)
		return true
	})
	if len(toFix) == 0 {
		return nil, nil
	}
	for _, f := range toFix {
		data, err = sjson.SetBytes(data, fmt.Sprintf("assets.%d.base", f.idx), f.expected)
		if err != nil {
			return nil, fmt.Errorf("EditAssetListJSON: %w", err)
		}
		// The schema requires base to be present in denom_units, so the wrong
		// hash there must be rewritten in the same pass.
		for _, u := range f.units {
			data, err = sjson.SetBytes(data, fmt.Sprintf("assets.%d.denom_units.%d.denom", f.idx, u), f.expected)
			if err != nil {
				return nil, fmt.Errorf("EditAssetListJSON: %w", err)
			}
		}
	}
	return data, nil
}

// lastTracePath returns chain.path from the last trace that has one set —
// the same resolution rule registry.LoadAssetList uses to produce HashFix.Path.
func lastTracePath(traces gjson.Result) string {
	arr := traces.Array()
	for i := len(arr) - 1; i >= 0; i-- {
		if p := arr[i].Get("chain.path").String(); p != "" {
			return p
		}
	}
	return ""
}

// BuildHashMismatchPRBody renders the PR description for an IBC denom hash fix.
func BuildHashMismatchPRBody(chainName string, fixes []HashFix) string {
	var sb strings.Builder
	sb.WriteString("## IBC denom hash mismatches\n\n")
	sb.WriteString("The `base` field of these IBC assets does not match SHA256 of the declared trace path.\n")
	sb.WriteString("The path is treated as ground truth; the `base` field and matching `denom_units` entries are updated.\n\n")
	sb.WriteString("| Asset | Declared base | Expected base | Path |\n")
	sb.WriteString("|-------|--------------|--------------|------|\n")
	for _, f := range fixes {
		fmt.Fprintf(&sb, "| `%s` | `%s` | `%s` | `%s` |\n", f.AssetName, f.Base, f.Expected, f.Path)
	}
	sb.WriteString("\n---\n\n")
	fmt.Fprintf(&sb, "> Opened automatically by chain-registry-sentinel CI for chain `%s`.\n", chainName)
	sb.WriteString("> Hash is recomputed from the `chain.path` of the last trace entry (SHA256 of the denom trace path string).\n")
	return sb.String()
}

// OpenHashPR opens a PR to fix IBC denom hashes in a chain's assetlist.json.
// Returns ("", nil) when a PR is already open or the edit is a no-op.
func OpenHashPR(ctx context.Context, client *Client, req HashPRRequest) (string, error) {
	baseBranch := req.BaseBranch
	if baseBranch == "" {
		var err error
		baseBranch, err = client.DefaultBranch(ctx, req.Owner, req.Repo)
		if err != nil {
			return "", fmt.Errorf("OpenHashPR: %w", err)
		}
	}
	title := "[sentinel] fix IBC denom hash: " + req.ChainName
	open, err := client.HasOpenPR(ctx, req.Owner, req.Repo, title)
	if err != nil {
		return "", fmt.Errorf("OpenHashPR: %w", err)
	}
	if open {
		return "", nil
	}
	n, err := client.NextBranchN(ctx, req.Owner, req.Repo, req.ChainName+"-hash")
	if err != nil {
		return "", fmt.Errorf("OpenHashPR: %w", err)
	}
	branch := fmt.Sprintf("sentinel/%s-hash-%d", req.ChainName, n)
	filePath := req.ChainName + "/assetlist.json"
	baseSHA, blobSHA, err := prepareCommit(ctx, client, req.Owner, req.Repo, baseBranch, filePath)
	if err != nil {
		return "", fmt.Errorf("OpenHashPR: %w", err)
	}
	content, err := EditAssetListJSON(req.RegistryPath, req.ChainName, req.Fixes)
	if err != nil {
		return "", fmt.Errorf("OpenHashPR: %w", err)
	}
	if content == nil {
		return "", nil
	}
	if err := client.CreateBranch(ctx, req.Owner, req.Repo, branch, baseSHA); err != nil {
		return "", fmt.Errorf("OpenHashPR: %w", err)
	}
	commitMsg := "sentinel: fix IBC denom hashes in " + req.ChainName + "/assetlist.json"
	if err := client.CommitFile(ctx, req.Owner, req.Repo, filePath, branch, commitMsg, blobSHA, content); err != nil {
		return "", fmt.Errorf("OpenHashPR: %w", err)
	}
	if err := client.EnsureLabel(ctx, req.Owner, req.Repo, labelSentinel, colorSentinel); err != nil {
		return "", fmt.Errorf("OpenHashPR: %w", err)
	}
	if err := client.EnsureLabel(ctx, req.Owner, req.Repo, labelAutomated, colorAutomated); err != nil {
		return "", fmt.Errorf("OpenHashPR: %w", err)
	}
	body := BuildHashMismatchPRBody(req.ChainName, req.Fixes)
	prNum, prURL, err := client.CreatePR(ctx, req.Owner, req.Repo, title, body, branch, baseBranch)
	if err != nil {
		return "", fmt.Errorf("OpenHashPR: %w", err)
	}
	if err := client.AddLabels(ctx, req.Owner, req.Repo, prNum, []string{labelSentinel, labelAutomated}); err != nil {
		return "", fmt.Errorf("OpenHashPR: %w", err)
	}
	return prURL, nil
}

// WithdrawnOperator is one healthy operator that dropped the chain: its domain serves other
// chains fine but returns nothing for this one. The strongest machine-gatherable evidence of
// deliberate chain abandonment.
type WithdrawnOperator struct {
	Domain        string
	LiveElsewhere int // live endpoints this operator serves on other chains, this run
	DeadHere      int // this chain's dead endpoints on this domain
}

// StatusPREvidence is everything the status-flip PR body presents. All of it is gathered by
// the sentinel's own probing — no human research step, by design; the maintainer reviewing the
// PR is the human check, exactly as with endpoint-removal PRs.
type StatusPREvidence struct {
	Streak          int       // consecutive runs the chain has looked dead
	FirstSeen       time.Time // when the current streak started
	EndpointsProbed int       // every declared endpoint probed this run, all types
	ClassLines      []string  // per-endpoint-type failure summary, pre-rendered by the caller
	Withdrawn       []WithdrawnOperator
	// NewestBlockTime is the freshest latest_block_time any answering node reported; zero when
	// nothing answered at all. A halted chain's survivors keep answering with this frozen.
	NewestBlockTime time.Time
	SampleHost      string // one dead service hostname for the "verify it yourself" line
}

// StatusPRRequest contains everything needed to open a status-flip PR for one chain.
type StatusPRRequest struct {
	Owner        string
	Repo         string
	BaseBranch   string // empty → resolved via DefaultBranch
	ChainName    string
	RegistryPath string
	Evidence     StatusPREvidence
}

// EditChainStatus reads {registryPath}/{chainName}/chain.json and flips status to "killed".
// Returns nil, nil when the status is already not "live" — the registry may have been fixed
// between detection and PR, and flipping anything other than live→killed is never correct.
func EditChainStatus(registryPath, chainName string) ([]byte, error) {
	p := filepath.Join(registryPath, chainName, "chain.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("EditChainStatus: %w", err)
	}
	if gjson.GetBytes(data, "status").String() != "live" {
		return nil, nil
	}
	out, err := sjson.SetBytes(data, "status", "killed")
	if err != nil {
		return nil, fmt.Errorf("EditChainStatus: %w", err)
	}
	return out, nil
}

// BuildStatusPRBody renders the PR description for marking a chain as killed.
func BuildStatusPRBody(chainName string, ev StatusPREvidence) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## `%s` appears to be a dead chain\n\n", chainName)
	fmt.Fprintf(&sb, "This PR flips `status` from `live` to `killed` — one line that removes %d declared endpoints\n", ev.EndpointsProbed)
	sb.WriteString("from every downstream consumer's view at once, instead of deleting them one by one.\n\n")

	fmt.Fprintf(&sb, "**The chain has looked dead for %d consecutive sentinel runs** (since %s).\n\n",
		ev.Streak, ev.FirstSeen.Format("2006-01-02"))

	if len(ev.ClassLines) > 0 {
		sb.WriteString("| Check | Result |\n|-------|--------|\n")
		for _, line := range ev.ClassLines {
			sb.WriteString(line + "\n")
		}
		sb.WriteString("\n")
	}

	if len(ev.Withdrawn) > 0 {
		sb.WriteString("**Healthy operators have withdrawn from this chain.** Each of these providers serves\n")
		sb.WriteString("other chains normally but returns nothing for this one — deliberate decommission, not outage:\n\n")
		sb.WriteString("| Operator | Dead here | Live on other chains |\n|----------|-----------|----------------------|\n")
		for _, w := range ev.Withdrawn {
			fmt.Fprintf(&sb, "| `%s` | %d | %d |\n", w.Domain, w.DeadHere, w.LiveElsewhere)
		}
		sb.WriteString("\n")
	}

	if !ev.NewestBlockTime.IsZero() {
		fmt.Fprintf(&sb, "The freshest `latest_block_time` any still-answering node reports is **%s** — block\n",
			ev.NewestBlockTime.Format(time.RFC3339))
		sb.WriteString("production has stopped; the survivors are answering about a chain that no longer advances.\n\n")
	}

	if ev.SampleHost != "" {
		sb.WriteString("Verify in one line:\n\n")
		fmt.Fprintf(&sb, "```\ndig +short %s   # no address\n```\n\n", ev.SampleHost)
	}

	sb.WriteString("---\n\n")
	fmt.Fprintf(&sb, "> Opened automatically by chain-registry-sentinel for chain `%s`.\n", chainName)
	sb.WriteString("> If this chain is alive somewhere the sentinel cannot see, close this PR — endpoint\n")
	sb.WriteString("> streaks reset on any successful probe, and a fresh PR would need the full streak again.\n")
	return sb.String()
}

// OpenStatusPR opens a PR flipping a dead chain's status to "killed".
// Returns ("", nil) when a PR is already open or the edit is a no-op.
func OpenStatusPR(ctx context.Context, client *Client, req StatusPRRequest) (string, error) {
	baseBranch := req.BaseBranch
	if baseBranch == "" {
		var err error
		baseBranch, err = client.DefaultBranch(ctx, req.Owner, req.Repo)
		if err != nil {
			return "", fmt.Errorf("OpenStatusPR: %w", err)
		}
	}
	title := "[sentinel] mark " + req.ChainName + " as killed"
	open, err := client.HasOpenPR(ctx, req.Owner, req.Repo, title)
	if err != nil {
		return "", fmt.Errorf("OpenStatusPR: %w", err)
	}
	if open {
		return "", nil
	}
	n, err := client.NextBranchN(ctx, req.Owner, req.Repo, req.ChainName+"-status")
	if err != nil {
		return "", fmt.Errorf("OpenStatusPR: %w", err)
	}
	branch := fmt.Sprintf("sentinel/%s-status-%d", req.ChainName, n)
	filePath := req.ChainName + "/chain.json"
	baseSHA, blobSHA, err := prepareCommit(ctx, client, req.Owner, req.Repo, baseBranch, filePath)
	if err != nil {
		return "", fmt.Errorf("OpenStatusPR: %w", err)
	}
	content, err := EditChainStatus(req.RegistryPath, req.ChainName)
	if err != nil {
		return "", fmt.Errorf("OpenStatusPR: %w", err)
	}
	if content == nil {
		return "", nil
	}
	if err := client.CreateBranch(ctx, req.Owner, req.Repo, branch, baseSHA); err != nil {
		return "", fmt.Errorf("OpenStatusPR: %w", err)
	}
	commitMsg := "sentinel: mark " + req.ChainName + " as killed"
	if err := client.CommitFile(ctx, req.Owner, req.Repo, filePath, branch, commitMsg, blobSHA, content); err != nil {
		return "", fmt.Errorf("OpenStatusPR: %w", err)
	}
	if err := client.EnsureLabel(ctx, req.Owner, req.Repo, labelSentinel, colorSentinel); err != nil {
		return "", fmt.Errorf("OpenStatusPR: %w", err)
	}
	if err := client.EnsureLabel(ctx, req.Owner, req.Repo, labelAutomated, colorAutomated); err != nil {
		return "", fmt.Errorf("OpenStatusPR: %w", err)
	}
	body := BuildStatusPRBody(req.ChainName, req.Evidence)
	prNum, prURL, err := client.CreatePR(ctx, req.Owner, req.Repo, title, body, branch, baseBranch)
	if err != nil {
		return "", fmt.Errorf("OpenStatusPR: %w", err)
	}
	if err := client.AddLabels(ctx, req.Owner, req.Repo, prNum, []string{labelSentinel, labelAutomated}); err != nil {
		return "", fmt.Errorf("OpenStatusPR: %w", err)
	}
	notifySuperseded(ctx, client, req.Owner, req.Repo, req.ChainName, prNum)
	return prURL, nil
}

// notifySuperseded comments on any open sentinel endpoint-removal or hash-fix PR for the
// chain, pointing at the status-flip PR that supersedes them. It informs; it never closes —
// closing is the maintainers' call, and an automation deleting PRs could collide with their
// own processes. Best-effort: the status PR is already open, so a failure here must not fail
// the flow; it surfaces as a warning instead.
func notifySuperseded(ctx context.Context, client *Client, owner, repo, chain string, statusPRNum int) {
	titles := []string{
		"[sentinel] remove dead endpoints: " + chain,
		"[sentinel] fix IBC denom hash: " + chain,
	}
	for _, title := range titles {
		num, found, err := client.FindOpenPR(ctx, owner, repo, title)
		if err != nil {
			slog.Warn("supersession notice: PR search failed", "title", title, "err", err)
			continue
		}
		if !found {
			continue
		}
		if err := client.AddComment(ctx, owner, repo, num, BuildSupersededComment(chain, statusPRNum)); err != nil {
			slog.Warn("supersession notice: comment failed", "pr", num, "err", err)
		}
	}
}

// BuildSupersededComment is the notice posted on an open fix PR once a status-flip PR exists
// for the same chain.
func BuildSupersededComment(chain string, statusPRNum int) string {
	return fmt.Sprintf("The sentinel has opened #%d proposing to mark `%s` as `killed` — the whole chain now "+
		"looks dead, not just the entries fixed here. If #%d merges, this PR is superseded and can be closed. "+
		"(Automated notice; the sentinel never closes PRs itself.)",
		statusPRNum, chain, statusPRNum)
}
