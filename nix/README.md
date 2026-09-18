# Nix dev environment

A Nix flake for storage-layer work on the e2b runtime: a dev shell with the
repo toolchain, and a dev VM that mirrors what e2b runs in production.

## Production fidelity

The dev VM is **Ubuntu 24.04**, because that is e2b's production host shape.
The evidence is in the repo itself:

- `embed/compose/scripts/preflight.sh` requires Ubuntu 24.04 (kernel >= 6.8,
  arm64 >= 6.10), glibc >= 2.34, 4 KiB pages, cgroup v2, `/dev/kvm`,
  `/dev/net/tun`, the `nbd` module, and `iptables`, `rsync`, `mkfs.ext4`,
  `tune2fs`, `e2fsck`, `ip`.
- `embed/compose/scripts/host-setup.sh` loads `nbd` (`nbds_max`), `tun`, `kvm`,
  writes the nbd udev rules, reserves hugepages and applies
  `net.ipv4.tcp_max_syn_backlog=65535`, `vm.max_map_count=1048576`.
- `embed/kubernetes/README.md` and `embed/compose/README.md` both recommend
  Ubuntu 24.04 (HWE kernel where noted), and `embed/terraform/gcp/main.tf`
  provisions Ubuntu-based GCE instances with nested virtualization and a
  startup script that installs Docker and brings the stack up.
- Production nodes run the orchestrator as a host binary (root for Firecracker,
  namespaces, NBD, cgroups), with Docker/containers only for datastores.

Nix's role is to make that environment reproducible: pin the cloud image, generate
the cloud-init, and run QEMU with the right shape. The guest is real Ubuntu, not
NixOS, so kernel, glibc, module and package behavior match prod.

## Dev shell

```sh
nix develop
go version
```

The shell follows `.tool-versions` where nixpkgs provides the tool (it resolves
`go_1_26`; 1.26.8 as of this writing) and skips optional tools that nixpkgs
does not have. `mockery` is not packaged at the pinned version; install it with
`go install github.com/vektra/mockery/v3@v3.7.0` when regenerating mocks.

If direnv is installed, `.envrc` prefers the flake and falls back to mise.

## Dev VM

```sh
nix build .#dev-vm
./result/bin/e2b-dev-vm up      # first boot runs cloud-init, then reboots
./result/bin/e2b-dev-vm ssh     # dev@127.0.0.1:2222, password e2b-dev
./result/bin/e2b-dev-vm reset   # delete the disk, cloud-init runs again
```

What the VM provides (all via cloud-init, mirroring e2b's own host setup):

- pinned Ubuntu 24.04 cloud image (`nix/ubuntu-vm.nix`), 200 GiB sparse qcow2
  overlay so the base image stays immutable and `reset` is instant;
- `linux-generic-hwe-24.04`, `nbd` (with `nbds_max=4096 max_part=16`), `tun`,
  the nbd udev rules, 2048 hugepages, e2b's sysctls;
- Docker with the compose v2 plugin (for `make local-infra`), Go 1.26.8,
  build tools, `fio`, `rsync`, `iptables`, `iproute2`, `e2fsprogs`;
- `dev`/`root` login (password `e2b-dev`), autologin on serial;
- a boot check written to `/var/log/e2b-dev-vm-boot-check.log` recording kernel,
  page size, module state, `/dev/kvm`, cgroup v2, glibc and hugepages.

Also run e2b's own gate inside the VM to confirm the host shape:

```sh
scp -P 2222 embed/compose/scripts/preflight.sh dev@127.0.0.1:/tmp/
./result/bin/e2b-dev-vm ssh 'sudo sh /tmp/preflight.sh'
```

`E2B_DEV_VM_SHARE=/path/to/worktree` exposes a host directory as a 9p mount
(`sudo mount -t 9p -o trans=virtio,version=9p2000.L host /mnt`); for real work
prefer cloning the repo inside the VM so builds run on the VM disk.

## Notes and known gaps

- The host must have nested virtualization (this host: `kvm_amd nested=1`), so
  Firecracker and `/dev/kvm` work inside the VM. The runner uses `-cpu host`.
- `ublk_drv` is probed by the boot check. If the HWE kernel does not ship it,
  the ublk work needs a newer kernel or a switch of the dev VM to Ubuntu 26.04
  (kernel 7.0, per `embed/compose/README.md`); the runner takes a different
  image via `E2B_DEV_VM_IMAGE` if needed.
- Template artifacts (kernels, firecrackers) are downloaded by the repo's
  `make download-public-*` targets, which expect `gsutil`; do that on the host
  (the dev shell has the Google Cloud SDK) and transfer, or install `gsutil`
  in the VM.
