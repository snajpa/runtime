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
# `./result/bin/e2b-dev-vm reset`. The VM is started in the background (its
# console lands in the VM state directory as `qemu.log`); stop it with
# `./result/bin/e2b-dev-vm stop`.
set -eu

DIR=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
MODE=${1:---ensure}

VM_PORT=${E2B_VM_PORT:-2222}
VM_HOST=${E2B_VM_HOST:-dev@127.0.0.1}
VM_PASSWORD=${E2B_VM_PASSWORD:-e2b-dev}
VM_BIN=${E2B_DEV_VM_BIN:-$DIR/result/bin/e2b-dev-vm}
SILO_IMAGE=${E2B_SILO_IMAGE:-pgsty/silo:latest}

# The Firecracker guest tests need a firecracker binary and a kernel inside the
# VM. The kernel must carry ublk_drv for the ublk guest tests, so the known-good
# pair is kept on the host and restored into fresh VMs; E2B_FC_ARTIFACTS_DIR
# points somewhere else, and the stock e2b artifacts (public bucket) are the
# fallback for machines that never had a copy.
ARTIFACT_DIR=${E2B_FC_ARTIFACTS_DIR:-$HOME/ai/artifacts/e2b-ublk-fc}
FC_VERSION=${E2B_FC_VERSION:-v1.12.1_717921c}
FC_KERNEL=${E2B_FC_KERNEL:-vmlinux-6.1.102/amd64/vmlinux.bin}

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

vm_runner() {
	if [ -x "$VM_BIN" ]; then
		printf '%s\n' "$VM_BIN"
	elif [ -x "$DIR/result/bin/e2b-dev-vm" ]; then
		printf '%s\n' "$DIR/result/bin/e2b-dev-vm"
	else
		return 127
	fi
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

	vm status 2>/dev/null | grep -qE 'qemu: +running'
}

ssh_vm() {
	# No persistent known_hosts: a re-created VM (`e2b-dev-vm reset`, a fresh
	# state dir) has a new host key, and a stale entry would fail every
	# connection with "REMOTE HOST IDENTIFICATION HAS CHANGED"; the runner's
	# own ssh uses the same local-only policy.
	SSHPASS=$VM_PASSWORD sshpass -e ssh -p "$VM_PORT" \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o LogLevel=ERROR -o ConnectTimeout=10 \
		"$VM_HOST" "$@"
}

vm_provisioning_done() {
	# ssh comes up while cloud-init is still installing; the guest is ready
	# for provisioning once cloud-init reports done (a cold first boot runs
	# the full stage and reboots once before that).
	ssh_vm 'cloud-init status 2>/dev/null | grep -q "status: done"' 2>/dev/null
}

ensure_vm() {
	vm_state=${E2B_DEV_VM_DIR:-$PWD/e2b-dev-vm}
	vm_log=$vm_state/qemu.log

	if vm_running && vm_provisioning_done; then
		return 0
	fi

	if ! vm_running; then
		have_vm_runner || build_vm_runner || return 1

		runner=$(vm_runner) || return 1
		mkdir -p "$vm_state" 2>/dev/null || true

		# `vm up` is a foreground QEMU (Ctrl-A X quits it), so it can never be
		# the automated start: run it in its own session, detached from this
		# shell - no tty to stop on (SIGTTIN), no hangup, no supervisor's
		# process-group cleanup - and wait for the guest here instead.
		log "dev: starting the Ubuntu dev VM in the background (console -> $vm_log)"
		if command -v setsid >/dev/null 2>&1; then
			setsid "$runner" up </dev/null >>"$vm_log" 2>&1 &
		else
			nohup "$runner" up </dev/null >>"$vm_log" 2>&1 &
		fi
	fi

	i=0
	while [ "$i" -lt 240 ]; do
		if vm_provisioning_done; then
			return 0
		fi

		# ssh alone is not readiness - it comes up while cloud-init is still
		# installing. Give the start a short grace (disk creation, qemu boot),
		# then treat a VM nowhere to be seen as a failed start instead of
		# waiting out the whole budget.
		if [ "$i" -ge 6 ] && ! vm_running; then
			break
		fi

		i=$((i + 1))
		[ $((i % 12)) -ne 0 ] || log "dev: still waiting for the VM ($((i * 5))s; a cold first boot runs cloud-init)"
		sleep 5
	done

	log "dev: the VM did not become reachable on port $VM_PORT (see $vm_log)"
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

provision_artifacts() {
	if ssh_vm 'test -x /home/dev/ublk-fc/firecracker && test -f /home/dev/ublk-fc/vmlinux.bin' 2>/dev/null; then
		return 0
	fi

	ssh_vm 'mkdir -p /home/dev/ublk-fc' 2>/dev/null || return 1

	if [ -x "$ARTIFACT_DIR/firecracker" ] && [ -f "$ARTIFACT_DIR/vmlinux.bin" ]; then
		log "dev: restoring Firecracker test artifacts from $ARTIFACT_DIR"
		# POSIX sh: stream a tar over ssh (no process substitution here).
		if (cd "$ARTIFACT_DIR" && tar -cf - firecracker vmlinux.bin) | ssh_vm 'tar -C /home/dev/ublk-fc -xf -' 2>/dev/null; then
			:
		else
			log "dev: artifact copy failed; set E2B_FC_ARTIFACTS_DIR or fetch with 'make download-public-firecrackers'"
		fi

		return 0
	fi

	log "dev: fetching stock Firecracker artifacts into the VM (no host copies in $ARTIFACT_DIR)"
	ssh_vm "
		set -eu
		curl -fsSL -o /home/dev/ublk-fc/firecracker 			https://storage.googleapis.com/e2b-artifact-binaries/firecrackers/$FC_VERSION/amd64/firecracker
		curl -fsSL -o /home/dev/ublk-fc/vmlinux.bin 			https://storage.googleapis.com/e2b-artifact-binaries/kernels/$FC_KERNEL
		chmod +x /home/dev/ublk-fc/firecracker
	" 2>/dev/null || log "dev: artifact fetch failed; the ublk guest tests need a kernel with ublk_drv (see the ublk subproject note)"
}

print_status() {
	printf 'dev environment\n'
	printf '  vm:        '
	if vm_running; then
		printf 'running (ssh %s:%s)\n' "$VM_HOST" "$VM_PORT"
	else
		printf 'not running\n'
	fi
	if runner=$(vm_runner 2>/dev/null); then
		printf '  stop:      %s stop\n' "$runner"
	fi
	printf '  ublk:      '
	ssh_vm 'test -c /dev/ublk-control && echo present || echo missing' 2>/dev/null || printf 'unknown\n'
	printf '  silo:      '
	ssh_vm 'sudo -n docker ps --filter name=silo --format "{{.Status}}" 2>/dev/null | head -1' 2>/dev/null || printf 'unknown\n'
	printf '  nfs:       '
	ssh_vm 'sudo -n exportfs 2>/dev/null | grep /srv/nfs-cache | head -1' 2>/dev/null || printf 'unknown\n'
	printf '  artifacts: '
	ssh_vm 'test -x /home/dev/ublk-fc/firecracker && echo "firecracker + vmlinux.bin present" || echo missing' 2>/dev/null || printf 'unknown\n'
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
	provision_artifacts
	print_status
	;;
--ensure|--shell)
	ensure_vm || exit 1
	provision_services
	provision_artifacts
	print_status
	;;
*)
	log "usage: $0 [--ensure|--status|--services]"
	exit 2
	;;
esac
