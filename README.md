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

| Input              | Default  | Description                                                                                      |
|--------------------|----------|--------------------------------------------------------------------------------------------------|
| `registry`         | `.`      | Path to a local chain-registry clone, relative to the workspace.                                 |
| `chains`           | `all`    | Comma-separated chain names to check, or `all`.                                                  |
| `timeout`          | `30s`    | Per-request probe timeout (Go duration syntax: `30s`, `1m`).                                     |
| `concurrency`      | `250`    | Maximum simultaneous endpoint probes.                                                            |
| `state-path`       | _(none)_ | Directory for per-chain state files. Use when managing persistence externally.                   |
| `reset-state`      | `false`  | Start unreadable state files from scratch instead of failing the step.                           |
| `state-branch`     | _(none)_ | Branch to persist state automatically (created on first push).                                   |
| `min-failures`     | `14`     | Consecutive failing **runs** before an endpoint is flagged.                                      |
| `dry-run`          | `false`  | Report and show would-be PRs, but write nothing and open nothing.                                |
| `github-token`     | _(none)_ | Token with `contents: write` and `pull-requests: write`. Needed for PRs and `state-branch`.      |
| `max-new-prs`      | `5`      | Maximum **new endpoint-removal PRs** opened per run. Does not apply to hash PRs.                 |
| `pr-cooldown-days` | `7`      | Minimum days since the last PR was **opened** for a chain before another may open. `0` disables. |
| `verbose`          | `false`  | Enable debug logging to stderr.                                                                  |

### Input details

**`registry`** — the checkout the sentinel reads. In the usual setup (`actions/checkout` of a chain-registry fork,
sentinel step after it), this is `.`.

**`chains`** — names must match registry directory names. Chains whose `status` is not `live`
are skipped. Names not found in the registry are warned about at the end of the run. The filter applies to every check,
including the IBC denom hash check.

**`concurrency`** — applies to endpoint probes only; hash checks are local and run synchronously after probing.

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

**`max-new-prs`** — a per-run ceiling on newly opened endpoint-removal PRs; chains beyond it wait for a later run. It
does **not** bound the total number of simultaneously open PRs, and hash-fix PRs are not counted against it.

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

## Roadmap

Two further IBC checks are under research. Both detect problems that have no unambiguous automated fix, so what a
finding should trigger (a PR removing the entry, an issue, or a report) is an open policy question — input from registry
maintainers is welcome before these are built.

- **IBC channel state** — verify that channels declared in `_IBC/*.json` actually report `OPEN` on both chains, with
  failure streaks since closed channels can be reopened by a relayer.
- **IBC client expiry** — a channel can report `OPEN` while its underlying light client has expired past its trusting
  period, silently breaking the connection. This check would catch that failure mode underneath the channel check.