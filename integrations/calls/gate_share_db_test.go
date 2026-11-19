package calls

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	callrules "github.com/znasllc-io/memql/component/calls"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/memql"
)

// gate_share_db_test.go -- calls decisions take a bounded share of the process's write-gate
// pool (service.go gates), against a real engine, a real database and a
// gate pool shaped as the bff's.
//
// Postgres-gated: an unreachable database skips, and MEMQL_REQUIRE_DB=1 turns the skip
// into a failure.

// TestABurstOfCallsDecisionsLeavesOtherWriteGatesRoom: every calls decision takes its write
// gates on the process's gate pool, the one every write gate of every feature shares, and
// holds its connection from the moment it asks for its locks, waiting included. A burst of
// joins behind one call's lock -- a lock a decision on another replica holds -- would
// otherwise hold every connection of the pool while it waits, and an unrelated feature's
// write gate in the same process would wait out its time and answer busy. Calls decisions
// hold at most their share of the pool at once (callsGateShare of the bff's four) and wait
// for a slot in the process, so another feature's gate is taken at once; and every join of
// the burst is admitted once the lock is given back.
func TestABurstOfCallsDecisionsLeavesOtherWriteGatesRoom(t *testing.T) {
	cw := newCallWorld(t, "gateshare", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, started := cw.started("Host")

	// The bff's pool, this service's alone, and a service on it beside the world's.
	pool, err := database.NewGatePool(sharedBoot.dsn, database.DefaultGatePoolSize)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	svc := New(Deps{
		Engine:              liveEngine{cw.eng},
		GateDB:              func() *sql.DB { return pool },
		ResolveSystemSecret: cw.eng.ResolveSystemSecret,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:                 cw.clock.Now,
		NewMediaBackend:     cw.media.backend,
	}).Service()

	// Another replica's decision holds the call's lock, on a pool of its own.
	elsewhere, err := database.NewGatePool(sharedBoot.dsn, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = elsewhere.Close() })
	held, err := memql.AcquireWriteGates(ctx, elsewhere, roomGate(started.Call.ID))
	require.NoError(t, err)
	releaseHeld := sync.OnceFunc(held)
	t.Cleanup(releaseHeld)

	// A burst of joins to that call, more than the pool has connections.
	const burst = 6
	var wg sync.WaitGroup
	results := make([]callrules.JoinResult, burst)
	errs := make([]error, burst)
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _, errs[i] = svc.Join(ctx, cw.ref, nil, callrules.JoinBody{
				Token: started.Call.GuestToken, Name: fmt.Sprintf("Guest %d", i), Confirm: confirmed,
			})
		}(i)
	}

	// While the burst waits on the lock, it holds no more than its share of the pool, and
	// another feature's write gate in the same process is taken at once.
	deadline := time.Now().Add(time.Second)
	most := 0
	for time.Now().Before(deadline) {
		most = max(most, pool.Stats().InUse)
		time.Sleep(20 * time.Millisecond)
	}
	other, err := memql.AcquireWriteGatesWithin(ctx, pool, time.Second, "calls.test.another-feature")
	require.NoError(t, err, "another feature's write gate could not be taken while calls decisions waited on a lock")
	other()
	require.LessOrEqual(t, most, callsGateShare, "a burst of calls decisions held %d of the pool's %d gate connections at once", most, database.DefaultGatePoolSize)

	// The lock is given back, and every join of the burst is decided and let in.
	releaseHeld()
	wg.Wait()
	for i := 0; i < burst; i++ {
		require.NoError(t, errs[i], "join %d of the burst failed", i)
		require.Equal(t, callrules.OutcomeAdmit, results[i].Outcome, "join %d of the burst was not let in", i)
	}
}

// TestACallsDecisionThatFindsNoSlotIsBusy: a decision that cannot get a slot of the calls
// share within the write gate's own wait answers as a gate that could not be taken in time
// answers -- busy, nothing attempted -- and gives back nothing it did not take.
func TestACallsDecisionThatFindsNoSlotIsBusy(t *testing.T) {
	// A pool no gate is ever taken on: the decision must give up before it asks for one.
	pool := &sql.DB{}
	slots := gateSlotsFor(pool)
	for i := 0; i < cap(slots); i++ {
		slots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(slots); i++ {
			<-slots
		}
	}()
	s := &Service{gateDB: func() *sql.DB { return pool }}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := s.gates(ctx, "calls.room.anything")
	require.True(t, errors.Is(err, memql.ErrWriteGateBusy), "a decision with no slot answered %v, want busy", err)
	require.Len(t, slots, cap(slots), "a decision that took no slot gave one back")
}
