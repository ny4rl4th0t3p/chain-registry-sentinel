# chain-registry-sentinel

`cosmos/chain-registry` is the source of truth for RPC endpoints, IBC channels, asset metadata, and chain configuration
across the Cosmos ecosystem. It is community-maintained and slowly decays — RPCs go offline, channels close... Nobody is
continuously checking whether what's listed actually works.

This project does that check automatically. It reads the registry, queries each chain directly, and reports what no
longer matches reality. When the evidence is strong enough — consistent failures over days, not a one-off blip — it
proposes a correction through a pull request, with the evidence attached and a clear way for maintainers to reject it.

The goal is not to replace human judgment. Every proposed change goes through a normal PR that a maintainer approves or
closes. The sentinel just does the tedious part: watching endpoints, counting failures, and writing up findings.

## What it checks

- **Endpoint liveness** — probes every declared RPC, REST, gRPC, gRPC-web, EVM JSON-RPC, and WSS endpoint. Failures are
  tracked across runs; an endpoint is only flagged after `min-failures` consecutive failing runs, and re-probed once
  more before a removal PR is opened.
- **Chain ID** — RPC endpoints that respond with a different chain ID than the registry declares are reported.
- **IBC denom hash integrity** — recomputes each IBC asset's `ibc/<HASH>` denom from its declared trace path and opens a
  fix PR on any mismatch. Deterministic and local — no failure streak needed.

Checks that only detect (and can't safely auto-fix) are on the [roadmap](#roadmap).

---

## Using the GitHub Action

Add the sentinel to your chain-registry fork. On each run it probes every endpoint, tracks failures across runs, and
opens a PR to remove any endpoint that has failed consistently.

### Minimal workflow

```yaml
# .github/workflows/sentinel.yml
name: sentinel

on:
  schedule:
    - cron: '0 6 * * *'   # daily at 06:00 UTC
  workflow_dispatch:

permissions:
  contents: write        # push sentinel branches and state branch
  pull-requests: write   # open PRs

jobs:
  sentinel:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: ny4rl4th0t3p/chain-registry-sentinel@v0.5.1
        with:
          registry: .
          state-branch: sentinel-state
          github-token: ${{ secrets.GITHUB_TOKEN }}
```

The action restores state from the `sentinel-state` branch before probing and pushes the updated state back after. The
branch is created automatically on the first run.

### Persisting state

**`state-branch` (recommended).** Pass a branch name and the action handles everything:
restore before probing, push after. State is kept on a dedicated branch isolated from the default branch and its
protection rules — the same pattern `gh-pages` uses. It has a full git history and never expires.

```yaml
      - uses: ny4rl4th0t3p/chain-registry-sentinel@v0.5.1
        with:
          registry: .
          state-branch: sentinel-state
          github-token: ${{ secrets.GITHUB_TOKEN }}
```

**`state-path` with `actions/cache` (simpler, less reliable).** Manage persistence yourself with a cache step. The
entire state directory is saved and restored as one entry. It expires if the workflow has not run for 7 days — at which
point all accumulated streaks are lost. Only use this when you cannot write to the repo.

```yaml
      - uses: actions/cache@v4
        with:
          path: .sentinel-state
          key: sentinel-state-${{ github.run_id }}
          restore-keys: sentinel-state-

      - uses: ny4rl4th0t3p/chain-registry-sentinel@v0.5.1
        with:
          registry: .
          state-path: .sentinel-state
          github-token: ${{ secrets.GITHUB_TOKEN }}
```

### All inputs

| Input                     | Default  | Description                                                                                      |
|---------------------------|----------|--------------------------------------------------------------------------------------------------|
| `registry`                | `.`      | Path to a local chain-registry clone, relative to the workspace.                                 |
| `chains`                  | `all`    | Comma-separated chain names to check, or `all`.                                                  |
| `timeout`                 | `60s`    | Per-request probe timeout (Go duration syntax: `30s`, `1m`).                                     |
| `concurrency`             | `16`     | Maximum simultaneous endpoint probes. See the DNS note below before raising it.                  |
| `state-path`              | _(none)_ | Directory for per-chain state files. Use when managing persistence externally.                   |
| `reset-state`             | `false`  | Start unreadable state files from scratch instead of failing the step.                           |
| `state-branch`            | _(none)_ | Branch to persist state automatically (created on first push).                                   |
| `min-failures`            | `14`     | Consecutive failing **runs** before an endpoint is flagged.                                      |
| `chain-death-min-runs`    | `14`     | Consecutive dead-looking **runs** before a chain status-flip PR. See PR behaviour.               |
| `chain-death-stale-after` | `168h`   | Newest-block age before an answering chain counts as halted. Raise it during a known recovery.   |
| `max-status-prs`          | `3`      | Maximum new chain status-flip PRs per run.                                                       |
| `dry-run`                 | `false`  | Report and show would-be PRs, but write nothing and open nothing.                                |
| `github-token`            | _(none)_ | Token with `contents: write` and `pull-requests: write`. Needed for PRs and `state-branch`.      |
| `max-endpoint-prs`        | `5`      | Maximum new **endpoint-removal PRs** opened per run. `0` disables the flow.                      |
| `max-hash-prs`            | `5`      | Maximum new **IBC hash-fix PRs** opened per run. `0` disables the flow.                          |
| `pr-cooldown-days`        | `7`      | Minimum days since the last PR was **opened** for a chain before another may open. `0` disables. |
| `verbose`                 | `false`  | Enable debug logging to stderr.                                                                  |

### Input details

**`registry`** — the checkout the sentinel reads. In the usual setup (`actions/checkout` of a chain-registry fork,
sentinel step after it), this is `.`.

**`chains`** — names must match registry directory names. Chains whose `status` is not `live`
are skipped. Names not found in the registry are warned about at the end of the run. The filter applies to every check,
including the IBC denom hash check.

**`concurrency`** — applies to endpoint probes only; hash checks are local and run synchronously after probing. The
default is deliberately conservative: every probe fires A and AAAA DNS lookups, and a large burst overruns dnsmasq-class
forwarders (default limit: 150 in-flight queries) — home routers, container stub resolvers, VM NAT chains. An overloaded
resolver does not just slow the run down, it corrupts the results: dropped lookups surface as DNS failures and even
false NXDOMAINs for healthy endpoints. In one measured case, concurrency 250 behind a VM's dnsmasq halved the number of
endpoints reported live. Raise this only on infrastructure whose resolver chain you trust (measured fine on
GitHub-hosted runners), and treat a nonzero `dns_failure`/`vantage_no_route` count in the report as the signal to lower
it.

**`state-path`** — holds one JSON state file per chain (failure streaks, PR timestamps). **If neither `state-path` nor
`state-branch` is set, there is no state**: endpoints are probed and problems are reported in the log, but nothing is
ever flagged and no PRs of any kind are opened — including hash-fix PRs, which need state for cooldown tracking.

**`state-branch`** — before probing, the branch is fetched and unpacked into `state-path`
(a missing branch just means a fresh start). After a non-dry run, changed state is committed as `github-actions[bot]`
and pushed; nothing is pushed when state didn't change. Requires
`github-token`. When `state-path` is unset, it defaults to `.sentinel-state`. Both inputs can coexist: `state-branch`
drives persistence, `state-path` says where the files live.

**`min-failures`** — counted per **run**, not per day; a nightly schedule at `3` means three days, three back-to-back
dispatches mean an hour. Any successful probe resets that endpoint's streak to zero. Before a PR is opened, every
flagged endpoint is re-probed once more; endpoints that recovered are excluded and their streak reset.

**`dry-run`** — reads state but never writes it (streaks do **not** advance), opens no PRs, pushes no state branch.
Would-be PRs are printed as `DRY-RUN:` lines. No token needed.

**`github-token`** — only needed to open PRs and to push `state-branch`. A report-only or dry-run setup works without
it. `GITHUB_REPOSITORY` is injected by the runner: PRs always target the repository the workflow runs in.

**`max-endpoint-prs` / `max-hash-prs` / `max-status-prs`** — per-run ceilings on newly opened PRs, one per PR type;
chains beyond a cap wait for a later run. They do **not** bound the total number of simultaneously open PRs, and the
pools are independent — a run at the endpoint cap can still open hash-fix and status-flip PRs. Setting a cap to `0`
turns that PR flow off entirely, which is how a scheduled workflow is scoped to a single PR type.

**`pr-cooldown-days`** — measured from when the last sentinel PR for that chain was opened, and tracked separately for
endpoint-removal and hash-fix PRs. In practice this governs how soon a *closed* (rejected) PR may be re-proposed — an
open PR already blocks duplicates on its own (see PR behaviour below).

### Step outcome

The step is **green whenever the sentinel ran**, findings included. Dead endpoints are the expected steady state of a
decaying registry, so a red run for them would fire every time and mean nothing. The signal is the log summary and the
opened PRs.

The step **fails only when the sentinel could not run**: an unreadable registry path, no chains found, or state files
that exist but cannot be parsed. A run that probed nothing must not look like a clean one, or the only symptom is PRs
quietly ceasing to appear days later.

Nothing is absorbed or rewritten in between — the step outcome is exactly the binary's exit code.

### PR behaviour

- The sentinel searches for an existing open PR with the `sentinel` label before opening a new one. If one is already
  open for a chain, it skips that chain.
- Before opening a PR, the sentinel re-probes all flagged endpoints. If any have recovered, their failure streak is
  reset, and they are excluded from the PR. If all recover, no PR is opened.
- Branch names follow the pattern `sentinel/{chain}-{N}` where N increments each time. Branches are never deleted by the
  sentinel.
- PRs are labelled `sentinel` and `automated` (both created automatically if missing).

### Dead-chain detection

Sometimes the dead thing is the chain, not its endpoints — and then the right fix is flipping the chain's `status` from
`live` to `killed` in `chain.json` (one line, schema-supported, corrects every downstream consumer at once) rather than
deleting dozens of endpoints one by one.

A chain counts as dead-looking in a run when either signature holds:

- **abandoned** — zero live RPC/REST/EVM endpoints, while its operators are demonstrably alive on other chains:
  three of them for normal chains, every one of them for chains with fewer than three operators (never fewer than two —
  one witness is an anecdote, so single-operator chains can only be caught by the halted signature). Healthy operators
  deleting their records for one specific chain is deliberate withdrawal, not an outage.
- **halted** — nodes still answer, but every synced one reports a `latest_block_time` older than
  `chain-death-stale-after` (default 7 days). The survivors are answering about a chain that stopped advancing. Working
  through a known outage or disaster recovery? Raise this above the expected downtime and the halt never starts a death
  streak. The two dials also stack: even at the default, a halt only yields a PR after it *also*
  persists for `chain-death-min-runs` runs — a 10-day recovery at daily cadence never reaches the default 14.

Only after `chain-death-min-runs` consecutive dead-looking runs does the sentinel open a PR flipping the status, with
all the evidence in the body: the streak duration, per-check failure summary, the operators that withdrew (with their
live counts elsewhere), the newest observed block time, and a one-line way to verify. At most `max-status-prs` open per
run (default 3); a chain flagged for a status PR is excluded from endpoint-removal and hash-fix PRs that run — no point
grooming a chain that is about to be marked killed.

Two safety rules: streaks freeze entirely on runs whose failures look vantage-caused (a broken resolver would otherwise
make every chain look dead at once), and a chain's streak only moves on runs where its core checks actually executed. As
everywhere else: the evidence is machine-gathered, and the maintainer reviewing the PR is the human check.

### IBC denom hash check

IBC denoms in `assetlist.json` are derived values: `ibc/<HASH>` where `<HASH>` is the uppercase hex SHA256 of the
asset's denom trace path (e.g. `transfer/channel-0/uatom`). Copy-paste errors and manual edits break this silently — the
asset still looks valid to tooling that doesn't verify the derivation.

On every run the sentinel recomputes the hash from the declared trace path and compares it to the `base` field. Unlike
endpoint liveness, this is a deterministic local fact — no network calls, no failure streak, no `min-failures`
threshold. A mismatch found on the first run opens a PR on the first run.

- The trace path is treated as ground truth: it carries human-readable information (channel ID, denom) that the hash
  does not. The `base` field is corrected, along with any `denom_units` entries carrying the same wrong hash (the schema
  requires `base` to appear in `denom_units`).
- For multi-hop assets, the last trace's `chain.path` already encodes the full accumulated path and is hashed as-is.
  Assets with an `ibc/` base but no trace path cannot be verified and are skipped.
- All wrong hashes for a chain are fixed in one commit, in a PR titled `[sentinel] fix IBC denom hash: {chain}` on a
  branch named `sentinel/{chain}-hash-{N}`, labelled `sentinel` and `automated`.
- The open-PR guard and `pr-cooldown-days` apply, tracked separately from endpoint-removal PRs — a hash PR and an
  endpoint PR for the same chain can be open at the same time.
- In dry-run mode the sentinel prints the would-be fixes instead of opening PRs.

### Checking a subset of chains

```yaml
      - uses: ny4rl4th0t3p/chain-registry-sentinel@v0.5.1
        with:
          registry: .
          chains: cosmoshub,osmosis,juno
          state-path: .sentinel-state
          github-token: ${{ secrets.GITHUB_TOKEN }}
```

### Dry-run (no writes, no PRs)

```yaml
      - uses: ny4rl4th0t3p/chain-registry-sentinel@v0.5.1
        with:
          registry: .
          state-path: .sentinel-state
          dry-run: true
```

---

## Running locally

```bash
# build
go build -o sentinel ./cmd/sentinel/

# probe cosmoshub and osmosis, track state, open PRs if min-failures is crossed
./sentinel \
  --registry /path/to/chain-registry \
  --chains cosmoshub,osmosis \
  --state-path /tmp/sentinel-state \
  --min-failures 14 \
  --github-token ghp_... \
  --github-repo your-org/chain-registry

# dry-run: read state, show what would happen, do not write or open PRs
./sentinel \
  --registry /path/to/chain-registry \
  --state-path /tmp/sentinel-state \
  --dry-run
```

Exit code is `0` whenever the sentinel ran, whether or not it found dead endpoints, chain ID mismatches or IBC denom
hash errors — reporting that is the job, not a failure. It is `1` only when the sentinel could not run: an unreadable
registry path, no chains found, or state files that exist but cannot be parsed.

Unreadable state aborts rather than starting those chains from scratch, because a silent reset discards failure streaks
that take days to rebuild while the run still finishes normally. Fix or delete the offending files, or pass
`--reset-state` to accept the loss deliberately.

---

## The report

Every run ends with an aggregate report on stdout: failure classes split into *structural* (provably broken regardless
of how the probe behaved — DNS gone, TLS broken, the server itself answering that nothing is there) versus *ambiguous*
(timeouts, throttling, blocks — anything an aggressive or badly-networked prober could have caused itself), a per-domain
live/dead table, a remedy taxonomy for fully dead domains, chain reachability, and node quality. No post-processing
needed — run it, read it.

If the report opens with a **vantage health warning**, believe it: the failures are dominated by classes that describe
the probing machine (a failing resolver, a missing IPv6 route), and nothing in that run — including the structural
counts — should be quoted. Fix the machine's DNS or routing, or lower `concurrency`, and re-run.

### Records: keep a run, re-analyze it later

```bash
# probe and keep one JSONL file per run (per-endpoint records: class, status, latency, evidence...)
./sentinel --registry /path/to/chain-registry --report ./records --vantage home

# re-render the report from a saved run — no probing, no registry, no network
./sentinel --from ./records/20260728T113530Z-home.jsonl
```

`--report <dir>` writes the run's per-endpoint records as one JSONL file, named after the run's UTC timestamp and the
`--vantage` label. The label exists so runs from different networks can be told apart later — a home connection and a
datacenter are different measurements of the same registry.

Probes identify themselves: the default User-Agent is `chain-registry-sentinel/<version>` with a link to this
repository, so endpoint operators see a specific string to allowlist (or a place to complain) instead of a generic Go
client. Override with `--user-agent`.

`--from <file>` renders the report from one previously saved run instead of probing. It takes the file, not its
directory, on purpose: the report should be traceable to exactly one named input, never to whichever file an implicit
rule happened to pick. A file containing more than one run (hand-concatenated) is refused rather than mixed, since
mixing runs would double-count endpoints. Records are plain JSONL: every record carries both the machine-derived
`failure_class` and the raw `evidence` it was derived from, so anything beyond the built-in report — and any future
reclassification — is one `jq` away.

---

## Roadmap

Two further IBC checks are under research. Both detect problems that have no unambiguous automated fix, so what a
finding should trigger (a PR removing the entry, an issue, or a report) is an open policy question — input from registry
maintainers is welcome before these are built.

- **IBC channel state** — verify that channels declared in `_IBC/*.json` actually report `OPEN` on both chains, with
  failure streaks since closed channels can be reopened by a relayer.
- **IBC client expiry** — a channel can report `OPEN` while its underlying light client has expired past its trusting
  period, silently breaking the connection. This check would catch that failure mode underneath the channel check.