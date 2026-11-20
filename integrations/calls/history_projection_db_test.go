package calls

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

type interruptedHistoryEngine struct {
	Engine
	mode     string
	failures int
}

func (e *interruptedHistoryEngine) Execute(ctx context.Context, query string) (any, error) {
	fail := e.mode == "projection" && (strings.Contains(query, "createCallsHistory(") || strings.Contains(query, "updateCallsHistory("))
	fail = fail || e.mode == "acknowledgment" && strings.Contains(query, "updateCallsRoom(") && strings.Contains(query, "historyPending")
	if fail {
		e.failures++
		return nil, errors.New("test history persistence interruption")
	}
	return e.Engine.Execute(ctx, query)
}

func TestHistoryProjectionFailureStaysPendingAcrossReplicas(t *testing.T) {
	for _, mode := range []string{"projection", "acknowledgment"} {
		t.Run(mode, func(t *testing.T) {
			cw := newCallWorld(t, "history-pending-"+mode, 20)
			if cw == nil {
				return
			}
			ctx := context.Background()
			host := cw.host()
			require.False(t, completeHistory(t, cw, host).Collecting)
			store := *cw.svc.store
			broken := &interruptedHistoryEngine{Engine: store.engine, mode: mode}
			store.engine = broken
			service := *cw.svc
			service.store = &store
			started, err := service.Start(ctx, host, callrules.StartBody{Name: "Host", Title: "Durable source", Confirm: confirmed})
			require.NoError(t, err, "secondary history must not lose the one-time call links")
			require.NotEmpty(t, started.Call.GuestToken)
			require.Positive(t, broken.failures)
			pending, err := cw.svc.store.historyPendingForKey(ctx, cw.ref.OwnerUserID, cw.ref.SiteID, host.KeyID)
			require.NoError(t, err)
			require.True(t, pending, "the bit survived outside the failed projection process")
			page, err := service.History(ctx, host, callrules.HistoryRequest{})
			if mode == "projection" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.True(t, page.Collecting)
				require.Nil(t, page.Overview.HostedCalls)
				require.Nil(t, page.Overview.HostedSeconds)
			}
			peer := secondReplica(t, cw.eng).Service()
			peer.now, peer.media = cw.clock.Now, cw.media.backend
			page, err = peer.History(ctx, host, callrules.HistoryRequest{})
			require.NoError(t, err)
			require.False(t, page.Collecting)
			require.Len(t, page.Calls, 1)
			require.Equal(t, int64(1), *page.Overview.HostedCalls)
			pending, err = cw.svc.store.historyPendingForKey(ctx, cw.ref.OwnerUserID, cw.ref.SiteID, host.KeyID)
			require.NoError(t, err)
			require.False(t, pending)
		})
	}
}

func TestHistorySourceAcknowledgmentCannotClearANewerChange(t *testing.T) {
	cw := newCallWorld(t, "history-source-fence", 20)
	if cw == nil {
		return
	}
	ctx := memql.ContextWithFreshRead(context.Background())
	host, started := cw.started("Host")
	room, err := cw.svc.store.roomByID(ctx, cw.ref.OwnerUserID, started.Call.ID)
	require.NoError(t, err)
	require.NoError(t, cw.svc.store.updateRoom(ctx, cw.ref.OwnerUserID, *room, map[string]any{"title": "First"}, memql.VersionTimeAfter(room.Version, cw.clock.Now())))
	source, err := cw.svc.store.roomByID(ctx, cw.ref.OwnerUserID, room.ID)
	require.NoError(t, err)
	require.True(t, source.HistoryPending)
	h, err := cw.svc.authenticatedKey(ctx, host)
	require.NoError(t, err)
	row, err := cw.svc.roomHistory(ctx, h.site, h.row.KeyID, *source, h.pepper)
	require.NoError(t, err)
	_, err = cw.svc.upsertHistory(ctx, row)
	require.NoError(t, err)
	require.NoError(t, cw.svc.store.updateRoom(ctx, cw.ref.OwnerUserID, *source, map[string]any{"title": "Second"}, memql.VersionTimeAfter(source.Version, cw.clock.Now())))
	cw.svc.acknowledgeHistorySource(ctx, cw.ref.OwnerUserID, host.KeyID, *source)
	latest, err := cw.svc.store.roomByID(ctx, cw.ref.OwnerUserID, room.ID)
	require.NoError(t, err)
	require.True(t, latest.HistoryPending)
	require.Equal(t, "Second", latest.Title)
	page := completeHistory(t, cw, host)
	require.Equal(t, "Second", page.Calls[0].Title)
	// An unopened expired booking never becomes an attended/hosted call merely
	// because its scheduled window finished.
	booked, err := cw.svc.Schedule(ctx, host, callrules.ScheduleRequest{Start: cw.clock.Now().Add(time.Hour), Minutes: 30})
	require.NoError(t, err)
	cw.asOwnerMutate(scheduledCallConcept, booked.ID, "updateCallsScheduledCall", map[string]any{"scheduledCallId": booked.ID, "status": "ended", "historyPending": true})
	page = completeHistory(t, cw, host)
	require.Equal(t, int64(1), *page.Overview.HostedCalls)
	for _, call := range page.Calls {
		if call.Status == "ended" {
			require.Nil(t, call.DurationSeconds)
		}
	}
}

func TestGuestHistoryReadCannotAcknowledgeTheHostsPendingProjection(t *testing.T) {
	cw := newCallWorld(t, "history-guest-pending-scope", 20)
	if cw == nil {
		return
	}
	ctx := memql.ContextWithFreshRead(context.Background())
	host, first := cw.started("First host")
	_, guest, err := cw.join(first.Call.GuestToken, "Guest")
	require.NoError(t, err)
	completeHistory(t, cw, guest)
	require.NoError(t, cw.svc.Leave(ctx, guest))
	_, second := cw.started("Other host")
	_, guest, err = cw.rejoin(guest, second.Call.GuestToken)
	require.NoError(t, err)
	_, err = cw.svc.RenameCall(ctx, host, "Host projection still pending", first.Join.Identity)
	require.NoError(t, err)
	room, err := cw.svc.store.roomByID(ctx, cw.ref.OwnerUserID, first.Call.ID)
	require.NoError(t, err)
	require.True(t, room.HistoryPending)
	// This read observes the guest's different current room, then refreshes its
	// older guest projection. Updating that guest title must not certify the host.
	page, err := cw.svc.History(ctx, guest, callrules.HistoryRequest{Limit: 50})
	require.NoError(t, err)
	found := false
	for _, row := range page.Calls {
		found = found || row.Title == "Host projection still pending"
	}
	require.True(t, found)
	room, err = cw.svc.store.roomByID(ctx, cw.ref.OwnerUserID, first.Call.ID)
	require.NoError(t, err)
	require.True(t, room.HistoryPending, "a guest read cannot clear the owner's dirty bit")
	pending, err := cw.svc.store.historyPendingForKey(ctx, cw.ref.OwnerUserID, cw.ref.SiteID, host.KeyID)
	require.NoError(t, err)
	require.True(t, pending)
	completeHistory(t, cw, host)
	room, err = cw.svc.store.roomByID(ctx, cw.ref.OwnerUserID, first.Call.ID)
	require.NoError(t, err)
	require.False(t, room.HistoryPending, "the actual owner projection clears its own source")
}
