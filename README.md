# chain-registry-sentinel

`cosmos/chain-registry` is the source of truth for RPC endpoints, IBC channels, asset metadata, and chain configuration
across the Cosmos ecosystem. Its own README already states the policy: *"Endpoints that consistently fail to respond
successfully may be removed without warning."* But applying that policy by hand is not a human-scale job: it means
re-checking thousands of endpoints across hundreds of chains, continuously, for changes nobody announces. CI can
validate what arrives in a PR; no one can keep re-validating everything already merged. So entries outlive the
infrastructure behind them quietly — probed from a GitHub-hosted runner on 2026-08-01 against registry commit
`e1c86c4`, 60.0% of declared endpoint entries failed their liveness check, and on 96 of the 213 chains with RPC
entries (45.1%) that included the **first-listed** RPC, the entry many clients default to. That 60% is three problems,
not one: a fifth of the dead entries sit on 59 chains that are dead in their entirety (one status flip each), nearly
half are exits of operators with nothing live anywhere (operator-wide removals) — and the remainder is the ordinary
decay this tool grooms continuously: even on living chains from living operators, 32.4% of endpoints are dead. (An
earlier measurement of the May 2026 registry state, taken from two vantages that agreed within 0.3 points, read
69.1% — maintainer cleanup moved the number nine points in nine weeks.) Every figure here is
[auditable from frozen artifacts](#auditing-the-published-numbers) with one script.

The sentinel makes the registry's existing policy self-executing, packaged as a GitHub Action. It probes every declared
endpoint, tracks failures across runs, and — when the evidence is consistent failure over days, not a one-off blip —
proposes the removal the policy already authorizes, as a pull request with the evidence attached and a clear way for
maintainers to reject it.

The goal is not to replace human judgment. Every proposed change goes through a normal PR that a maintainer approves or
closes. The sentinel just does the tedious part: watching endpoints, counting failures, and writing up findings.

## What it checks

- **Endpoint liveness** — probes every declared RPC, REST, gRPC, gRPC-web, WSS, and EVM JSON-RPC endpoint on Cosmos
  chains (`eip155` chains get their EVM JSON-RPC endpoints). Failures are tracked across runs; an endpoint is only
  flagged after `min-failures` consecutive failing runs, and re-probed once more before a removal PR is opened.
- **Chain liveness** — sometimes the dead thing is the chain, not its endpoints. When a chain looks dead for
  `chain-death-min-runs` consecutive runs (either of two signatures, abandoned or halted — see
  [Dead-chain detection](#dead-chain-detection)), the sentinel proposes a one-line status flip (`live` → `killed`)
  instead of gutting the endpoint arrays: a single edit that marks the chain dead in the data every downstream
  consumer reads.
- **Chain ID** — endpoints that answer with a different chain ID than the registry declares are reported. A small,
  shifting set — around 1% of live endpoints on any given run, and it fluctuates because gateways rotate what sits
  behind a hostname — most of them serving *a different chain entirely*; the automated remedy is on the
  [roadmap](#roadmap).
- **IBC denom hash integrity** — recomputes each IBC asset's `ibc/<HASH>` denom from its declared trace path and opens a
  fix PR on any mismatch. Deterministic and local — no failure streak needed.

**See it in action**: the sentinel runs on a live chain-registry fork at
[**ny4rl4th0t3p/chain-registry**](https://github.com/ny4rl4th0t3p/chain-registry) — scheduled runs, persistent state on
a branch, and every kind of PR it opens, browsable under
[open pull requests](https://github.com/ny4rl4th0t3p/chain-registry/pulls). Two exhibits: an
[endpoint-removal PR](https://github.com/ny4rl4th0t3p/chain-registry/pull/70) whose per-endpoint evidence table spans
three distinct failure stories (a certificate now answering for an unrelated domain, a live gateway with no backends,
and an operator's own "unsupported platform" refusals), and a
[status-flip PR](https://github.com/ny4rl4th0t3p/chain-registry/pull/72) marking a dead chain killed in one line —
streak, per-check failure summary, the operators that withdrew with their live counts elsewhere, and copy-paste
verification commands. Everything in those PR bodies was machine-gathered. (When a status-flip PR supersedes an open
fix PR, the sentinel cross-links them with a comment instead of closing anything — see
[Dead-chain detection](#dead-chain-detection).)

## The verifiability rule

One policy position underlies every liveness verdict, and it is deliberate:

> **An endpoint earns "live" by answering its protocol's standard public query without credentials. One that cannot
> be verified without credentials is treated as dead — regardless of what may still work behind payment plans or
> allowlists.**

The registry is a public claim, and a claim only a paying customer can verify is not one the registry's consumers can
rely on. The sentinel does go out of its way to let restricted endpoints demonstrate themselves: when a gRPC endpoint
refuses its standard node-info method by name (`method_restricted`, observed on a major multi-chain provider's
gateways), it retries with a harmless parameterless query, and an answer counts as live. But when no credential-free
request gets an answer, "it partially works" is not a liveness defence — it is the documented policy working as
intended. The disagreement mechanism is the same as everywhere else: close the PR.

One honest boundary: liveness verifies that the endpoint answers its protocol correctly, not that it answers *for the
declared chain* — that is the separate chain ID check, currently report-only (see the roadmap). The exact request each
probe sends and what counts as a pass are specified in [PROBES.md](PROBES.md).

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

      - uses: ny4rl4th0t3p/chain-registry-sentinel@v0.8.3
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
      - uses: ny4rl4th0t3p/chain-registry-sentinel@v0.8.3
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

      - uses: ny4rl4th0t3p/chain-registry-sentinel@v0.8.3
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
| `timeout`                 | `60s`    | Per-endpoint probe timeout (Go duration syntax: `30s`, `1m`).                                    |
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

**`pr-cooldown-days`** — measured from when the last sentinel PR for that chain was opened, and tracked in three
separate per-chain buckets: endpoint-removal, hash-fix, and status-flip PRs each keep their own timestamp. In practice
this governs how soon a *closed* (rejected) PR may be re-proposed — an open PR already blocks duplicates on its own
(see PR behaviour below).

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
- Branch names follow the pattern `sentinel/{chain}-{N}` for endpoint removals, `sentinel/{chain}-hash-{N}` for hash
  fixes, and `sentinel/{chain}-status-{N}` for status flips, where N increments each time. Branches are never deleted
  by the sentinel.
- PRs are labelled `sentinel` and `automated` (both created automatically if missing).

### Dead-chain detection

Sometimes the dead thing is the chain, not its endpoints — and then the right fix is flipping the chain's `status` from
`live` to `killed` in `chain.json` (one line, schema-supported, visible to every downstream consumer at once) rather
than deleting dozens of endpoints one by one.

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
run (default 3). A chain that looks dead — from the very first run of its streak, not just once it matures — is
excluded from endpoint-removal and hash-fix PRs: no point grooming a chain that may be about to be marked killed — and
for an abandoned chain, an endpoint PR would remove every core endpoint anyway. If a fix PR was already open before the chain started
dying (the common gradual-decay path), the sentinel comments on it when the status-flip PR opens, noting it is
superseded and can be closed once the status PR merges. It only ever comments — the sentinel never closes PRs, so it
cannot collide with a maintainer's own process.

What merging or closing does:

- **Merging** closes the loop: chains whose `status` is not `live` are skipped entirely, so probing — and any further
  PRs — stop with the merge.
- **Closing** works exactly as the PR body states: the death streak resets on any run where the chain looks alive again
  (a live core endpoint, or a fresh block time), and a fresh PR then needs the full streak from zero. While the chain
  *still* looks dead, a closed PR is re-proposed only after `pr-cooldown-days` — status-flip PRs keep their own
  per-chain cooldown bucket, separate from endpoint and hash PRs, all governed by the same input.
- Status-flip PRs draw from their own `max-status-prs` pool and never count against `max-endpoint-prs`.

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
      - uses: ny4rl4th0t3p/chain-registry-sentinel@v0.8.3
        with:
          registry: .
          chains: cosmoshub,osmosis,juno
          state-path: .sentinel-state
          github-token: ${{ secrets.GITHUB_TOKEN }}
```

### Dry-run (no writes, no PRs)

```yaml
      - uses: ny4rl4th0t3p/chain-registry-sentinel@v0.8.3
        with:
          registry: .
          state-path: .sentinel-state
          dry-run: true
```

---

## Running locally

### Try it without installing anything

The action's image is published to GHCR, and the binary inside it runs standalone — a registry clone and Docker are
the only requirements. The image's default entrypoint is the Action wrapper (it expects a GitHub workspace), so point
the entrypoint at the binary directly:

```bash
git clone --depth 1 https://github.com/cosmos/chain-registry

# probe two chains, report to stdout, touch nothing — no state, no PRs, no writes
docker run --rm --entrypoint /sentinel \
  -v "$PWD/chain-registry:/registry:ro" \
  ghcr.io/ny4rl4th0t3p/chain-registry-sentinel:v0.8.3 \
  --registry /registry --chains cosmoshub,osmosis
```

The registry mounts read-only on purpose: without `--state-path` nothing is ever flagged and no PRs of any kind can
open, so this is a pure measurement. To keep the run's records for later analysis, add a writable mount:
`-v "$PWD/records:/records"` plus `--report /records --vantage local`.

### Building from source

```bash
# build (stamps the version from git describe; plain `go build -o sentinel ./cmd/sentinel/` works too)
make build

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

### Failure classes and evidence weight

Every failure is classified from the live error value (not string-matched after the fact), and the classes are not
equal as evidence. Kill PRs aggregate their evidence by class and removal PRs quote each endpoint's raw evidence, so
it matters what each class implies — strongest first:

- **`not_served_by_provider`** — a *serving* provider answers with an explicit "unsupported platform" body for this
  chain, distinct from its 404 for names it never heard of. This is an affirmative retirement statement by the
  operator — the strongest single class, and immune to vantage effects: the provider is reachable and answering, it is
  just saying no. Classification is deliberately narrow, extended only from messages actually observed in the wild.
- **`dns_nxdomain`** — the name no longer resolves. Very strong, and block-immune: an operator can firewall probes at
  the connection level, but NXDOMAIN is the DNS system's answer, not the operator's.
- **`gateway_no_backend`** — a live gateway (502/503/504) with nothing behind it for this chain. The
  infrastructure survives; the chain was silently dropped from it.
- **`conn_refused` / `net_unreachable` / TLS classes** (`tls_expired`, `tls_hostname`, …) — nothing listens, or the
  TLS layer contradicts the claim. Strong once sustained over a streak: certificates do not stay expired for weeks by
  accident.
- **Ambiguous classes** (`timeout`, `conn_reset`, `http_403`, `dns_failure`, `vantage_no_route`, …) — anything an
  aggressive or badly-networked prober could have caused itself. The report counts these separately from the
  structural set so quoted numbers never lean on them. Be precise about what they do drive, though: the failure
  streak counts a failure as a failure regardless of class, so an endpoint that answers 403 to every probe for
  `min-failures` consecutive runs is still proposed for removal — that is the verifiability rule, applied
  deliberately. Two classes (`dns_failure`, `vantage_no_route`) indict the probing machine and feed the vantage health
  warning; one (`http_429`) marks the probe as skipped, so a rate-limited endpoint accrues no streak at all and can
  never be proposed for removal.

Every record keeps the raw `evidence` string its class was derived from, so any verdict can be re-audited — or
reclassified later — without re-probing.

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

### Auditing the published numbers

Every figure quoted in this README's opening (and in the accompanying measurement report) is recomputed by
[`scripts/report-audit.sh`](scripts/report-audit.sh) from three frozen artifacts attached to the
[v0.8.3 release](https://github.com/ny4rl4th0t3p/chain-registry-sentinel/releases/tag/v0.8.3): the measurement dataset,
the registry's own `test_endpoints` log it is corroborated against, and the May-2026-state dataset behind the
historical comparison.

```bash
curl -LO https://github.com/ny4rl4th0t3p/chain-registry-sentinel/releases/download/v0.8.3/20260801T172659Z-github-runner.jsonl
curl -LO https://github.com/ny4rl4th0t3p/chain-registry-sentinel/releases/download/v0.8.3/0_test-endpoints.txt
curl -LO https://github.com/ny4rl4th0t3p/chain-registry-sentinel/releases/download/v0.8.3/20260731T080445Z-github-runner.jsonl
sh scripts/report-audit.sh 20260801T172659Z-github-runner.jsonl 0_test-endpoints.txt 20260731T080445Z-github-runner.jsonl
```

Each value prints alongside the arithmetic that produced it. POSIX `sh` + `jq` + `awk`, nothing else. A mismatch
between the script's output and any published claim is a bug in the claim — please report it.

---

## Known limitations

**One vantage.** Every verdict is measured from a single network location — on GitHub-hosted runners, that means
Azure's address space, and the runner's resolver IP is visible in the evidence strings. An endpoint that firewalls
datacenter ASNs at the connection level while serving residential users will look dead from CI; that residual risk is
real and cannot be fully closed from one vantage. What narrows it:

- **Streaks, not snapshots** — a verdict needs `min-failures` consecutive runs, plus one more re-probe at PR time.
- **NXDOMAIN is block-immune** — the heaviest dead class comes from DNS, which an endpoint-side firewall cannot fake.
- **The withdrawn-operator differential compares within one run** — "this operator is alive on 108 chains and dead on
  this one" holds regardless of where the probe stands, because both measurements share the vantage.
- **Vantage health checks** — runs whose failures look self-inflicted freeze all chain-death streaks and print a
  warning. Endpoint streaks are not frozen on such runs — they are protected by the other two layers instead: streak
  length and the PR-time re-probe.

And a parity note: the registry's own CI runs from the same GitHub-hosted vantage, so the sentinel's view is no
narrower than the status quo's — it is the same view, applied continuously instead of only at PR time.

## Roadmap

Next up:

- **Wrong-chain-ID remedy** — the chain ID check currently only reports. Measured on real data: around 1% of live
  endpoints answer for a different chain, a set that shifts run to run — most through recycled multi-chain gateways
  (an endpoint listed for one chain serving another's data), which pass every liveness check while being worse than
  dead. Planned: wrong-ID failures accrue
  streaks into the existing removal flow, with a unanimity guard — when *every* live endpoint of a chain agrees on the
  same ID that differs from the registry, the registry is what's wrong, and the fix is a one-line `chain_id`
  correction, not removals. Wrong-ID endpoints also stop counting as live for dead-chain detection, so a phantom
  gateway can no longer keep a dying chain looking alive.
- **Malformed-entry lint** — entries that fail before any probe (e.g. a hostname with a trailing space, already
  classified as `malformed_registry_address`). Deterministic and local like the hash check: no streak needed, a PR on
  the first run.
- **Second-vantage confirmation at PR time** — one confirming probe from a genuinely different network before a PR
  opens, narrowing the ASN-firewall gap described under limitations. Honest caveat: a second GitHub-hosted runner is
  *not* a second vantage (same Azure address space), so this means an external probe API with community probes on
  non-datacenter networks, or an operator-supplied proxy — an added dependency, which is why it is a roadmap item and
  not a design. Its scope is also narrow by nature: the dominant removal classes (`dns_nxdomain`,
  `not_served_by_provider`) are already vantage-immune, so a second vantage would only ever arbitrate
  connection-level failures.

Two further IBC checks are under research. Both detect problems whose automated fix is an open policy question — input
from registry maintainers is welcome before these are built:

- **IBC channel state** — verify that channels declared in `_IBC/*.json` actually report `OPEN` on both chains, with
  failure streaks since closed channels can be reopened by a relayer.
- **IBC client expiry** — a channel can report `OPEN` while its underlying light client has expired past its trusting
  period, silently breaking the connection. This check would catch that failure mode underneath the channel check.