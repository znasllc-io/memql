package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestHistoryCurrentCallIsOwnedAndIndependentOfHistoryPage(t *testing.T) {
	cw := newCallWorld(t, "history-current", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host := cw.host()
	far := cw.booked(host, 3*time.Hour, 30, "Later booking")
	near := cw.booked(host, time.Hour, 30, "Next booking")
	other := cw.host()
	cw.booked(other, 30*time.Minute, 15, "Another key's booking")
	read := func(request callrules.HistoryRequest) callrules.HistoryPage {
		t.Helper()
		page, err := allowanceReplica(t, cw).History(ctx, host, request)
		require.NoError(t, err)
		return page
	}
	first := read(callrules.HistoryRequest{Limit: 1})
	require.NotNil(t, first.CurrentCall)
	require.Equal(t, near.Title, first.CurrentCall.Title)
	require.Equal(t, historyStamp(near.Start), first.CurrentCall.ScheduledAt)
	require.Equal(t, "scheduled", first.CurrentCall.Status)
	require.Equal(t, "host", first.CurrentCall.Role)
	require.Nil(t, first.CurrentCall.CostCents)
	require.NotEqual(t, near.ID, first.CurrentCall.ID, "only the key-scoped history id is exposed")
	require.Len(t, first.Calls, 1)
	require.Equal(t, far.Title, first.Calls[0].Title, "newest-first paging does not choose the next booking")
	require.NotEmpty(t, first.NextCursor)
	second := read(callrules.HistoryRequest{Limit: 1, Cursor: first.NextCursor})
	require.Equal(t, first.CurrentCall.ID, second.CurrentCall.ID)
	filtered := read(callrules.HistoryRequest{Archive: true, Search: "nothing matches", Role: "guest", Status: "ended", Period: 90})
	require.Empty(t, filtered.Calls)
	require.Equal(t, first.CurrentCall.ID, filtered.CurrentCall.ID)

	started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Title: "Current conversation", Confirm: confirmed})
	require.NoError(t, err)
	active := read(callrules.HistoryRequest{Archive: true})
	require.NotNil(t, active.CurrentCall)
	require.Equal(t, "active", active.CurrentCall.Status)
	require.Equal(t, "Current conversation", active.CurrentCall.Title)
	_, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	guestPage, err := cw.svc.History(ctx, guest, callrules.HistoryRequest{})
	require.NoError(t, err)
	require.Nil(t, guestPage.CurrentCall, "an attended room never becomes the guest key's hosted call")
	otherPage, err := cw.svc.History(ctx, other, callrules.HistoryRequest{})
	require.NoError(t, err)
	require.Equal(t, "Another key's booking", otherPage.CurrentCall.Title)

	require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostEnd}))
	after := read(callrules.HistoryRequest{})
	require.Equal(t, first.CurrentCall.ID, after.CurrentCall.ID)
	require.NoError(t, cw.svc.Cancel(ctx, host, near.ID))
	after = read(callrules.HistoryRequest{})
	require.Equal(t, far.Title, after.CurrentCall.Title)
	require.NoError(t, cw.svc.Cancel(ctx, host, far.ID))
	require.Nil(t, read(callrules.HistoryRequest{}).CurrentCall)
}
