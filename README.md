# chain-registry-sentinel

`cosmos/chain-registry` is the source of truth for RPC endpoints, IBC channels, asset metadata, and chain configuration
across the Cosmos ecosystem. It is community-maintained and slowly decays — RPCs go offline, channels close... Nobody is
continuously checking whether what's listed actually works.

This project does that check automatically. It reads the registry, queries each chain directly, and reports what no
longer matches reality. When the evidence is strong enough — consistent failures over days, not a one-off blip — it
proposes a correction through a pull request, with the evidence attached and a clear way for maintainers to reject it.

The goal is not to replace human judgment. Every proposed change goes through a normal PR that a maintainer approves or
closes. The sentinel just does the tedious part: watching endpoints, counting failures, and writing up findings.