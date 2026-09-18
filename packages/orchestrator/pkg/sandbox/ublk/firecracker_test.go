//go:build linux

package ublk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Firecracker tests boot a real guest whose root disk is a ublk device.
// They need the guest artifacts and KVM, and skip without them:
//
//	UBLK_TEST_FIRECRACKER=/path/to/firecracker
//	UBLK_TEST_KERNEL=/path/to/vmlinux.bin
//	UBLK_TEST_BUSYBOX=/bin/busybox   (optional, this is the default)
//
// The guest proves itself with a second ublk device: a raw one, no filesystem,
// that the init keeps writing its tick counter to. Reading that counter out of
// the host-side file the device serves is the check: the data went guest ->
// virtio-blk -> ublk device -> backend.
const (
	envFirecracker = "UBLK_TEST_FIRECRACKER"
	envKernel      = "UBLK_TEST_KERNEL"
	envBusybox     = "UBLK_TEST_BUSYBOX"

	guestRootfsSize = 64 << 20
	guestDataSize   = 1 << 20
)

type guestArtifacts struct {
	firecracker string
	kernel      string
	busybox     string
}

func requireGuest(t *testing.T) guestArtifacts {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("the Firecracker tests need root")
	}

	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("Firecracker needs /dev/kvm: %v", err)
	}

	artifacts := guestArtifacts{
		firecracker: os.Getenv(envFirecracker),
		kernel:      os.Getenv(envKernel),
		busybox:     os.Getenv(envBusybox),
	}
	if artifacts.busybox == "" {
		artifacts.busybox = "/bin/busybox"
	}

	if artifacts.firecracker == "" || artifacts.kernel == "" {
		t.Skipf("set %s and %s to run the Firecracker tests", envFirecracker, envKernel)
	}

	for _, path := range []string{artifacts.firecracker, artifacts.kernel, artifacts.busybox} {
		if _, err := os.Stat(path); err != nil {
			t.Skipf("missing artifact %s: %v", path, err)
		}
	}

	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}

	return artifacts
}

// runTool runs a host tool and fails the test with its output when it fails.
func runTool(t *testing.T, name string, args ...string) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

// buildGuestRootfs writes an ext4 image that boots straight into a static
// busybox init: no initramfs, no modules, nothing the test has to install
// inside the guest.
func buildGuestRootfs(t *testing.T, artifacts guestArtifacts, path string) {
	t.Helper()

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}

	if err := file.Truncate(guestRootfsSize); err != nil {
		t.Fatalf("sizing %s: %v", path, err)
	}

	if err := file.Close(); err != nil {
		t.Fatalf("closing %s: %v", path, err)
	}

	runTool(t, "mkfs.ext4", "-q", "-F", path)

	// The guest mounts the pseudo filesystems and then writes its tick counter
	// to the raw second disk, whose bytes the host can read without a
	// filesystem in the way. A console echo keeps the Firecracker log useful
	// when something goes wrong.
	initScript := `#!/bin/busybox sh
/bin/busybox mount -t proc proc /proc
/bin/busybox mount -t sysfs sysfs /sys
/bin/busybox mount -t devtmpfs devtmpfs /dev
echo "ublk-test: guest init up" > /dev/kmsg
n=0
while true; do
	n=$((n + 1))
	printf 'tick %s\n' "$n" | /bin/busybox dd of=/dev/vdb bs=512 seek=0 conv=notrunc 2>/dev/null
	/bin/busybox sync
	echo "ublk-test: tick $n" > /dev/console 2>/dev/null
	/bin/busybox sleep 1
done
`

	initPath := filepath.Join(t.TempDir(), "init")
	if err := os.WriteFile(initPath, []byte(initScript), 0o755); err != nil {
		t.Fatalf("writing the guest init: %v", err)
	}

	// debugfs writes the files into the image without mounting it, so the test
	// needs no loop device. The directories the init mounts have to exist.
	runTool(t, "debugfs", "-w", "-R", "mkdir /bin", path)
	runTool(t, "debugfs", "-w", "-R", "mkdir /proc", path)
	runTool(t, "debugfs", "-w", "-R", "mkdir /sys", path)
	runTool(t, "debugfs", "-w", "-R", "mkdir /dev", path)
	runTool(t, "debugfs", "-w", "-R", "mkdir /tmp", path)
	runTool(t, "debugfs", "-w", "-R", fmt.Sprintf("write %s /bin/busybox", artifacts.busybox), path)
	runTool(t, "debugfs", "-w", "-R", fmt.Sprintf("write %s /init", initPath), path)
	runTool(t, "debugfs", "-w", "-R", "sif /init mode 0100755", path)
	runTool(t, "debugfs", "-w", "-R", "sif /bin/busybox mode 0100755", path)
}

// openExistingFileBackend serves an existing file, so a device can be opened
// over a rootfs image the test built.
func openExistingFileBackend(t *testing.T, path string) *fileBackend {
	t.Helper()

	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}

	t.Cleanup(func() { _ = file.Close() })

	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	return &fileBackend{file: file, size: info.Size()}
}

// rawTick reads the tick counter the guest writes to the raw device, straight
// out of the file that device serves.
func rawTick(t *testing.T, path string) int {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()

	buf := make([]byte, 512)

	n, err := file.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0
	}

	text := string(bytes.TrimRight(buf[:n], "\x00"))
	if !strings.HasPrefix(text, "tick ") {
		return 0
	}

	count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(text, "tick ")))
	if err != nil {
		return 0
	}

	return count
}

// waitForTick waits for the guest's tick counter to pass want.
func waitForTick(t *testing.T, path string, want int, timeout time.Duration) int {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		if count := rawTick(t, path); count > want {
			return count
		}

		if time.Now().After(deadline) {
			t.Fatalf("the guest did not get past tick %d in %s (last read %d)", want, timeout, rawTick(t, path))
		}

		time.Sleep(time.Second)
	}
}

type fcBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type fcDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type fcMachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

type fcConfigFile struct {
	BootSource    fcBootSource    `json:"boot-source"`
	Drives        []fcDrive       `json:"drives"`
	MachineConfig fcMachineConfig `json:"machine-config"`
}

func fcBootArgs() string {
	return "console=ttyS0 root=/dev/vda rw init=/init panic=1"
}

// fcDrives puts the root filesystem on the first device and the raw tick
// device on the second; Firecracker names them /dev/vda and /dev/vdb.
func fcDrives(rootPath, dataPath string) []fcDrive {
	return []fcDrive{
		{
			DriveID:      "rootfs",
			PathOnHost:   rootPath,
			IsRootDevice: true,
			IsReadOnly:   false,
		},
		{
			DriveID:    "data",
			PathOnHost: dataPath,
		},
	}
}

// startFirecracker runs Firecracker and returns it with a cleanup that kills
// it. Its output goes to a log the test prints when it fails.
func startFirecracker(t *testing.T, artifacts guestArtifacts, args ...string) {
	t.Helper()

	logPath := filepath.Join(t.TempDir(), "firecracker.log")

	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating the Firecracker log: %v", err)
	}

	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), artifacts.firecracker, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting Firecracker: %v", err)
	}

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}

		_ = logFile.Close()

		if out, err := os.ReadFile(logPath); err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(lines) > 20 {
				lines = lines[len(lines)-20:]
			}

			t.Logf("firecracker log (tail):\n%s", strings.Join(lines, "\n"))
		}
	})
}

// guestSetup builds the rootfs image, a copy of it for the device to serve and
// the raw data device, and starts both ublk devices.
func guestSetup(t *testing.T, mgr *Manager, artifacts guestArtifacts) (rootDevice *Device, dataPath string) {
	t.Helper()

	dir := t.TempDir()
	imagePath := filepath.Join(dir, "rootfs.img")

	buildGuestRootfs(t, artifacts, imagePath)

	diskPath := filepath.Join(dir, "rootfs-disk.img")
	if err := copyFile(imagePath, diskPath); err != nil {
		t.Fatalf("copying the rootfs: %v", err)
	}

	ctx := t.Context()

	rootDevice, err := mgr.Open(ctx, openExistingFileBackend(t, diskPath), DefaultOptions())
	if err != nil {
		t.Fatalf("opening the rootfs device: %v", err)
	}

	t.Cleanup(func() {
		if err := rootDevice.Close(ctx); err != nil {
			t.Errorf("closing the rootfs device: %v", err)
		}
	})

	dataPath = filepath.Join(dir, "data.img")

	dataDevice, err := mgr.Open(ctx, newFileBackend(t, dataPath, guestDataSize), DefaultOptions())
	if err != nil {
		t.Fatalf("opening the data device: %v", err)
	}

	t.Cleanup(func() {
		if err := dataDevice.Close(ctx); err != nil {
			t.Errorf("closing the data device: %v", err)
		}
	})

	return rootDevice, dataPath
}

// TestFirecrackerBootFromUblkDevice boots a guest with a ublk device as its
// root disk and another one as a raw data disk. Everything the guest writes to
// the data disk has to show up in the file behind that device, which the test
// reads directly while the guest keeps running.
func TestFirecrackerBootFromUblkDevice(t *testing.T) { //nolint:paralleltest // boots a real guest: heavy, keep it serial
	mgr := requireUblk(t)
	artifacts := requireGuest(t)

	rootDevice, dataPath := guestSetup(t, mgr, artifacts)

	configPath := filepath.Join(t.TempDir(), "firecracker.json")

	config, err := json.Marshal(fcConfigFile{
		BootSource:    fcBootSource{KernelImagePath: artifacts.kernel, BootArgs: fcBootArgs()},
		Drives:        fcDrives(rootDevice.Path(), dataPath),
		MachineConfig: fcMachineConfig{VCPUCount: 1, MemSizeMiB: 256},
	})
	if err != nil {
		t.Fatalf("building the Firecracker config: %v", err)
	}

	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatalf("writing the Firecracker config: %v", err)
	}

	// The device the guest boots from is the one served by the ublk device
	// path, so the guest's root filesystem is this process's data path.
	if !strings.HasPrefix(rootDevice.Path(), "/dev/ublkb") {
		t.Fatalf("root device is %s, not a ublk device", rootDevice.Path())
	}

	startFirecracker(t, artifacts, "--no-api", "--config-file", configPath)

	tick := waitForTick(t, dataPath, 0, 3*time.Minute)
	t.Logf("guest booted from %s and wrote tick %d to the backend of its data device", rootDevice.Path(), tick)

	// The guest keeps writing; every new tick has to reach the backend too.
	final := waitForTick(t, dataPath, tick+1, 30*time.Second)
	t.Logf("guest progressed to tick %d in the backend", final)
}

// fcAPI is Firecracker's HTTP API over its unix socket.
type fcAPI struct {
	client *http.Client
}

func newFCAPI(t *testing.T, socket string) *fcAPI {
	t.Helper()

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer

			return dialer.DialContext(ctx, "unix", socket)
		},
	}

	return &fcAPI{client: &http.Client{Transport: transport, Timeout: 30 * time.Second}}
}

func (a *fcAPI) do(t *testing.T, method, path string, body any) {
	t.Helper()

	payload := []byte("")
	if body != nil {
		var err error

		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshalling %s %s: %v", method, path, err)
		}
	}

	req, err := http.NewRequestWithContext(t.Context(), method, "http://localhost"+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	out, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(out)))
	}
}

func waitForSocket(t *testing.T, path string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		if _, err := os.Stat(path); err == nil {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("Firecracker did not create %s", path)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// TestFirecrackerSnapshotResume pauses a guest whose root disk is a ublk
// device, snapshots it, brings it back from the snapshot and checks that the
// same devices keep serving it: writes made after the resume have to land in
// the backend next to the ones made before the pause.
func TestFirecrackerSnapshotResume(t *testing.T) { //nolint:paralleltest // boots a real guest: heavy, keep it serial
	mgr := requireUblk(t)
	artifacts := requireGuest(t)

	rootDevice, dataPath := guestSetup(t, mgr, artifacts)

	dir := t.TempDir()

	// First instance, configured over the API so it can be snapshotted later.
	socketA := filepath.Join(dir, "api-a.sock")
	apiA := newFCAPI(t, socketA)

	startFirecracker(t, artifacts, "--api-sock", socketA)
	waitForSocket(t, socketA, 30*time.Second)

	apiA.do(t, http.MethodPut, "/boot-source", fcBootSource{KernelImagePath: artifacts.kernel, BootArgs: fcBootArgs()})
	apiA.do(t, http.MethodPut, "/drives/rootfs", fcDrives(rootDevice.Path(), dataPath)[0])
	apiA.do(t, http.MethodPut, "/drives/data", fcDrives(rootDevice.Path(), dataPath)[1])
	apiA.do(t, http.MethodPut, "/machine-config", fcMachineConfig{VCPUCount: 1, MemSizeMiB: 256})
	apiA.do(t, http.MethodPut, "/actions", map[string]string{"action_type": "InstanceStart"})

	before := waitForTick(t, dataPath, 0, 3*time.Minute)
	t.Logf("guest reached tick %d before the pause", before)

	snapshotPath := filepath.Join(dir, "state.snap")
	memPath := filepath.Join(dir, "mem.snap")

	// Firecracker only saves a paused microVM.
	apiA.do(t, http.MethodPatch, "/vm", map[string]string{"state": "Paused"})

	apiA.do(t, http.MethodPut, "/snapshot/create", map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": snapshotPath,
		"mem_file_path": memPath,
	})

	if _, err := os.Stat(snapshotPath); err != nil {
		t.Fatalf("the snapshot was not written: %v", err)
	}

	// Second instance, resuming from the snapshot with the same devices.
	socketB := filepath.Join(dir, "api-b.sock")

	startFirecracker(t, artifacts, "--api-sock", socketB)
	waitForSocket(t, socketB, 30*time.Second)

	apiB := newFCAPI(t, socketB)
	apiB.do(t, http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path": snapshotPath,
		"mem_backend": map[string]string{
			"backend_path": memPath,
			"backend_type": "File",
		},
	})
	apiB.do(t, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})

	after := waitForTick(t, dataPath, before+1, 3*time.Minute)
	t.Logf("guest resumed and the backend reports tick %d (was %d before the pause)", after, before)
}

// copyFile duplicates a file, for the device to serve a copy of the rootfs the
// test built.
func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(to, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return err
	}

	return dst.Sync()
}
