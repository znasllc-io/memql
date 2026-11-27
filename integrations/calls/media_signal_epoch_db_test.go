package calls

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Advance the service clock while the final gated admission is still reading
// its journal. This represents lock/database delay without a wall-clock sleep:
// the provider has not been contacted and no pending intent exists yet.
type mediaSignalAdmissionClockEngine struct {
	Engine
	clock        *testClock
	delay        time.Duration
	journalReads int
}

func (e *mediaSignalAdmissionClockEngine) Execute(ctx context.Context, query string) (any, error) {
	if strings.HasPrefix(query, "query callsMediaSignalsById(") {
		e.journalReads++
		if e.journalReads == 2 { // First read belongs to the preceding cleanup audit.
			e.clock.Advance(e.delay)
		}
	}
	return e.Engine.Execute(ctx, query)
}

func TestMediaSignalDatabaseHandshakeEpochStartsAtPendingIntentCreation(t *testing.T) {
	for _, delay := range []time.Duration{mediaHandshakeTTL, mediaHandshakeTTL + time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			cw, _, start := mediaSignalFixture(t, "signal-creation-epoch")
			if cw == nil {
				return
			}
			ctx := context.Background()
			original := cw.svc.store.engine
			clocked := &mediaSignalAdmissionClockEngine{Engine: original, clock: cw.clock, delay: delay}
			cw.svc.store.engine = clocked
			lease, err := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
			cw.svc.store.engine = original
			require.NoError(t, err)
			require.Equal(t, 2, clocked.journalReads)
			require.NotNil(t, lease)

			room, err := cw.svc.store.roomByID(ctx, cw.owner, start.Call.ID)
			require.NoError(t, err)
			require.NotNil(t, room)
			journal, err := cw.svc.store.mediaSignals(ctx, *room)
			require.NoError(t, err, "a slow admission must never persist ExpiresAt <= CreatedAt and poison the room journal")
			intent := journal.find(lease.IntentID)
			require.NotNil(t, intent)
			require.Equal(t, "pending", intent.State)
			require.Equal(t, mediaHandshakeTTL, intent.ExpiresAt.Sub(intent.CreatedAt))
			require.True(t, intent.CreatedAt.Equal(cw.clock.Now()))
			require.True(t, lease.ExpiresAt.Equal(intent.ExpiresAt))

			cw.clock.Advance(mediaHandshakeTTL - time.Second)
			forwarded := false
			require.NoError(t, lease.Forward(ctx, func(context.Context) error { forwarded = true; return nil }))
			require.True(t, forwarded)
			cw.clock.Advance(time.Second)
			forwarded = false
			require.ErrorContains(t, lease.Forward(ctx, func(context.Context) error { forwarded = true; return nil }), "media_connection_expired")
			require.False(t, forwarded, "the same twenty-second handshake bound remains enforced before any upstream write")
			require.NoError(t, lease.Close(ctx, true))
			journal, err = cw.svc.store.mediaSignals(ctx, *room)
			require.NoError(t, err)
			require.Empty(t, journal.Intents)
		})
	}
}
