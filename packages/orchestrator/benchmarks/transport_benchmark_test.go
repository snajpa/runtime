//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/nbd"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/nbd/testutils"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/ublk"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// BenchmarkTransportThroughput compares the two rootfs transports serving the
// same thing: one overlay over one writable cache, read and written by fio
// through the transport's device node. Each sub-benchmark brings its own
// cache up, runs every workload once and prints the numbers; the printed table
// is what goes into the subproject doc.
//
// Run it with -benchtime=1x, as root, inside the dev VM:
//
//	sudo $(which go) test -run='^$' -bench=BenchmarkTransportThroughput \
//	  -benchtime=1x -timeout=30m -v ./benchmarks/
func BenchmarkTransportThroughput(b *testing.B) {
	if os.Geteuid() != 0 {
		b.Skip("the transport comparison needs root")
	}

	if _, err := exec.LookPath("fio"); err != nil {
		b.Skip("fio is not installed")
	}

	for _, tc := range []struct {
		name  string
		start func(b *testing.B) (devicePath string, stop func())
	}{
		{"ublk-queues-1", startUblkDevice(1)},
		{"ublk-queues-4", startUblkDevice(4)},
		{"nbd", startNBDDevice()},
	} {
		b.Run(tc.name, func(b *testing.B) {
			path, stop := tc.start(b)
			defer stop()

			for _, workload := range transportWorkloads() {
				b.StartTimer()

				stats := runTransportFio(b, path, workload)

				b.StopTimer()
				// One metric per workload: a repeated unit would overwrite
				// the previous workload's number.
				b.ReportMetric(stats.bytesPerSecond/1e6, workload.name+"/MB/s")
				b.ReportMetric(stats.iops, workload.name+"/iops")
				b.Logf("%-14s %-10s %8.1f MB/s %10.0f iops", tc.name, workload.name, stats.bytesPerSecond/1e6, stats.iops)
			}
		})
	}
}

type transportWorkload struct {
	name string
	rw   string
	bs   string
}

func transportWorkloads() []transportWorkload {
	return []transportWorkload{
		{"seq-write-1m", "write", "1m"},
		{"seq-read-1m", "read", "1m"},
		{"rand-write-4k", "randwrite", "4k"},
		{"rand-read-4k", "randread", "4k"},
	}
}

// transportBackend is the part of the stack both transports serve: an overlay
// whose writable layer is a fresh cache file.
func transportBackend(b *testing.B) *block.Overlay {
	b.Helper()

	const size = 1 << 30

	source, err := testutils.NewZeroDevice(size, header.RootfsBlockSize)
	require.NoError(b, err)

	cache, err := block.NewCache(size, header.RootfsBlockSize, filepath.Join(b.TempDir(), "rootfs.cow"), false)
	require.NoError(b, err)

	b.Cleanup(func() { _ = cache.Close() })

	return block.NewOverlay(source, cache)
}

func startUblkDevice(queues int) func(b *testing.B) (string, func()) {
	return func(b *testing.B) (string, func()) {
		b.Helper()

		manager, err := ublk.NewManager()
		if err != nil {
			b.Skipf("ublk is not available: %v", err)
		}

		opts := ublk.DefaultOptions()
		opts.Queues = queues

		device, err := manager.Open(b.Context(), transportBackend(b), opts)
		require.NoError(b, err)

		return device.Path(), func() {
			if err := device.Close(context.WithoutCancel(b.Context())); err != nil {
				b.Errorf("closing the ublk device: %v", err)
			}

			if err := manager.Close(); err != nil {
				b.Errorf("closing the ublk manager: %v", err)
			}
		}
	}
}

func startNBDDevice() func(b *testing.B) (string, func()) {
	return func(b *testing.B) (string, func()) {
		b.Helper()

		featureFlags, err := featureflags.NewClient()
		require.NoError(b, err)

		pool, err := nbd.NewDevicePool(1)
		if err != nil {
			b.Skipf("the nbd module is not available: %v", err)
		}

		go pool.Populate(context.WithoutCancel(b.Context()))

		mount := nbd.NewDirectPathMount(transportBackend(b), pool, featureFlags)

		deviceIndex, err := mount.Open(b.Context())
		require.NoError(b, err)

		return nbd.GetDevicePath(deviceIndex), func() {
			ctx := context.WithoutCancel(b.Context())

			if err := mount.Close(ctx); err != nil {
				b.Errorf("closing the nbd mount: %v", err)
			}

			if err := pool.Close(ctx); err != nil {
				b.Errorf("closing the nbd pool: %v", err)
			}

			_ = featureFlags.Close(ctx)
		}
	}
}

type fioStats struct {
	bytesPerSecond float64
	iops           float64
}

type fioReport struct {
	Jobs []struct {
		Read struct {
			BandwidthBytes float64 `json:"bw_bytes"`
			IOPS           float64 `json:"iops"`
		} `json:"read"`
		Write struct {
			BandwidthBytes float64 `json:"bw_bytes"`
			IOPS           float64 `json:"iops"`
		} `json:"write"`
	} `json:"jobs"`
}

// runTransportFio runs one workload for a fixed ten seconds, directly against
// the transport's device node.
func runTransportFio(b *testing.B, devicePath string, workload transportWorkload) fioStats {
	b.Helper()

	const runtime = 10 * time.Second

	args := []string{
		"--name=" + workload.name,
		"--filename=" + devicePath,
		"--ioengine=libaio",
		"--direct=1",
		"--iodepth=16",
		"--rw=" + workload.rw,
		"--bs=" + workload.bs,
		"--size=256m",
		fmt.Sprintf("--runtime=%d", int(runtime.Seconds())),
		"--time_based=1",
		"--output-format=json",
	}

	out, err := exec.CommandContext(context.WithoutCancel(b.Context()), "fio", args...).Output()
	if err != nil {
		b.Fatalf("fio %s failed: %v", workload.name, err)
	}

	var report fioReport
	require.NoError(b, json.Unmarshal(out, &report))
	require.NotEmpty(b, report.Jobs)

	// fio reports the direction the workload used; the timing is the same
	// either way.
	if strings.HasPrefix(workload.rw, "read") || strings.HasPrefix(workload.rw, "randread") {
		return fioStats{bytesPerSecond: report.Jobs[0].Read.BandwidthBytes, iops: report.Jobs[0].Read.IOPS}
	}

	return fioStats{bytesPerSecond: report.Jobs[0].Write.BandwidthBytes, iops: report.Jobs[0].Write.IOPS}
}
