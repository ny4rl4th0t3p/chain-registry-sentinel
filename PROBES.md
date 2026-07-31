# Probes: exact requests and pass conditions

This document specifies what the sentinel actually sends to an endpoint and what counts as a pass, per endpoint type. It
exists so a maintainer reviewing a sentinel PR can reproduce any verdict by hand — every probe below is a plain request
that `curl`, `grpcurl`, or `wscat` can replay.

Equally important is what the sentinel never does: no ICMP ping, no port scans, no crawling, no retries hammering a
host. One probe per declared endpoint per run (the exceptions are listed under gRPC below, plus the single re-probe of
flagged endpoints before a PR opens), at most `concurrency` probes in flight across the whole run.

The unit under test is always the individual endpoint entry, never the chain: each entry is an independent claim ("this
URL serves this chain") by a specific operator, so every check below — including the chain ID comparison — runs once per
endpoint. That is what makes a single wrong entry detectable while its siblings answer correctly.

## Shared mechanics

- **Timeout** — each endpoint gets one `timeout` budget (default 60s) covering its whole probe, including the gRPC
  dual-transport attempt described below.
- **Identification** — every request carries the User-Agent `chain-registry-sentinel/<version>
  (+https://github.com/ny4rl4th0t3p/chain-registry-sentinel)` unless overridden, including the gRPC and WSS probes.
- **Redirects** — HTTP redirects are followed (Go's default policy, at most 10 hops).
- **Rate limiting** — an HTTP 429 (anywhere, including a gRPC status message carrying "429") marks the probe *skipped*:
  it counts in the report as unmeasured, accrues no failure streak, and can never drive a removal PR.
- **Evidence** — on a non-200 response, the first 512 bytes of the body are retained (flattened to one line) and travel
  into state, records, and PR bodies. Failure classes are derived from the live Go error chain (DNS, TLS, syscall
  types), not from string matching.
- **DNS** — there is no separate DNS probe. Resolution happens inside each dial, and DNS outcomes (NXDOMAIN, SERVFAIL)
  are classified from the dial error.

## RPC (CometBFT)

```
GET {address}/status
```

**Pass:** HTTP 200 and the body parses as JSON. The declared address is used as-is (one trailing-slash trim); both
response shapes seen in the wild are accepted — `result.node_info` (standard) and top-level `node_info` (some gateways).

**Extracted, never affecting the verdict:** `sync_info.catching_up` and `node_info.other.tx_index` (node quality,
report-only) and `sync_info.latest_block_time` (feeds the dead-chain *halted* signature — see the README — but never
this endpoint's own pass/fail or streak).

**Chain ID check:** `node_info.network` compared byte-for-byte to chain.json's `chain_id`. Skipped when the probe failed
(liveness already reported that).

Try it (endpoint verified live from a GitHub-hosted runner, 2026-07-31 — examples rot, the shape doesn't):

```bash
curl -s https://celestia-rpc.polkachu.com/status | jq .result.node_info.network
# "celestia"
```

## REST (Cosmos SDK API)

```
GET {address}/cosmos/base/tendermint/v1beta1/node_info
```

**Pass:** HTTP 200 and the body parses as JSON.

**Chain ID check:** `default_node_info.network` versus `chain_id`, byte-for-byte. Skipped when the probe failed.

Try it:

```bash
curl -s https://api.celestia.nodestake.org/cosmos/base/tendermint/v1beta1/node_info | jq .default_node_info.network
# "celestia"
```

## gRPC

One unary call, raw protobuf over HTTP/2:

```
/cosmos.base.tendermint.v1beta1.Service/GetNodeInfo   (empty request)
```

**Pass:** the call returns, and `default_node_info.network` decodes from the response (protobuf field 1, then field 4).

**Transport selection** — chain.json gRPC addresses carry no scheme most of the time, so the dial mode follows these
rules, in order:

| Address form          | Attempt                       |
|-----------------------|-------------------------------|
| `https://host[:port]` | TLS only (operator stated it) |
| `http://host[:port]`  | plaintext only                |
| `host` (no port)      | TLS to :443                   |
| `host:443`            | TLS only                      |
| `host:<other>`        | **TLS first, then plaintext** |

The last rule is measured, not guessed: operators terminate TLS on nonstandard ports (:9090, :2083) as readily as they
serve plaintext there. TLS is tried first because the failure costs are asymmetric — TLS against a plaintext server
fails in one round trip, plaintext against a TLS server silently burns the whole timeout. The second mode is attempted
only after a transport-level failure; an application-level answer ends the probe, because the channel worked and that
answer *is* the endpoint's response.

**Fallback for restricted gateways** — if the gateway speaks well-formed gRPC but refuses `GetNodeInfo` at the
application level (not a transport failure, not an HTML error page), one more method is tried:

```
/cosmos.bank.v1beta1.Query/TotalSupply   (empty request)
```

An answer means the endpoint is live but its gateway blocklists the standard method by name — recorded as
`method_restricted` (observed on a major multi-chain provider). The chain ID check is then skipped, since no method that
answers can state the network name.

Try it (`grpcurl` uses TLS by default; it needs server reflection, which most Cosmos nodes enable — the sentinel itself
does not, since it decodes the raw response):

```bash
grpcurl celestia-mainnet-grpc.itrocket.net:443 cosmos.base.tendermint.v1beta1.Service/GetNodeInfo | jq .defaultNodeInfo.network
# "celestia"
```

## gRPC-web

```
POST {address}/cosmos.base.tendermint.v1beta1.Service/GetNodeInfo
Content-Type: application/grpc-web+proto
Accept: application/grpc-web+proto
X-Grpc-Web: 1

body: 00 00 00 00 00   (empty data frame: 1 flag byte + 4-byte length)
```

**Pass:** HTTP 200, the first response frame is a data frame (not trailers), and `default_node_info.network` decodes
from it — same protobuf path as native gRPC.

There is no live example to point this at: the registry lists exactly three gRPC-web endpoints, and all three fail —
usefully, each in a different way, so they double as worked examples of the failure classes (verdicts from the
2026-07-31 runner measurement):

| Entry                                     | Chain         | Failure class                                                                                    |
|-------------------------------------------|---------------|--------------------------------------------------------------------------------------------------|
| `axelar-grpcweb.chainode.tech`            | axelar        | `malformed_registry_address` (no scheme — the sentinel does not guess one for a URL-based probe) |
| `https://migaloo-grpc-web.publicnode.com` | migaloo       | `not_served_by_provider` (403 "unsupported platform")                                            |
| `https://grpc.mainnet.secretsaturn.net`   | secretnetwork | `dns_nxdomain`                                                                                   |

The request shape, against a `{host}` of your choice:

```bash
printf '\x00\x00\x00\x00\x00' | curl -s -X POST \
  -H 'Content-Type: application/grpc-web+proto' -H 'X-Grpc-Web: 1' \
  --data-binary @- https://{host}/cosmos.base.tendermint.v1beta1.Service/GetNodeInfo | strings | head
# the network name is visible in the binary frame's strings
```

## EVM JSON-RPC

```
POST {address}
Content-Type: application/json

{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}
```

**Pass:** HTTP 200, the body parses as JSON with no `error` member, and `result` parses as a hex integer.

**Chain ID check:** the parsed integer versus chain.json's `chain_id` read as decimal. Only meaningful on `eip155`
chains; skipped otherwise, and skipped when the declared `chain_id` is not a decimal number.

Coverage: probed on every chain that declares an `apis."evm-http-jsonrpc"` list — the 46 Cosmos-type EVM-compatible
chains at the 2026-05-15 registry snapshot (138 endpoints; 43 answered on the 2026-07-31 validation run), plus any
`eip155` chain. The registry's only `eip155` entries (ethereum, rootstock) live under `_non-cosmos/`, which the
sentinel skips along with every underscore-prefixed directory — those stay out of scope deliberately.

Try it (endpoint verified live on the 2026-07-31 validation run — Cronos's first-party EVM endpoint):

```bash
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}' https://evm.cronos.org/
# {"id":1,"result":"0x19","jsonrpc":"2.0"}
```

## WSS

WebSocket dial of the declared address, then one CometBFT status request over the socket:

```
{"jsonrpc":"2.0","method":"status","params":{},"id":1}
```

**Pass:** the upgrade succeeds, the request is written, one message is read back, and `result.node_info.network` in it
is non-empty. Note this bar is deliberately higher than the HTTP checks — a server that accepts the upgrade but does not
speak the CometBFT protocol fails. A rejected upgrade still carries an HTTP response, so status codes, bodies, and 429
handling work as in the HTTP probes.

**Chain ID check:** that same `network` value versus `chain_id`. Skipped when the probe failed.

Try it (`npx wscat`):

```bash
wscat -c wss://rpc.nibiru.fi/websocket
> {"jsonrpc":"2.0","method":"status","params":{},"id":1}
# response carries result.node_info.network = "cataclysm-1", nibiru's declared chain_id
```

## IBC denom hash (no network)

Local recomputation over `assetlist.json`: for each asset with an `ibc/` base and a declared trace path,

```
expected = "ibc/" + uppercase(hex(SHA256(path)))
```

compared to the `base` field. For multi-hop assets the last trace's `chain.path` already carries the accumulated path
and is hashed as-is. Assets with an `ibc/` base but no trace path cannot be verified and are skipped.

Try it — ATOM on Osmosis, the best-known IBC denom in the ecosystem:

```bash
printf 'transfer/channel-0/uatom' | sha256sum | tr a-f A-F
# 27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2
# matching osmosis/assetlist.json: ibc/27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2
```