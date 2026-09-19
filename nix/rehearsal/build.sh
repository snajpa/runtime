#!/bin/sh
# Build the S3 rehearsal driver against one checkout of the e2b runtime.
#
#   E2B_CHECKOUT=/path/to/runtime ./build.sh [outdir]
#
# The same driver source built against two checkouts is the mixed-version
# fleet: the storage and header code is the only difference between the
# binaries, so "old node" and "new node" differ exactly as they would in a
# rollout.
#
# The build happens in a throwaway workspace (driver sources copied there, a
# go.work pointing at the checkout under test) for two reasons: the repository
# stays clean — no generated go.work in the tree — and any checkout can be
# targeted, including ones that predate this driver.
set -eu

CHECKOUT=${E2B_CHECKOUT:?set E2B_CHECKOUT to the runtime checkout to build against}
OUTDIR=${1:-bin}
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REV=$(git -C "$CHECKOUT" rev-parse --short HEAD)
VERSION="$CHECKOUT@$REV"
NAME=$(basename "$CHECKOUT")

[ -f "$CHECKOUT/packages/shared/go.mod" ] || {
	echo "E2B_CHECKOUT=$CHECKOUT does not look like an e2b runtime checkout (no packages/shared/go.mod)" >&2
	exit 1
}

mkdir -p "$DIR/$OUTDIR"

WORKSPACE=$(mktemp -d)
trap 'rm -rf "$WORKSPACE"' EXIT

cp -r "$DIR/driver" "$WORKSPACE/driver"
cp "$DIR/go.mod" "$DIR/go.sum" "$WORKSPACE/"

cat > "$WORKSPACE/go.work" <<EOF
go 1.26.8

use (
	.
	$CHECKOUT/packages/shared
)
EOF

cd "$WORKSPACE"

# Resolve the driver's requirements against the checkout under test, then build
# with that checkout stamped into the binary. CGO_ENABLED=0: this binary runs
# inside the Ubuntu VM, where a Nix-linked binary would not start.
go mod tidy >/dev/null 2>&1 || true
CGO_ENABLED=0 go build -ldflags "-X main.version=$VERSION" -o "$DIR/$OUTDIR/s3-rehearsal-$NAME" ./driver

echo "built $OUTDIR/s3-rehearsal-$NAME ($VERSION)"
