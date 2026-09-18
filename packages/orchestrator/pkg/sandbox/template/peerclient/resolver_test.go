package peerclient

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func mustTestConn(t *testing.T) *grpc.ClientConn {
	t.Helper()

	// grpc.NewClient builds the connection lazily, so no peer needs to exist
	// for lifecycle tests.
	conn, err := grpc.NewClient("passthrough:///peer-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	return conn
}

func TestPeerResolverSweepEvictsIdleConns(t *testing.T) {
	t.Parallel()

	r := &peerResolver{registry: NopRegistry()}

	idle := &peerConn{conn: mustTestConn(t)}
	idle.lastUsed.Store(time.Now().Add(-2 * peerConnIdleTTL).UnixNano())
	fresh := &peerConn{conn: mustTestConn(t)}
	fresh.touch()

	r.peerConns.Store("idle-peer", idle)
	r.peerConns.Store("fresh-peer", fresh)

	r.sweepPeerConns(time.Now())

	_, ok := r.peerConns.Load("idle-peer")
	require.False(t, ok, "an idle connection must be swept")

	_, ok = r.peerConns.Load("fresh-peer")
	require.True(t, ok, "a recently used connection must survive")
}

func TestPeerResolverSweepBoundsCache(t *testing.T) {
	t.Parallel()

	r := &peerResolver{registry: NopRegistry()}

	now := time.Now()
	for i := range maxPeerConns + 3 {
		pc := &peerConn{conn: mustTestConn(t)}
		// Higher i means older, i.e. least recently used first.
		pc.lastUsed.Store(now.Add(-time.Duration(i) * time.Second).UnixNano())
		r.peerConns.Store(fmt.Sprintf("peer-%02d", i), pc)
	}

	r.sweepPeerConns(now)

	count := 0

	r.peerConns.Range(func(_, _ any) bool {
		count++

		return true
	})

	require.Equal(t, maxPeerConns, count, "the connection cache must be bounded")
}

func TestPeerResolverMaybeSweepIsThrottled(t *testing.T) {
	t.Parallel()

	r := &peerResolver{registry: NopRegistry()}

	idle := &peerConn{conn: mustTestConn(t)}
	idle.lastUsed.Store(time.Now().Add(-2 * peerConnIdleTTL).UnixNano())
	r.peerConns.Store("idle-peer", idle)

	// A sweep just ran: the throttled call must not sweep again.
	r.lastSweep.Store(time.Now().UnixNano())
	r.maybeSweep(time.Now())

	_, ok := r.peerConns.Load("idle-peer")
	require.True(t, ok, "the idle sweep is throttled")
}

func TestPeerResolverDropPeerConn(t *testing.T) {
	t.Parallel()

	r := &peerResolver{registry: NopRegistry()}
	r.peerConns.Store("peer-a", &peerConn{conn: mustTestConn(t)})

	r.dropPeerConn("peer-a")

	_, ok := r.peerConns.Load("peer-a")
	require.False(t, ok, "the dropped connection must be forgotten")

	// Dropping an unknown address is a no-op.
	r.dropPeerConn("peer-missing")
}
