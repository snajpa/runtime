#!/bin/sh
# perf-env-valid — comparability/validity classifier for the perf-regression suite (lane A).
# Usage: sh perf-env-valid.sh <baseline-capture> <current-capture> [--traced]
# Compares two perf-env-capture.sh outputs and classifies per the R23 design:
#   validity drift (kernel/config/microcode/topology/fingerprint) => INCONCLUSIVE
#   to_execute failures (tmpfs/missing artifact fs; perf absent with --traced) => SETUP_ERROR
#   optional gaps => recorded, run proceeds.
# Emits "validity: ok|inconclusive|setup_error" + reasons; exit 0/30/40 (schema scheme).
set -u
base=${1:?usage: perf-env-valid.sh <baseline-capture> <current-capture> [--traced]}
cur=${2:?usage: perf-env-valid.sh <baseline-capture> <current-capture> [--traced]}
traced=${3:-}
val() { awk -F': ' -v k="$2" '$1==k{print $2; exit}' "$1"; }
incon=""; setup=""
for k in uname_r kernel_config_sha256 microcode cpu_topology_sha fingerprint_sha256; do
  b=$(val "$base" "$k"); c=$(val "$cur" "$k")
  [ "$b" = "$c" ] || incon="$incon$k(baseline=$b current=$c) "
done
afs=$(val "$cur" artifacts_fs)
case "$afs" in tmpfs|missing) setup="$setup""artifacts_fs=$afs ";; esac
if [ "$traced" = "--traced" ]; then
  [ "$(val "$cur" perf_version)" = absent ] && setup="$setup""perf absent (traced run) "
fi
echo "== reasons:"
[ -n "$incon" ] && echo "inconclusive: $incon"
[ -n "$setup" ] && echo "setup_error: $setup"
[ -z "$incon$setup" ] && echo "ok: captures agree on validity-critical fields"
if [ -n "$setup" ]; then echo "validity: setup_error"; exit 40; fi
if [ -n "$incon" ]; then echo "validity: inconclusive"; exit 30; fi
echo "validity: ok"; exit 0
