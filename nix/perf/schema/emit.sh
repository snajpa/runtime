#!/bin/sh
# perf/2 T1 emitter (lane F) — the only writer to perf-runs.jsonl.
# Usage: emit.sh append --stream <path> --kind <K> --phase <p> --mode <m>
#        (body JSON on stdin). See emit.py for the contract.
set -eu
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec python3 "$DIR/emit.py" "$@"
