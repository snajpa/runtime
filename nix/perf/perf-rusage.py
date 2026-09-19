#!/usr/bin/env python3
"""perf-rusage — run a unit, retain stdout/stderr, emit a perf/2 `note` body
carrying the unit summary. Exact child peak RSS via
getrusage(RUSAGE_CHILDREN).ru_maxrss (no sampling); wall/user/sys also recorded.

Usage: perf-rusage.py <logbase> -- <cmd...>
"""

import json
import resource
import subprocess
import sys
import time


def main() -> int:
    if len(sys.argv) < 4 or sys.argv[2] != "--":
        print("usage: perf-rusage.py <logbase> -- <cmd...>", file=sys.stderr)
        return 2
    base, cmd = sys.argv[1], sys.argv[3:]
    with open(base + ".stdout", "wb") as out, open(base + ".stderr", "wb") as err:
        t0 = time.monotonic()
        rc = subprocess.call(cmd, stdout=out, stderr=err)
        wall = time.monotonic() - t0
    ru = resource.getrusage(resource.RUSAGE_CHILDREN)
    print(
        json.dumps(
            {
                "record": "note",
                "text": "unit-summary",
                "kind": "unit-summary",
                "unit_id": base.rsplit("/", 1)[-1],
                "cmd": " ".join(cmd),
                "rc": rc,
                "wall_s": round(wall, 3),
                "user_s": round(ru.ru_utime, 3),
                "sys_s": round(ru.ru_stime, 3),
                "peak_rss_kib": ru.ru_maxrss,
            },
            separators=(",", ":"),
        )
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
