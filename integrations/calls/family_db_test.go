package calls

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

func familyFixture(t *testing.T, name string) (*callWorld, accountTestKey, callrules.StartResult, roomRow) {
	t.Helper()
	t.Setenv("MEMQL_CALLS_FAMILY_MODE", "mock")
	cw, k, _ := commerceFixture(t, name)
	if cw == nil {
		return nil, k, callrules.StartResult{}, roomRow{}
	}
	commerceVerified(t, cw, k)
	q, e := cw.svc.Account(context.Background(), cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "quote_call", AccountID: k.result.Account.AccountID, ParticipantLimit: 4, DurationMinutes: 60, ParticipationMode: familyMode})
	require.NoError(t, e)
	start, e := cw.svc.Start(context.Background(), *k.result.Session, callrules.StartBody{Name: "Guardian", Confirm: confirmed, QuoteID: q.Quote.QuoteID, IdempotencyKey: "family-start-0001"})
	require.NoError(t, e)
	r, e := cw.svc.store.roomByID(context.Background(), cw.owner, start.Call.ID)
	require.NoError(t, e)
	require.NotNil(t, r)
	return cw, k, start, *r
}
func familyGuardianRequest(cw *callWorld, k accountTestKey, start callrules.StartResult, action string) callrules.FamilyRequest {
	return callrules.FamilyRequest{Action: action, RoomID: start.Call.ID, Identity: start.Join.Identity, IdempotencyKey: "family-" + action + "-0001"}
}
func familyInviteClaim(t *testing.T, cw *callWorld, k accountTestKey, start callrules.StartResult) (callrules.FamilyInvitation, callrules.Session) {
	t.Helper()
	ctx := context.Background()
	req := familyGuardianRequest(cw, k, start, "invite")
	invited, e := cw.svc.Family(ctx, cw.ref, k.result.Session.Token, req)
	require.NoError(t, e)
	require.NotNil(t, invited.Invitation)
	again, e := cw.svc.Family(ctx, cw.ref, k.result.Session.Token, req)
	require.NoError(t, e)
	require.Equal(t, invited.Invitation, again.Invitation)
	claimed, e := cw.svc.Family(ctx, cw.ref, "", callrules.FamilyRequest{Action: "claim", InvitationToken: invited.Invitation.Token})
	require.NoError(t, e)
	require.Equal(t, "pending", claimed.Status)
	require.NotNil(t, claimed.Session)
	require.Empty(t, claimed.Session.SeatID)
	return *invited.Invitation, *claimed.Session
}
func familyApprove(t *testing.T, cw *callWorld, k accountTestKey, start callrules.StartResult, inv callrules.FamilyInvitation) {
	t.Helper()
	req := familyGuardianRequest(cw, k, start, "mock_approve")
	req.InvitationID = inv.ID
	req.ParentalAuthority = true
	req.ConsentVersion = callrules.FamilyMockConsentVersion
	_, e := cw.svc.Family(context.Background(), cw.ref, k.result.Session.Token, req)
	require.NoError(t, e)
}
func TestFamilyChildRequiresSeparateBoundConsentAndLiveGuardian(t *testing.T) {
	cw, k, start, r := familyFixture(t, "family-consent")
	if cw == nil {
		return
	}
	ctx := context.Background()
	// Neither ordinary guest nor anonymous cohost bearer can enter a family room.
	_, _, e := cw.svc.Join(ctx, cw.ref, nil, callrules.JoinBody{Token: start.Call.GuestToken, Name: "Guest", Confirm: confirmed})
	require.ErrorContains(t, e, "family_invitation_required")
	_, _, e = cw.svc.Join(ctx, cw.ref, nil, callrules.JoinBody{HostToken: start.Call.HostToken, Name: "Cohost", Confirm: confirmed})
	require.ErrorContains(t, e, "family_invitation_required")
	inv, child := familyInviteClaim(t, cw, k, start)
	restore, e := cw.svc.FamilySession(ctx, cw.ref, child.Token)
	require.NoError(t, e)
	require.Equal(t, "child", restore.Role)
	require.Equal(t, inv.ID, restore.InvitationID)
	require.Equal(t, "pending", restore.Status)
	body := callrules.JoinBody{Family: true, FamilyInvitationID: inv.ID, Name: "Child"}
	_, _, e = cw.svc.Join(ctx, cw.ref, &child, body)
	require.ErrorContains(t, e, "family_consent_required")
	// A host's verified-adult evidence alone never grants parental consent.
	req := familyGuardianRequest(cw, k, start, "mock_approve")
	req.InvitationID = inv.ID
	_, e = cw.svc.Family(ctx, cw.ref, k.result.Session.Token, req)
	require.ErrorContains(t, e, "parental_consent_required")
	familyApprove(t, cw, k, start, inv)
	_, _, e = cw.svc.Join(ctx, cw.ref, &child, body)
	require.ErrorContains(t, e, "family_guardian_required")
	cw.media.setPresent(r.MediaRoom, MediaParticipant{Identity: start.Join.Identity, SID: "guardian-1"})
	joined, child, e := cw.svc.Join(ctx, cw.ref, &child, body)
	require.NoError(t, e)
	require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	require.NotEmpty(t, joined.Pass)
	require.False(t, joined.Host)
	_, child, e = cw.svc.Join(ctx, cw.ref, &child, body)
	require.NoError(t, e)
	repeated, e := cw.svc.store.seatByID(ctx, cw.owner, child.SeatID)
	require.NoError(t, e)
	require.False(t, repeated.ConfirmAdult)
	require.False(t, repeated.ConfirmNoMinors)
	status, e := cw.svc.Family(ctx, cw.ref, child.Token, callrules.FamilyRequest{Action: "status", InvitationID: inv.ID})
	require.NoError(t, e)
	require.Equal(t, "approved", status.Status)
	require.Empty(t, status.Invitation.Token)
	_, e = cw.svc.Family(ctx, cw.ref, "", callrules.FamilyRequest{Action: "claim", InvitationToken: inv.Token})
	require.ErrorContains(t, e, "family_invitation_claimed")
	wrong := body
	wrong.FamilyInvitationID = "different"
	_, _, e = cw.svc.Join(ctx, cw.ref, &child, wrong)
	require.ErrorContains(t, e, "family_invitation_changed")
	// Losing presence ends immediately at reconciliation, not the normal 120s seat timeout or 5m host grace.
	cw.media.setPresent(r.MediaRoom, MediaParticipant{Identity: joined.Identity, SID: "child-1"})
	handled, e := cw.svc.reconcileFamily(ctx, r)
	require.True(t, handled)
	require.NoError(t, e)
	ended, e := cw.svc.store.roomByID(ctx, cw.owner, r.ID)
	require.NoError(t, e)
	require.False(t, ended.open())
	_, e = cw.svc.Queue(ctx, child)
	require.ErrorIs(t, e, callrules.ErrEnded)
	// Rejoin and room recreation are permanently blocked, even for the returning guardian.
	_, e = cw.svc.Queue(ctx, *k.result.Session)
	require.ErrorIs(t, e, callrules.ErrEnded)
	require.NotEmpty(t, cw.media.deletions())
}
func TestFamilyMockCapabilityScopeAndFeatureIsolation(t *testing.T) {
	t.Setenv("MEMQL_CALLS_FAMILY_MODE", "")
	cw, k, c := commerceFixture(t, "family-scope")
	if cw == nil {
		return
	}
	commerceVerified(t, cw, k)
	ctx := context.Background()
	req := callrules.AccountRequest{Action: "quote_call", AccountID: k.result.Account.AccountID, ParticipantLimit: 2, DurationMinutes: 60, ParticipationMode: familyMode}
	_, e := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &req)
	require.ErrorContains(t, e, "family_unavailable")
	t.Setenv("MEMQL_CALLS_FAMILY_MODE", "mock")
	c.Mode = providers.Sandbox
	_, e = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &req)
	require.ErrorContains(t, e, "family_unavailable")
	c.Mode = providers.Mock
	req.Insights = true
	_, e = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &req)
	require.Error(t, e)
	req.Insights = false
	req.Resources = true
	_, e = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &req)
	require.Error(t, e)
	req.Resources = false
	q, e := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &req)
	require.NoError(t, e)
	require.Equal(t, "mock", q.Capabilities.FamilyMode)
	req.ParticipationMode = "unsafe"
	_, e = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &req)
	require.Error(t, e)
}
func TestFamilyApprovalScopeExpiryAndRevocationClose(t *testing.T) {
	cw, k, start, r := familyFixture(t, "family-revoke")
	if cw == nil {
		return
	}
	ctx := context.Background()
	bad := familyGuardianRequest(cw, k, start, "invite")
	bad.Identity = "stale-seat"
	_, e := cw.svc.Family(ctx, cw.ref, k.result.Session.Token, bad)
	require.ErrorContains(t, e, "family_call_changed")
	inv, child := familyInviteClaim(t, cw, k, start)
	familyApprove(t, cw, k, start, inv)
	cw.media.setPresent(r.MediaRoom, MediaParticipant{Identity: start.Join.Identity, SID: "guardian-1"})
	_, child, e = cw.svc.Join(ctx, cw.ref, &child, callrules.JoinBody{Family: true, FamilyInvitationID: inv.ID, Name: "Child"})
	require.NoError(t, e)
	req := familyGuardianRequest(cw, k, start, "revoke")
	req.InvitationID = inv.ID
	_, e = cw.svc.Family(ctx, cw.ref, child.Token, req)
	require.Error(t, e)
	_, e = cw.svc.Family(ctx, cw.ref, k.result.Session.Token, req)
	require.NoError(t, e)
	ended, e := cw.svc.store.roomByID(ctx, cw.owner, r.ID)
	require.NoError(t, e)
	require.False(t, ended.open())
	require.NotNil(t, ended.UsageCostCents)
	status, e := cw.svc.Family(ctx, cw.ref, child.Token, callrules.FamilyRequest{Action: "status", InvitationID: inv.ID})
	require.NoError(t, e)
	require.Equal(t, "ended", status.Status)
	// The original hard deadline was not extended by consent or revocation.
	require.Equal(t, r.FundingDeadline, ended.FundingDeadline)
}
func TestFamilyGuardianBoundLeaveAndSignedDisconnect(t *testing.T) {
	for _, how := range []string{"leave", "disconnect"} {
		t.Run(how, func(t *testing.T) {
			cw, k, start, r := familyFixture(t, "family-"+how)
			if cw == nil {
				return
			}
			ctx := context.Background()
			cw.clock.Advance(time.Second)
			require.NoError(t, cw.svc.familyParticipant(ctx, r, mediaEvent{Kind: eventParticipantJoined, Identity: start.Join.Identity, ParticipantSID: "guardian-current", At: cw.clock.Now()}))
			if how == "leave" {
				seat, e := cw.svc.SessionCall(ctx, *k.result.Session)
				require.NoError(t, e)
				require.NoError(t, cw.svc.LeaveWithOptions(ctx, *k.result.Session, callrules.LeaveRequest{LeaveToken: seat.LeaveToken, IdempotencyKey: "family-leave-0001"}))
			} else {
				// A stale older connection's disconnect cannot close the current connection.
				require.NoError(t, cw.svc.familyParticipant(ctx, r, mediaEvent{Kind: eventParticipantLeft, Identity: start.Join.Identity, ParticipantSID: "guardian-old", At: cw.clock.Now().Add(time.Second)}))
				before, e := cw.svc.store.roomByID(ctx, cw.owner, r.ID)
				require.NoError(t, e)
				require.True(t, before.open())
				require.NoError(t, cw.svc.familyParticipant(ctx, r, mediaEvent{Kind: eventParticipantLeft, Identity: start.Join.Identity, ParticipantSID: "guardian-current", At: cw.clock.Now().Add(time.Second)}))
			}
			ended, e := cw.svc.store.roomByID(ctx, cw.owner, r.ID)
			require.NoError(t, e)
			require.False(t, ended.open())
		})
	}
}

func TestFamilyKnownChildCannotSelfDeclareAdultOrCreateAccount(t *testing.T) {
	cw, k, start, _ := familyFixture(t, "family-no-escape")
	if cw == nil {
		return
	}
	ctx := context.Background()
	inv, child := familyInviteClaim(t, cw, k, start)
	_, _, err := cw.svc.Join(ctx, cw.ref, &child, callrules.JoinBody{Token: start.Call.GuestToken, Name: "Adult now", Confirm: confirmed})
	require.ErrorContains(t, err, "family_invitation_required")
	for _, action := range []string{"claim_begin", "register_begin", "verify_begin", "quote_call"} {
		_, err = cw.svc.Account(ctx, cw.ref, child.Token, &callrules.AccountRequest{Action: action})
		require.ErrorContains(t, err, "family_child_account_unavailable")
	}
	// No display name or adult declaration has been collected for the pending child.
	require.Empty(t, child.SeatID)
	require.Len(t, cw.seatsIn(start.Call.ID), 1)
	state, err := cw.svc.store.family(ctx, cw.owner, inv.ID)
	require.NoError(t, err)
	require.Empty(t, state.Data.ConsentVersion)
	require.Empty(t, state.Data.ApprovalCredential)
	cw.clock.Advance(31 * time.Minute)
	status, err := cw.svc.Family(ctx, cw.ref, child.Token, callrules.FamilyRequest{Action: "status", InvitationID: inv.ID})
	require.NoError(t, err)
	require.Equal(t, "expired", status.Status)
}

func TestFamilyReconciliationClosesOnPolicyOrConsentCredentialLoss(t *testing.T) {
	for _, change := range []string{"flag_off", "expired_envelope", "consent_passkey_revoked"} {
		t.Run(change, func(t *testing.T) {
			cw, k, start, r := familyFixture(t, "fam-loss-"+change[:4])
			if cw == nil {
				return
			}
			ctx := context.Background()
			inv, child := familyInviteClaim(t, cw, k, start)
			familyApprove(t, cw, k, start, inv)
			cw.media.setPresent(r.MediaRoom, MediaParticipant{Identity: start.Join.Identity, SID: "g"})
			joined, child, err := cw.svc.Join(ctx, cw.ref, &child, callrules.JoinBody{Family: true, FamilyInvitationID: inv.ID, Name: "Child"})
			require.NoError(t, err)
			row, err := cw.svc.store.seatByID(ctx, cw.owner, child.SeatID)
			require.NoError(t, err)
			require.False(t, row.ConfirmAdult)
			require.False(t, row.ConfirmNoMinors)
			cw.media.setPresent(r.MediaRoom, MediaParticipant{Identity: start.Join.Identity, SID: "g"}, MediaParticipant{Identity: joined.Identity, SID: "c"})
			switch change {
			case "flag_off":
				t.Setenv("MEMQL_CALLS_FAMILY_MODE", "")
			case "expired_envelope":
				config := cw.svc.commerceConfig()
				config.ValidUntil = cw.clock.Now().Add(-time.Second)
				cw.svc.commerce = func() CommerceConfig { return config }
			case "consent_passkey_revoked":
				// Keep the guardian's live credential valid; invalidate only the receipt's
				// credential reference to prove active-child consent is checked separately.
				receipt, err := cw.svc.store.family(ctx, cw.owner, inv.ID)
				require.NoError(t, err)
				receipt.Data.ApprovalCredential = "revoked-consent-credential"
				require.NoError(t, cw.svc.store.saveFamily(ctx, cw.owner, receipt, cw.clock.Now()))
			}
			handled, err := cw.svc.reconcileFamily(ctx, r)
			require.True(t, handled)
			require.NoError(t, err)
			ended, err := cw.svc.store.roomByID(ctx, cw.owner, r.ID)
			require.NoError(t, err)
			require.False(t, ended.open())
			require.NotEmpty(t, cw.media.deletions())
		})
	}
}

func TestFamilyDeletionFailureRetainsEarliestBarrierAndNeverRecreatesMedia(t *testing.T) {
	cw, k, start, r := familyFixture(t, "family-delete-failure")
	if cw == nil {
		return
	}
	ctx := context.Background()
	inv, child := familyInviteClaim(t, cw, k, start)
	familyApprove(t, cw, k, start, inv)
	cw.media.setPresent(r.MediaRoom, MediaParticipant{Identity: start.Join.Identity, SID: "guardian-current"})
	_, child, err := cw.svc.Join(ctx, cw.ref, &child, callrules.JoinBody{Family: true, FamilyInvitationID: inv.ID, Name: "Child"})
	require.NoError(t, err)
	created := len(cw.media.rooms())
	require.NotEqual(t, r.ID, r.MediaRoom, "physical family room name must differ from retry operation")
	_, err = cw.svc.Queue(ctx, child)
	require.NoError(t, err)
	require.Len(t, cw.media.rooms(), created, "renewal never calls CreateRoom")
	cw.clock.Advance(time.Second)
	require.NoError(t, cw.svc.familyParticipant(ctx, r, mediaEvent{Kind: eventParticipantJoined, Identity: start.Join.Identity, ParticipantSID: "guardian-current", At: cw.clock.Now()}))
	cw.clock.Advance(time.Minute)
	leftAt := cw.clock.Now()
	ev := mediaEvent{Kind: eventParticipantLeft, Identity: start.Join.Identity, ParticipantSID: "guardian-current", At: leftAt}
	cw.media.failDelete = errors.New("temporary provider failure")
	require.Error(t, cw.svc.familyParticipant(ctx, r, ev))
	state, err := cw.svc.store.family(ctx, cw.owner, r.ID)
	require.NoError(t, err)
	require.Equal(t, leftAt, state.Data.ClosingAt)
	meta, err := cw.svc.FamilySession(ctx, cw.ref, k.result.Session.Token)
	require.NoError(t, err)
	require.Equal(t, "ended", meta.Status)
	_, err = cw.svc.Queue(ctx, child)
	require.Error(t, err)
	require.Len(t, cw.media.rooms(), created)
	cw.clock.Advance(time.Minute)
	require.Error(t, cw.svc.familyParticipant(ctx, r, ev))
	state, err = cw.svc.store.family(ctx, cw.owner, r.ID)
	require.NoError(t, err)
	require.Equal(t, leftAt, state.Data.ClosingAt)
	cw.media.failDelete = nil
	replica := allowanceReplica(t, cw)
	replica.commerce = cw.svc.commerce
	handled, err := replica.reconcileFamily(ctx, r)
	require.True(t, handled)
	require.NoError(t, err)
	ended, err := replica.store.roomByID(ctx, cw.owner, r.ID)
	require.NoError(t, err)
	require.False(t, ended.open())
	require.Equal(t, leftAt, ended.EndedAt)
	require.Error(t, replica.ensureFamilyMedia(ctx, r))
	require.Len(t, cw.media.rooms(), created)
	balance, err := replica.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 0, *balance.Account.ReservedCents)
	before := *balance.Account.BalanceCents
	require.NoError(t, replica.closeFamily(ctx, r, "retry"))
	after, err := replica.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.Equal(t, before, *after.Account.BalanceCents)
}

type interruptedFamilyReceipt struct {
	Engine
	writes int
	failed bool
}

func (e *interruptedFamilyReceipt) Execute(ctx context.Context, query string) (any, error) {
	if strings.Contains(query, "saveCallsFamily(") {
		e.writes++
	}
	if !e.failed && e.writes == 2 && strings.Contains(query, "saveCallsFamily(") {
		e.failed = true
		return nil, errors.New("injected receipt write failure")
	}
	return e.Engine.Execute(ctx, query)
}
func TestFamilyRevokePartialWriteStillEndsExistingMedia(t *testing.T) {
	cw, k, start, r := familyFixture(t, "family-partial-revoke")
	if cw == nil {
		return
	}
	ctx := context.Background()
	inv, child := familyInviteClaim(t, cw, k, start)
	familyApprove(t, cw, k, start, inv)
	cw.media.setPresent(r.MediaRoom, MediaParticipant{Identity: start.Join.Identity, SID: "g"})
	_, _, err := cw.svc.Join(ctx, cw.ref, &child, callrules.JoinBody{Family: true, FamilyInvitationID: inv.ID, Name: "Child"})
	require.NoError(t, err)
	original := cw.svc.store.engine
	injected := &interruptedFamilyReceipt{Engine: original}
	cw.svc.store.engine = injected
	req := familyGuardianRequest(cw, k, start, "revoke")
	req.InvitationID = inv.ID
	_, err = cw.svc.Family(ctx, cw.ref, k.result.Session.Token, req)
	require.Error(t, err)
	require.True(t, injected.failed)
	cw.svc.store.engine = original
	state, err := cw.svc.store.family(ctx, cw.owner, r.ID)
	require.NoError(t, err)
	require.NotZero(t, state.Data.ClosingAt)
	_, err = cw.svc.Queue(ctx, child)
	require.Error(t, err)
	handled, err := cw.svc.reconcileFamily(ctx, r)
	require.True(t, handled)
	require.NoError(t, err)
	ended, err := cw.svc.store.roomByID(ctx, cw.owner, r.ID)
	require.NoError(t, err)
	require.False(t, ended.open())
	require.NotEmpty(t, cw.media.deletions())
}

func TestFamilyInvitationConcurrencyAndBookingPolicy(t *testing.T) {
	t.Run("concurrent claims", func(t *testing.T) {
		cw, k, start, _ := familyFixture(t, "fam-concurrent")
		if cw == nil {
			return
		}
		ctx := context.Background()
		req := familyGuardianRequest(cw, k, start, "invite")
		type answer struct {
			v callrules.FamilyResult
			e error
		}
		answers := make(chan answer, 6)
		for range 6 {
			go func() {
				replica := allowanceReplica(t, cw)
				replica.commerce = cw.svc.commerce
				v, e := replica.Family(ctx, cw.ref, k.result.Session.Token, req)
				answers <- answer{v, e}
			}()
		}
		var invitation *callrules.FamilyInvitation
		for range 6 {
			a := <-answers
			require.NoError(t, a.e)
			if invitation == nil {
				invitation = a.v.Invitation
			} else {
				require.Equal(t, invitation, a.v.Invitation)
			}
		}
		for range 2 {
			go func() {
				v, e := cw.svc.Family(ctx, cw.ref, "", callrules.FamilyRequest{Action: "claim", InvitationToken: invitation.Token})
				answers <- answer{v, e}
			}()
		}
		won, refused := 0, 0
		for range 2 {
			a := <-answers
			if a.e == nil {
				won++
				require.NotNil(t, a.v.Session)
			} else {
				refused++
				require.ErrorContains(t, a.e, "family_invitation_claimed")
				require.Nil(t, a.v.Session)
			}
		}
		require.Equal(t, 1, won)
		require.Equal(t, 1, refused)
	})
	t.Run("booked policy", func(t *testing.T) {
		t.Setenv("MEMQL_CALLS_FAMILY_MODE", "mock")
		cw, k, _ := commerceFixture(t, "fam-booked")
		if cw == nil {
			return
		}
		commerceVerified(t, cw, k)
		ctx := context.Background()
		quote := func(mode string) *callrules.CallQuote {
			q, e := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "quote_call", AccountID: k.result.Account.AccountID, ParticipantLimit: 4, DurationMinutes: 30, ParticipationMode: mode})
			require.NoError(t, e)
			return q.Quote
		}
		q := quote(familyMode)
		req := callrules.ScheduleRequest{Start: cw.clock.Now().Add(10 * time.Minute), Minutes: 30, QuoteID: q.QuoteID, IdempotencyKey: "family-booking-0001", Title: "Family"}
		booked, e := cw.svc.Schedule(ctx, *k.result.Session, req)
		require.NoError(t, e)
		require.Equal(t, familyMode, booked.Configuration.ParticipationMode)
		changed := req
		changed.IdempotencyKey = "family-booking-move"
		changed.QuoteID = quote("").QuoteID
		_, e = cw.svc.Reschedule(ctx, *k.result.Session, booked.ID, changed)
		require.ErrorContains(t, e, "family_policy_immutable")
		cw.clock.Advance(10 * time.Minute)
		_, _, e = cw.svc.Join(ctx, cw.ref, nil, callrules.JoinBody{HostToken: booked.HostToken, Name: "Anonymous cohost", Confirm: confirmed})
		require.ErrorContains(t, e, "family_guardian_required")
		joined, sess, e := cw.svc.Join(ctx, cw.ref, k.result.Session, callrules.JoinBody{HostToken: booked.HostToken, Name: "Guardian", Confirm: confirmed})
		require.NoError(t, e)
		require.True(t, joined.Host)
		call, e := cw.svc.SessionCall(ctx, sess)
		require.NoError(t, e)
		require.Equal(t, familyMode, call.ParticipationMode)
	})
}
