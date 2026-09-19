#!/bin/sh
# genstore — W4 fixture generator wrapper (lane B).
#
# Builds the generator from the product module and runs it:
#   genstore.sh --class tiny|latency|large --out <dir> [--ids <file>]
#
# The store tree + ids file are the fixture; the ids file is the correctness
# oracle's reference. Persistent LV only (never tmpfs) — see fixtures.md.
set -eu

HERE=$(cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(cd -- "$HERE/../../.." && pwd)
BIN_DIR=${PERF_GENSTORE_BIN_DIR:-"$HOME/ai/logs/e2b-perf/.cache/bin"}
BIN="$BIN_DIR/perf-genstore"

mkdir -p "$BIN_DIR"

(cd "$ROOT" && go build -o "$BIN" ./packages/orchestrator/cmd/perf-genstore)

exec "$BIN" "$@"
