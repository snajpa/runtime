#!/bin/sh
# Render the rehearsal's JSONL results (stdin) as a Markdown table.
#   render.sh [run-id]
set -eu

RUN=${1:-run}

printf '### S3 rehearsal: %s\n\n' "$RUN"
printf '| phase | version | outcome | objects | bytes | s | detail |\n'
printf '|-------|---------|---------|--------:|------:|--:|--------|\n'

# The JSONL results arrive on stdin, so the program cannot also come from a
# stdin heredoc: hand the program in on fd 3 and leave stdin to the data.
python3 /dev/fd/3 3<<'PY'
import json
import sys

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue

    try:
        r = json.loads(line)
    except json.JSONDecodeError:
        print("| parse | | error | | | | %s |" % line.replace("|", "\\|")[:200])
        continue

    def cell(value):
        return str(value).replace("|", "\\|") if value is not None else ""

    print("| %s | %s | %s | %s | %s | %.2f | %s |" % (
        cell(r.get("phase")),
        cell(r.get("version")),
        cell(r.get("outcome")),
        cell(r.get("objects", "")),
        cell(r.get("bytes", "")),
        float(r.get("seconds") or 0),
        cell(r.get("detail"))[:200],
    ))
PY
