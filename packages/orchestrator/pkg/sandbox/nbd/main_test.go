//go:build linux

package nbd

import (
	"context"
	"fmt"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// testSpanRecorder collects the package's spans. Close builds its span from the
// package-level tracer, which resolves against the global provider, so the
// provider has to be installed globally rather than passed in -- and only once,
// because otel's global instruments delegate on the first SetTracerProvider.
var testSpanRecorder = tracetest.NewSpanRecorder()

// testMetricReader and testLogObserver capture the package's instruments and
// warn logs the same way: both resolve against process-wide globals, so the
// test doubles are installed once here and every test filters what it reads.
var (
	testMetricReader = sdkmetric.NewManualReader()
	testLogObserver  *observer.ObservedLogs
)

func TestMain(m *testing.M) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(testSpanRecorder)))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMetricReader)))

	observerCore, logs := observer.New(zap.InfoLevel)
	testLogObserver = logs
	logger.ReplaceGlobals(context.Background(), logger.NewTracedLoggerFromCore(observerCore))

	// /dev/nbd* are host-global: serialize device-touching test binaries so
	// concurrent gate runs cannot attach/detach each other's devices (S-56).
	lock, err := acquireDeviceTestLock(context.Background(), deviceTestLockPath(), deviceTestLockWait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nbd tests: %v\n", err)

		// A bare return would exit 0 and silently skip the whole suite, so the
		// explicit exit stays despite the usual redundancy of os.Exit here.
		//nolint:revive // os.Exit is required: the device tests must not run without the lock
		os.Exit(1)
	}

	// Recover devices left attached to dead pids by an aborted run before the
	// pool sees them (S-56): a stale pid file makes a device look busy forever.
	if cleared, failures := clearStaleDeviceAttachments(context.Background()); cleared > 0 || len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "nbd tests: cleared %d stale NBD attachment(s), %d failure(s)",
			cleared, len(failures))
		for _, failure := range failures {
			fmt.Fprintf(os.Stderr, "; %v", failure)
		}

		fmt.Fprintln(os.Stderr)
	}

	// m.Run's result becomes the process exit status via the test wrapper.
	m.Run()

	releaseDeviceTestLock(lock)
}
