//go:build linux

// W1 bench driver — ublk device benches for the perf-regression suite.
//
// This file is NOT part of the frozen tree: nix/perf/modules/w1.sh ships it and
// compiles it into the tree's ublk test binary with `go test -c -overlay`, so
// the device code under test is exactly the tree's. Opt-in gates:
// W1_BENCH=1 (measured job) and W1_ORACLE=1 (integrity fixture).
//
// Raw producer only (design §5/§6): it creates the fixture (file-backed sparse
// file, 2 queues, 1 MiB max I/O — the ublk/fio_test.go fixture), optionally
// pre-fills the device, runs one fio job from the caller's template, and writes
// fio.json + meta.json under W1_OUTDIR. Verdicts are the harness's job.
//
// Env (bench):
//
//	W1_BENCH=1        gate (required; otherwise the test skips)
//	W1_PROFILE        profile id (recorded)
//	W1_OUTDIR         output dir (fio.json, meta.json, fio.log, fio.job)
//	W1_BACKEND        backend sparse-file path (required)
//	W1_SIZE_MIB       backend size in MiB (default 1024)
//	W1_JOB            fio job template with %DEV% placeholder (required)
//	W1_PREFILL=0|1    pre-fill the device before the measured job
//	W1_PREFILL_JOB    pre-fill job template (required when W1_PREFILL=1)
//	W1_FIO            fio binary (default "fio")
//
// Env (oracle):
//
//	W1_ORACLE=1       gate
//	W1_OUTDIR         output dir (meta.json, oracle-fio.json, oracle-verify.json)
//	W1_BACKEND        backend sparse-file path (required)
//	W1_ORACLE_SIZE_MIB  fixture size in MiB (default 64)
//	W1_FIO            fio binary (default "fio")
package ublk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestW1Bench(t *testing.T) { //nolint:paralleltest // one device, one measured job: keep it serial
	if os.Getenv("W1_BENCH") != "1" {
		t.Skip("W1 bench is opt-in (set W1_BENCH=1)")
	}

	outDir := os.Getenv("W1_OUTDIR")
	backendPath := os.Getenv("W1_BACKEND")
	jobPath := os.Getenv("W1_JOB")

	sizeMiB, err := strconv.Atoi(envOr("W1_SIZE_MIB", "1024"))
	if err != nil || sizeMiB <= 0 {
		sizeMiB = 1024
	}

	meta := map[string]any{
		"record":     "w1-bench-meta",
		"bench":      "W1",
		"profile":    os.Getenv("W1_PROFILE"),
		"ok":         false,
		"started_at": time.Now().UTC().Format(time.RFC3339),
	}

	if outDir == "" || backendPath == "" || jobPath == "" {
		meta["error"] = "W1_OUTDIR, W1_BACKEND and W1_JOB are required"
		meta["error_class"] = "setup"
		writeW1Meta(outDir, meta)
		t.Fatal(meta["error"])
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		meta["error"] = fmt.Sprintf("creating %s: %v", outDir, err)
		meta["error_class"] = "setup"
		writeW1Meta(outDir, meta)
		t.Fatal(meta["error"])
	}

	defer func() {
		meta["finished_at"] = time.Now().UTC().Format(time.RFC3339)
		writeW1Meta(outDir, meta)
	}()

	if os.Geteuid() != 0 {
		meta["error"] = "the W1 bench needs root (ublk control device)"
		meta["error_class"] = "setup"
		t.Fatal(meta["error"])
	}

	fioBin, err := exec.LookPath(envOr("W1_FIO", "fio"))
	if err != nil {
		meta["error"] = fmt.Sprintf("fio not found: %v", err)
		meta["error_class"] = "setup"
		t.Fatal(meta["error"])
	}

	meta["fio_version"] = w1CommandOutput(fioBin, "--version")

	mgr, err := NewManager()
	if err != nil {
		meta["error"] = fmt.Sprintf("ublk control device: %v", err)
		meta["error_class"] = "setup"
		t.Fatal(meta["error"])
	}

	t.Cleanup(func() { _ = mgr.Close() })

	ctx := context.Background()
	backend := newFileBackend(t, backendPath, int64(sizeMiB)<<20)

	opts := DefaultOptions()
	opts.Queues = 2
	opts.MaxIOBufBytes = 1 << 20

	dev, err := mgr.Open(ctx, backend, opts)
	if err != nil {
		meta["error"] = fmt.Sprintf("ublk Open: %v", err)
		meta["error_class"] = "setup"
		t.Fatal(meta["error"])
	}

	t.Cleanup(func() {
		if err := dev.Close(context.Background()); err != nil {
			t.Errorf("device close: %v", err)
		}
	})

	meta["device"] = dev.Path()
	meta["backend"] = backendPath
	meta["backend_bytes"] = int64(sizeMiB) << 20
	meta["options"] = dev.Options()
	meta["loadavg_before"] = readW1Trim("/proc/loadavg")
	meta["kernel"] = readW1Trim("/proc/sys/kernel/osrelease")

	runFio := func(label, jobIn, outPrefix string) error {
		body, err := os.ReadFile(jobIn)
		if err != nil {
			return fmt.Errorf("reading %s: %w", label, err)
		}

		jobOut := filepath.Join(outDir, label+".job")
		if err := os.WriteFile(jobOut,
			[]byte(strings.ReplaceAll(string(body), "%DEV%", dev.Path())), 0o644); err != nil {
			return err
		}

		logPath := filepath.Join(outDir, label+".log")

		logf, err := os.Create(logPath)
		if err != nil {
			return err
		}
		defer logf.Close()

		cmd := exec.CommandContext(context.Background(), fioBin,
			"--output-format=json", "--output="+filepath.Join(outDir, outPrefix+".json"), jobOut)
		cmd.Stdout = logf
		cmd.Stderr = logf

		t0 := time.Now()

		err = cmd.Run()

		meta[label+"_wall_s"] = w1Round3(time.Since(t0).Seconds())

		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				meta[label+"_rc"] = ee.ExitCode()
			} else {
				meta[label+"_rc"] = -1
			}

			return fmt.Errorf("%s fio failed: %w (see %s)", label, err, logPath)
		}

		meta[label+"_rc"] = 0

		return nil
	}

	if envOr("W1_PREFILL", "0") == "1" {
		prefillJob := os.Getenv("W1_PREFILL_JOB")
		if prefillJob == "" {
			meta["error"] = "W1_PREFILL_JOB is required when W1_PREFILL=1"
			meta["error_class"] = "setup"
			t.Fatal(meta["error"])
		}

		meta["prefill"] = true

		if err := runFio("prefill", prefillJob, "prefill"); err != nil {
			meta["error"] = err.Error()
			meta["error_class"] = "measurement"
			t.Fatal(meta["error"])
		}
	} else {
		meta["prefill"] = false
	}

	if err := runFio("fio", jobPath, "fio"); err != nil {
		meta["error"] = err.Error()
		meta["error_class"] = "measurement"
		t.Fatal(meta["error"])
	}

	// The device barrier after the measured window: it makes acknowledged
	// writes visible in the backend before the device is torn down.
	if err := dev.Sync(ctx); err != nil {
		meta["error"] = fmt.Sprintf("device Sync: %v", err)
		meta["error_class"] = "measurement"
		t.Fatal(meta["error"])
	}

	meta["loadavg_after"] = readW1Trim("/proc/loadavg")
	meta["ok"] = true
}

// TestW1Oracle is the step-0 integrity fixture: mixed direct I/O with fio's
// crc32c verification through the device, then the same regions verified
// against the backend file with the device gone (the ublk/fio_test.go pattern).
func TestW1Oracle(t *testing.T) { //nolint:paralleltest // real device: keep it serial
	if os.Getenv("W1_ORACLE") != "1" {
		t.Skip("W1 oracle is opt-in (set W1_ORACLE=1)")
	}

	outDir := os.Getenv("W1_OUTDIR")
	backendPath := os.Getenv("W1_BACKEND")

	sizeMiB, err := strconv.Atoi(envOr("W1_ORACLE_SIZE_MIB", "64"))
	if err != nil || sizeMiB <= 0 {
		sizeMiB = 64
	}

	meta := map[string]any{
		"record":     "w1-oracle-meta",
		"bench":      "W1",
		"ok":         false,
		"started_at": time.Now().UTC().Format(time.RFC3339),
	}

	if outDir == "" || backendPath == "" {
		meta["error"] = "W1_OUTDIR and W1_BACKEND are required"
		meta["error_class"] = "setup"
		writeW1Meta(outDir, meta)
		t.Fatal(meta["error"])
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		meta["error"] = fmt.Sprintf("creating %s: %v", outDir, err)
		meta["error_class"] = "setup"
		writeW1Meta(outDir, meta)
		t.Fatal(meta["error"])
	}

	defer func() {
		meta["finished_at"] = time.Now().UTC().Format(time.RFC3339)
		writeW1Meta(outDir, meta)
	}()

	if os.Geteuid() != 0 {
		meta["error"] = "the W1 oracle needs root (ublk control device)"
		meta["error_class"] = "setup"
		t.Fatal(meta["error"])
	}

	fioBin, err := exec.LookPath(envOr("W1_FIO", "fio"))
	if err != nil {
		meta["error"] = fmt.Sprintf("fio not found: %v", err)
		meta["error_class"] = "setup"
		t.Fatal(meta["error"])
	}

	meta["fio_version"] = w1CommandOutput(fioBin, "--version")

	mgr, err := NewManager()
	if err != nil {
		meta["error"] = fmt.Sprintf("ublk control device: %v", err)
		meta["error_class"] = "setup"
		t.Fatal(meta["error"])
	}

	t.Cleanup(func() { _ = mgr.Close() })

	ctx := context.Background()
	backend := newFileBackend(t, backendPath, int64(sizeMiB)<<20)

	opts := DefaultOptions()
	opts.Queues = 2
	opts.MaxIOBufBytes = 1 << 20

	dev, err := mgr.Open(ctx, backend, opts)
	if err != nil {
		meta["error"] = fmt.Sprintf("ublk Open: %v", err)
		meta["error_class"] = "setup"
		t.Fatal(meta["error"])
	}

	meta["device"] = dev.Path()
	meta["backend"] = backendPath
	meta["backend_bytes"] = int64(sizeMiB) << 20
	meta["options"] = dev.Options()
	meta["loadavg_before"] = readW1Trim("/proc/loadavg")

	runFio := func(label, job string) error {
		jobPath := filepath.Join(outDir, label+".job")
		if err := os.WriteFile(jobPath, []byte(job), 0o600); err != nil {
			return err
		}

		logPath := filepath.Join(outDir, label+".log")

		logf, err := os.Create(logPath)
		if err != nil {
			return err
		}
		defer logf.Close()

		cmd := exec.CommandContext(context.Background(), fioBin,
			"--output-format=json", "--output="+filepath.Join(outDir, label+".json"), jobPath)
		cmd.Stdout = logf
		cmd.Stderr = logf

		return cmd.Run()
	}

	writeJob := fmt.Sprintf(`[global]
ioengine=io_uring
direct=1
verify=crc32c
group_reporting=1
filename=%s
[randwrite]
rw=randwrite
bs=4k
size=%dm
`, dev.Path(), sizeMiB)

	if err := runFio("oracle-fio", writeJob); err != nil {
		meta["error"] = fmt.Sprintf("oracle write+verify pass failed: %v", err)
		meta["error_class"] = "integrity"
		t.Fatal(meta["error"])
	}

	if err := dev.Sync(ctx); err != nil {
		meta["error"] = fmt.Sprintf("device Sync: %v", err)
		meta["error_class"] = "integrity"
		t.Fatal(meta["error"])
	}

	if err := dev.Close(ctx); err != nil {
		meta["error"] = fmt.Sprintf("device Close: %v", err)
		meta["error_class"] = "integrity"
		t.Fatal(meta["error"])
	}

	if err := backend.Sync(); err != nil {
		meta["error"] = fmt.Sprintf("backend sync: %v", err)
		meta["error_class"] = "integrity"
		t.Fatal(meta["error"])
	}

	// Read the written range straight from the backend file and let fio verify
	// the blocks that went through the device (the device is gone now).
	verifyJob := fmt.Sprintf(`[global]
ioengine=psync
direct=1
verify=crc32c
group_reporting=1
filename=%s
[verify-randwrite]
rw=read
bs=4k
size=%dm
`, backendPath, sizeMiB)

	if err := runFio("oracle-verify", verifyJob); err != nil {
		meta["error"] = fmt.Sprintf("oracle backend verify pass failed: %v", err)
		meta["error_class"] = "integrity"
		t.Fatal(meta["error"])
	}

	meta["loadavg_after"] = readW1Trim("/proc/loadavg")
	meta["ok"] = true
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func writeW1Meta(outDir string, meta map[string]any) {
	if outDir == "" {
		return
	}

	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return
	}

	_ = os.WriteFile(filepath.Join(outDir, "meta.json"), append(body, '\n'), 0o644)
}

func readW1Trim(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(body))
}

func w1CommandOutput(name string, args ...string) string {
	out, err := exec.CommandContext(context.Background(), name, args...).Output()
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(out))
}

func w1Round3(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}
