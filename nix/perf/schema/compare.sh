#!/bin/sh
# perf/2 comparator (lane F): calibrate | compare | replay | verify.
# Fixed bands (schema/bands.json); five outcomes/exits 0/10/20/30/40.
# See compare.py for the semantics.
set -eu
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec python3 "$DIR/compare.py" "$@"
