#!/bin/sh
# The dev environment behind `make dev` and the `nix develop` shell.
#
# The product is validated on Ubuntu, as the repo's own spec requires
# (embed/compose/scripts/preflight.sh and host-setup.sh); Nix defines that
# Ubuntu VM and this script makes sure everything needed to develop is up: the
# VM, and inside it the services the storage work uses — ublk_drv, the Silo
# object store, and the NFS directory the chunk cache can be pointed at.
#
#   nix/scripts/dev.sh --ensure    (default) best-effort bring-up, quiet
#   nix/scripts/dev.sh --status    print the state, change nothing
#   nix/scripts/dev.sh --services  provision the in-VM services only
#
# Safe to run repeatedly. It never destroys VM state: resetting is the explicit
# `./result/bin/e2b-dev-vm reset`.
set -eu

DIR=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
MODE=${1:---ensure}

VM_PORT=${E2B_VM_PORT:-2222}
VM_HOST=${E2B_VM_HOST:-dev@127.0.0.1}
VM_PASSWORD=${E2B_VM_PASSWORD:-e2b-dev}
VM_BIN=${E2B_DEV_VM_BIN:-$DIR/result/bin/e2b-dev-vm}
SILO_IMAGE=${E2B_SILO_IMAGE:-pgsty/silo:latest}

log() { printf '%s\n' "$*" >&2; }

have_nix() { command -v nix >/dev/null 2>&1; }

have_vm_runner() {
	[ -x "$VM_BIN" ] || [ -x "$DIR/result/bin/e2b-dev-vm" ]
}

build_vm_runner() {
	have_nix || { log "dev: nix is required to build the dev VM"; return 1; }
	log "dev: building the dev VM runner (nix build .#dev-vm)"
	nix build --out-link "$DIR/result" "$DIR#dev-vm" >/dev/null
}

vm() {
	if [ -x "$VM_BIN" ]; then
		"$VM_BIN" "$@"
	elif [ -x "$DIR/result/bin/e2b-dev-vm" ]; then
		"$DIR/result/bin/e2b-dev-vm" "$@"
	else
		return 127
	fi
}

vm_running() {
	# Probe the VM itself. The VM's state directory belongs to whichever
	# worktree started it, so this worktree's runner `status` is not
	# authoritative: checking it said "not running" while the VM was healthy,
	# serving Silo and exporting NFS.
	if ssh_vm true 2>/dev/null; then
		return 0
	fi

	vm status 2>/dev/null | grep -q qemu-system
}

ssh_vm() {
	SSHPASS=$VM_PASSWORD sshpass -e ssh -p "$VM_PORT" \
		-o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 \
		"$VM_HOST" "$@"
}

ensure_vm() {
	if vm_running; then
		return 0
	fi

	have_vm_runner || build_vm_runner || return 1

	log "dev: starting the Ubuntu dev VM (first boot runs cloud-init, then reboots)"
	vm up >/dev/null 2>&1 || true

	i=0
	while [ "$i" -lt 60 ]; do
		if ssh_vm true 2>/dev/null; then
			return 0
		fi

		i=$((i + 1))
		sleep 5
	done

	log "dev: the VM did not become reachable on port $VM_PORT"
	return 1
}

provision_services() {
	ssh_vm "
		set -eu
		sudo -n modprobe ublk_drv 2>/dev/null || true
		command -v busybox >/dev/null 2>&1 || sudo -n apt-get install -y -qq busybox-static >/dev/null 2>&1 || true
		sudo -n mkdir -p /srv/silo-data /srv/nfs-cache

		if ! sudo -n docker ps --format '{{.Names}}' | grep -qx silo; then
			sudo -n docker rm -f silo >/dev/null 2>&1 || true
			sudo -n docker run -d --name silo --restart unless-stopped -p 9000:9000 \
				-e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
				-v /srv/silo-data:/data $SILO_IMAGE server /data >/dev/null
		fi

		if ! sudo -n exportfs 2>/dev/null | grep -q /srv/nfs-cache; then
			sudo -n apt-get install -y -qq nfs-kernel-server >/dev/null 2>&1 || true
			sudo -n sh -c \"grep -q /srv/nfs-cache /etc/exports 2>/dev/null || echo '/srv/nfs-cache 127.0.0.1(rw,sync,no_subtree_check,no_root_squash)' >> /etc/exports\"
			sudo -n exportfs -ra >/dev/null 2>&1 || true
		fi
	"
}

print_status() {
	printf 'dev environment\n'
	printf '  vm:        '
	if vm_running; then
		printf 'running (ssh %s:%s)\n' "$VM_HOST" "$VM_PORT"
	else
		printf 'not running\n'
	fi
	printf '  ublk:      '
	ssh_vm 'test -c /dev/ublk-control && echo present || echo missing' 2>/dev/null || printf 'unknown\n'
	printf '  silo:      '
	ssh_vm 'sudo -n docker ps --filter name=silo --format "{{.Status}}" 2>/dev/null | head -1' 2>/dev/null || printf 'unknown\n'
	printf '  nfs:       '
	ssh_vm 'sudo -n exportfs 2>/dev/null | grep /srv/nfs-cache | head -1' 2>/dev/null || printf 'unknown\n'
	printf '  store:     s3://e2b-rehearsal?endpoint=http://127.0.0.1:9000&s3ForcePathStyle=true&region=us-east-1\n'
	printf '  next:      make tests      validate changes (host + Ubuntu VM suites)\n'
	printf '             make rehearsal  S3 mixed-version storage rehearsal on Silo\n'
}

case "$MODE" in
--status)
	print_status
	;;
--services)
	ensure_vm || exit 1
	provision_services
	print_status
	;;
--ensure|--shell)
	ensure_vm || exit 1
	provision_services
	print_status
	;;
*)
	log "usage: $0 [--ensure|--status|--services]"
	exit 2
	;;
esac
