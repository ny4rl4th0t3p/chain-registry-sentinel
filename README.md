# chain-registry-sentinel

`cosmos/chain-registry` is the source of truth for RPC endpoints, IBC channels, asset metadata, and chain configuration
across the Cosmos ecosystem. It is community-maintained and slowly decays — RPCs go offline, channels close... Nobody is
continuously checking whether what's listed actually works.

This project does that check automatically. It reads the registry, queries each chain directly, and reports what no
longer matches reality. When the evidence is strong enough — consistent failures over days, not a one-off blip — it
proposes a correction through a pull request, with the evidence attached and a clear way for maintainers to reject it.

The goal is not to replace human judgment. Every proposed change goes through a normal PR that a maintainer approves or
closes. The sentinel just does the tedious part: watching endpoints, counting failures, and writing up findings.

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

      - uses: YOUR_ORG/chain-registry-sentinel@main
        with:
          registry: .
          state-branch: sentinel-state
          github-token: ${{ secrets.GITHUB_TOKEN }}
```

The action restores state from the `sentinel-state` branch before probing and pushes the
updated state back after. The branch is created automatically on the first run.

### Persisting state

**`state-branch` (recommended).** Pass a branch name and the action handles everything:
restore before probing, push after. State is kept on a dedicated branch isolated from the
default branch and its protection rules — the same pattern `gh-pages` uses. It has a full
git history and never expires.

```yaml
      - uses: YOUR_ORG/chain-registry-sentinel@main
        with:
          registry: .
          state-branch: sentinel-state
          github-token: ${{ secrets.GITHUB_TOKEN }}
```

**`state-path` with `actions/cache` (simpler, less reliable).** Manage persistence
yourself with a cache step. The entire state directory is saved and restored as one entry.
It expires if the workflow has not run for 7 days — at which point all accumulated
streaks are lost. Only use this when you cannot write to the repo.

```yaml
      - uses: actions/cache@v4
        with:
          path: .sentinel-state
          key: sentinel-state-${{ github.run_id }}
          restore-keys: sentinel-state-

      - uses: YOUR_ORG/chain-registry-sentinel@main
        with:
          registry: .
          state-path: .sentinel-state
          github-token: ${{ secrets.GITHUB_TOKEN }}
```

### All inputs

| Input              | Default        | Description                                                                    |
|--------------------|----------------|--------------------------------------------------------------------------------|
| `registry`         | `.`            | Path to a local chain-registry clone.                                          |
| `chains`           | `all`          | Comma-separated chain names to check, or `all`.                                |
| `timeout`          | `30s`          | HTTP/gRPC timeout per request.                                                 |
| `concurrency`      | `250`          | Maximum simultaneous endpoint probes.                                          |
| `state-path`       | _(none)_       | Directory for per-chain state files. Use when managing persistence externally. |
| `state-branch`     | _(none)_       | Branch to persist state automatically (created if missing).                    |
| `min-failures`     | `14`           | Consecutive failures before flagging an endpoint.                              |
| `dry-run`          | `false`        | Read state but do not write files or open PRs.                                 |
| `github-token`     | _(required)_   | Token with `contents: write` and `pull-requests: write`.                       |
| `max-new-prs`      | `5`            | Maximum PRs to open per run (hard ceiling: 5).                                 |
| `pr-cooldown-days` | `7`            | Minimum days between PRs for the same chain.                                   |

`GITHUB_REPOSITORY` is injected automatically by the Actions runner — no extra input needed.

`state-branch` and `state-path` can coexist: `state-branch` drives persistence while
`state-path` points the binary at the working directory. If only `state-branch` is set,
the action defaults `state-path` to `.sentinel-state` internally.

### PR behaviour

- The sentinel searches for an existing open PR with the `sentinel` label before opening a new one. If one is already
  open for a chain, it skips that chain.
- Before opening a PR, the sentinel re-probes all flagged endpoints. If any have recovered, their failure streak is
  reset, and they are excluded from the PR. If all recover, no PR is opened.
- Branch names follow the pattern `sentinel/{chain}-{N}` where N increments each time. Branches are never deleted by
  the sentinel.
- PRs are labelled `sentinel` and `automated` (both created automatically if missing).

### Checking a subset of chains

```yaml
      - uses: YOUR_ORG/chain-registry-sentinel@main
        with:
          registry: .
          chains: cosmoshub,osmosis,juno
          state-path: .sentinel-state
          github-token: ${{ secrets.GITHUB_TOKEN }}
```

### Dry-run (no writes, no PRs)

```yaml
      - uses: YOUR_ORG/chain-registry-sentinel@main
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

Exit code is `0` when all endpoints are live; `1` when any are dead or have chain ID mismatches.