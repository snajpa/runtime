#!/bin/sh
# perf source guard — forbid persistent stream redirection on a fd-close-anchored
# no-command `exec` line (lane C; reviewer1 2874/2876/2905; D 2894).
set -u
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
pat='^[[:space:]]*exec([[:space:]]+[0-9]+>&-)+[[:space:]]+(2>|1>|&>)[[:space:]]*/dev/null([[:space:]]|$)'
hits=$(grep -rnE "$pat" --include='*.sh' "$DIR" 2>/dev/null); rc=$?
if [ "$rc" -eq 0 ]; then
	echo "source-guard: forbidden persistent-redirect exec line(s):" >&2
	printf '%s\n' "$hits" >&2
	exit 1
fi
[ "$rc" -eq 1 ] || { echo "source-guard: scan failed (grep rc=$rc)" >&2; exit 2; }
echo "source-guard: clean"
