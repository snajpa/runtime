#!/bin/sh
# w1 device-lock stderr regression (lane C; reviewer1 2922). On the landed
# fixed `w1.sh`, `device_lock_release` must leave stderr visible (release line
# + a post-call marker); on a synthetic buggy copy generated OUT-OF-TREE the
# captured stderr must be EMPTY. Exit 0 = both hold; 1 = regression failure;
# 2 = harness/setup error. Run from `cmd_selftest` or directly.
set -u
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
W1=${1:-$DIR/w1.sh}
[ -f "$W1" ] || { echo "w1-lock-regression: no w1.sh at $W1" >&2; exit 2; }
TMP=$(mktemp -d "${TMPDIR:-/tmp}/w1-lock-regression.XXXXXX") || { echo "w1-lock-regression: mktemp failed" >&2; exit 2; }
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

capture() { # $1 = file to extract device_lock_release from; prints captured stderr
	sh -c '
		set -eu
		W1X=$1; LK=$2
		say() { echo "+ $*" >&2; }
		fn=$(sed -n "/^device_lock_release()/,/^}/p" "$W1X")
		[ -n "$fn" ] || { echo "cannot extract device_lock_release" >&2; exit 2; }
		exec 9>"$LK"
		flock -x 9
		DEVICE_LOCK_HELD=1
		eval "$fn"
		device_lock_release
		echo "stderr-visible-marker" >&2
	' sh "$1" "$TMP/lock" 2>&1 >/dev/null
}

# the fixed line must be present (the W1 stderr fix landed)
if ! grep -q 'exec 9>&- || true' "$W1"; then
	echo "w1-lock-regression: fixed line 'exec 9>&- || true' not found in $W1 — is the W1 stderr fix landed?" >&2
	exit 1
fi

# A) landed fixed w1.sh -> release line AND marker visible on stderr
OUT_A=$(capture "$W1")
VISIBLE=0
if printf '%s' "$OUT_A" | grep -q 'device window released' && \
   printf '%s' "$OUT_A" | grep -q 'stderr-visible-marker'; then
	VISIBLE=1
fi

# B) synthetic buggy copy, generated out-of-tree -> captured stderr must be empty
sed 's#exec 9>&- || true#exec 9>\&- 2>/dev/null || true#' "$W1" >"$TMP/w1-buggy.sh"
if ! grep -q 'exec 9>&- 2>/dev/null || true' "$TMP/w1-buggy.sh"; then
	echo "w1-lock-regression: buggy transform did not produce the buggy line" >&2
	exit 2
fi
OUT_B=$(capture "$TMP/w1-buggy.sh")
EMPTY=0
[ -z "$OUT_B" ] && EMPTY=1

if [ "$VISIBLE" = 1 ] && [ "$EMPTY" = 1 ]; then
	echo "w1-lock-regression: ok (fixed: release+marker visible; synthetic buggy: stderr empty)"
	exit 0
fi
echo "w1-lock-regression: FAILED (fixed-visible=$VISIBLE buggy-empty=$EMPTY)" >&2
[ -n "$OUT_A" ] && printf '  fixed capture:\n%s\n' "$OUT_A" >&2
printf '  buggy capture: [%s]\n' "$OUT_B" >&2
exit 1
