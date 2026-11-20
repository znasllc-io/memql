package calls

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func enableUsagePreview(t *testing.T, cw *callWorld, enabled int) {
	t.Helper()
	_, err := cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "simulatedFunding": 1, "usagePreview": enabled, "maxPeople": 20})
	require.NoError(t, err)
}

func usagePreviewAccount(t *testing.T, cw *callWorld) (accountTestKey, callrules.Session) {
	t.Helper()
	enableUsagePreview(t, cw, 1)
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	funded := fundAction(t, cw, k, topupRequest(k, 1000, "usage-preview-topup-0001"))
	require.True(t, funded.Capabilities.UsagePreview)
	require.True(t, funded.Account.CanHost)
	require.Equal(t, usagePreviewMode, funded.Account.HostingMode)
	require.Nil(t, funded.Account.ActivePass)
	return k, *k.result.Session
}

func TestUsagePreviewFundingPersistsWithoutActivationOrUsageDebit(t *testing.T) {
	cw := newCallWorld(t, "preview-persist", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host := usagePreviewAccount(t, cw)
	replay := fundAction(t, cw, k, topupRequest(k, 1000, "usage-preview-topup-0001"))
	require.True(t, replay.Replayed)
	require.Len(t, cw.rowsOf(simulationWalletConcept), 1)
	_, err := cw.svc.Account(ctx, cw.ref, host.Token, &callrules.AccountRequest{Action: "logout"})
	require.NoError(t, err)
	signed, err := accountAssert(t, cw, k, "", "login", "")
	require.NoError(t, err)
	host = *signed.Session
	require.True(t, signed.Account.CanHost)
	require.EqualValues(t, 1000, *signed.Account.BalanceCents)
	started, err := allowanceReplica(t, cw).Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, started.Join.Outcome)
	room := cw.row(roomConcept, started.Call.ID)
	require.Equal(t, usagePreviewMode, rowString(room, "fundingMode"))
	require.Empty(t, rowString(room, "fundingPassId"))
	require.Equal(t, cw.clock.Now().Add(usagePreviewDuration), rowTime(room, "fundingDeadline"))
	for range 7 {
		joined, _, err := cw.join(started.Call.GuestToken, "Guest")
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	}
	ninth, _, err := cw.join(started.Call.GuestToken, "Ninth")
	require.Error(t, err)
	require.Equal(t, "room_full", ninth.Reason)
	cw.clock.Advance(time.Minute)
	require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostEnd}))
	page := historyPage(t, cw, host, false)
	require.Len(t, page.Calls, 1)
	require.Nil(t, page.Calls[0].CostCents, "an estimate is never a billed cost")
	preview, err := cw.svc.HistoryAction(ctx, host, callrules.HistoryActionRequest{Action: "archive_preview", ID: page.Calls[0].ID})
	require.NoError(t, err)
	_, err = cw.svc.HistoryAction(ctx, host, callrules.HistoryActionRequest{Action: "archive", ID: page.Calls[0].ID, ConfirmationToken: preview.ConfirmationToken, Acknowledged: true})
	require.NoError(t, err)
	after, err := allowanceReplica(t, cw).Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1000, *after.Account.BalanceCents)
	require.Len(t, cw.rowsOf(simulationWalletConcept), 1, "no activation, debit or archive refund")
}

func TestUsagePreviewRequiresOptInFundsAndClaimedKey(t *testing.T) {
	cw := newCallWorld(t, "preview-authority", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	enableUsagePreview(t, cw, 1)
	empty, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.False(t, empty.Account.CanHost)
	_, err = cw.svc.Start(ctx, *k.result.Session, callrules.StartBody{Name: "Empty", Confirm: confirmed})
	require.Error(t, err)
	funded := fundAction(t, cw, k, topupRequest(k, 1000, "preview-authority-fund"))
	require.True(t, funded.Account.CanHost)
	enableUsagePreview(t, cw, 0)
	disabled, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.False(t, disabled.Capabilities.UsagePreview)
	require.False(t, disabled.Account.CanHost)
	_, err = cw.svc.Start(ctx, *k.result.Session, callrules.StartBody{Name: "Disabled", Confirm: confirmed})
	require.Error(t, err)
	enableUsagePreview(t, cw, 1)
	_, err = cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "simulatedFunding": 0})
	require.NoError(t, err)
	disabled, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.False(t, disabled.Capabilities.UsagePreview, "preview alone cannot enable simulation")
	require.False(t, disabled.Account.CanHost)
	_, err = cw.svc.Start(ctx, *k.result.Session, callrules.StartBody{Name: "Simulation disabled", Confirm: confirmed})
	requireReason(t, err, callrules.ErrForbidden, "simulation_disabled")
	enableUsagePreview(t, cw, 1)
	wallet, err := cw.svc.simulatedWallet(ctx, cw.owner, cw.site, k.result.Account.KeyID)
	require.NoError(t, err)
	policy, _ := cw.svc.store.PolicyFor(ctx, cw.owner, cw.site)
	wrong := cw.ref
	wrong.SiteID = "another-site"
	eligible, err := cw.svc.usagePreviewEligible(ctx, wrong, k.result.Account.KeyID, wallet, policy)
	require.NoError(t, err)
	require.False(t, eligible)
	// A guest key with no passkey cannot borrow somebody else's wallet eligibility.
	key, err := cw.svc.store.keyByID(ctx, cw.owner, k.result.Account.KeyID)
	require.NoError(t, err)
	credentials, err := cw.svc.store.credentialsForKey(ctx, cw.owner, key.ID)
	require.NoError(t, err)
	require.NotEmpty(t, credentials)
	// Revocation is tested through the same durable predicate used when an invited
	// guest opens a scheduled preview without the payer's browser session.
	for _, credential := range credentials {
		require.NoError(t, cw.svc.store.updateCredential(ctx, cw.owner, credential, map[string]any{"revokedAt": cw.clock.Now()}, cw.clock.Now()))
	}
	eligible, err = cw.svc.usagePreviewEligible(ctx, cw.ref, key.ID, wallet, policy)
	require.NoError(t, err)
	require.False(t, eligible)
}

func TestUsagePreviewScheduleAndStartSerializeAcrossReplicas(t *testing.T) {
	cw := newCallWorld(t, "preview-race", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, host := usagePreviewAccount(t, cw)
	_, err := cw.book(host, time.Minute, 181, "Too long")
	requireReason(t, err, callrules.ErrInvalid, "preview_duration")
	booking := cw.booked(host, time.Minute, 180, "Preview")
	other := allowanceReplica(t, cw)
	var errs [2]error
	var wg sync.WaitGroup
	ready := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-ready
		_, errs[0] = cw.svc.Start(ctx, host, callrules.StartBody{Name: "Instant", Confirm: confirmed})
	}()
	go func() {
		defer wg.Done()
		<-ready
		_, _, errs[1] = other.Join(ctx, cw.ref, nil, callrules.JoinBody{Token: booking.GuestToken, Name: "Guest", Confirm: confirmed})
	}()
	close(ready)
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, callrules.ErrConflict)
		}
	}
	require.Equal(t, 1, wins)
	open, err := other.store.openRoomsForKey(ctx, cw.owner, host.KeyID)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, usagePreviewMode, open[0].FundingMode)
}

func TestUsagePreviewDeadlineSurvivesTopUpAndFailedMediaCleanup(t *testing.T) {
	cw := newCallWorld(t, "preview-deadline", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host := usagePreviewAccount(t, cw)
	started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed})
	require.NoError(t, err)
	deadline := rowTime(cw.row(roomConcept, started.Call.ID), "fundingDeadline")
	cw.clock.Advance(time.Hour)
	fundAction(t, cw, k, topupRequest(k, 1000, "preview-extra-funds01"))
	other := allowanceReplica(t, cw)
	view, err := other.SessionCall(ctx, host)
	require.NoError(t, err)
	require.Equal(t, usagePreviewMode, view.FundingMode)
	require.Equal(t, deadline, view.AllowanceEndsAt)
	cw.advanceTo(deadline.Add(time.Second))
	_, err = other.Queue(ctx, host)
	requireReason(t, err, callrules.ErrEnded, endReasonAllowance)
	cw.media.mu.Lock()
	cw.media.failDelete = errors.New("temporary outage")
	cw.media.mu.Unlock()
	require.Error(t, other.Reconcile(ctx))
	require.Equal(t, "open", rowString(cw.row(roomConcept, started.Call.ID), "status"))
	_, err = other.Start(ctx, host, callrules.StartBody{Name: "Cannot overlap cleanup", Confirm: confirmed})
	require.Error(t, err)
	cw.media.mu.Lock()
	cw.media.failDelete = nil
	cw.media.mu.Unlock()
	require.NoError(t, other.Reconcile(ctx))
	require.Equal(t, deadline, rowTime(cw.row(roomConcept, started.Call.ID), "endedAt"))
	after, err := other.Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 2000, *after.Account.BalanceCents)
	_, err = other.Start(ctx, host, callrules.StartBody{Name: "New bounded preview", Confirm: confirmed})
	require.NoError(t, err)
}

func TestUsagePreviewRepairsFirstAdmissionWithoutExtendingDeadline(t *testing.T) {
	cw := newCallWorld(t, "preview-repair", 1)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, host := usagePreviewAccount(t, cw)
	filler, _ := cw.started("Full")
	started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Queued", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, started.Join.Outcome)
	deadline := rowTime(cw.row(roomConcept, started.Call.ID), "fundingDeadline")
	cw.clock.Advance(time.Minute)
	require.NoError(t, cw.svc.Leave(ctx, filler))
	broken := *cw.svc
	store := *broken.store
	interruption := &interruptedFundingStartEngine{Engine: store.engine}
	store.engine, broken.store = interruption, &store
	_, err = broken.Queue(ctx, host)
	require.Error(t, err)
	require.True(t, interruption.failed)
	began := cw.clock.Now()
	cw.clock.Advance(time.Minute)
	_, err = allowanceReplica(t, cw).Queue(ctx, host)
	require.NoError(t, err)
	room := cw.row(roomConcept, started.Call.ID)
	require.Equal(t, began, rowTime(room, "fundingStartedAt"))
	require.Equal(t, deadline, rowTime(room, "fundingDeadline"))
	cw.advanceTo(deadline.Add(-10 * time.Second))
	renewed, err := cw.svc.Queue(ctx, host)
	require.NoError(t, err)
	require.Equal(t, 10*time.Second, renewed.ExpiresIn)
}

func TestUsagePreviewPreservesExistingPassRoomAndHostLeftCountdown(t *testing.T) {
	cw := newCallWorld(t, "preview-compat", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	k, host, pass := allowanceAccount(t, cw, "day")
	started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Old pass host", Confirm: confirmed})
	require.NoError(t, err)
	deadline := rowTime(cw.row(roomConcept, started.Call.ID), "fundingDeadline")
	enableUsagePreview(t, cw, 1)
	after, err := cw.svc.Account(ctx, cw.ref, host.Token, nil)
	require.NoError(t, err)
	require.Equal(t, pass.ID, after.Account.ActivePass.ID)
	require.EqualValues(t, 850, *after.Account.BalanceCents)
	view, err := allowanceReplica(t, cw).SessionCall(ctx, host)
	require.NoError(t, err)
	require.Empty(t, view.FundingMode)
	require.Equal(t, deadline, view.AllowanceEndsAt)
	require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostEnd}))
	started, err = cw.svc.Start(ctx, host, callrules.StartBody{Name: "Preview host", Confirm: confirmed})
	require.NoError(t, err)
	joined, guest := cw.keyHolderJoin(started.Call.GuestToken, "Guest")
	cw.media.setPresent(started.Call.ID, person(joined.Identity))
	cw.clock.Advance(time.Minute)
	require.NoError(t, cw.svc.Leave(ctx, host))
	view, err = cw.svc.SessionCall(ctx, guest)
	require.NoError(t, err)
	require.False(t, view.Host)
	require.Equal(t, cw.clock.Now().Add(5*time.Minute), view.CountdownEndsAt)
	require.Equal(t, usagePreviewMode, view.FundingMode)
	cw.advanceTo(view.CountdownEndsAt)
	require.NoError(t, cw.svc.Reconcile(ctx))
	require.Equal(t, callrules.EndReasonHostLeft, rowString(cw.row(roomConcept, started.Call.ID), "endReason"))
	wallet, err := cw.svc.simulatedWallet(ctx, cw.owner, cw.site, k.result.Account.KeyID)
	require.NoError(t, err)
	require.EqualValues(t, 850, wallet.BalanceCents)
}
