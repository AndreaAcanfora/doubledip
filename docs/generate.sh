#!/bin/sh
# Regenerates docs/runs.js from a real run of doubledip against the
# example shop. Needs Go, curl and a local Postgres:
#   SHOP_DB=postgres://postgres@localhost:5432/shop docs/generate.sh
set -eu
cd "$(dirname "$0")/.."
: "${SHOP_DB:?set SHOP_DB to a postgres:// URL of a scratch database}"
go build -o /tmp/doubledip-docs ./cmd/doubledip
set +e
/tmp/doubledip-docs --config examples/shop/doubledip.yml --json > /tmp/doubledip-runs.json
code=$?
set -e
[ "$code" = 1 ] || { echo "expected races to be confirmed (exit 1), got $code" >&2; exit 1; }
{ printf 'window.RUNS = '; cat /tmp/doubledip-runs.json; printf ';\n'; } > docs/runs.js
echo "wrote docs/runs.js"
