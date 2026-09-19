{ pkgs }:
let
  # Pinned Ubuntu 24.04 LTS cloud image. Ubuntu 24.04 is e2b's production host
  # floor (embed/compose/scripts/preflight.sh and embed/kubernetes/README.md):
  # kernel >= 6.8 (arm64 >= 6.10), glibc >= 2.34, 4 KiB pages, cgroup v2, KVM,
  # tun, nbd, iptables/rsync/e2fsprogs/iproute2.
  image = pkgs.fetchurl {
    url = "https://cloud-images.ubuntu.com/releases/noble/release-20260911/ubuntu-24.04-server-cloudimg-amd64.img";
    hash = "sha256-YSssDMG8QTpsuMOP1hF5TK8PK0NsUAE9izeU2xKtc1Q=";
    name = "ubuntu-24.04-server-cloudimg-amd64.img";
  };

  metaData = pkgs.writeText "meta-data" ''
    instance-id: e2b-dev-vm
    local-hostname: e2b-dev
  '';

  # cloud-init mirrors what e2b's own host setup does (embed/compose/scripts/
  # host-setup.sh): kernel modules, nbd options, udev rules, sysctls and
  # hugepages, plus the packages preflight.sh expects on a production host.
  # linux-generic-hwe-24.04 is what the compose README tells hosts to run.
  # Go is a dev-only addition so the stack can be built and tested inside the
  # same kernel/root environment it runs in.
  userData = pkgs.writeText "user-data" ''
    #cloud-config
    hostname: e2b-dev
    manage_etc_hosts: true
    ssh_pwauth: true
    package_update: true
    package_upgrade: false
    packages:
      - build-essential
      - busybox-static
      - ca-certificates
      - curl
      - docker.io
      - docker-compose-v2
      - e2fsprogs
      - fio
      - git
      - gnupg
      - iptables
      - iproute2
      - jq
      - linux-generic-hwe-24.04
      - nfs-kernel-server
      - pkg-config
      - python3-venv
      - rsync
      - socat
      - tmux
      - unzip
    users:
      - name: dev
        gecos: e2b dev
        groups: [adm, sudo, docker, kvm]
        shell: /bin/bash
        lock_passwd: false
        sudo: ["ALL=(ALL) NOPASSWD:ALL"]
    chpasswd:
      expire: false
      users:
        - {name: root, password: e2b-dev, type: text}
        - {name: dev, password: e2b-dev, type: text}
    write_files:
      - path: /etc/modules-load.d/e2b.conf
        content: |
          nbd
          tun
      - path: /etc/modprobe.d/e2b-nbd.conf
        content: |
          options nbd nbds_max=4096 max_part=16
      - path: /etc/udev/rules.d/97-nbd-device.rules
        content: |
          KERNEL=="nbd*", GROUP="disk", MODE="0660"
          ACTION=="add|change", KERNEL=="nbd*", OPTIONS:="nowatch"
      - path: /etc/sysctl.d/90-e2b.conf
        content: |
          vm.nr_hugepages=2048
          net.ipv4.tcp_max_syn_backlog=65535
          vm.max_map_count=1048576
      - path: /etc/modules-load.d/e2b-ublk.conf
        content: |
          ublk_drv
      - path: /etc/systemd/system/silo.service
        content: |
          [Unit]
          Description=Silo object store (S3-compatible) for e2b dev/test work
          After=docker.service
          Requires=docker.service

          [Service]
          Restart=always
          ExecStartPre=-/usr/bin/docker rm -f silo
          ExecStart=/usr/bin/docker run --rm --name silo -p 9000:9000 -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin -v /srv/silo-data:/data pgsty/silo:latest server /data
          ExecStop=/usr/bin/docker stop silo

          [Install]
          WantedBy=multi-user.target
      - path: /etc/exports.d/e2b.exports
        content: |
          /srv/nfs-cache 127.0.0.1(rw,sync,no_subtree_check,no_root_squash)
      - path: /usr/local/bin/e2b-dev-vm-boot-check
        permissions: '0755'
        content: |
          #!/bin/sh
          # Records the host facts the e2b stack cares about. Read it with
          # `e2b-dev-vm ssh` -> `cat /var/log/e2b-dev-vm-boot-check.log`.
          exec >/var/log/e2b-dev-vm-boot-check.log 2>&1
          set -x
          uname -r
          getconf PAGESIZE
          modprobe nbd nbds_max=4096 max_part=16
          modprobe ublk_drv || echo "ublk_drv: not available on this kernel"
          lsmod | grep -E '^(nbd|ublk_drv)'
          ls -l /dev/kvm /dev/net/tun
          [ -f /sys/fs/cgroup/cgroup.controllers ] && echo "cgroup v2: yes"
          ldd --version | head -1
          grep -E 'HugePages_Total|Hugepagesize' /proc/meminfo
    runcmd:
      - [systemctl, enable, --now, docker]
      - [systemctl, enable, --now, ssh]
      - [sh, -c, "sysctl --system"]
      - [sh, -c, "udevadm control --reload && udevadm trigger"]
      - [sh, -c, "usermod -aG docker,kvm dev"]
      - [sh, -c, "mkdir -p /srv/silo-data /srv/nfs-cache && chown 1000:1000 /srv/silo-data"]
      - [sh, -c, "systemctl enable --now silo.service"]
      - [sh, -c, "systemctl enable --now nfs-kernel-server && exportfs -ra"]
      - [sh, -c, "curl -fsSL https://go.dev/dl/go1.26.8.linux-amd64.tar.gz -o /tmp/go.tgz && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz && printf 'export PATH=$PATH:/usr/local/go/bin\n' > /etc/profile.d/go.sh"]
      - [/usr/local/bin/e2b-dev-vm-boot-check]
    power_state:
      mode: reboot
      message: "rebooting into linux-generic-hwe-24.04"
      timeout: 60
      condition: true
  '';

  # NoCloud seed: cloud images auto-detect a filesystem with the cidata label.
  seed = pkgs.runCommand "e2b-dev-vm-seed.iso" {
    nativeBuildInputs = [ pkgs.xorriso ];
  } ''
    mkdir -p seed
    cp ${metaData} seed/meta-data
    cp ${userData} seed/user-data
    xorriso -as mkisofs -output "$out" -volid cidata -joliet -rock seed/meta-data seed/user-data
  '';

  runner = pkgs.writeShellApplication {
    name = "e2b-dev-vm";
    runtimeInputs = [ pkgs.qemu pkgs.openssh pkgs.coreutils pkgs.procps ];
    text = ''
      IMAGE="''${E2B_DEV_VM_IMAGE:-${image}}"
      SEED="''${E2B_DEV_VM_SEED:-${seed}}"
      DIR="''${E2B_DEV_VM_DIR:-$PWD/e2b-dev-vm}"
      DISK="$DIR/disk.qcow2"
      CPUS="''${E2B_DEV_VM_CPUS:-16}"
      MEM="''${E2B_DEV_VM_MEM:-32768}"
      SSH_PORT="''${E2B_DEV_VM_SSH_PORT:-2222}"

      usage() {
        cat <<'EOF'
      e2b-dev-vm — Ubuntu 24.04 dev VM shaped like an e2b production host

      Usage: e2b-dev-vm <command>

      Commands:
        up      create the disk if needed and start QEMU in the foreground
        ssh     ssh into the running VM (dev@127.0.0.1, password e2b-dev)
        status  print image/seed/disk paths and whether QEMU is running
        stop    power this state directory's VM off (SIGTERM to its recorded QEMU pid)
        reset   delete the VM disk (asks for confirmation)
        help    this text

      Environment:
        E2B_DEV_VM_DIR       VM state directory (default: ./e2b-dev-vm)
        E2B_DEV_VM_CPUS      vCPUs (default: 16)
        E2B_DEV_VM_MEM       memory in MiB (default: 32768)
        E2B_DEV_VM_SSH_PORT  host SSH port (default: 2222)
        E2B_DEV_VM_SHARE     optional host directory exposed as a 9p mount

      Quit QEMU: Ctrl-A X (foreground `up`); a VM started in the background by
      `make dev` is stopped with `e2b-dev-vm stop`. The guest keeps its state
      in the qcow2 disk; delete it with `reset` to boot a fresh VM (cloud-init
      runs again).
      EOF
      }

      cmd_up() {
        mkdir -p "$DIR"
        if [ ! -f "$DISK" ]; then
          echo "e2b-dev-vm: creating $DISK (200 GiB sparse qcow2 over the pinned Ubuntu 24.04 image)"
          qemu-img create -f qcow2 -F qcow2 -b "$IMAGE" "$DISK" 200G >/dev/null
        fi
        set -- -name e2b-dev-vm -machine q35 -enable-kvm -cpu host \
          -smp "$CPUS" -m "$MEM" \
          -device virtio-rng-pci \
          -drive "file=$DISK,if=virtio,format=qcow2,discard=unmap" \
          -drive "file=$SEED,if=virtio,format=raw,readonly=on" \
          -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22" \
          -device virtio-net-pci,netdev=net0
        if [ -n "''${E2B_DEV_VM_SHARE:-}" ]; then
          set -- "$@" -virtfs "local,path=$E2B_DEV_VM_SHARE,mount_tag=host,security_model=none,multidevs=remap"
        fi
        # Record this VM's pid and its start time before the exec (both survive
        # it): stop and status act on this identity only, never on a
        # process-wide pattern.
        echo "$$ $(awk '{print $22}' "/proc/$$/stat")" >"$DIR/qemu.pid"
        echo "e2b-dev-vm: starting; ssh with 'e2b-dev-vm ssh' (Ctrl-A X quits QEMU)"
        exec qemu-system-x86_64 "$@" -nographic
      }

      cmd_ssh() {
        exec ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
          -o LogLevel=ERROR -p "$SSH_PORT" dev@127.0.0.1
      }

      # Prints the pid of this state directory's QEMU, or nothing. The pid file
      # records the pid and its start time; both are re-verified against the
      # live process (rejecting PID reuse), whose executable must be
      # qemu-system-x86_64 itself or the Nix wrapper .qemu-system-x86_64-wrapped
      # (only those terminal basenames) and whose argv must carry this VM's exact
      # -drive
      # file= argument (delimiter included). Anything else is stale or foreign
      # and is refused.
      vm_pid() {
        [ -f "$DIR/qemu.pid" ] || return 1
        pid=""
        start=""
        read -r pid start <"$DIR/qemu.pid" || return 1
        case "$pid" in
          *[!0-9]*|"") return 1 ;;
        esac
        case "$start" in
          *[!0-9]*|"") return 1 ;;
        esac
        [ -r "/proc/$pid/stat" ] || return 1
        live=$(awk '{print $22}' "/proc/$pid/stat" 2>/dev/null) || return 1
        [ "$live" = "$start" ] || return 1
        exe=$(readlink "/proc/$pid/exe" 2>/dev/null) || return 1
        case "$exe" in
          */qemu-system-x86_64|*/.qemu-system-x86_64-wrapped) ;;
          *) return 1 ;;
        esac
        tr '\0' '\n' <"/proc/$pid/cmdline" 2>/dev/null | awk -v disk="$DISK" '
          NR == 1 && $0 !~ /^(.*\/)?qemu-system-x86_64$/ { exit 1 }
          prev == "-drive" && index($0, "file=" disk ",") == 1 { found = 1 }
          { prev = $0 }
          END { exit(found ? 0 : 1) }
        ' || return 1
        printf '%s\n' "$pid"
      }

      cmd_status() {
        echo "image: $IMAGE"
        echo "seed:  $SEED"
        echo "dir:   $DIR"
        if [ -f "$DISK" ]; then echo "disk:  $DISK (present)"; else echo "disk:  $DISK (not created yet)"; fi
        if pid=$(vm_pid); then echo "qemu:  running (pid $pid)"; else echo "qemu:  not running"; fi
      }

      cmd_stop() {
        if ! pid=$(vm_pid); then
          if [ -f "$DIR/qemu.pid" ]; then
            echo "e2b-dev-vm: $DIR/qemu.pid does not name this VM's live QEMU; refusing to signal anything (stale or foreign state - inspect it or run reset)"
            return 1
          fi
          echo "e2b-dev-vm: not running"
          return 0
        fi
        kill -TERM "$pid" || { echo "e2b-dev-vm: cannot signal pid $pid"; return 1; }
        i=0
        while [ "$i" -lt 40 ] && [ -d "/proc/$pid" ]; do
          i=$((i + 1))
          sleep 0.25
        done
        if [ -d "/proc/$pid" ]; then
          echo "e2b-dev-vm: still running after SIGTERM"
          return 1
        fi
        rm -f "$DIR/qemu.pid"
        echo "e2b-dev-vm: stopped"
      }

      cmd_reset() {
        [ -f "$DISK" ] || { echo "e2b-dev-vm: no disk at $DISK"; exit 0; }
        printf 'delete %s? [y/N] ' "$DISK"
        read -r answer
        case "$answer" in
          y|Y) rm -f "$DISK"; echo "e2b-dev-vm: disk deleted" ;;
          *) echo "e2b-dev-vm: aborted" ;;
        esac
      }

      case "''${1:-}" in
        up) cmd_up ;;
        ssh) cmd_ssh ;;
        status) cmd_status ;;
        stop) cmd_stop ;;
        reset) cmd_reset ;;
        ""|help|-h|--help) usage ;;
        *) usage; exit 2 ;;
      esac
    '';
  };
in
{
  inherit image seed runner;
}
