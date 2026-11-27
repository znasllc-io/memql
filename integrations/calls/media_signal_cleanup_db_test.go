package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMediaSignalDatabaseUnavailableBackendStaysPendingAndPanicReleasesGates(t *testing.T) {
	cw, _, start := mediaSignalFixture(t, "signal-backend-disappeared")
	if cw == nil {
		return
	}
	ctx := context.Background()
	room, _, _, binding, _, err := cw.svc.authenticateMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, err)
	status, err := cw.svc.ReconcileMediaSignals(ctx, room.MediaRoom)
	require.NoError(t, err)
	require.Equal(t, "clear", status.State)
	original := cw.svc.media
	t.Cleanup(func() { cw.svc.media = original })
	cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return nil, nil }
	require.NotPanics(t, func() { status, err = cw.svc.ReconcileMediaSignals(ctx, room.MediaRoom) })
	require.ErrorContains(t, err, "media_server_unavailable")
	require.Equal(t, "pending", status.State)
	require.Equal(t, "pending", cw.svc.mediaCleanupStatus(ctx, *room).State, "a missing backend invalidates the earlier clear receipt")

	cw.svc.media = original
	cw.media.setPresent(room.MediaRoom, person("unadmitted-late-connection"))
	calls := 0
	cw.svc.media = func(server MediaServer, credential MediaCredential) (MediaBackend, error) {
		calls++
		if calls > 1 {
			return nil, nil
		}
		return original(server, credential)
	}
	require.NotPanics(t, func() { status, err = cw.svc.ReconcileMediaSignals(ctx, room.MediaRoom) })
	require.NoError(t, err)
	require.Equal(t, "pending", status.State)
	require.Equal(t, "provider_cleanup_pending", status.Reason)
	require.NotEmpty(t, cw.media.present[room.MediaRoom], "failed removal cannot imply a participant was disconnected")
	cw.svc.media = original
	status, err = cw.svc.ReconcileMediaSignals(ctx, room.MediaRoom)
	require.NoError(t, err)
	require.Equal(t, "clear", status.State)
	require.Empty(t, cw.media.present[room.MediaRoom])

	require.Panics(t, func() {
		_ = cw.svc.mediaSignalGated(ctx, *room, binding, func(context.Context, roomRow) error { panic("synthetic callback failure") })
	})
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	require.NoError(t, cw.svc.mediaSignalGated(bounded, *room, binding, func(context.Context, roomRow) error { return nil }), "the recovery boundary must not inherit leaked write gates")
}
