//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// transportKind identifies the host block transport selected for one sample.
// The value is returned by the adapter, rather than inferred from a device path
// or a feature flag, so a fallback cannot be reported as the requested arm.
type transportKind string

const (
	transportNBD  transportKind = "nbd"
	transportUblk transportKind = "ublk"
)

// transportSampleStatus describes one sample. It is deliberately separate from
// any paired comparison verdict: one valid timed sample is not by itself a
// non-regression PASS.
type transportSampleStatus string

const (
	transportSampleValid        transportSampleStatus = "VALID"
	transportSampleNotRun       transportSampleStatus = "NOT_RUN"
	transportSampleInconclusive transportSampleStatus = "INCONCLUSIVE"
	transportSampleFail         transportSampleStatus = "FAIL"
)

const transportCleanupTimeout = 30 * time.Second

// transportComparisonStatus is reserved for a paired before/after decision.
// The comparison layer must only produce PASS from valid observations and an
// explicit comparison rule; runTransportObservation never produces this type.
type transportComparisonStatus string

const (
	transportComparisonPass         transportComparisonStatus = "PASS"
	transportComparisonNotRun       transportComparisonStatus = "NOT_RUN"
	transportComparisonInconclusive transportComparisonStatus = "INCONCLUSIVE"
	transportComparisonFail         transportComparisonStatus = "FAIL"
)

// transportEndpoint is the minimum lifecycle surface already provided by a
// rootfs.Provider. The comparison does not need to know how NBD or ublk is
// constructed, and therefore does not duplicate provider ownership logic.
type transportEndpoint interface {
	Path() (string, error)
	Close(context.Context) error
}

// transportFactory constructs one isolated endpoint and reports the transport
// that was actually selected. A requested ublk arm that falls back to NBD is
// consequently visible to the comparison instead of becoming a false ublk
// sample.
type transportFactory func(context.Context) (transportEndpoint, transportKind, error)

type transportArm struct {
	Name      string
	Tree      string
	Requested transportKind
	Start     transportFactory
}

type transportWorkload struct {
	Name      string
	Operation string
	Mix       string
	BlockSize int64
}

// defaultTransportWorkloads is intentionally a bounded product workload set,
// not a matrix registry. The measurement callback chooses duration, cache
// preparation and the tool invocation while these names remain stable.
func defaultTransportWorkloads() []transportWorkload {
	return []transportWorkload{
		{Name: "random-read-4k", Operation: "randread", BlockSize: 4 << 10},
		{Name: "random-modify-4k", Operation: "randrw", Mix: "70/30", BlockSize: 4 << 10},
		{Name: "sequential-read-128k", Operation: "read", BlockSize: 128 << 10},
		{Name: "buffered-write-flush", Operation: "write", BlockSize: 4 << 10},
	}
}

type transportMetrics struct {
	BytesPerSecond float64
	IOPS           float64
	P50            time.Duration
	P99            time.Duration
	CPUTime        time.Duration
}

type transportObservation struct {
	Arm       string
	Tree      string
	Workload  string
	Requested transportKind
	Actual    transportKind
	Status    transportSampleStatus
	Reason    string
	Metrics   transportMetrics
}

// transportStatusError lets an adapter or measurement callback preserve an
// explicit sample status while still returning an ordinary Go error. Most setup
// errors are NOT_RUN; correctness and cleanup failures default to FAIL;
// incomplete measurements default to INCONCLUSIVE.
type transportStatusError struct {
	Status transportSampleStatus
	Err    error
}

func (e *transportStatusError) Error() string {
	if e == nil || e.Err == nil {
		return "transport comparison failed"
	}

	return e.Err.Error()
}

func (e *transportStatusError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

func transportError(status transportSampleStatus, err error) error {
	if err == nil {
		err = errors.New("transport comparison failed")
	}

	return &transportStatusError{Status: status, Err: err}
}

func transportErrorStatus(err error, fallback transportSampleStatus) transportSampleStatus {
	var statusErr *transportStatusError
	if errors.As(err, &statusErr) && statusErr.Status != "" {
		return statusErr.Status
	}

	return fallback
}

func transportCleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), transportCleanupTimeout)
}

// runTransportObservation executes one correctness-first observation. Setup,
// path publication and cleanup are outside the measurement callback's timing;
// cleanup failure always invalidates the observation, even if the callback
// returned apparently good numbers.
func runTransportObservation(
	ctx context.Context,
	arm transportArm,
	workload transportWorkload,
	verify func(context.Context, string) error,
	measure func(context.Context, string, transportWorkload) (transportMetrics, error),
) (observation transportObservation) {
	observation = transportObservation{
		Arm:       arm.Name,
		Tree:      arm.Tree,
		Workload:  workload.Name,
		Requested: arm.Requested,
		Status:    transportSampleNotRun,
	}

	if arm.Start == nil {
		observation.Status = transportSampleFail
		observation.Reason = "transport arm has no factory"
		return observation
	}

	endpoint, actual, startErr := arm.Start(ctx)
	observation.Actual = actual
	if endpoint == nil {
		if startErr == nil {
			startErr = errors.New("transport factory returned a nil endpoint")
		}
		observation.Status = transportErrorStatus(startErr, transportSampleNotRun)
		observation.Reason = startErr.Error()
		return observation
	}

	defer func() {
		cleanupCtx, cancel := transportCleanupContext(ctx)
		defer cancel()

		closeErr := endpoint.Close(cleanupCtx)
		if closeErr != nil {
			observation.Status = transportSampleFail
			observation.Reason = joinTransportReasons(observation.Reason, "close: "+closeErr.Error())
		}
	}()

	if startErr != nil {
		observation.Status = transportErrorStatus(startErr, transportSampleNotRun)
		observation.Reason = startErr.Error()
		return observation
	}

	if actual == "" {
		observation.Status = transportSampleFail
		observation.Reason = "transport factory did not report the selected transport"
		return observation
	}

	if actual != arm.Requested {
		observation.Status = transportSampleNotRun
		observation.Reason = fmt.Sprintf("requested %s, selected %s", arm.Requested, actual)
		return observation
	}

	path, err := endpoint.Path()
	if err != nil {
		observation.Status = transportSampleFail
		observation.Reason = "getting transport path: " + err.Error()
		return observation
	}
	if path == "" {
		observation.Status = transportSampleFail
		observation.Reason = "transport provider returned an empty path"
		return observation
	}
	if verify == nil {
		observation.Status = transportSampleFail
		observation.Reason = "transport workload has no correctness check"
		return observation
	}
	if err := verify(ctx, path); err != nil {
		observation.Status = transportErrorStatus(err, transportSampleFail)
		observation.Reason = "correctness: " + err.Error()
		return observation
	}
	if measure == nil {
		observation.Status = transportSampleFail
		observation.Reason = "transport workload has no measurement callback"
		return observation
	}

	metrics, err := measure(ctx, path, workload)
	if err != nil {
		observation.Status = transportErrorStatus(err, transportSampleInconclusive)
		observation.Reason = "measurement: " + err.Error()
		return observation
	}

	observation.Metrics = metrics
	observation.Status = transportSampleValid
	return observation
}

func joinTransportReasons(reasons ...string) string {
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if strings.TrimSpace(reason) != "" {
			parts = append(parts, reason)
		}
	}

	return strings.Join(parts, "; ")
}
