#!/bin/sh
# Run the mixed-version storage rehearsal against one shared object store.
#
#   ./run-matrix.sh [old-checkout] [new-checkout]
#
# Defaults: upstream 44a8a7549 (/root/tmp/runtime) as the old node and the
# delivery-branch worktree (~/ai/worktrees/e2b/scale-rehearsal) as the new one.
# The same driver source is built against both checkouts and run inside the dev
# VM against the Silo instance there, so what is mixed is the runtime's storage
# code — not the harness.
#
# Environment overrides:
#   E2B_VM, E2B_VM_PORT, E2B_VM_PASSWORD, E2B_STORAGE_URL, E2B_PROFILE
set -eu

OLD=${1:-/root/tmp/runtime}
NEW=${2:-/root/ai/worktrees/e2b/scale-rehearsal}
VM=${E2B_VM:-dev@127.0.0.1}
PORT=${E2B_VM_PORT:-2222}
PROFILE=${E2B_PROFILE:-auto}
# E2B_REHEARSAL_NFS=1 puts each node's chunk cache on the VM's NFS export
# (mountable at /mnt/nfs-cache), so the cache-on-NFS path is exercised rather
# than a local directory. `make dev-up` provisions the export.
NFS=${E2B_REHEARSAL_NFS:-0}
# E2B_REHEARSAL_NODES=N runs N concurrent nodes (mixed versions) after the
# sequential matrix; E2B_REHEARSAL_SPRAY=N adds an object-count leg (N small
# objects, inventory before and after delete); E2B_REHEARSAL_FAULT=1 tampers
# with one artifact and checks the other version detects it;
# E2B_REHEARSAL_SOAK=K repeats the whole matrix K times.
NODES=${E2B_REHEARSAL_NODES:-0}
SPRAY=${E2B_REHEARSAL_SPRAY:-0}
SPRAY_CONCURRENCY=${E2B_REHEARSAL_SPRAY_CONCURRENCY:-0}
# Attempts per object write: the storage layer bounds every write with its own
# 30s deadline, and a store under sustained pressure can exceed it, so the ramp
# retries and reports how often (that signal is the point).
SPRAY_RETRIES=${E2B_REHEARSAL_SPRAY_RETRIES:-3}
# E2B_REHEARSAL_FLAG_ROLLBACK=1 rehearses a format-affecting setting: the old
# binary writes the older header format, the new one reads it, the new one
# writes the current format, the old one reads it, and then a backfill rewrites
# the old artifacts in the current format - after which both readers must still
# work and nothing may be stranded.
FLAG_ROLLBACK=${E2B_REHEARSAL_FLAG_ROLLBACK:-0}
FAULT=${E2B_REHEARSAL_FAULT:-0}
# E2B_REHEARSAL_PEER=1 runs the peer-prefetch leg: an old-build peer process
# serves, a new-build client fetches (and the other way around), verifying every
# range against the store and reporting both latencies.
PEER=${E2B_REHEARSAL_PEER:-0}
SOAK=${E2B_REHEARSAL_SOAK:-1}
STORE=${E2B_STORAGE_URL:-s3://e2b-rehearsal?endpoint=http://127.0.0.1:9000&s3ForcePathStyle=true&region=us-east-1}
RUN=${E2B_RUN_ID:-run-$(date +%Y%m%dT%H%M%S)}
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

# The exit status is the verdict; the evidence is rendered either way. The
# remote matrix reports its own verdict and the fetched results are re-checked
# here across soak rounds (reviewer1 R2).
failed=0

export SSHPASS=${E2B_VM_PASSWORD:-e2b-dev}
SSH="sshpass -e ssh -p $PORT -o StrictHostKeyChecking=accept-new $VM"
SCP="sshpass -e scp -P $PORT -o StrictHostKeyChecking=accept-new"

echo "# S3 rehearsal $RUN" >&2
echo "== building drivers from both checkouts ==" >&2
E2B_CHECKOUT=$OLD "$DIR/build.sh" bin >/dev/null
E2B_CHECKOUT=$NEW "$DIR/build.sh" bin >/dev/null

OLD_BIN=s3-rehearsal-$(basename "$OLD")
NEW_BIN=s3-rehearsal-$(basename "$NEW")
OLD_PEER_BIN=s3-rehearsal-peer-$(basename "$OLD")
NEW_PEER_BIN=s3-rehearsal-peer-$(basename "$NEW")

echo "== shipping to $VM ==" >&2
$SSH 'mkdir -p ~/s3-rehearsal/bin ~/s3-rehearsal/manifests' >/dev/null

# Unlink our own binaries before upload: scp cannot overwrite a binary a
# lingering process still has mapped (it fails with ETXTBSY), and unlinking is
# what makes the upload work. Only our four names - the directory is shared
# with other lanes working on the same VM.
$SSH "rm -f ~/s3-rehearsal/bin/$OLD_BIN ~/s3-rehearsal/bin/$NEW_BIN ~/s3-rehearsal/bin/$OLD_PEER_BIN ~/s3-rehearsal/bin/$NEW_PEER_BIN" >/dev/null

$SCP "$DIR/bin/$OLD_BIN" "$DIR/bin/$NEW_BIN" "$DIR/bin/$OLD_PEER_BIN" "$DIR/bin/$NEW_PEER_BIN" "$VM:~/s3-rehearsal/bin/" >/dev/null

# The runtime's own migration tool, when the checkout under test has one: the
# flag-rollback leg drives it, so the product path is what gets rehearsed.
MIGRATE_BIN=s3-rehearsal-migrate-$(basename "$NEW")
if [ -f "$DIR/bin/$MIGRATE_BIN" ]; then
	$SSH "rm -f ~/s3-rehearsal/bin/$MIGRATE_BIN" >/dev/null
	$SCP "$DIR/bin/$MIGRATE_BIN" "$VM:~/s3-rehearsal/bin/" >/dev/null
else
	MIGRATE_BIN=""
fi

# The remote script runs the phases inside the VM, where Silo and the box are:
# it is the mixed-version matrix and it records one JSON result per phase.
cat > "$DIR/bin/remote-matrix.sh" <<'REMOTE'
#!/bin/sh
# Runs as the unprivileged dev user inside the VM. Env: STORE, RUN, PROFILE,
# OLD_BIN, NEW_BIN, MANIFEST_DIR, OUT.
set -eu

export AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID:-minioadmin}
export AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY:-minioadmin}

# The chunk cache is where a node keeps decompressed chunks; with NFS=1 it
# lives on the NFS export, which is what production does. Each node gets its
# own directory so one node's warm cache can never mask another's reads.
if [ "${NFS:-0}" = "1" ]; then
	if ! mountpoint -q /mnt/nfs-cache; then
		sudo -n mkdir -p /mnt/nfs-cache
		sudo -n mount -t nfs -o vers=4.2 127.0.0.1:/srv/nfs-cache /mnt/nfs-cache
	fi

	sudo -n mkdir -p "/mnt/nfs-cache/$RUN"
	sudo -n chown "$(id -u):$(id -g)" "/mnt/nfs-cache/$RUN"
	NFS_BASE="/mnt/nfs-cache/$RUN"
fi

BASE=$HOME/s3-rehearsal
OLD=$BASE/bin/$OLD_BIN
NEW=$BASE/bin/$NEW_BIN
MAN=$BASE/manifests
OUT=${OUT:-$BASE/results-$RUN.jsonl}

mkdir -p "$MAN"
: > "$OUT"

# The aggregate verdict: every phase still runs and is recorded, but this
# script exits non-zero if a required outcome did not hold: a failed driver,
# an undetected tamper, or a rollback the reader floor blocks (reviewer1 R2).
failed=0

# Rollback readability (reviewer1 R10): the rollback binary must read every
# rollout-created artifact. A loud refusal is format safety, but it is not
# rollback evidence; it blocks rollback below the reader floor, so the run
# records that explicitly instead of passing silently.
require_rollback_read() {
	record=$1
	leg=$2

	case "$record" in
	*'"outcome":"ok"'*)
		if printf '%s' "$record" | grep -q 'refused'; then
			printf '{"phase":"rollback-readability","outcome":"error","detail":"%s: the rollback binary refused rollout-created artifacts; the reader floor is below the write format, so rollback is blocked and the matrix must not claim rollback safety"}\n' "$leg" >>"$OUT"
			failed=1
		else
			printf '{"phase":"rollback-readability","outcome":"ok","detail":"%s: the rollback binary read every rollout-created artifact"}\n' "$leg" >>"$OUT"
		fi
		;;
	*)
		printf '{"phase":"rollback-readability","outcome":"error","detail":"%s: the rollback binary did not read the rollout-created artifacts cleanly"}\n' "$leg" >>"$OUT"
		failed=1
		;;
	esac
}

# Per-artifact fault detection (reviewer1 R2): the tampered entry (index 0,
# the first build in the manifest) must be the one the reader names, in a
# loud refusal or a misread. "Something was refused" is not evidence.
fault_detected() {
	build=$1
	result=$2

	[ -n "$build" ] || return 1
	grep -qF "$build" "$result" || return 1

	if grep -qE ', 1 refused loudly' "$result"; then
		return 0
	fi

	if grep -qE '1 of [0-9]+ MISREAD' "$result"; then
		return 0
	fi

	return 1
}

phase() {
	bin=$1
	shift

	if [ -n "${NFS_BASE:-}" ]; then
		S3_REHEARSAL_CACHE_DIR="$NFS_BASE/$(basename "$bin")"
		export S3_REHEARSAL_CACHE_DIR
	fi

	# Record the phase, then keep going: a rejection is an outcome here, not a
	# reason to stop the matrix. A failed driver still marks the run.
	if "$bin" "$@" >> "$OUT" 2>>"$OUT.err"; then
		:
	else
		echo "{\"phase\":\"$1\",\"outcome\":\"exit\",\"detail\":\"non-zero exit\"}" >> "$OUT"
		failed=1
	fi
}

echo "== object store =="
phase "$NEW" ensure-bucket --storage-url "$STORE"

echo "== which versions =="
"$OLD" versions
"$NEW" versions

echo "== plumbing: both versions can round-trip through the object store =="
phase "$OLD" probe --storage-url "$STORE" --prefix "$RUN/probe-old"
phase "$NEW" probe --storage-url "$STORE" --prefix "$RUN/probe-new"

echo "== new node writes, old node reads (upgrade leg) =="
phase "$NEW" write --storage-url "$STORE" --prefix "$RUN/new" --manifest "$MAN/$RUN-new.json" --profile "$PROFILE"
phase "$OLD" read  --manifest "$MAN/$RUN-new.json"
require_rollback_read "$(tail -n 1 "$OUT")" "upgrade leg"
phase "$OLD" exists --manifest "$MAN/$RUN-new.json"

echo "== old node writes, new node reads (rollback leg) =="
phase "$OLD" write --storage-url "$STORE" --prefix "$RUN/old" --manifest "$MAN/$RUN-old.json" --profile "$PROFILE"
phase "$NEW" read  --manifest "$MAN/$RUN-old.json"
phase "$NEW" exists --manifest "$MAN/$RUN-old.json"

if [ "${NODES:-0}" -gt 1 ]; then
	echo "== fan-out: $NODES concurrent mixed-version nodes =="
	FAN="$BASE/fanout-$RUN"
	mkdir -p "$FAN"

	i=0
	while [ "$i" -lt "$NODES" ]; do
		if [ $((i % 2)) -eq 0 ]; then bin=$NEW; else bin=$OLD; fi

		(
			if [ -n "${NFS_BASE:-}" ]; then
				S3_REHEARSAL_CACHE_DIR="$NFS_BASE/node-$i"
				export S3_REHEARSAL_CACHE_DIR
			fi

			"$bin" write --storage-url "$STORE" --prefix "$RUN/fanout/node-$i" \
				--manifest "$MAN/$RUN-fanout-$i.json" --profile "$PROFILE"
		) >"$FAN/write-$i.json" 2>"$FAN/write-$i.err" &

		i=$((i + 1))
	done
	wait

	i=0
	while [ "$i" -lt "$NODES" ]; do
		next=$(( (i + 1) % NODES ))

		# The reader is the opposite version of the writer of $next, so every
		# cross-node read in the fan-out is a mixed-version read.
		if [ $((next % 2)) -eq 0 ]; then bin=$OLD; else bin=$NEW; fi

		(
			if [ -n "${NFS_BASE:-}" ]; then
				S3_REHEARSAL_CACHE_DIR="$NFS_BASE/node-$i-read"
				export S3_REHEARSAL_CACHE_DIR
			fi

			"$bin" read --manifest "$MAN/$RUN-fanout-$next.json"
			"$bin" exists --manifest "$MAN/$RUN-fanout-$next.json"
		) >>"$FAN/read-$i.json" 2>>"$FAN/read-$i.err" &

		i=$((i + 1))
	done
	wait

	cat "$FAN"/*.json >>"$OUT"
	cat "$FAN"/*.err >>"$OUT.err" 2>/dev/null || true
fi

if [ "${SPRAY:-0}" -gt 0 ]; then
	echo "== object-count shape: $SPRAY small objects =="
	phase "$NEW" spray --storage-url "$STORE" --prefix "$RUN/objects" --count "$SPRAY" --concurrency "$SPRAY_CONCURRENCY" --retry-attempts "$SPRAY_RETRIES"
	phase "$NEW" count --storage-url "$STORE" --prefix "$RUN/objects"
	# A destructive operation gets a dry run first: it must report the same
	# objects and bytes the purge then removes.
	phase "$NEW" purge --dry-run --storage-url "$STORE" --prefix "$RUN/objects"
	phase "$NEW" purge --storage-url "$STORE" --prefix "$RUN/objects"
	phase "$NEW" count --storage-url "$STORE" --prefix "$RUN/objects"
fi

if [ "${PEER:-0}" = "1" ]; then
	echo "== peer prefetch: two node processes, mixed versions =="
	PEER_OLD=$BASE/bin/$OLD_PEER_BIN
	PEER_NEW=$BASE/bin/$NEW_PEER_BIN

	# The old build serves, the new build fetches (upgrade direction).
	"$PEER_OLD" serve --addr 127.0.0.1:9101 --manifest "$MAN/$RUN-new.json" >>"$OUT" 2>>"$OUT.err" &
	server_old=$!
	sleep 2
	"$PEER_NEW" fetch --peer 127.0.0.1:9101 --manifest "$MAN/$RUN-new.json" >>"$OUT" 2>>"$OUT.err"
	kill "$server_old" 2>/dev/null || true
	wait "$server_old" 2>/dev/null || true

	# The new build serves, the old build fetches (rollback direction).
	"$PEER_NEW" serve --addr 127.0.0.1:9102 --manifest "$MAN/$RUN-old.json" >>"$OUT" 2>>"$OUT.err" &
	server_new=$!
	sleep 2
	"$PEER_OLD" fetch --peer 127.0.0.1:9102 --manifest "$MAN/$RUN-old.json" >>"$OUT" 2>>"$OUT.err"
	kill "$server_new" 2>/dev/null || true
	wait "$server_new" 2>/dev/null || true
fi

if [ "${FLAG_ROLLBACK:-0}" = "1" ]; then
	echo "== flag rollback: older write format, backfill with the runtime's tool, read back by both =="
	# 1) the old binary writes the older header format; the new binary must read it
	phase "$OLD" write --storage-url "$STORE" --prefix "$RUN/flagroll/v4" --manifest "$MAN/$RUN-v4.json" --profile "$PROFILE" --header-version 4 --layout product
	phase "$NEW" read --manifest "$MAN/$RUN-v4.json"
	# 2) the new binary writes the current format; the old binary must read that too
	phase "$NEW" write --storage-url "$STORE" --prefix "$RUN/flagroll/v5" --manifest "$MAN/$RUN-v5.json" --profile "$PROFILE" --layout product
	phase "$OLD" read --manifest "$MAN/$RUN-v5.json"
	require_rollback_read "$(tail -n 1 "$OUT")" "flag-rollback leg"

	# 3) backfill the older artifacts onto the current format. When the checkout
	# under test ships the runtime's own migrate-builds, that is what runs - the
	# product path, not a harness primitive; older checkouts fall back to the
	# harness phase, which says so in the report.
	if [ -n "${MIGRATE_BIN:-}" ] && [ -x "$BASE/bin/$MIGRATE_BIN" ]; then
		echo "== backfill with the runtime's own migrate-builds =="
		jq -r '.entries[].build' "$MAN/$RUN-v4.json" | sort -u > "$BASE/migrate-$RUN-builds.txt"

		if "$BASE/bin/$MIGRATE_BIN" -builds-file "$BASE/migrate-$RUN-builds.txt" \
			-storage-url "$STORE" -apply -report "$BASE/migrate-$RUN-report.json" \
			>"$BASE/migrate-$RUN.out" 2>&1; then
			tail -n 1 "$BASE/migrate-$RUN-report.json" \
				| jq -c '{phase:"migrate",version:"product:migrate-builds",outcome:(if ((.summary.failed // 0) > 0) then "error" else "ok" end),
					detail:("product migrate-builds: migrated=" + ((.summary.migrate // 0)|tostring)
						+ " skipped=" + ((.summary.skip // 0)|tostring)
						+ " missing=" + ((.summary.missing // 0)|tostring) + " failed=" + ((.summary.failed // 0)|tostring))}' >>"$OUT" \
				|| printf '%s\n' '{"phase":"migrate","version":"product:migrate-builds","outcome":"ok","detail":"product migrate-builds ran (no summary)"}' >>"$OUT"
			# A zero exit can still carry per-artifact failures: the tool records them
			# without failing the run, so the leg checks the summary itself - a
			# migration with failed artifacts must not pass the battery.
			migrate_failed=$(tail -n 1 "$BASE/migrate-$RUN-report.json" | jq -r '.summary.failed // 0' 2>/dev/null || echo 0)
			if [ "$migrate_failed" != "0" ]; then failed=1; fi
		else
			printf '%s\n' '{"phase":"migrate","version":"product:migrate-builds","outcome":"error","detail":"product migrate-builds failed; see the VM log"}' >>"$OUT"
			failed=1
		fi

		# The reconcile pass is the "before anything is removed" check: every
		# reference the headers carry must resolve to an object that verifies.
		if "$BASE/bin/$MIGRATE_BIN" -mode reconcile -builds-file "$BASE/migrate-$RUN-builds.txt" \
			-storage-url "$STORE" -verify -report "$BASE/reconcile-$RUN-report.json" \
			>"$BASE/reconcile-$RUN.out" 2>&1; then
			tail -n 1 "$BASE/reconcile-$RUN-report.json" \
				| jq -c '{phase:"reconcile",version:"product:migrate-builds",outcome:"ok",
					detail:("product reconcile: complete=" + ((.summary.complete // 0)|tostring)
						+ " missing-payload=" + ((.summary["missing-payload"] // 0)|tostring)
						+ " mismatch=" + ((.summary.mismatch // 0)|tostring))}' >>"$OUT" \
				|| printf '%s\n' '{"phase":"reconcile","version":"product:migrate-builds","outcome":"ok","detail":"product reconcile ran (no summary)"}' >>"$OUT"
		else
			printf '%s\n' '{"phase":"reconcile","version":"product:migrate-builds","outcome":"error","detail":"product reconcile failed; see the VM log"}' >>"$OUT"
			failed=1
		fi
	else
		printf '%s\n' '{"phase":"migrate","version":"harness:migrate","outcome":"ok","detail":"checkout has no migrate-builds; used the harness migrate phase"}' >>"$OUT"
		phase "$NEW" migrate --manifest "$MAN/$RUN-v4.json" --header-version 5
	fi

	# 4) after the rewrite both readers must still read them, and nothing may be stranded
	phase "$OLD" read --manifest "$MAN/$RUN-v4.json"
	phase "$NEW" read --manifest "$MAN/$RUN-v4.json"
	phase "$OLD" exists --manifest "$MAN/$RUN-v4.json"
	phase "$NEW" exists --manifest "$MAN/$RUN-v4.json"
fi

if [ "${FAULT:-0}" = "1" ]; then
	echo "== fault injection: tamper with one artifact, the old version must detect it =="
	phase "$NEW" tamper --manifest "$MAN/$RUN-old.json" --index 0

	# Detection must be per-artifact (reviewer1 R2): the tampered entry
	# (index 0, the first build in the manifest) must be the one the reader
	# names, in a loud refusal or a misread; an unrelated refusal is not
	# evidence that the tamper was seen.
	tampered_build=$(grep -o '"build": *"[^"]*"' "$MAN/$RUN-old.json" | head -n 1 | cut -d'"' -f4)
	fault_read="$BASE/fault-read.json"
	# A fault read must be cold: an inherited, already-warm cache would serve
	# the pre-tamper bytes and mask the tamper. Dedicated empty cache dir.
	fault_cache=$(mktemp -d)
	S3_REHEARSAL_CACHE_DIR="$fault_cache" "$OLD" read --manifest "$MAN/$RUN-old.json" \
		>"$fault_read" 2>"$fault_read.err" || true
	rm -rf "$fault_cache"
	cat "$fault_read" >>"$OUT"
	cat "$fault_read.err" >>"$OUT.err"

	if fault_detected "$tampered_build" "$fault_read"; then
		printf '%s\n' '{"phase":"fault-injection","outcome":"ok","detail":"tampering detected (loud refusal or misread), never silent"}' >>"$OUT"
	else
		printf '%s\n' '{"phase":"fault-injection","outcome":"error","detail":"tampering went undetected for the tampered entry: not named in a loud refusal or misread"}' >>"$OUT"
		failed=1
	fi
fi

echo "== results =="
cat "$OUT"

if [ "$failed" != "0" ]; then
	echo "rehearsal FAILED: at least one required outcome did not hold" >&2
fi

exit "$failed"
REMOTE

$SCP "$DIR/bin/remote-matrix.sh" "$VM:~/s3-rehearsal/remote-matrix.sh" >/dev/null

i=1
while [ "$i" -le "$SOAK" ]; do
	run_id="$RUN"
	if [ "$SOAK" -gt 1 ]; then
		run_id="$RUN-$i"
	fi

	$SSH "RUN='$run_id' PROFILE='$PROFILE' OLD_BIN='$OLD_BIN' NEW_BIN='$NEW_BIN' STORE='$STORE' NFS='$NFS' NODES='$NODES' SPRAY='$SPRAY' SPRAY_CONCURRENCY='$SPRAY_CONCURRENCY' SPRAY_RETRIES='$SPRAY_RETRIES' FAULT='$FAULT' FLAG_ROLLBACK='$FLAG_ROLLBACK' PEER='$PEER' OLD_PEER_BIN='$OLD_PEER_BIN' NEW_PEER_BIN='$NEW_PEER_BIN' MIGRATE_BIN='$MIGRATE_BIN' sh ~/s3-rehearsal/remote-matrix.sh" || failed=1


	i=$((i + 1))
done

# Aggregate the required outcomes and render the JSONL (all soak rounds) as
# Markdown for the notes; the evidence is printed even when the verdict fails.
results=$(mktemp)
$SSH "cat ~/s3-rehearsal/results-$RUN*.jsonl" > "$results" || failed=1
if [ ! -s "$results" ]; then
	failed=1
fi
if grep -qE '"outcome":"(error|exit)"' "$results"; then
	failed=1
fi
"$DIR/render.sh" "$RUN" < "$results"
rm -f "$results"

if [ "$failed" != "0" ]; then
	echo "rehearsal FAILED: at least one required outcome did not hold" >&2
fi

exit "$failed"
