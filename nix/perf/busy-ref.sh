#!/bin/sh
# Lane E — busy-class reference load (R23). Pinned fio + stress-ng; emits the
# load identity (recipe/params_digest) and its signature/envelope JSON for
# `--env busy` runs. Envelope miss => exit 3: the caller records INCONCLUSIVE,
# never retries or discards. Load definition only — verdict bands come
# exclusively from the fixed A/A resolution.
#
# Usage:
#   busy-ref.sh run   [--seconds N] [--rate R] [--io-cpus S] [--cpu-cpus S] [--out F]
#   busy-ref.sh start [--rate R] [--io-cpus S] [--cpu-cpus S] [--out F] [--state F]
#   busy-ref.sh stop  [--state F]
# Env: BUSY_REF_DIR (scratch; default ${TMPDIR:-/tmp}/e2b-busy-ref),
#      BUSY_REF_STATE (session state; default <dir>/busy-ref.state)
set -u
# never keep an inherited suite-run lock fd open in daemons (lane E 2273)
exec 9>&- || true
CMD=run; [ $# -gt 0 ] && { CMD=$1; shift; }
case "$CMD" in run|start|stop|derive|settle-check) ;; *) echo "busy-ref: unknown subcommand: $CMD" >&2; exit 2 ;; esac

SECS=30; R=2000; IO_CPUS=0-1; CPU_CPUS=8-15; CPU_WORKERS=8; LOAD_FLOOR=0; ADAPT_MULT=0; ADAPT_FLOOR_FRAC=auto; SETTLE_S=180; SMOKE_S=240; OUT=""; STATE=""
while [ $# -gt 0 ]; do case "$1" in
  --seconds) SECS=$2; shift 2 ;; --rate) R=$2; shift 2 ;;
  --io-cpus) IO_CPUS=$2; shift 2 ;; --cpu-cpus) CPU_CPUS=$2; shift 2 ;;
  --cpu-workers) CPU_WORKERS=$2; shift 2 ;; --load-floor) LOAD_FLOOR=$2; shift 2 ;;
  --adaptive-mult) ADAPT_MULT=$2; shift 2 ;; --adaptive-floor-frac) ADAPT_FLOOR_FRAC=$2; shift 2 ;;
  --out) OUT=$2; shift 2 ;; --state) STATE=$2; shift 2 ;;
  *) echo "busy-ref: unknown arg: $1" >&2; exit 2 ;;
esac; done

BUSY_REF_DIR=${BUSY_REF_DIR:-${TMPDIR:-/tmp}/e2b-busy-ref}
mkdir -p "$BUSY_REF_DIR"
STATE=${STATE:-${BUSY_REF_STATE:-$BUSY_REF_DIR/busy-ref.state}}
OUT=${OUT:-$BUSY_REF_DIR/busy-ref.json}

# machine-adaptive class derivation — FROZEN spec (reviewer1 2744); host discovery only
cpuset() { # canon LIST | count LIST | first LIST K | inter A B
  awk -v mode="$1" -v a="${2:-}" -v b="${3:-}" -v k="${4:-0}" '
  function load(s,set,  n,i,j,r,part){ n=split(s,part,","); for(i=1;i<=n;i++){
      if(part[i] ~ /^[0-9]+-[0-9]+$/){ split(part[i],r,"-"); for(j=r[1]+0;j<=r[2]+0;j++) set[j]=1 }
      else if(part[i] ~ /^[0-9]+$/) set[part[i]+0]=1 } }
  function emit(set,  j,first,out){ first=-1; out=0;
      for(j=0;j<=8192;j++){ if(j in set){ if(first<0) first=j }
        else if(first>=0){ printf "%s%d-%d", (out++?",":""), first, j-1; first=-1 } }
      if(first>=0) printf "%s%d-%d", (out++?",":""), first, 8192 }
  function ranges(arr,c,  i,first,prev,out){ first=-1; out=0;
      for(i=0;i<c;i++){ if(first<0) first=arr[i]; prev=arr[i];
        if(i+1==c || arr[i+1]!=prev+1){ printf "%s%d-%d", (out++?",":""), first, prev; first=-1 } } }
  BEGIN{
    if(mode=="canon"){ load(a,S); emit(S); print "" }
    else if(mode=="count"){ load(a,S); c=0; for(j in S) c++; print c }
    else if(mode=="first"){ load(a,S); c=0; for(j=0;j<=8192;j++){ if(j in S){ arr[c++]=j; if(c>=k) break } } ranges(arr,c); print "" }
    else if(mode=="inter"){ load(a,A); load(b,B); for(j in A) if(j in B) I[j]=1; emit(I); print "" }
  }'
}
online_set() { cat /sys/devices/system/cpu/online 2>/dev/null || echo "0-$(( $(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1) - 1 ))"; }
cgroup_set() { cat /sys/fs/cgroup/cpuset.cpus.effective 2>/dev/null || cat /sys/fs/cgroup/cpuset/cpuset.effective_cpus 2>/dev/null || true; }
affinity_set() { taskset -pc $$ 2>/dev/null | sed "s/.*: //" || true; }
derive() {
  O=$(online_set); CG=$(cgroup_set); AF=$(affinity_set)
  AV=$(cpuset canon "$O")
  [ -n "$CG" ] && AV=$(cpuset inter "$AV" "$CG")
  [ -n "$AF" ] && AV=$(cpuset inter "$AV" "$AF")
  N=$(cpuset count "$AV")
  [ "$N" -ge 1 ] || { echo "derive: empty available CPU set" >&2; return 1; }
  IOK=$(( N < 2 ? N : 2 ))
  IO_CPUS=$(cpuset first "$AV" "" "$IOK")
  CPU_CPUS="$AV"
  CPU_WORKERS=$(awk -v n="$N" 'BEGIN{x=n*1.25; i=int(x); if (x>i) i++; print i}')
  if [ "$ADAPT_FLOOR_FRAC" = auto ]; then LOAD_FLOOR="$N"; else
    LOAD_FLOOR=$(awk -v n="$N" -v f="$ADAPT_FLOOR_FRAC" 'BEGIN{x=n*f; i=int(x); if (x>i) i++; print (i<1?1:i)}'); fi
  PRELOAD_CEIL=$(awk -v n="$N" 'BEGIN{x=n*0.25; i=int(x); if (x>i) i++; print i}')
  DERIV="{\"v\":\"busy-adaptive-v1+reviewer1-2744\",\"rule\":\"available=online∩cgroup∩affinity; workers=ceil(1.25xN); floor=N; io=first min(2,N); rate=2000; no-memory-generator\",\"online\":\"$O\",\"cgroup\":\"$CG\",\"affinity\":\"$AF\",\"available\":\"$AV\",\"N\":$N,\"io_cpu_set\":\"$IO_CPUS\",\"stress_cpu_set\":\"$CPU_CPUS\",\"workers\":$CPU_WORKERS,\"floor\":$LOAD_FLOOR,\"rate\":$R,\"preload_ceiling\":$PRELOAD_CEIL,\"settle_s\":$SETTLE_S,\"smoke_s\":$SMOKE_S}"
}
[ "${ADAPT_MULT:-0}" != 0 ] && derive
# class recipe set at top level too (C 2815: the params() subshell cannot set it for the JSONs)
if [ "${ADAPT_MULT:-0}" != 0 ]; then RCP="busy-adaptive-v1-fio+stressng";
elif [ "$CPU_WORKERS" != 8 ] || [ "$LOAD_FLOOR" != 0 ]; then RCP="busy80-fio-fixed+stressng"; else RCP="fio-fixed+stressng"; fi

load1() { cut -d' ' -f1 /proc/loadavg; }
stat_sample() { awk 'NR==1 {print $2+$4, $2+$3+$4+$5+$6+$7+$8}' /proc/stat; }
pct() { awk -v b0="$1" -v t0="$2" -v b1="$3" -v t1="$4" 'BEGIN{d=t1-t0; printf "%.1f", (d>0? (b1-b0)/d*100 : 0)}'; }
params() {
  printf '{"recipe":"%s","rate_iops":%s,"io_cpus":"%s","cpu_cpus":"%s","fio":"%s","stressng":"%s"' \
    "$RCP" "$1" "$IO_CPUS" "$CPU_CPUS" "$(fio --version 2>/dev/null)" "$(stress-ng --version 2>/dev/null | head -1)"
  if [ "${ADAPT_MULT:-0}" != 0 ]; then
    printf ',"derivation":%s,"cpu_workers":%s,"load_floor":%s' "$DERIV" "$CPU_WORKERS" "$LOAD_FLOOR"
  elif [ "$CPU_WORKERS" != 8 ] || [ "$LOAD_FLOOR" != 0 ]; then
    printf ',"cpu_workers":%s,"load_floor":%s' "$CPU_WORKERS" "$LOAD_FLOOR"
  fi
  printf '}'
}
digest() { printf '%s' "$1" | sha256sum | cut -d' ' -f1; }
start_fio() { taskset -c "$IO_CPUS" fio --name=refio --directory="$BUSY_REF_DIR" --ioengine=libaio --direct=1 \
  --rw=randread --bs=4k --size=256M --rate_iops="$R" --runtime="$1" --time_based --group_reporting > "$2" 2>&1 & echo $!; }
start_cpu() { stress-ng --cpu "$CPU_WORKERS" --taskset "$CPU_CPUS" --timeout "$1" >/dev/null 2>&1 & echo $!; }
achieved() { grep -m1 'read: IOPS=' "$1" 2>/dev/null | sed 's/.*IOPS=\([0-9]*\).*/\1/'; }
rate_ok() { [ -n "${1:-}" ] && [ "$1" -ge $((R * 9 / 10)) ] && [ "$1" -le $((R * 11 / 10)) ] && echo 1 || echo 0; }
wait_gone() { i=0; while kill -0 "$1" 2>/dev/null && [ "$i" -lt 50 ]; do sleep 0.2; i=$((i + 1)); done; sleep 0.5; }

case "$CMD" in
derive)
  [ "${ADAPT_MULT:-0}" != 0 ] || { echo "derive: --adaptive-mult required" >&2; exit 2; }
  derive; printf '%s\n' "$DERIV"; exit 0 ;;
settle-check) # --state F: wait to start+settle_s, sample load1, require >= floor
  [ -e "$STATE" ] || { echo "busy-ref: no session state ($STATE)" >&2; exit 2; }
  . "$STATE"
  NOW=$(date +%s); LEFT=$(( ${T0_EPOCH:-$NOW} + ${SETTLE_S:-0} - NOW ))
  [ "$LEFT" -gt 0 ] && sleep "$LEFT"
  L=$(load1)
  if awk -v l="$L" -v f="${FLOOR:-0}" 'BEGIN{exit !(f>0 && l<f)}'; then
    echo "busy-ref: settle floor miss (load1=$L < floor=${FLOOR:-0})" >&2; exit 3; fi
  echo "busy-ref: settled (load1=$L >= floor=${FLOOR:-0})"
  exit 0 ;;
run)
  P=$(params "$R"); D=$(digest "$P")
  if [ "${ADAPT_MULT:-0}" != 0 ]; then
    L0=$(load1); if awk -v l="$L0" -v c="$PRELOAD_CEIL" 'BEGIN{exit !(l>c)}'; then
      echo "busy-ref: preload ineligible (load1=$L0 > ceiling=$PRELOAD_CEIL) — refusing" >&2; exit 3; fi
  fi
  FLOG="$BUSY_REF_DIR/busy-ref-fio.$$.log"
  B=$(load1);
  S=$(stat_sample); B0=${S% *}; T0=${S#* }
  taskset -c "$IO_CPUS" fio --name=refio --directory="$BUSY_REF_DIR" --ioengine=libaio --direct=1 \
    --rw=randread --bs=4k --size=256M --rate_iops="$R" --runtime="$SECS" --time_based --group_reporting > "$FLOG" 2>&1 & FPID=$!
  stress-ng --cpu "$CPU_WORKERS" --taskset "$CPU_CPUS" --timeout "${SECS}s" >/dev/null 2>&1 & SPID=$!
  S1=$SECS; [ "${ADAPT_MULT:-0}" != 0 ] && [ "$SETTLE_S" -lt "$SECS" ] && S1=$SETTLE_S
  sleep "$S1"; M=$(load1); S=$(stat_sample); B1=${S% *}; T1=${S#* }
  [ "$S1" = "$SECS" ] || sleep $((SECS - S1))
  wait "$FPID"; FRC=$?; wait "$SPID"; SRC=$?
  A=$(load1); S=$(stat_sample); B2=${S% *}; T2=${S#* }
  PM=$(pct "$B0" "$T0" "$B1" "$T1"); P1=$(pct "$B1" "$T1" "$B2" "$T2"); PT=$(pct "$B0" "$T0" "$B2" "$T2")
  ACH=$(achieved "$FLOG"); rm -f "$FLOG"
  ROK=$(rate_ok "${ACH:-}"); EOK=0; [ "$FRC" = 0 ] && [ "$SRC" = 0 ] && EOK=1
  LOK=$(awk -v a="$A" -v m="$M" -v f="$LOAD_FLOOR" 'BEGIN{print (f<=0 || (a>=f && m>=f))?1:0}')
  OK=0; [ "$ROK" = 1 ] && [ "$EOK" = 1 ] && [ "$LOK" = 1 ] && OK=1
  cat > "$OUT" <<JSON
{"recipe":"${RCP:-fio-fixed+stressng}","params":$P,"params_digest":"$D","ts":"$(date -u '+%Y-%m-%dT%H:%M:%SZ')","phase":{"load1_before":"$B","load1_mid":"$M","load1_after":"$A"},"cpu_util_pct":{"start_to_mid":"$PM","mid_to_end":"$P1","start_to_end":"$PT"},"fio":{"rate_target":$R,"rate_achieved":${ACH:-null},"exit":$FRC},"stressng":{"exit":$SRC},"envelope":{"checks":{"achieved_rate_pm10pct":$ROK,"load_floor_ok":$LOK,"exit_zero":$EOK},"load_floor":$LOAD_FLOOR,"ok":$OK}}
JSON
  echo "busy-ref: ok=$OK digest=$D rate=${ACH:-none}/$R -> $OUT"
  [ "$OK" = 1 ] || exit 3
  ;;
start)
  [ -e "$STATE" ] && { echo "busy-ref: session already running ($STATE)" >&2; exit 2; }
  P=$(params "$R"); D=$(digest "$P")
  if [ "${ADAPT_MULT:-0}" != 0 ]; then
    L0=$(load1); if awk -v l="$L0" -v c="$PRELOAD_CEIL" 'BEGIN{exit !(l>c)}'; then
      echo "busy-ref: preload ineligible (load1=$L0 > ceiling=$PRELOAD_CEIL) — refusing" >&2; exit 1; fi
  fi
  FLOG="$BUSY_REF_DIR/busy-ref-fio.session.$$.log"
  S=$(stat_sample); B0=${S% *}; T0=${S#* }
  FPID=$(start_fio 86400 "$FLOG"); SPID=$(start_cpu 86400s)
  printf '%s' "$P" > "$STATE.params"
  { echo "R=$R"; echo "PARAMS=$P"; echo "DIGEST=$D"; echo "OUT=$OUT"; echo "FLOG=$FLOG";
    echo "FPID=$FPID"; echo "SPID=$SPID"; echo "LOAD1_B=$(load1)"; echo "B0=$B0"; echo "T0=$T0"; echo "RCP=$RCP";
    echo "T0_EPOCH=$(date +%s)"; echo "SETTLE_S=$SETTLE_S"; echo "FLOOR=$LOAD_FLOOR"; echo "PRELOAD_CEIL=${PRELOAD_CEIL:-0}"; } > "$STATE"
  echo "busy-ref: started (fio $FPID, cpu $SPID; digest $D; state $STATE)"
  ;;
stop)
  [ -e "$STATE" ] || { echo "busy-ref: no session state ($STATE)" >&2; exit 2; }
  . "$STATE"
  PARAMS=$(cat "$STATE.params" 2>/dev/null || printf '%s' "${PARAMS:-}")
  kill -INT "$FPID" 2>/dev/null; wait_gone "$FPID"; FRC=$?
  kill -TERM "$SPID" 2>/dev/null; wait_gone "$SPID"; SRC=$?
  A=$(load1); S=$(stat_sample); B1=${S% *}; T1=${S#* }
  ACH=$(achieved "$FLOG"); rm -f "$FLOG"
  PM=$(pct "$B0" "$T0" "$B1" "$T1"); ROK=$(rate_ok "${ACH:-}")
  LOK=$(awk -v a="$A" -v f="$LOAD_FLOOR" 'BEGIN{print (f<=0 || a>=f)?1:0}')
  OK=0; [ "$ROK" = 1 ] && [ "$LOK" = 1 ] && OK=1
  cat > "$OUT" <<JSON
{"recipe":"${RCP:-fio-fixed+stressng}","params":$PARAMS,"params_digest":"$DIGEST","ts":"$(date -u '+%Y-%m-%dT%H:%M:%SZ')","seconds":"session","phase":{"load1_before":"$LOAD1_B","load1_after":"$A"},"cpu_util_pct":{"start_to_end":"$PM"},"fio":{"rate_target":$R,"rate_achieved":${ACH:-null},"exit":$FRC},"stressng":{"exit":$SRC},"envelope":{"checks":{"achieved_rate_pm10pct":$ROK,"load_floor_ok":$LOK,"stopped":1},"load_floor":$LOAD_FLOOR,"ok":$OK}}
JSON
  rm -f "$STATE" "$STATE.params"
  echo "busy-ref: stopped (ok=$OK digest=$DIGEST rate=${ACH:-none}/$R) -> $OUT"
  [ "$OK" = 1 ] || exit 3
  ;;
esac
