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
- `ublk_drv` is present in the HWE kernel (validated 2026-09-18: Ubuntu
  24.04.5, kernel 7.0.0-31-generic, module at
  `/lib/modules/7.0.0-31-generic/kernel/drivers/block/ublk_drv.ko.zst`), so
  the ublk transport can be developed here directly. The runner accepts a
  different image via `E2B_DEV_VM_IMAGE` if a future kernel change needs it.
## Running the ublk transport tests in the VM

The kernel-touching tests build on the host and run inside the VM, so the host
never creates ublk devices:

```sh
# on the host
go test -c -o /tmp/ublk.test ./packages/orchestrator/pkg/sandbox/ublk/
go test -c -o /tmp/rootfs.test ./packages/orchestrator/pkg/sandbox/rootfs/
scp -P 2222 /tmp/ublk.test /tmp/rootfs.test dev@127.0.0.1:/tmp/

# in the VM
sudo modprobe ublk_drv     # not loaded by default after a reboot
sudo /tmp/ublk.test -test.v
sudo /tmp/rootfs.test -test.run TestUblkProviderOverlayLifecycle -test.v
```

Prerequisites inside the VM:

- root, and `/dev/kvm` for the Firecracker guest tests (the runner enables
  nested virtualization with `-cpu host`);
- `fio`, `mkfs.ext4` and `debugfs` for the device tests (all in the image);
- `busybox-static` (`/bin/busybox`) for the rootfs the guest tests build:
  `sudo apt-get install -y busybox-static`;
- a kernel and a Firecracker binary for the guest tests, passed as
  `UBLK_TEST_KERNEL` and `UBLK_TEST_FIRECRACKER`. The public artifacts work and
  need no credentials, over plain HTTPS:
  `https://storage.googleapis.com/e2b-artifact-binaries/kernels/vmlinux-6.1.158/vmlinux.bin`
  and
  `https://storage.googleapis.com/e2b-artifact-binaries/firecrackers/v1.14-0.2.0/amd64/firecracker`.
  `UBLK_TEST_BUSYBOX` overrides the busybox path. The guest tests skip when the
  artifacts are missing, the same way the fio tests skip without fio.

If a test kills a daemon with a request still in flight, the device it leaves
behind can wedge the driver's control mutex, and every later device operation
blocks with it. Reboot the VM in that case instead of hunting the device: the
daemon-side fix is in the transport, but a device already in that state cannot
be cleaned up any other way.

## Two easy ways to develop

The product is validated on Ubuntu, as the repo's spec requires
(`embed/compose/scripts/preflight.sh`, `host-setup.sh`); Nix defines that
environment and brings it up. Two commands are all a developer needs, on any
machine with Nix:

```sh
make dev        # enter the dev environment: Nix shell + Ubuntu dev VM + services
make tests      # validate changes: host build/format/lint/tests, then the VM suites
```

- `make dev` enters `nix develop`, and the shell's hook runs
  `nix/scripts/dev.sh --ensure`: it builds the VM runner if needed, starts the
  VM, and provisions the services inside it. Re-running is cheap and safe;
  `E2B_DEV_AUTO=0 make dev` gives a plain shell without the bring-up.
- `make tests` runs `nix/scripts/dev-tests.sh`: `go build` for the modules,
  `gofmt` on changed files, `golangci-lint` and `go test` for changed packages,
  and then the root-gated storage suites **inside the Ubuntu VM** (the only
  place the host-shape requirements hold). `--no-vm` skips the VM half,
  `--base <rev>` changes the diff base (default `origin/main`).
- `make rehearsal` runs the S3 mixed-version rehearsal (below).

## Services inside the VM

`nix/scripts/dev.sh` provisions, and `make dev` keeps alive:

| service | why |
|---------|-----|
| `ublk_drv` | the ublk transport's control plane (`/dev/ublk-control`) |
| Silo (`pgsty/silo`) | the S3-compatible object store the storage code and the rehearsal use (S-53) |
| NFS export `/srv/nfs-cache` | the chunk-cache path (`WrapInNFSCache`) can be pointed at it |
| `busybox-static` | the guest rootfs the Firecracker tests build |

State lives on the VM disk (`e2b-dev-vm/disk.qcow2`); `./result/bin/e2b-dev-vm
reset` is the explicit way to start from scratch.

## S3 rehearsal (mixed versions on one store)

`nix/rehearsal/` builds one driver against two runtime checkouts — an old one
and a new one — and runs the upgrade and rollback legs against the Silo store
inside the VM: new writes/old reads, old writes/new reads, and existence checks
proving nothing is stranded. Profiles (`tiny`/`small`/`big`/`auto`) size the run
to the machine. See `nix/rehearsal/README.md`.
