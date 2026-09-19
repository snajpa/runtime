#!/usr/bin/env python3
"""Run a command; print exact child accounting: rc, wall, user, sys, maxrss_kb.

Uses resource.getrusage(RUSAGE_CHILDREN).ru_maxrss - the kernel's exact peak
RSS of the waited child, no sampling gaps.
"""
import os
import resource
import subprocess
import sys
import time

if len(sys.argv) < 2:
    sys.exit("usage: measure.py CMD [ARGS...]")

t0 = time.time()
rc = subprocess.call(sys.argv[1:])
wall = time.time() - t0
ru = resource.getrusage(resource.RUSAGE_CHILDREN)
line = f"rc={rc} wall={wall:.3f} user={ru.ru_utime:.3f} sys={ru.ru_stime:.3f} maxrss_kb={ru.ru_maxrss}"
acct = os.environ.get("ACCOUNT_FILE")
if acct:
    with open(acct, "w") as f:
        f.write(line + "\n")
else:
    print(line)
