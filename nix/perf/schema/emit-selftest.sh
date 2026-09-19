#!/bin/sh
# perf/2 emitter selftest — bite cases for emit.{sh,py}. Exit 0 = all pass.
set -u
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
TMP=$(mktemp -d)
S="$TMP/perf-runs.jsonl"
fails=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; fails=$((fails+1)); }

PERF_RUN_ID=run-20260919T000000 PERF_SUITE_REF=t PERF_SUITE_SHA=abc1234
export PERF_RUN_ID PERF_SUITE_REF PERF_SUITE_SHA

run() { # run <expected-rc> <name> <body>
  rc=$1; name=$2; body=$3
  printf '%s' "$body" | sh "$DIR/emit.sh" append --stream "$S" --phase run --mode clean >/dev/null 2>&1
  got=$?
  if [ "$got" = "$rc" ]; then ok "$name"; else bad "$name (rc=$got want $rc)"; fi
}

run 0  'valid sample' \
  '{"record":"sample","workload":"W1","profile":"p","side":"baseline","leg":"A","block":1,"stage":"measure","metric":{"id":"iops","unit":"count/s"},"unit_id":"u","chunk":{"i":1,"n":1},"samples":[1]}'
run 40 'W7 sample without evidence (must fail)' \
  '{"block":1,"chunk":{"i":1,"n":1},"leg":"A1","metric":{"id":"restore_to_ready","unit":"s"},"profile":"restore-warm","record":"sample","samples":[0.42],"side":"candidate","stage":"measure","unit_id":"restore","workload":"W7"}'
run 0 'valid W7 sample with evidence' \
  '{"block":1,"chunk":{"i":1,"n":1},"class":"warm","leg":"A1","metric":{"id":"restore_to_ready","unit":"s"},"profile":"restore-warm","profile_digest":"abc1234","record":"sample","samples":[0.42],"side":"candidate","snapshot_id":"snap-1","stage":"measure","unit_id":"restore","workload":"W7"}'
run 0 'valid W8 sample with evidence' \
  '{"block":1,"chunk":{"i":1,"n":1},"class":"warm","leg":"B1","metric":{"id":"post_resume_lat_p95_us","unit":"us"},"op_class":"read","profile":"post-resume-read","profile_digest":"abc1234","record":"sample","samples":[123.0],"side":"candidate","snapshot_id":"snap-1","stage":"measure","unit_id":"window","window":"steady","workload":"W8","workload_digest":"beef1234"}'
run 0 'valid snapshot record' \
  '{"cache_state_evidence":{"page_cache":"warm"},"class":"warm","config":{"cmdline":"console=ttyS0"},"content_sha256":"deadbeef","cpu_template":"none","firecracker":{"version":"1.12.1"},"init":{"binary":"/init"},"kernel":{"image":"vmlinux-6.1.102"},"memory_mib":1024,"network":{"mode":"tap"},"object_store_state":{"local":true},"ports":[80],"quiesced_marker":{"rw":"ok","sha256":"abc1234"},"record":"snapshot","reset_procedure":{"drop_caches":true},"restore_state_dir":"/state","rootfs":{"path":"/rootfs"},"snapshot_id":"snap-1","source_tree":{"ref":"5eb54defe"},"uffd":{"mode":"lazy"},"vcpu":2}'
run 40 'chunk must be an object' \
  '{"record":"sample","workload":"W1","profile":"p","side":"baseline","leg":"A","block":1,"stage":"measure","metric":{"id":"iops","unit":"count/s"},"unit_id":"u","chunk":0,"samples":[1]}'
run 40 'missing chunk' \
  '{"record":"sample","workload":"W1","profile":"p","side":"baseline","leg":"A","block":1,"stage":"measure","metric":{"id":"iops","unit":"count/s"},"unit_id":"u","samples":[1]}'
run 40 'bad enum (order)' \
  '{"record":"block","index":1,"warmup":false,"order":"XX","namespace":"n","seed":1}'
run 40 'malformed json' \
  'not json'
run 40 'unknown metric id' \
  '{"record":"sample","workload":"W1","profile":"p","side":"baseline","leg":"A","block":1,"stage":"measure","metric":{"id":"bogus_metric","unit":"x"},"unit_id":"u","chunk":{"i":1,"n":1},"samples":[1]}'

# kind mismatch: --kind note but body record sample
printf '%s' '{"record":"sample","workload":"W1","profile":"p","side":"baseline","leg":"A","block":1,"stage":"measure","metric":{"id":"iops","unit":"count/s"},"unit_id":"u","chunk":{"i":1,"n":1},"samples":[1]}' \
  | sh "$DIR/emit.sh" append --stream "$S" --kind note --phase run --mode clean >/dev/null 2>&1
[ $? = 40 ] && ok 'kind mismatch' || bad 'kind mismatch'

# bad run id via env
( PERF_RUN_ID=bad sh "$DIR/emit.sh" append --stream "$S" --phase run --mode clean >/dev/null 2>&1 <<'EOF'
{"record":"note","text":"x"}
EOF
) ; [ $? = 40 ] && ok 'bad run-id' || bad 'bad run-id'

run 0  'note ok' '{"record":"note","text":"x"}'

n=$(wc -l < "$S" 2>/dev/null || echo 0)
if [ "$n" = 5 ]; then ok 'stream has only the valid lines (5)'; else bad "stream lines=$n want 5"; fi

rm -rf "$TMP"
if [ "$fails" = 0 ]; then echo 'selftest ok'; exit 0; fi
echo "selftest FAIL ($fails)"; exit 1
