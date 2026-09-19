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
STORE=${E2B_STORAGE_URL:-s3://e2b-rehearsal?endpoint=http://127.0.0.1:9000&s3ForcePathStyle=true&region=us-east-1}
RUN=${E2B_RUN_ID:-run-$(date +%Y%m%dT%H%M%S)}
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

export SSHPASS=${E2B_VM_PASSWORD:-e2b-dev}
SSH="sshpass -e ssh -p $PORT -o StrictHostKeyChecking=accept-new $VM"
SCP="sshpass -e scp -P $PORT -o StrictHostKeyChecking=accept-new"

echo "# S3 rehearsal $RUN" >&2
echo "== building drivers from both checkouts ==" >&2
E2B_CHECKOUT=$OLD "$DIR/build.sh" bin >/dev/null
E2B_CHECKOUT=$NEW "$DIR/build.sh" bin >/dev/null

OLD_BIN=s3-rehearsal-$(basename "$OLD")
NEW_BIN=s3-rehearsal-$(basename "$NEW")

echo "== shipping to $VM ==" >&2
$SSH 'mkdir -p ~/s3-rehearsal/bin ~/s3-rehearsal/manifests' >/dev/null
$SCP "$DIR/bin/$OLD_BIN" "$DIR/bin/$NEW_BIN" "$VM:~/s3-rehearsal/bin/" >/dev/null

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

phase() {
	bin=$1
	shift

	if [ -n "${NFS_BASE:-}" ]; then
		S3_REHEARSAL_CACHE_DIR="$NFS_BASE/$(basename "$bin")"
		export S3_REHEARSAL_CACHE_DIR
	fi

	# Record the phase, then keep going: a rejection is an outcome here, not a
	# reason to stop the matrix.
	if "$bin" "$@" >> "$OUT" 2>>"$OUT.err"; then
		:
	else
		echo "{\"phase\":\"$1\",\"outcome\":\"exit\",\"detail\":\"non-zero exit\"}" >> "$OUT"
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
phase "$OLD" exists --manifest "$MAN/$RUN-new.json"

echo "== old node writes, new node reads (rollback leg) =="
phase "$OLD" write --storage-url "$STORE" --prefix "$RUN/old" --manifest "$MAN/$RUN-old.json" --profile "$PROFILE"
phase "$NEW" read  --manifest "$MAN/$RUN-old.json"
phase "$NEW" exists --manifest "$MAN/$RUN-old.json"

echo "== results =="
cat "$OUT"
REMOTE

$SCP "$DIR/bin/remote-matrix.sh" "$VM:~/s3-rehearsal/remote-matrix.sh" >/dev/null

$SSH "RUN='$RUN' PROFILE='$PROFILE' OLD_BIN='$OLD_BIN' NEW_BIN='$NEW_BIN' STORE='$STORE' NFS='$NFS' sh ~/s3-rehearsal/remote-matrix.sh"

# Render the JSONL as Markdown for the notes.
$SSH "cat ~/s3-rehearsal/results-$RUN.jsonl" | "$DIR/render.sh" "$RUN"
