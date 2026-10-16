package memql

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/id"
)

// Two concurrent Ask observations each retain an observer and briefly a
// transcript writer. The deployed request pool has four connections. Those
// holds must leave the pool available for their own reads and the CI driver.
func TestAskHoldsLeaveRequestConnectionsAvailable(t *testing.T) {
	e, db, _ := readMergeTestEngine(t)
	db.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 2 {
		conversation := id.NewShortId()
		for _, kind := range []string{"observer:turn", "write"} {
			hold, err := e.holdAskConversation(ctx, conversation, kind)
			require.NoError(t, err)
			t.Cleanup(hold.release)
		}
	}
	read, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	require.NoError(t, db.PingContext(read), "conversation coordination exhausted the request pool")
}

func TestAskObserversDoNotReserveConnectionsAndSerializeAcrossReplicas(t *testing.T) {
	a, db, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	db.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conversation := id.NewShortId()
	for n := range 8 {
		kind := "observer:" + strconv.Itoa(n)
		h, err := a.holdAskConversation(ctx, conversation, kind)
		require.NoError(t, err)
		t.Cleanup(h.release)
		_, err = b.holdAskConversation(ctx, conversation, kind)
		require.ErrorContains(t, err, "already working")
		require.NoError(t, h.renew())
	}
	require.Zero(t, db.Stats().InUse, "long observations must reserve no request connection")
	write, err := a.holdAskConversation(ctx, conversation, "write")
	require.NoError(t, err)
	defer write.release()
	require.NoError(t, db.PingContext(ctx))
}

func TestAskExpiredObserverCannotRenewSaveOrReleaseItsSuccessor(t *testing.T) {
	a, db, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := askTestActor()
	conversation := askTestConversation(t, a, ctx)
	first, err := a.holdAskConversation(ctx, conversation, "observer:turn")
	require.NoError(t, err)
	defer first.release()
	observing := first.observationContext(ctx)
	_, err = db.DB.ExecContext(ctx, `UPDATE automation_execution_claims SET expires_at=clock_timestamp()-interval '1 second'
		WHERE automation_name=$1 AND dedup_key=$2 AND claimed_by=$3`, askObserverNamespace, strconv.FormatInt(first.key, 10), first.token)
	require.NoError(t, err)
	second, err := b.holdAskConversation(ctx, conversation, "observer:turn")
	require.NoError(t, err)
	defer second.release()
	require.NotEqual(t, first.token, second.token)
	require.ErrorIs(t, first.renew(), ErrWriteGateLost)
	select {
	case <-observing.Done():
		require.ErrorIs(t, context.Cause(observing), ErrWriteGateLost)
	case <-time.After(time.Second):
		t.Fatal("losing the lease did not stop the observation")
	}
	// RunAsk's final receipt detaches request cancellation. It still must not
	// overwrite the new observer's transcript after losing its token.
	require.ErrorIs(t, a.askSave(context.WithoutCancel(observing), conversation, "stale", askTranscript{}), ErrWriteGateLost)
	first.release()
	require.NoError(t, second.verify(ctx))
	require.NoError(t, second.renew())
}

func TestAskWriterRefusesMissingPoolAndBoundsCapacityWait(t *testing.T) {
	e, request, _ := readMergeTestEngine(t)
	e.SetAskCoordinationPool(nil)
	ctx := context.Background()
	_, err := e.holdAskConversation(ctx, id.NewShortId(), "write")
	require.ErrorIs(t, err, ErrWriteGatePoolMissing)
	gate := deployedGatePool(t, 1)
	e.SetAskCoordinationPool(func() *sql.DB { return gate })
	first, err := e.holdAskConversation(ctx, id.NewShortId(), "write")
	require.NoError(t, err)
	defer first.release()
	competing, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	_, err = e.holdAskConversation(competing, id.NewShortId(), "write")
	require.ErrorIs(t, err, ErrWriteGateBusy)
	require.Zero(t, request.Stats().InUse)
	require.NoError(t, request.PingContext(ctx))
	first.release()
	next, err := e.holdAskConversation(ctx, id.NewShortId(), "write")
	require.NoError(t, err)
	next.release()
}

func TestAskWriterStillSerializesWithPreviousSessionLock(t *testing.T) {
	e, db, _ := readMergeTestEngine(t)
	ctx := context.Background()
	conversation := id.NewShortId()
	probe, err := e.holdAskConversation(ctx, conversation, "write")
	require.NoError(t, err)
	key := probe.key
	probe.release()
	previous, err := db.DB.Conn(ctx)
	require.NoError(t, err)
	defer previous.Close()
	_, err = previous.ExecContext(ctx, "SELECT pg_advisory_lock($1)", key)
	require.NoError(t, err)
	defer previous.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", key)
	competing, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err = e.holdAskConversation(competing, conversation, "write")
	require.ErrorIs(t, err, ErrWriteGateBusy)
	_, err = previous.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", key)
	require.NoError(t, err)
	next, err := e.holdAskConversation(ctx, conversation, "write")
	require.NoError(t, err)
	next.release()
}
