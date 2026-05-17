#!/bin/sh
set -e
git clone --depth=1 "$REGISTRY_URL" /tmp/registry
exec /sentinel --registry /tmp/registry "$@"