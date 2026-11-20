package calls

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func historyPage(t *testing.T, cw *callWorld, session callrules.Session, archive bool) callrules.HistoryPage {
	t.Helper()
	page, err := cw.svc.History(context.Background(), session, callrules.HistoryRequest{Archive: archive, Period: 7})
	require.NoError(t, err)
	return page
}
func completeHistory(t *testing.T, cw *callWorld, session callrules.Session) callrules.HistoryPage {
	t.Helper()
	for i := 0; i < 20; i++ {
		page := historyPage(t, cw, session, false)
		if !page.Collecting {
			return page
		}
	}
	t.Fatal("bounded history repair did not finish")
	return callrules.HistoryPage{}
}
func TestPrivateHistorySeparatesKeysAndPreservesArchiveDeadline(t *testing.T) {
	cw := newCallWorld(t, "history-privacy", 20)
	ctx := context.Background()
	host, start := cw.started("Host")
	other := cw.host()
	title, err := cw.svc.RenameCall(ctx, host, "<b>Review</b>", start.Join.Identity)
	require.NoError(t, err)
	require.Equal(t, "<b>Review</b>", title)
	_, err = cw.svc.RenameCall(ctx, host, "Stale", "different-old-identity")
	require.Error(t, err)
	page := completeHistory(t, cw, host)
	require.Len(t, page.Calls, 1)
	row := page.Calls[0]
	require.Equal(t, title, row.Title)
	require.Equal(t, "host", row.Role)
	require.Equal(t, "active", row.Status)
	require.True(t, row.CanRename)
	require.Nil(t, row.CostCents)
	require.Empty(t, completeHistory(t, cw, other).Calls)
	_, err = cw.svc.HistoryAction(ctx, other, callrules.HistoryActionRequest{Action: "archive_preview", ID: row.ID})
	require.ErrorIs(t, err, callrules.ErrNotFound)
	_, err = cw.svc.HistoryAction(ctx, host, callrules.HistoryActionRequest{Action: "archive_preview", ID: row.ID})
	require.Equal(t, "history_not_finished", callrules.ReasonOf(err))
	cw.clock.Advance(90 * time.Second)
	require.NoError(t, cw.act(host, callrules.HostEnd, ""))
	page = historyPage(t, cw, host, false)
	require.Len(t, page.Calls, 1)
	row = page.Calls[0]
	require.Equal(t, int64(90), *row.DurationSeconds)
	renamedTitle := "Updated review"
	renamed, err := cw.svc.HistoryAction(ctx, host, callrules.HistoryActionRequest{Action: "rename", ID: row.ID, Title: &renamedTitle})
	require.NoError(t, err)
	require.Equal(t, renamedTitle, renamed.Call.Title)
	require.True(t, renamed.Call.CanRename, "a successful rename keeps the authorized rename affordance")
	preview, err := cw.svc.HistoryAction(ctx, host, callrules.HistoryActionRequest{Action: "archive_preview", ID: row.ID})
	require.NoError(t, err)
	require.NotEmpty(t, preview.ConfirmationToken)
	request := callrules.HistoryActionRequest{Action: "archive", ID: row.ID, ConfirmationToken: preview.ConfirmationToken, Acknowledged: true}
	refused := request
	refused.Acknowledged = false
	_, err = cw.svc.HistoryAction(ctx, host, refused)
	require.ErrorIs(t, err, callrules.ErrInvalid)
	archived, err := cw.svc.HistoryAction(ctx, host, request)
	require.NoError(t, err)
	require.Equal(t, preview.DeleteAt, archived.DeleteAt)
	cw.clock.Advance(time.Minute)
	duplicate, err := cw.svc.HistoryAction(ctx, host, request)
	require.NoError(t, err)
	require.Equal(t, preview.DeleteAt, duplicate.DeleteAt)
	require.Empty(t, historyPage(t, cw, host, false).Calls)
	require.Len(t, historyPage(t, cw, host, true).Calls, 1)
	restored, err := cw.svc.HistoryAction(ctx, host, callrules.HistoryActionRequest{Action: "restore", ID: row.ID})
	require.NoError(t, err)
	require.Empty(t, restored.Call.ArchivedAt)
	_, err = cw.svc.HistoryAction(ctx, host, callrules.HistoryActionRequest{Action: "restore", ID: row.ID})
	require.NoError(t, err)
	require.Len(t, historyPage(t, cw, host, false).Calls, 1)
	// A fresh confirmation after restore owns a fresh deadline; an old intent may
	// not silently archive the restored metadata again.
	_, err = cw.svc.HistoryAction(ctx, host, request)
	require.Equal(t, "archive_changed", callrules.ReasonOf(err))
	preview, err = cw.svc.HistoryAction(ctx, host, callrules.HistoryActionRequest{Action: "archive_preview", ID: row.ID})
	require.NoError(t, err)
	request.ConfirmationToken = preview.ConfirmationToken
	_, err = cw.svc.HistoryAction(ctx, host, request)
	require.NoError(t, err)
	deadline, err := time.Parse(time.RFC3339Nano, preview.DeleteAt)
	require.NoError(t, err)
	cw.clock.Advance(deadline.Sub(cw.clock.Now()))
	fresh, err := cw.svc.Unlock(ctx, cw.ref, cw.keyOf(host))
	require.NoError(t, err)
	_, err = cw.svc.HistoryAction(ctx, fresh, callrules.HistoryActionRequest{Action: "restore", ID: row.ID})
	require.Equal(t, "history_expired", callrules.ReasonOf(err))
	require.Empty(t, historyPage(t, cw, fresh, true).Calls)
	require.Equal(t, "ended", cw.row(roomConcept, start.Call.ID)["status"], "history expiry leaves operational room intact")
	// The retention component proves the gated physical delete separately. Once
	// that delete has happened, source repair must not reconstruct this metadata.
	_, err = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept=? AND id=?", historyConcept, historyConcept+":"+row.ID).Exec(ctx)
	require.NoError(t, err)
	for i := 0; i < 4; i++ {
		page, err = cw.svc.History(memql.ContextWithFreshRead(ctx), fresh, callrules.HistoryRequest{})
		require.NoError(t, err)
		require.Empty(t, page.Calls)
	}
	require.Empty(t, cw.rowsOf(historyConcept))
	require.Len(t, cw.rowsOf(historyReceiptConcept), 1)
}

func TestJoinedHistoryAndCursorCannotCrossKeys(t *testing.T) {
	cw := newCallWorld(t, "history-cursor", 20)
	ctx := context.Background()
	host, start := cw.started("Host")
	guest := cw.host()
	joined, guest, err := cw.svc.Join(ctx, cw.ref, &guest, callrules.JoinBody{Token: start.Call.GuestToken, Name: "Guest", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	page := completeHistory(t, cw, guest)
	require.Len(t, page.Calls, 1)
	require.Equal(t, "guest", page.Calls[0].Role)
	require.False(t, page.Calls[0].CanRename)
	title := "Forbidden"
	_, err = cw.svc.HistoryAction(ctx, guest, callrules.HistoryActionRequest{Action: "rename", ID: page.Calls[0].ID, Title: &title})
	require.ErrorIs(t, err, callrules.ErrForbidden)
	require.NoError(t, cw.svc.Leave(ctx, host))
	require.False(t, historyPage(t, cw, host, false).Calls[0].CanRename, "a departed owner cannot rename a live call")
	_, host, err = cw.rejoin(host, "")
	require.NoError(t, err)
	require.True(t, historyPage(t, cw, host, false).Calls[0].CanRename)
	require.NoError(t, cw.act(host, callrules.HostEnd, ""))
	cw.clock.Advance(time.Second)
	_, err = cw.svc.Start(ctx, host, callrules.StartBody{Name: "Again", Confirm: confirmed})
	require.NoError(t, err)
	completeHistory(t, cw, host)
	first, err := cw.svc.History(ctx, host, callrules.HistoryRequest{Limit: 1})
	require.NoError(t, err)
	require.Len(t, first.Calls, 1)
	require.NotEmpty(t, first.NextCursor)
	// New calls sort before this cursor. They must not shift the second page to
	// repeat the first row, as an offset page would do.
	require.NoError(t, cw.act(host, callrules.HostEnd, ""))
	cw.clock.Advance(time.Second)
	_, err = cw.svc.Start(ctx, host, callrules.StartBody{Name: "New while paging", Confirm: confirmed})
	require.NoError(t, err)
	second, err := cw.svc.History(ctx, host, callrules.HistoryRequest{Limit: 1, Cursor: first.NextCursor})
	require.NoError(t, err)
	require.Len(t, second.Calls, 1)
	require.NotEqual(t, first.Calls[0].ID, second.Calls[0].ID)
	require.Equal(t, page.Calls[0].StartedAt, second.Calls[0].StartedAt)
	_, err = cw.svc.History(ctx, guest, callrules.HistoryRequest{Limit: 1, Cursor: first.NextCursor})
	require.Equal(t, "cursor_invalid", callrules.ReasonOf(err))
	wrongSite := host
	wrongSite.Site.SiteID = "other-site"
	_, err = cw.svc.History(ctx, wrongSite, callrules.HistoryRequest{})
	require.ErrorIs(t, err, callrules.ErrNoSession)
}

func TestGuestKeyHistoryAndConcurrentArchiveUsePersistedAuthority(t *testing.T) {
	cw := newCallWorld(t, "history-guest-key", 20)
	ctx := context.Background()
	host, start := cw.started("Host")
	account := accountRegister(t, cw, newSoftwareAuthenticator(t))
	guest := *account.result.Session
	_, guest, err := cw.svc.Join(ctx, cw.ref, &guest, callrules.JoinBody{Token: start.Call.GuestToken, Name: "Guest", Confirm: confirmed})
	require.NoError(t, err)
	page := completeHistory(t, cw, guest)
	require.Len(t, page.Calls, 1)
	require.Equal(t, account.result.Account.KeyID, page.AccountID)
	require.Equal(t, "guest", page.Calls[0].Role)
	cw.clock.Advance(time.Minute)
	require.NoError(t, cw.act(host, callrules.HostEnd, ""))
	page = historyPage(t, cw, guest, false)
	id := page.Calls[0].ID
	preview, err := cw.svc.HistoryAction(ctx, guest, callrules.HistoryActionRequest{Action: "archive_preview", ID: id})
	require.NoError(t, err)
	peer := secondReplica(t, cw.eng).Service()
	peer.now, peer.media = cw.clock.Now, cw.media.backend
	request := callrules.HistoryActionRequest{Action: "archive", ID: id, Acknowledged: true, ConfirmationToken: preview.ConfirmationToken}
	type result struct {
		out callrules.HistoryActionResult
		err error
	}
	done := make(chan result, 2)
	for _, service := range []*Service{cw.svc, peer} {
		go func(service *Service) {
			out, err := service.HistoryAction(ctx, guest, request)
			done <- result{out, err}
		}(service)
	}
	for i := 0; i < 2; i++ {
		got := <-done
		require.NoError(t, got.err)
		require.Equal(t, preview.DeleteAt, got.out.DeleteAt)
	}
	require.Empty(t, historyPage(t, cw, guest, false).Calls)
	require.Len(t, historyPage(t, cw, guest, true).Calls, 1)
	require.Len(t, historyPage(t, cw, host, false).Calls, 1, "guest archive is private to guest key")
	cw.asOwnerMutate(keyConcept, guest.KeyID, "updateCallsKey", map[string]any{"keyId": guest.KeyID, "status": "frozen"})
	require.Len(t, historyPage(t, cw, guest, true).Calls, 1, "frozen account can manage its private metadata")
}
