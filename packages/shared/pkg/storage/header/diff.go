package header

import (
	"bytes"

	"go.opentelemetry.io/otel"

	"github.com/e2b-dev/infra/packages/shared/pkg/units"
)

// The canonical size definitions (and their derivation checks) live in
// packages/shared/pkg/units; these names stay for the call sites.
const (
	PageSize        = units.PageSize
	HugepageSize    = units.HugepageSize
	RootfsBlockSize = units.RootfsBlockSize
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/shared/pkg/storage/header")

var EmptyHugePage = make([]byte, HugepageSize)

// IsZero reports whether b is all-zero. Samples first/middle/last byte to
// reject most non-zero buffers from one cache line, then falls back to
// bytes.Equal(b[:n-1], b[1:]): true exactly when every adjacent pair of
// bytes is equal, i.e. all bytes equal b[0] (which the sample already
// proved is zero).
func IsZero(b []byte) bool {
	n := len(b)
	if n == 0 {
		return true
	}
	if b[0]|b[n-1]|b[n/2] != 0 {
		return false
	}
	if n <= 3 {
		return true
	}

	return bytes.Equal(b[:n-1], b[1:])
}
