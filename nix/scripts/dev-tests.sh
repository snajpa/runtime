#!/bin/sh
# `make tests` — validate changes on this monorepo.
#
# Two halves, because the product has two requirements at once: the code has to
# build, lint and pass its host tests, and it has to work on the Ubuntu host
# shape the repo specifies (embed/compose/scripts/preflight.sh), so the
# root-gated storage suites run inside the dev VM.
#
#   nix/scripts/dev-tests.sh [--base <rev>] [--no-vm] [--quiet]
#
# Changed packages are derived from the diff against --base (default
# origin/main) plus uncommitted work; with no changes to speak of, the e2b
# storage packages are validated, which is what this fork is working on.
set -eu

DIR=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
if [ -n "${E2B_TEST_BASE:-}" ]; then
	BASE=$E2B_TEST_BASE
elif upstream=$(git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' 2>/dev/null) && [ -n "$upstream" ]; then
	# The branch's own upstream: validates *this* work rather than every commit
	# the branch already carried when it was cut.
	BASE=$upstream
else
	BASE=origin/main
fi
NO_VM=0
QUIET=0

while [ $# -gt 0 ]; do
	case "$1" in
	--base) BASE=$2; shift 2 ;;
	--no-vm) NO_VM=1; shift ;;
	--quiet) QUIET=1; shift ;;
	*) printf 'unknown flag %s\n' "$1" >&2; exit 2 ;;
	esac
done

say() { [ "$QUIET" = 1 ] || printf '%s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*" >&2; FAILURES=$((FAILURES + 1)); }

FAILURES=0
MODULES="packages/orchestrator packages/shared packages/api packages/envd"

cd "$DIR"

# --- what changed ----------------------------------------------------------
CHANGED=$(git diff --name-only "$BASE"...HEAD 2>/dev/null || true)
CHANGED="$CHANGED
$(git status --porcelain | awk '{print $2}')"
CHANGED=$(printf '%s\n' "$CHANGED" | sed '/^$/d' | sort -u)

GO_PKGS=$(printf '%s\n' "$CHANGED" | grep '\.go$' | xargs -r -n1 dirname | sort -u || true)

if [ -z "$GO_PKGS" ]; then
	GO_PKGS="packages/orchestrator/pkg/sandbox/ublk
packages/orchestrator/pkg/sandbox/rootfs"
	say "tests: no changed Go packages; validating the storage packages instead"
fi

say "tests: base $BASE"
say "tests: packages under test:"
printf '%s\n' "$GO_PKGS" | sed 's/^/  /' | { [ "$QUIET" = 1 ] && cat >/dev/null || cat; }

# --- host: build, format, lint, unit tests ---------------------------------
say "tests: host build"
for m in $MODULES; do
	[ -d "$m" ] || continue
	(cd "$m" && go build ./... ) || fail "go build $m"
done

say "tests: gofmt on changed files"
CHANGED_GO=$(printf '%s\n' "$CHANGED" | grep '\.go$' || true)
if [ -n "$CHANGED_GO" ]; then
	# shellcheck disable=SC2086
	UNFMT=$(gofmt -l $CHANGED_GO || true)
	[ -z "$UNFMT" ] || fail "gofmt: $UNFMT"
fi

LINT=$(command -v golangci-lint || true)
if [ -z "$LINT" ]; then
	fail "golangci-lint is not on PATH — run inside the dev shell ('make dev'), or set PATH"
else
	say "tests: golangci-lint on changed packages"
fi

for p in $GO_PKGS; do
	[ -d "$p" ] || continue
	mod=$(printf '%s' "$p" | cut -d/ -f1-2)
	if [ -n "$LINT" ] && [ -f "$mod/go.mod" ]; then
		(cd "$mod" && "$LINT" run "./${p#"$mod"/}") || fail "golangci-lint $p"
	fi
done

say "tests: go test on changed packages"
for p in $GO_PKGS; do
	[ -d "$p" ] || continue
	mod=$(printf '%s' "$p" | cut -d/ -f1-2)
	if [ -f "$mod/go.mod" ]; then
		(cd "$mod" && go test "./${p#"$mod"/}") || fail "go test $p"
	fi
done

# --- Ubuntu VM: the suites that need the host shape ------------------------
if [ "$NO_VM" = 1 ]; then
	say "tests: skipping the Ubuntu VM suites (--no-vm)"
else
	say "tests: Ubuntu VM suites"
	if ! "$DIR/nix/scripts/dev.sh" --ensure >/dev/null 2>&1; then
		fail "the dev VM is not available (run 'make dev')"
	else
		VM_PORT=${E2B_VM_PORT:-2222}
		VM_HOST=${E2B_VM_HOST:-dev@127.0.0.1}
		export SSHPASS=${E2B_VM_PASSWORD:-e2b-dev}

		for pkg in packages/orchestrator/pkg/sandbox/ublk packages/orchestrator/pkg/sandbox/rootfs; do
			bin=$(basename "$pkg")
			# CGO_ENABLED=0: the binary runs inside the Ubuntu VM, and a
			# Nix-shell build links the Nix loader (/nix/store/.../ld-linux),
			# which does not exist there.
			(cd "$DIR/packages/orchestrator" && CGO_ENABLED=0 go test -c -o "/tmp/$bin.test" "./${pkg#packages/orchestrator/}") \
				|| { fail "build VM test binary $pkg"; continue; }

			sshpass -e scp -P "$VM_PORT" -o StrictHostKeyChecking=accept-new "/tmp/$bin.test" "$VM_HOST:/tmp/$bin.test" >/dev/null \
				|| { fail "ship VM test binary $pkg"; continue; }

			sshpass -e ssh -p "$VM_PORT" "$VM_HOST" \
				"sudo -n env UBLK_TEST_FIRECRACKER=\${UBLK_TEST_FIRECRACKER:-/home/dev/ublk-fc/firecracker} UBLK_TEST_KERNEL=\${UBLK_TEST_KERNEL:-/home/dev/ublk-fc/vmlinux.bin} UBLK_TEST_BUSYBOX=/bin/busybox /tmp/$bin.test -test.timeout 900s" \
				|| fail "VM suite $pkg"
		done
	fi
fi

# --- summary ---------------------------------------------------------------
if [ "$FAILURES" -gt 0 ]; then
	printf 'tests: %d stage(s) failed\n' "$FAILURES" >&2
	exit 1
fi

say "tests: all stages passed"
