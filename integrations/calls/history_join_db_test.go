package calls

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func historyJoin(t *testing.T, svc *Service, sess callrules.Session, id string) (callrules.JoinResult, callrules.Session, error) {
	t.Helper()
	return svc.Join(context.Background(), sess.Site, &sess, callrules.JoinBody{HistoryID: id, Name: "Returned", Confirm: confirmed})
}

func TestHistoryJoinRecoversHostAcrossSessionsAndReplicasWithoutAnotherSeat(t *testing.T) {
	cw := newCallWorld(t, "history-rejoin-host", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host, _ := allowanceAccount(t, cw, "day")
	started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed})
	require.NoError(t, err)
	host, err = cw.svc.Session(ctx, cw.ref, host.Token)
	require.NoError(t, err)
	page := completeHistory(t, cw, host)
	require.Len(t, page.Calls, 1)
	// A fresh passkey login carries the account but no seat or invitation.
	signed, err := accountAssert(t, cw, k, "", "login", "")
	require.NoError(t, err)
	fresh := *signed.Session
	require.Empty(t, fresh.SeatID)
	replica := allowanceReplica(t, cw)
	const clicks = 4
	results := make([]callrules.JoinResult, clicks)
	sessions := make([]callrules.Session, clicks)
	errs := make([]error, clicks)
	var wg sync.WaitGroup
	for i := range clicks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			svc := cw.svc
			if i%2 == 1 {
				svc = replica
			}
			results[i], sessions[i], errs[i] = svc.Join(ctx, cw.ref, &fresh, callrules.JoinBody{HistoryID: page.Calls[0].ID, Name: "Returned", Confirm: confirmed})
		}(i)
	}
	wg.Wait()
	for i := range clicks {
		require.NoError(t, errs[i])
		require.Equal(t, callrules.OutcomeAdmit, results[i].Outcome)
		require.True(t, results[i].Host)
		require.Equal(t, started.Join.Identity, results[i].Identity)
		require.Equal(t, host.SeatID, sessions[i].SeatID)
		require.Equal(t, started.Call.ID, sessions[i].RoomID)
	}
	require.Len(t, cw.rowsOf(roomConcept), 1)
	require.Len(t, cw.rowsOf(seatConcept), 1)
	// Recovery also clears the original host-left countdown after a real leave.
	_, _, err = cw.join(started.Call.GuestToken, "Keeps the call open")
	require.NoError(t, err)
	require.NoError(t, replica.Leave(ctx, sessions[0]))
	require.False(t, rowTime(cw.row(roomConcept, started.Call.ID), "hostLeftAt").IsZero())
	signed, err = accountAssert(t, cw, k, "", "login", "")
	require.NoError(t, err)
	joined, _, err := historyJoin(t, replica, *signed.Session, page.Calls[0].ID)
	require.NoError(t, err)
	require.True(t, joined.Host)
	require.NotEqual(t, started.Join.Identity, joined.Identity, "returning after Leave starts a new media admission")
	require.True(t, rowTime(cw.row(roomConcept, started.Call.ID), "hostLeftAt").IsZero())
	require.Len(t, cw.rowsOf(seatConcept), 2)
}

func TestHistoryJoinPreservesGuestAuthorityRemovalAndAnotherCall(t *testing.T) {
	cw := newCallWorld(t, "history-rejoin-guest", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, started := cw.started("Host")
	guest := cw.host()
	initial, guest, err := cw.svc.Join(ctx, cw.ref, &guest, callrules.JoinBody{Token: started.Call.GuestToken, Name: "Guest", Confirm: confirmed})
	require.NoError(t, err)
	page := completeHistory(t, cw, guest)
	require.Len(t, page.Calls, 1)
	id := page.Calls[0].ID
	// Even a wrong projection role cannot manufacture live host authority.
	row, err := cw.svc.store.historyByID(ctx, cw.owner, id)
	require.NoError(t, err)
	require.NoError(t, cw.svc.store.updateHistory(ctx, cw.owner, *row, map[string]any{"role": "host"}, cw.clock.Now()))
	fresh, err := cw.svc.Unlock(ctx, cw.ref, cw.keyOf(guest))
	require.NoError(t, err)
	joined, returned, err := historyJoin(t, cw.svc, fresh, id)
	require.NoError(t, err)
	require.False(t, joined.Host)
	require.Equal(t, initial.Identity, joined.Identity)
	require.Equal(t, guest.SeatID, returned.SeatID)
	require.Len(t, cw.rowsOf(seatConcept), 2)
	require.NoError(t, cw.svc.Leave(ctx, returned))
	otherHost, other := cw.started("Another host")
	_, returned, err = cw.svc.Join(ctx, cw.ref, &returned, callrules.JoinBody{Token: other.Call.GuestToken, Name: "Guest elsewhere", Confirm: confirmed})
	require.NoError(t, err)
	_, _, err = historyJoin(t, cw.svc, returned, id)
	requireReason(t, err, callrules.ErrConflict, callrules.ReasonInAnotherCall)
	require.NoError(t, cw.act(otherHost, callrules.HostEnd, ""))
	_, returned, err = historyJoin(t, cw.svc, returned, id)
	require.NoError(t, err)
	require.NoError(t, cw.act(host, callrules.HostRemove, returned.SeatID))
	fresh, err = cw.svc.Unlock(ctx, cw.ref, cw.keyOf(guest))
	require.NoError(t, err)
	_, _, err = historyJoin(t, cw.svc, fresh, id)
	requireReason(t, err, callrules.ErrForbidden, callrules.ReasonRemoved)
}

func TestHistoryJoinKeepsQueuePositionAcrossFreshSessions(t *testing.T) {
	cw := newCallWorld(t, "history-rejoin-queue", 1)
	if cw == nil {
		return
	}
	cw.started("Fills the server")
	host := cw.host()
	started, err := cw.svc.Start(context.Background(), host, callrules.StartBody{Name: "Queued host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, started.Join.Outcome)
	page := completeHistory(t, cw, host)
	require.Len(t, page.Calls, 1)
	before := cw.rowsOf(queueEntryConcept)
	fresh, err := cw.svc.Unlock(context.Background(), cw.ref, cw.keyOf(host))
	require.NoError(t, err)
	joined, returned, err := historyJoin(t, cw.svc, fresh, page.Calls[0].ID)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, joined.Outcome)
	require.Equal(t, started.Join.Position, joined.Position)
	require.Equal(t, started.Call.ID, returned.RoomID)
	after := cw.rowsOf(queueEntryConcept)
	require.Len(t, after, len(before))
	for id, row := range before {
		require.Equal(t, rowTime(row, "enqueuedAt"), rowTime(after[id], "enqueuedAt"))
	}
	require.Len(t, cw.rowsOf(seatConcept), 2)
}

func TestHistoryJoinRejectsOtherKeysSitesEndedAndUnavailableHistory(t *testing.T) {
	cw := newCallWorld(t, "history-rejoin-scope", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, started := cw.started("Host")
	page := completeHistory(t, cw, host)
	id := page.Calls[0].ID
	other := cw.host()
	_, _, err := historyJoin(t, cw.svc, other, id)
	require.ErrorIs(t, err, callrules.ErrNotFound)
	wrongSite := host
	wrongSite.Site.SiteID = "another-site"
	_, _, err = historyJoin(t, cw.svc, wrongSite, id)
	require.ErrorIs(t, err, callrules.ErrNoSession)
	for _, req := range []callrules.JoinBody{
		{HistoryID: "not-an-id", Name: "Guest", Confirm: confirmed},
		{HistoryID: id, Token: started.Call.GuestToken, Name: "Guest", Confirm: confirmed},
		{HistoryID: id, HostToken: started.Call.HostToken, Name: "Guest", Confirm: confirmed},
	} {
		_, _, err = cw.svc.Join(ctx, cw.ref, &host, req)
		require.ErrorIs(t, err, callrules.ErrInvalid)
	}
	_, _, err = cw.svc.Join(ctx, cw.ref, nil, callrules.JoinBody{HistoryID: id, Name: "Guest", Confirm: confirmed})
	require.ErrorIs(t, err, callrules.ErrNoSession)
	_, _, err = historyJoin(t, cw.svc, host, strings.Repeat("0", 64))
	require.ErrorIs(t, err, callrules.ErrNotFound)
	cw.asOwnerMutate(keyConcept, host.KeyID, "updateCallsKey", map[string]any{"keyId": host.KeyID, "status": "frozen"})
	_, _, err = historyJoin(t, cw.svc, host, id)
	requireReason(t, err, callrules.ErrForbidden, callrules.ReasonKeyNotActive)
	cw.asOwnerMutate(keyConcept, host.KeyID, "updateCallsKey", map[string]any{"keyId": host.KeyID, "status": "active"})
	require.NoError(t, cw.act(host, callrules.HostEnd, ""))
	_, _, err = historyJoin(t, cw.svc, host, id)
	requireReason(t, err, callrules.ErrEnded, callrules.ReasonCallEnded)
	preview, err := cw.svc.HistoryAction(ctx, host, callrules.HistoryActionRequest{Action: "archive_preview", ID: id})
	require.NoError(t, err)
	_, err = cw.svc.HistoryAction(ctx, host, callrules.HistoryActionRequest{Action: "archive", ID: id, ConfirmationToken: preview.ConfirmationToken, Acknowledged: true})
	require.NoError(t, err)
	_, _, err = historyJoin(t, cw.svc, host, id)
	require.ErrorIs(t, err, callrules.ErrNotFound)
	require.Len(t, cw.rowsOf(roomConcept), 1)
}

func TestHistoryJoinResolvesOpenedBookingWithoutOpeningAnotherRoom(t *testing.T) {
	cw := newCallWorld(t, "history-rejoin-booking", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host := cw.host()
	booking := cw.booked(host, time.Hour, 30, "Review")
	page := completeHistory(t, cw, host)
	require.Len(t, page.Calls, 1)
	_, _, err := historyJoin(t, cw.svc, host, page.Calls[0].ID)
	require.ErrorIs(t, err, callrules.ErrNotFound)
	require.Empty(t, cw.rowsOf(roomConcept))
	cw.clock.Advance(time.Hour)
	initial, host, err := cw.svc.Join(ctx, cw.ref, &host, callrules.JoinBody{HostToken: booking.HostToken, Name: "Host", Confirm: confirmed})
	require.NoError(t, err)
	fresh, err := cw.svc.Unlock(ctx, cw.ref, cw.keyOf(host))
	require.NoError(t, err)
	joined, returned, err := historyJoin(t, cw.svc, fresh, page.Calls[0].ID)
	require.NoError(t, err)
	require.Equal(t, initial.Identity, joined.Identity)
	require.Equal(t, host.RoomID, returned.RoomID)
	require.True(t, joined.Host)
	require.Len(t, cw.rowsOf(roomConcept), 1)
}

func TestHistoryJoinCannotBypassRemovalThroughAnotherSeatOfTheKey(t *testing.T) {
	cw := newCallWorld(t, "history-rejoin-removal", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, started := cw.started("Host")
	guest := cw.host()
	_, first, err := cw.svc.Join(ctx, cw.ref, &guest, callrules.JoinBody{Token: started.Call.GuestToken, Name: "First seat", Confirm: confirmed})
	require.NoError(t, err)
	page := completeHistory(t, cw, first)
	fresh, err := cw.svc.Unlock(ctx, cw.ref, cw.keyOf(guest))
	require.NoError(t, err)
	_, second, err := cw.svc.Join(ctx, cw.ref, &fresh, callrules.JoinBody{Token: started.Call.GuestToken, Name: "Another seat", Confirm: confirmed})
	require.NoError(t, err)
	require.NotEqual(t, first.SeatID, second.SeatID)
	require.NoError(t, cw.act(host, callrules.HostRemove, second.SeatID))
	_, _, err = historyJoin(t, cw.svc, first, page.Calls[0].ID)
	requireReason(t, err, callrules.ErrForbidden, callrules.ReasonRemoved)
}

type pausedHistoryJoinEngine struct {
	Engine
	once             sync.Once
	lookedUp, resume chan struct{}
}

func (e *pausedHistoryJoinEngine) Execute(ctx context.Context, query string) (any, error) {
	result, err := e.Engine.Execute(ctx, query)
	if strings.Contains(query, "callsRoomById(") {
		e.once.Do(func() {
			close(e.lookedUp)
			select {
			case <-e.resume:
			case <-ctx.Done():
			}
		})
	}
	return result, err
}

func TestHistoryJoinRechecksAuthorityAfterInitialLookup(t *testing.T) {
	for _, changed := range []string{"ended", "logged-out", "key-switched", "credential-revoked"} {
		t.Run(changed, func(t *testing.T) {
			cw := newCallWorld(t, "history-rejoin-race", 20)
			if cw == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var host callrules.Session
			var account accountTestKey
			if changed == "credential-revoked" {
				account, host, _ = allowanceAccount(t, cw, "day")
				_, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed})
				require.NoError(t, err)
			} else {
				host, _ = cw.started("Host")
			}
			page := completeHistory(t, cw, host)
			other := cw.host()
			var fresh callrules.Session
			var err error
			if changed == "credential-revoked" {
				signed, e := accountAssert(t, cw, account, "", "login", "")
				require.NoError(t, e)
				fresh = *signed.Session
			} else {
				fresh, err = cw.svc.Unlock(ctx, cw.ref, cw.keyOf(host))
				require.NoError(t, err)
			}
			paused := &pausedHistoryJoinEngine{Engine: cw.svc.store.engine, lookedUp: make(chan struct{}), resume: make(chan struct{})}
			release := sync.OnceFunc(func() { close(paused.resume) })
			defer release()
			store := *cw.svc.store
			store.engine = paused
			svc := *cw.svc
			svc.store = &store
			done := make(chan error, 1)
			go func() {
				_, _, e := svc.Join(ctx, cw.ref, &fresh, callrules.JoinBody{HistoryID: page.Calls[0].ID, Name: "Returned", Confirm: confirmed})
				done <- e
			}()
			select {
			case <-paused.lookedUp:
			case <-ctx.Done():
				t.Fatal("history lookup did not reach the pause")
			}
			want := callrules.ErrNoSession
			switch changed {
			case "ended":
				require.NoError(t, cw.act(host, callrules.HostEnd, ""))
				want = callrules.ErrEnded
			case "logged-out":
				_, err = cw.svc.Account(ctx, cw.ref, fresh.Token, &callrules.AccountRequest{Action: "logout"})
				require.NoError(t, err)
				want = callrules.ErrConflict
			case "key-switched":
				_, err = cw.svc.UnlockReplacing(ctx, cw.ref, cw.keyOf(other), fresh.Token)
				require.NoError(t, err)
				want = callrules.ErrConflict
			case "credential-revoked":
				credential, e := cw.svc.store.credentialByID(ctx, cw.owner, account.result.Credentials[0].ID)
				require.NoError(t, e)
				require.NoError(t, cw.svc.store.updateCredential(ctx, cw.owner, *credential, map[string]any{"revokedAt": versionStamp(cw.clock.Now())}, cw.clock.Now()))
				want = callrules.ErrConflict
			}
			release()
			require.ErrorIs(t, <-done, want)
			require.Len(t, cw.rowsOf(seatConcept), 1)
		})
	}
}

func TestHistoryJoinBindsRecoveredSeatToFreshCredential(t *testing.T) {
	cw := newCallWorld(t, "history-rejoin-credential", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, started := cw.started("Host")
	first := accountRegister(t, cw, newSoftwareAuthenticator(t))
	begin, err := cw.svc.Account(ctx, cw.ref, first.result.Session.Token, &callrules.AccountRequest{Action: "add_begin"})
	require.NoError(t, err)
	challenge, rp, _ := accountCreation(t, begin)
	second := newSoftwareAuthenticator(t)
	_, err = cw.svc.Account(ctx, cw.ref, first.result.Session.Token, &callrules.AccountRequest{Action: "add_finish", ChallengeID: begin.ChallengeID, Credential: second.Attest(challenge, rp, "https://"+rp)})
	require.NoError(t, err)
	initial, original, err := cw.svc.Join(ctx, cw.ref, first.result.Session, callrules.JoinBody{Token: started.Call.GuestToken, Name: "Guest", Confirm: confirmed})
	require.NoError(t, err)
	page := completeHistory(t, cw, original)
	secondKey := first
	secondKey.auth = second
	signed, err := accountAssert(t, cw, secondKey, "", "login", "")
	require.NoError(t, err)
	joined, returned, err := historyJoin(t, cw.svc, *signed.Session, page.Calls[0].ID)
	require.NoError(t, err)
	require.Equal(t, initial.Identity, joined.Identity)
	require.Equal(t, original.SeatID, returned.SeatID)
	_, err = cw.svc.Account(ctx, cw.ref, returned.Token, &callrules.AccountRequest{Action: "revoke", CredentialID: first.result.Credentials[0].ID})
	require.NoError(t, err)
	_, err = cw.svc.Session(ctx, cw.ref, original.Token)
	require.ErrorIs(t, err, callrules.ErrNoSession)
	_, err = cw.svc.Session(ctx, cw.ref, returned.Token)
	require.NoError(t, err)
	seat, err := cw.svc.store.seatByID(ctx, cw.owner, returned.SeatID)
	require.NoError(t, err)
	require.True(t, seat.occupying(), "revoking the old credential must not disconnect the recovered seat")
	require.NotEqual(t, first.result.Credentials[0].ID, seat.CredentialID)
	polled, err := cw.svc.Queue(ctx, returned)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, polled.Outcome)
	require.Equal(t, initial.Identity, polled.Identity)
}
