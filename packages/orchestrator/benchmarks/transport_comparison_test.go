//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/nbd"
	nbdtestutils "github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/nbd/testutils"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/rootfs"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

type fakeTransportEndpoint struct {
	path        string
	closeErr    error
	closeCtxErr error
	closeCount  int
}

func (e *fakeTransportEndpoint) Path() (string, error) {
	return e.path, nil
}

func (e *fakeTransportEndpoint) Close(ctx context.Context) error {
	e.closeCount++
	e.closeCtxErr = ctx.Err()
	return e.closeErr
}

func TestRunTransportObservationProducesValidSampleAfterCorrectness(t *testing.T) {
	endpoint := &fakeTransportEndpoint{path: "/dev/nbd-test"}
	verified := false
	measured := false

	observation := runTransportObservation(
		context.Background(),
		transportArm{
			Name:      "candidate-nbd",
			Tree:      "candidate",
			Requested: transportNBD,
			Start: func(context.Context) (transportEndpoint, transportKind, error) {
				return endpoint, transportNBD, nil
			},
		},
		transportWorkload{Name: "read"},
		func(_ context.Context, path string) error {
			verified = path == endpoint.path
			return nil
		},
		func(_ context.Context, path string, _ transportWorkload) (transportMetrics, error) {
			measured = path == endpoint.path
			return transportMetrics{BytesPerSecond: 100, IOPS: 20, P50: time.Millisecond}, nil
		},
	)

	require.Equal(t, transportSampleValid, observation.Status)
	require.True(t, verified)
	require.True(t, measured)
	require.Equal(t, 1, endpoint.closeCount)
	require.Equal(t, 100.0, observation.Metrics.BytesPerSecond)
}

func TestRunTransportObservationUsesIndependentCleanupContext(t *testing.T) {
	endpoint := &fakeTransportEndpoint{path: "/dev/nbd-test"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	observation := runTransportObservation(
		ctx,
		transportArm{
			Name:      "candidate-nbd",
			Tree:      "candidate",
			Requested: transportNBD,
			Start: func(context.Context) (transportEndpoint, transportKind, error) {
				return endpoint, transportNBD, nil
			},
		},
		transportWorkload{Name: "read"},
		func(context.Context, string) error { return nil },
		func(context.Context, string, transportWorkload) (transportMetrics, error) {
			return transportMetrics{BytesPerSecond: 1, IOPS: 1}, nil
		},
	)

	require.Equal(t, transportSampleValid, observation.Status)
	require.NoError(t, endpoint.closeCtxErr)
	require.Equal(t, 1, endpoint.closeCount)
}

func TestRunTransportObservationDoesNotCountFallbackAsUblk(t *testing.T) {
	endpoint := &fakeTransportEndpoint{path: "/dev/nbd-fallback"}
	measureCalled := false

	observation := runTransportObservation(
		context.Background(),
		transportArm{
			Name:      "candidate-ublk",
			Tree:      "candidate",
			Requested: transportUblk,
			Start: func(context.Context) (transportEndpoint, transportKind, error) {
				return endpoint, transportNBD, nil
			},
		},
		transportWorkload{Name: "read"},
		func(context.Context, string) error {
			t.Fatal("fallback must not reach the correctness callback")
			return nil
		},
		func(context.Context, string, transportWorkload) (transportMetrics, error) {
			measureCalled = true
			return transportMetrics{}, nil
		},
	)

	require.Equal(t, transportSampleNotRun, observation.Status)
	require.Contains(t, observation.Reason, "requested ublk, selected nbd")
	require.False(t, measureCalled)
	require.Equal(t, 1, endpoint.closeCount)
}

func TestRunTransportObservationClassifiesErrorsAndStillCloses(t *testing.T) {
	tests := []struct {
		name           string
		verifyErr      error
		measureErr     error
		closeErr       error
		wantStatus     transportSampleStatus
		wantReasonPart string
	}{
		{
			name:           "wrong bytes fail",
			verifyErr:      errors.New("wrong fixture bytes"),
			wantStatus:     transportSampleFail,
			wantReasonPart: "correctness: wrong fixture bytes",
		},
		{
			name:           "incomplete measurement is inconclusive",
			measureErr:     errors.New("missing latency tail"),
			wantStatus:     transportSampleInconclusive,
			wantReasonPart: "measurement: missing latency tail",
		},
		{
			name:           "explicit setup status is retained",
			measureErr:     transportError(transportSampleNotRun, errors.New("fio unavailable")),
			wantStatus:     transportSampleNotRun,
			wantReasonPart: "measurement: fio unavailable",
		},
		{
			name:           "close failure fails sample",
			closeErr:       errors.New("close timeout"),
			wantStatus:     transportSampleFail,
			wantReasonPart: "close: close timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := &fakeTransportEndpoint{path: "/dev/nbd-test", closeErr: tt.closeErr}
			observation := runTransportObservation(
				context.Background(),
				transportArm{
					Name:      "candidate-nbd",
					Tree:      "candidate",
					Requested: transportNBD,
					Start: func(context.Context) (transportEndpoint, transportKind, error) {
						return endpoint, transportNBD, nil
					},
				},
				transportWorkload{Name: "read"},
				func(context.Context, string) error { return tt.verifyErr },
				func(context.Context, string, transportWorkload) (transportMetrics, error) {
					return transportMetrics{}, tt.measureErr
				},
			)

			require.Equal(t, tt.wantStatus, observation.Status)
			require.Contains(t, observation.Reason, tt.wantReasonPart)
			require.Equal(t, 1, endpoint.closeCount)
		})
	}
}

func TestDefaultTransportWorkloadsAreBoundedAndDistinct(t *testing.T) {
	workloads := defaultTransportWorkloads()
	require.Len(t, workloads, 4)

	seen := make(map[string]struct{}, len(workloads))
	for _, workload := range workloads {
		require.NotEmpty(t, workload.Name)
		require.NotEmpty(t, workload.Operation)
		require.Positive(t, workload.BlockSize)
		require.NotContains(t, seen, workload.Name)
		seen[workload.Name] = struct{}{}
	}

	require.Contains(t, seen, "random-read-4k")
	require.Contains(t, seen, "random-modify-4k")
	require.Contains(t, seen, "sequential-read-128k")
	require.Contains(t, seen, "buffered-write-flush")
}

// managedTransportEndpoint is a benchmark-only wrapper around a provider and
// its zero-backed fixture resources. It keeps the first cleanup result sticky;
// callers must not turn a failed provider close into a later successful sample.
type managedTransportEndpoint struct {
	provider transportEndpoint
	cleanup  func(context.Context) error

	mu        sync.Mutex
	closed    bool
	closeErr  error
	closeDone chan struct{}
}

func (e *managedTransportEndpoint) Path() (string, error) {
	return e.provider.Path()
}

func (e *managedTransportEndpoint) Close(ctx context.Context) error {
	e.mu.Lock()
	if e.closed {
		done := e.closeDone
		e.mu.Unlock()
		<-done
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.closeErr
	}
	e.closed = true
	e.closeDone = make(chan struct{})
	e.mu.Unlock()

	var errs []error
	var providerErr error
	if e.provider != nil {
		if err := e.provider.Close(ctx); err != nil {
			providerErr = fmt.Errorf("provider: %w", err)
			errs = append(errs, providerErr)
		}
	}
	// A provider close error may mean that its backend is still in flight. The
	// fixture is deliberately retained in that case; this wrapper must never
	// model releasing a real backend after an uncertain provider teardown.
	if providerErr == nil && e.cleanup != nil {
		if err := e.cleanup(ctx); err != nil {
			errs = append(errs, fmt.Errorf("fixture: %w", err))
		}
	}

	closeErr := errors.Join(errs...)
	e.mu.Lock()
	e.closeErr = closeErr
	close(e.closeDone)
	e.mu.Unlock()
	return closeErr
}

const transportFixtureSize = int64(256 << 20)

// newNBDTransportFactory is the real baseline adapter. It uses the production
// NBDProvider and device pool, but a local zero-backed fixture so this focused
// caller does not depend on object storage or a VM.
func newNBDTransportFactory(t *testing.T) transportFactory {
	t.Helper()

	return func(ctx context.Context) (transportEndpoint, transportKind, error) {
		source, err := nbdtestutils.NewZeroDevice(transportFixtureSize, header.RootfsBlockSize)
		if err != nil {
			return nil, transportNBD, fmt.Errorf("create zero fixture: %w", err)
		}

		featureFlags, err := featureflags.NewClient()
		if err != nil {
			_ = source.Close()
			return nil, transportNBD, fmt.Errorf("create feature flags: %w", err)
		}

		devicePool, err := nbd.NewDevicePool(1)
		if err != nil {
			_ = featureFlags.Close(context.WithoutCancel(ctx))
			_ = source.Close()
			return nil, transportNBD, fmt.Errorf("create NBD device pool: %w", err)
		}
		go devicePool.Populate(context.WithoutCancel(ctx))

		provider, err := rootfs.NewNBDProvider(
			ctx,
			source,
			filepath.Join(t.TempDir(), "rootfs.cow"),
			devicePool,
			featureFlags,
		)
		if err != nil {
			_ = devicePool.Close(context.WithoutCancel(ctx))
			_ = featureFlags.Close(context.WithoutCancel(ctx))
			_ = source.Close()
			return nil, transportNBD, fmt.Errorf("create NBD provider: %w", err)
		}

		if err := provider.Start(ctx); err != nil {
			_ = provider.Close(context.WithoutCancel(ctx))
			_ = devicePool.Close(context.WithoutCancel(ctx))
			_ = featureFlags.Close(context.WithoutCancel(ctx))
			_ = source.Close()
			return nil, transportNBD, fmt.Errorf("start NBD provider: %w", err)
		}

		return &managedTransportEndpoint{
			provider: provider,
			cleanup: func(cleanupCtx context.Context) error {
				return errors.Join(
					devicePool.Close(cleanupCtx),
					featureFlags.Close(cleanupCtx),
					source.Close(),
				)
			},
		}, transportNBD, nil
	}
}

func rootfsTransportKind(transport rootfs.Transport) (transportKind, error) {
	switch transport {
	case rootfs.TransportNBD:
		return transportNBD, nil
	case rootfs.TransportUblk:
		return transportUblk, nil
	default:
		return "", fmt.Errorf("unknown rootfs transport %q", transport)
	}
}

// newUblkTransportFactory is the real candidate adapter. The provider owns
// the ublk device and writable overlay; the NBD pool remains available only so
// the provider can prove a pre-side-effect fallback. The selected transport is
// returned by the provider, never inferred from the endpoint path.
func newUblkTransportFactory(t *testing.T) transportFactory {
	t.Helper()

	return func(ctx context.Context) (transportEndpoint, transportKind, error) {
		source, err := nbdtestutils.NewZeroDevice(transportFixtureSize, header.RootfsBlockSize)
		if err != nil {
			return nil, "", transportError(transportSampleNotRun, fmt.Errorf("create zero fixture: %w", err))
		}

		featureFlags, err := featureflags.NewClient()
		if err != nil {
			_ = source.Close()
			return nil, "", transportError(transportSampleNotRun, fmt.Errorf("create feature flags: %w", err))
		}

		devicePool, err := nbd.NewDevicePool(1)
		if err != nil {
			_ = featureFlags.Close(context.WithoutCancel(ctx))
			_ = source.Close()
			return nil, "", transportError(transportSampleNotRun, fmt.Errorf("create NBD fallback pool: %w", err))
		}
		go devicePool.Populate(context.WithoutCancel(ctx))

		provider, selected, startErr := rootfs.NewUblkProviderWithFallback(
			ctx,
			source,
			filepath.Join(t.TempDir(), "rootfs.cow"),
			devicePool,
			featureFlags,
		)
		actual, kindErr := rootfsTransportKind(selected)
		cleanup := func(cleanupCtx context.Context) error {
			return errors.Join(
				devicePool.Close(cleanupCtx),
				featureFlags.Close(cleanupCtx),
				source.Close(),
			)
		}

		if provider == nil {
			if startErr == nil {
				startErr = errors.New("ublk provider factory returned nil provider")
			}
			if kindErr != nil {
				startErr = errors.Join(startErr, kindErr)
			}
			_ = cleanup(context.WithoutCancel(ctx))
			return nil, actual, transportError(transportSampleNotRun, startErr)
		}

		endpoint := &managedTransportEndpoint{provider: provider, cleanup: cleanup}
		if kindErr != nil {
			return endpoint, actual, transportError(transportSampleFail, kindErr)
		}
		if startErr != nil {
			// A non-nil provider means the admission path retained ownership after
			// a side effect. Let observation cleanup it, but never call this a
			// missing prerequisite or permit a false ublk sample.
			return endpoint, actual, transportError(transportSampleFail, startErr)
		}

		return endpoint, actual, nil
	}
}

// verifyTransportPath performs the pre-timing correctness check for the <redacted:secret>
// block fixture: initial zero bytes, a private write, flush and readback.
func verifyTransportPath(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		return fmt.Errorf("open transport path: %w", err)
	}
	defer file.Close()

	const blockSize = 4 << 10
	const offset = int64(blockSize)
	zeroes := make([]byte, blockSize)
	readback := make([]byte, blockSize)
	if n, err := file.ReadAt(readback, offset); err != nil {
		return fmt.Errorf("read initial fixture: %w", err)
	} else if n != len(readback) || !bytes.Equal(readback, zeroes) {
		return errors.New("initial fixture bytes are not zero")
	}

	pattern := bytes.Repeat([]byte{0xa5}, blockSize)
	if n, err := file.WriteAt(pattern, offset); err != nil {
		return fmt.Errorf("write fixture: %w", err)
	} else if n != len(pattern) {
		return fmt.Errorf("short fixture write: %d/%d", n, len(pattern))
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("flush fixture write: %w", err)
	}
	clear(readback)
	if n, err := file.ReadAt(readback, offset); err != nil {
		return fmt.Errorf("read fixture back: %w", err)
	} else if n != len(readback) || !bytes.Equal(readback, pattern) {
		return errors.New("fixture readback mismatch")
	}

	return nil
}

type fioDirection struct {
	BandwidthBytes float64 `json:"bw_bytes"`
	IOPS           float64 `json:"iops"`
	CompletionNS   struct {
		Percentile map[string]float64 `json:"percentile"`
	} `json:"clat_ns"`
}

type fioJob struct {
	Read  fioDirection `json:"read"`
	Write fioDirection `json:"write"`
}

type fioReport struct {
	Jobs []fioJob `json:"jobs"`
}

// runFioMeasurement is the runnable local measurement adapter. It is kept
// outside the timed correctness/setup path and returns INCONCLUSIVE for missing
// or malformed measurement output rather than manufacturing a zero result.
func runFioMeasurement(ctx context.Context, path string, workload transportWorkload) (transportMetrics, error) {
	args := []string{
		"--name=" + workload.Name,
		"--filename=" + path,
		"--ioengine=libaio",
		"--direct=1",
		"--iodepth=16",
		"--rw=" + workload.Operation,
		"--bs=" + strconv.FormatInt(workload.BlockSize, 10),
		"--size=64m",
		"--runtime=10s",
		"--time_based=1",
		"--group_reporting=1",
		"--output-format=json",
		"--clat_percentiles=1",
		"--eta=never",
	}
	if workload.Mix != "" {
		parts := strings.Split(workload.Mix, "/")
		if len(parts) == 2 && parts[0] == "70" {
			args = append(args, "--rwmixread=70")
		}
	}
	if workload.Name == "buffered-write-flush" {
		args = append(args, "--direct=0", "--fsync=1")
	}

	output, err := exec.CommandContext(ctx, "fio", args...).Output()
	if err != nil {
		return transportMetrics{}, transportError(transportSampleInconclusive, fmt.Errorf("fio: %w", err))
	}

	var report fioReport
	if err := json.Unmarshal(output, &report); err != nil {
		return transportMetrics{}, transportError(transportSampleInconclusive, fmt.Errorf("parse fio JSON: %w", err))
	}
	if len(report.Jobs) != 1 {
		return transportMetrics{}, transportError(transportSampleInconclusive, fmt.Errorf("expected one fio job, got %d", len(report.Jobs)))
	}

	job := report.Jobs[0]
	if workload.Operation == "randrw" {
		return transportMetrics{
			BytesPerSecond: job.Read.BandwidthBytes + job.Write.BandwidthBytes,
			IOPS:           job.Read.IOPS + job.Write.IOPS,
			P50:            maxFioPercentile(job.Read, job.Write, "50.000000"),
			P99:            maxFioPercentile(job.Read, job.Write, "99.000000"),
		}, nil
	}

	direction := job.Read
	if workload.Operation == "write" {
		direction = job.Write
	}

	return transportMetrics{
		BytesPerSecond: direction.BandwidthBytes,
		IOPS:           direction.IOPS,
		P50:            fioPercentile(direction, "50.000000"),
		P99:            fioPercentile(direction, "99.000000"),
	}, nil
}

func fioPercentile(direction fioDirection, key string) time.Duration {
	value, ok := direction.CompletionNS.Percentile[key]
	if !ok {
		return 0
	}

	return time.Duration(value)
}

func maxFioPercentile(left, right fioDirection, key string) time.Duration {
	return max(fioPercentile(left, key), fioPercentile(right, key))
}

// TestTransportNBDProviderComparison is the runnable baseline adaptation. It is
// opt-in because it needs root, the NBD module and fio; ordinary focused tests
// remain host-safe and do not claim measurement evidence.
func TestTransportNBDProviderComparison(t *testing.T) {
	if os.Getenv("E2B_RUN_TRANSPORT_COMPARISON") != "1" {
		t.Skip("set E2B_RUN_TRANSPORT_COMPARISON=1 to run the real NBD/fio adapter")
	}
	if os.Geteuid() != 0 {
		t.Skip("transport comparison needs root")
	}
	if _, err := exec.LookPath("fio"); err != nil {
		t.Skip("transport comparison needs fio")
	}

	for _, workload := range defaultTransportWorkloads() {
		workload := workload
		t.Run(workload.Name, func(t *testing.T) {
			observation := runTransportObservation(
				t.Context(),
				transportArm{
					Name:      "baseline-nbd",
					Tree:      "530d068c5",
					Requested: transportNBD,
					Start:     newNBDTransportFactory(t),
				},
				workload,
				verifyTransportPath,
				runFioMeasurement,
			)
			if observation.Status != transportSampleValid {
				t.Fatalf("baseline NBD sample was %s: %s", observation.Status, observation.Reason)
			}
			t.Logf("%s actual=%s bytes_per_second=%.1f iops=%.1f p50=%s p99=%s", workload.Name, observation.Actual, observation.Metrics.BytesPerSecond, observation.Metrics.IOPS, observation.Metrics.P50, observation.Metrics.P99)
		})
	}
}

// TestTransportUblkProviderComparison is the runnable candidate arm. It has a
// second explicit opt-in because it creates real ublk devices in the VM.
func TestTransportUblkProviderComparison(t *testing.T) {
	if os.Getenv("E2B_RUN_TRANSPORT_COMPARISON") != "1" || os.Getenv("E2B_RUN_TRANSPORT_UBLK") != "1" {
		t.Skip("set E2B_RUN_TRANSPORT_COMPARISON=1 and E2B_RUN_TRANSPORT_UBLK=1 to run the real ublk/fio adapter")
	}
	if os.Geteuid() != 0 {
		t.Skip("transport comparison needs root")
	}
	if _, err := exec.LookPath("fio"); err != nil {
		t.Skip("transport comparison needs fio")
	}

	for _, workload := range defaultTransportWorkloads() {
		workload := workload
		t.Run(workload.Name, func(t *testing.T) {
			observation := runTransportObservation(
				t.Context(),
				transportArm{
					Name:      "candidate-ublk",
					Tree:      "candidate",
					Requested: transportUblk,
					Start:     newUblkTransportFactory(t),
				},
				workload,
				verifyTransportPath,
				runFioMeasurement,
			)
			if observation.Status != transportSampleValid {
				t.Fatalf("candidate ublk sample was %s: %s", observation.Status, observation.Reason)
			}
			t.Logf("%s actual=%s bytes_per_second=%.1f iops=%.1f p50=%s p99=%s", workload.Name, observation.Actual, observation.Metrics.BytesPerSecond, observation.Metrics.IOPS, observation.Metrics.P50, observation.Metrics.P99)
		})
	}
}

func TestManagedTransportEndpointRetainsProviderCloseFailure(t *testing.T) {
	providerErr := errors.New("provider close failed")
	provider := &fakeTransportEndpoint{closeErr: providerErr}
	fixtureCleanupCalls := 0
	endpoint := &managedTransportEndpoint{
		provider: provider,
		cleanup: func(context.Context) error {
			fixtureCleanupCalls++
			return nil
		},
	}

	firstErr := endpoint.Close(context.Background())
	secondErr := endpoint.Close(context.Background())

	require.ErrorIs(t, firstErr, providerErr)
	require.ErrorIs(t, secondErr, providerErr)
	require.Equal(t, 1, provider.closeCount)
	require.Zero(t, fixtureCleanupCalls, "uncertain provider teardown must retain fixture ownership")
}
