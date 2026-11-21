package calls

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

func delegationTestRequest(cw *callWorld) callrules.DelegationRequest {
	return callrules.DelegationRequest{Action: "create", Label: "Local test agent", Scopes: []string{"read", "host", "schedule", "control"}, Terms: callrules.AgentTermsVersion, Purpose: "Help with the approved conversation", ConductVersion: callrules.AgentConductVersion, Participation: "adults_only", MaxCallCents: 1000, PeriodLimitCents: 1000, LifetimeLimitCents: 5000, MaxParticipants: 4, MaxDurationMinutes: 60, ExpiresAt: cw.clock.Now().Add(24 * time.Hour)}
}

// Existing behavior tests opt in to the explicit admission contract. Negative
// conduct tests call Service.Agent directly so missing acceptance stays covered.
func delegationTestAgent(t *testing.T, cw *callWorld, svc *Service, ctx context.Context, site callrules.SiteRef, credential string, req callrules.AgentRequest) (any, error) {
	t.Helper()
	if req.Action == "start" || req.Action == "schedule" || req.Action == "reschedule" || req.Action == "join" || req.Action == "topup" {
		d, _, err := svc.agentAuthority(ctx, site, credential)
		if err == nil {
			req.Acceptance = &callrules.AgentAcceptance{ApprovalRevision: d.State.ApprovalRevision, ApprovalDigest: delegationApprovalDigest(d), ConductVersion: callrules.AgentConductVersion, Terms: d.State.Request.Terms}
			if req.Action == "join" && req.IdempotencyKey == "" {
				req.IdempotencyKey = "test-join-" + req.CallID
			}
		}
	}
	return svc.Agent(ctx, site, credential, req)
}
func delegationTestGrant(t *testing.T, cw *callWorld, k accountTestKey, r callrules.DelegationRequest) (string, string) {
	t.Helper()
	out, err := cw.svc.Delegation(context.Background(), cw.ref, k.result.Session.Token, &r)
	require.NoError(t, err)
	require.NotEmpty(t, out.Credential)
	parts := strings.Split(out.Credential, ".")
	require.Len(t, parts, 2)
	// The credential is returned once; listing can never retrieve its secret.
	listed, err := cw.svc.Delegation(context.Background(), cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.Empty(t, listed.Credential)
	return out.Credential, parts[0]
}
func cleanupDelegationTestRows(t *testing.T, cw *callWorld) {
	t.Helper()
	t.Cleanup(func() {
		for _, site := range cw.sites {
			_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", delegationConcept).Where("payload->>'siteId' = ?", siteConcept+":"+site).Exec(context.Background())
		}
	})
}
func TestDelegationRequiresEnabledCommerceAdultAndRecentHumanAssertion(t *testing.T) {
	cw, k, c := commerceFixture(t, "delegation-authority")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	ctx := context.Background()
	req := delegationTestRequest(cw)
	c.Mode = providers.Disabled
	_, err := cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &req)
	require.ErrorContains(t, err, "commerce_disabled")
	c.Mode = providers.Mock
	_, err = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &req)
	require.ErrorContains(t, err, "adult_verification_required")
	commerceVerified(t, cw, k)
	cw.clock.Advance(6 * time.Minute)
	_, err = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &req)
	require.ErrorIs(t, err, callrules.ErrForbidden)
	_, err = accountAssert(t, cw, k, k.result.Session.Token, "verify", "")
	require.NoError(t, err)
	credential, _ := delegationTestGrant(t, cw, k, req)
	_, err = cw.svc.Delegation(ctx, cw.ref, credential, &req)
	require.Error(t, err, "agent bearer must never become a human approval")
}
func TestDelegationBackupPasskeyReusesSameAdultAccount(t *testing.T) {
	cw, k, _ := commerceFixture(t, "delegation-passkey")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	ctx := context.Background()
	begin, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "add_begin"})
	require.NoError(t, err)
	challenge, rp, _ := accountCreation(t, begin)
	backup := newSoftwareAuthenticator(t)
	_, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, &callrules.AccountRequest{Action: "add_finish", ChallengeID: begin.ChallengeID, Label: "Backup", Credential: backup.Attest(challenge, rp, "https://"+rp)})
	require.NoError(t, err)
	second := k
	second.auth = backup
	signed, err := accountAssert(t, cw, second, "", "login", "")
	require.NoError(t, err)
	second.result = signed
	credential, _ := delegationTestGrant(t, cw, second, delegationTestRequest(cw))
	require.NotEmpty(t, credential)
	snapshot, err := cw.svc.Account(ctx, cw.ref, second.result.Session.Token, nil)
	require.NoError(t, err)
	require.Equal(t, k.result.Account.AccountID, snapshot.Account.AccountID)
	require.Equal(t, 1, snapshot.Lifecycle.VerificationAttempts)
	require.Equal(t, "verified", snapshot.Lifecycle.Adult.Status)
}
func TestDelegationConcurrentReservationsShareDurableBudgetAndReplay(t *testing.T) {
	cw, k, _ := commerceFixture(t, "delegation-budget")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	req := delegationTestRequest(cw)
	req.MaxCallCents = 700
	req.PeriodLimitCents = 700
	req.LifetimeLimitCents = 1400
	credential, grant := delegationTestGrant(t, cw, k, req)
	replica := allowanceReplica(t, cw)
	replica.commerce = cw.svc.commerce
	type answer struct {
		request   callrules.AgentRequest
		operation delegatedOperation
		err       error
	}
	answers := make(chan answer, 2)
	var wg sync.WaitGroup
	for i, svc := range []*Service{cw.svc, replica} {
		wg.Add(1)
		go func(i int, svc *Service) {
			defer wg.Done()
			request := callrules.AgentRequest{Action: "start", IdempotencyKey: fmt.Sprintf("concurrent-delegate-%04d", i)}
			op, err := svc.reserveDelegated(context.Background(), cw.ref, credential, request, "usage", 700)
			answers <- answer{request, op, err}
		}(i, svc)
	}
	wg.Wait()
	close(answers)
	winners := 0
	var winner answer
	for got := range answers {
		if got.err == nil {
			winners++
			winner = got
		} else {
			require.ErrorContains(t, got.err, "delegation_budget")
		}
	}
	require.Equal(t, 1, winners)
	replay, err := replica.reserveDelegated(context.Background(), cw.ref, credential, winner.request, "usage", 700)
	require.NoError(t, err)
	require.Equal(t, winner.operation.ID, replay.ID)
	changed := winner.request
	changed.Title = "A different effect"
	_, err = replica.reserveDelegated(context.Background(), cw.ref, credential, changed, "usage", 700)
	require.ErrorContains(t, err, "idempotency_conflict")
	stored, err := cw.svc.store.delegation(memql.ContextWithFreshRead(context.Background()), cw.owner, grant)
	require.NoError(t, err)
	require.Len(t, stored.State.Operations, 1)
	period, total := delegationUsage(stored, "usage", cw.clock.Now())
	require.EqualValues(t, 700, period)
	require.EqualValues(t, 700, total)
}
func TestDelegationPendingReservationsCarryAcrossPeriodBoundary(t *testing.T) {
	cw, k, c := commerceFixture(t, "delegation-period")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	req := delegationTestRequest(cw)
	req.MaxCallCents = 700
	req.PeriodLimitCents = 700
	req.LifetimeLimitCents = 5000
	req.ExpiresAt = cw.clock.Now().Add(89 * 24 * time.Hour)
	c.ValidUntil = req.ExpiresAt
	credential, _ := delegationTestGrant(t, cw, k, req)
	_, err := cw.svc.reserveDelegated(context.Background(), cw.ref, credential, callrules.AgentRequest{Action: "start", IdempotencyKey: "prior-period-pending"}, "usage", 700)
	require.NoError(t, err)
	cw.clock.Advance(32 * 24 * time.Hour)
	_, err = cw.svc.reserveDelegated(context.Background(), cw.ref, credential, callrules.AgentRequest{Action: "start", IdempotencyKey: "new-period-request"}, "usage", 700)
	require.ErrorContains(t, err, "delegation_budget", "an uncompleted old-period commitment must retain current-period capacity")
}
func TestDelegationRevocationClosesNativeEffectAfterEarlierAuthentication(t *testing.T) {
	cw, k, _ := commerceFixture(t, "delegation-revoke-race")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	credential, grant := delegationTestGrant(t, cw, k, delegationTestRequest(cw))
	ctx := context.Background()
	d, pepper, err := cw.svc.agentAuthority(ctx, cw.ref, credential)
	require.NoError(t, err)
	sess, err := cw.svc.delegationSessionReady(ctx, cw.ref, d, pepper)
	require.NoError(t, err)
	quoted, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "quote", ParticipantLimit: 2, DurationMinutes: 60})
	require.NoError(t, err)
	quote, ok := quoted.(*callrules.CallQuote)
	require.True(t, ok)
	_, err = cw.svc.reserveDelegated(ctx, cw.ref, credential, callrules.AgentRequest{Action: "start", IdempotencyKey: "before-revocation-001", QuoteID: quote.QuoteID}, "usage", quote.MaxCostCents) // gitleaks:allow -- public idempotency fixture, not an authorization credential.
	require.NoError(t, err)
	_, err = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "revoke", GrantID: grant})
	require.NoError(t, err)
	// Simulate an in-flight request that already retained its internal session.
	late := context.WithValue(ctx, agentCallContext{}, grant)
	_, err = cw.svc.Start(late, sess, callrules.StartBody{Name: "Late agent", Confirm: confirmed, QuoteID: quote.QuoteID, IdempotencyKey: "late-native-start-001"})
	require.ErrorContains(t, err, "delegation_inactive")
	require.Empty(t, cw.media.rooms(), "revocation must win before creating media")
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "balance"})
	require.ErrorContains(t, err, "delegation_inactive")
}
func TestDelegationAgentStartReplayLabelsAgentAndHidesHostBearer(t *testing.T) {
	cw, k, _ := commerceFixture(t, "delegation-start")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	credential, _ := delegationTestGrant(t, cw, k, delegationTestRequest(cw))
	ctx := context.Background()
	quoted, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "quote", ParticipantLimit: 2, DurationMinutes: 60})
	require.NoError(t, err)
	quote := quoted.(*callrules.CallQuote)
	request := callrules.AgentRequest{Action: "start", IdempotencyKey: "agent-host-first-001", QuoteID: quote.QuoteID, Name: "Assistant", Terms: callrules.AgentTermsVersion, Participation: "adults_only"}
	result, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, request)
	require.NoError(t, err)
	started := result.(callrules.StartResult)
	require.NotEmpty(t, started.Call.ID)
	require.Empty(t, started.Call.HostToken)
	for _, seat := range cw.seatsIn(started.Call.ID) {
		require.Equal(t, string(callrules.KindAgent), rowString(seat, "kind"))
	}
	replay, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, request)
	require.NoError(t, err)
	require.Equal(t, started.Call.ID, replay.(callrules.StartResult).Call.ID)
	require.Empty(t, replay.(callrules.StartResult).Call.HostToken)
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "quote", ParticipantLimit: 5, DurationMinutes: 60})
	require.ErrorContains(t, err, "delegation_call_limit")
}
func TestDelegationAutomaticTopupsRespectSeparateCapsAndConcurrentThreshold(t *testing.T) {
	cw, k, _ := commerceFixture(t, "delegation-topup")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	req := delegationTestRequest(cw)
	req.Scopes = append(req.Scopes, "topup")
	req.Topup = callrules.AutoTopupLimit{Enabled: true, AmountCents: 1000, ThresholdCents: 1000, PerChargeLimitCents: 1000, PeriodLimitCents: 1000, LifetimeLimitCents: 1000}
	credential, grant := delegationTestGrant(t, cw, k, req)
	replica := allowanceReplica(t, cw)
	replica.commerce = cw.svc.commerce
	answers := make(chan error, 2)
	var wg sync.WaitGroup
	for i, svc := range []*Service{cw.svc, replica} {
		wg.Add(1)
		go func(i int, svc *Service) {
			defer wg.Done()
			_, err := delegationTestAgent(t, cw, svc, context.Background(), cw.ref, credential, callrules.AgentRequest{Action: "topup", IdempotencyKey: fmt.Sprintf("concurrent-topup-%04d", i)})
			answers <- err
		}(i, svc)
	}
	wg.Wait()
	close(answers)
	success := 0
	for err := range answers {
		if err == nil {
			success++
		} else {
			require.True(t, strings.Contains(err.Error(), "delegation_budget") || strings.Contains(err.Error(), "topup_threshold_not_reached"), err.Error())
		}
	}
	require.Equal(t, 1, success)
	snapshot, err := cw.svc.Account(context.Background(), cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.EqualValues(t, 2000, *snapshot.Account.BalanceCents)
	stored, err := cw.svc.store.delegation(memql.ContextWithFreshRead(context.Background()), cw.owner, grant)
	require.NoError(t, err)
	_, topups := delegationUsage(stored, "topup", cw.clock.Now())
	_, usage := delegationUsage(stored, "usage", cw.clock.Now())
	require.EqualValues(t, 1000, topups)
	require.Zero(t, usage, "funding must not consume or replenish usage authority")
	_, err = cw.svc.reserveDelegated(context.Background(), cw.ref, credential, callrules.AgentRequest{Action: "topup", IdempotencyKey: "topup-over-limit-001"}, "topup", 1000)
	require.ErrorContains(t, err, "delegation_budget")
}

func TestDelegationScheduledAgentJoinAndRevokedGuestOpening(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(fmt.Sprint(revoked), func(t *testing.T) {
			cw, k, c := commerceFixture(t, "delegation-booking")
			if cw == nil {
				return
			}
			cleanupDelegationTestRows(t, cw)
			c.ValidUntil = cw.clock.Now().Add(3 * time.Hour)
			commerceVerified(t, cw, k)
			credential, grant := delegationTestGrant(t, cw, k, delegationTestRequest(cw))
			ctx := context.Background()
			quoted, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "quote", ParticipantLimit: 2, DurationMinutes: 60})
			require.NoError(t, err)
			quote := quoted.(*callrules.CallQuote)
			response, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "schedule", IdempotencyKey: "agent-booking-000001", QuoteID: quote.QuoteID, Title: "Agent lesson", Start: cw.clock.Now().Add(time.Hour)})
			require.NoError(t, err)
			booking := response.(callrules.ScheduledCall)
			require.Empty(t, booking.HostToken)
			require.Empty(t, booking.HostLink)
			require.NotEmpty(t, booking.GuestToken)
			require.NotContains(t, booking.ICS, "&h=")
			if revoked {
				_, err = cw.svc.Delegation(ctx, cw.ref, k.result.Session.Token, &callrules.DelegationRequest{Action: "revoke", GrantID: grant})
				require.NoError(t, err)
			}
			cw.advanceTo(booking.Start)
			if revoked {
				_, _, err = cw.join(booking.GuestToken, "Invited guest")
				require.ErrorIs(t, err, callrules.ErrEnded)
				require.ErrorContains(t, err, "cancelled")
				require.Empty(t, cw.media.rooms(), "a guest invitation cannot revive a revoked spending grant")
			} else {
				joined, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "join", CallID: booking.ID, Name: "Teaching assistant"})
				require.NoError(t, err)
				result := joined.(callrules.JoinResult)
				require.Equal(t, callrules.OutcomeAdmit, result.Outcome)
				// The agent obtains media authority only through its current delegation.
				require.NotEmpty(t, result.Pass)
			}
		})
	}
}

func TestDelegationBookingAndHistoryMutationsStayWithinGrant(t *testing.T) {
	cw, k, _ := commerceFixture(t, "delegation-scope")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	credential, _ := delegationTestGrant(t, cw, k, delegationTestRequest(cw))
	other, _ := delegationTestGrant(t, cw, k, delegationTestRequest(cw))
	ctx := context.Background()
	quoted, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "quote", ParticipantLimit: 2, DurationMinutes: 60})
	require.NoError(t, err)
	quote := quoted.(*callrules.CallQuote)
	response, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "schedule", IdempotencyKey: "scope-booking-000001", QuoteID: quote.QuoteID, Start: cw.clock.Now().Add(time.Hour)})
	require.NoError(t, err)
	booking := response.(callrules.ScheduledCall)
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, other, callrules.AgentRequest{Action: "cancel", CallID: booking.ID})
	require.ErrorContains(t, err, "delegation_booking_scope")
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, other, callrules.AgentRequest{Action: "reschedule", IdempotencyKey: "scope-reschedule-001", CallID: booking.ID, QuoteID: quote.QuoteID, Start: cw.clock.Now().Add(2 * time.Hour)}) // gitleaks:allow -- public idempotency fixture, not an authorization credential.
	require.ErrorContains(t, err, "delegation_booking_scope")
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "cancel", CallID: booking.ID})
	require.NoError(t, err)
	listed, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "history"})
	require.NoError(t, err)
	page := listed.(callrules.HistoryPage)
	require.Len(t, page.Calls, 1)
	historyID := page.Calls[0].ID
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, other, callrules.AgentRequest{Action: "archive_preview", CallID: historyID})
	require.ErrorContains(t, err, "delegation_call_scope")
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "archive", CallID: historyID})
	require.ErrorIs(t, err, callrules.ErrInvalid, "standing permission does not bypass archive preview")
	preview, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "archive_preview", CallID: historyID})
	require.NoError(t, err)
	confirmation := preview.(callrules.HistoryActionResult)
	require.NotEmpty(t, confirmation.ConfirmationToken)
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "archive", CallID: historyID, ConfirmationToken: confirmation.ConfirmationToken, Acknowledged: true})
	require.NoError(t, err)
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "restore", CallID: historyID})
	require.NoError(t, err)
}

func TestDelegationSavedCardReplacementRequiresFreshMandate(t *testing.T) {
	cw, k, _ := commerceFixture(t, "delegation-card-binding")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	req := delegationTestRequest(cw)
	req.Scopes = append(req.Scopes, "topup")
	req.Topup = callrules.AutoTopupLimit{Enabled: true, AmountCents: 1000, ThresholdCents: 1000, PerChargeLimitCents: 1000, PeriodLimitCents: 1000, LifetimeLimitCents: 1000}
	credential, _ := delegationTestGrant(t, cw, k, req)
	ctx := memql.ContextWithFreshRead(context.Background())
	state, err := cw.svc.store.commerceState(ctx, cw.owner, cw.site, k.result.Account.AccountID)
	require.NoError(t, err)
	checkout := *state.latest("checkout")
	if checkout.ProviderID == "" {
		// The local completed checkout has no external reference. A signed
		// provider reconciliation can discover it from the durable operation.
		checkout.ProviderID = "mock_checkout_" + checkout.ID
	}
	originalMethod := state.PaymentMethodID
	state.PaymentMethodID = "pm_replacement_for_test"
	require.NoError(t, cw.svc.store.saveCommerce(ctx, cw.owner, state, cw.clock.Now()))
	err = cw.svc.applyCommerceCheckout(ctx, cw.ref, k.result.Account.AccountID, checkout.ID, providers.Checkout{ID: checkout.ProviderID, PaymentID: checkout.PaymentID, AccountID: k.result.Account.AccountID, OperationID: checkout.ID, AmountCents: checkout.AmountCents, Currency: "usd", Paid: true, CustomerID: state.CustomerID, PaymentMethodID: originalMethod})
	require.NoError(t, err)
	state, err = cw.svc.store.commerceState(ctx, cw.owner, cw.site, k.result.Account.AccountID)
	require.NoError(t, err)
	require.Equal(t, "pm_replacement_for_test", state.PaymentMethodID, "old payment events must not restore an old card")
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "topup", IdempotencyKey: "old-card-mandate-001"})
	require.ErrorContains(t, err, "payment_method_changed")
	state, err = cw.svc.store.commerceState(ctx, cw.owner, cw.site, k.result.Account.AccountID)
	require.NoError(t, err)
	for _, op := range state.Operations {
		require.NotEqual(t, "auto_topup", op.Kind, "changed card cannot dispatch a provider charge")
	}
	newCredential, _ := delegationTestGrant(t, cw, k, req)
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, newCredential, callrules.AgentRequest{Action: "topup", IdempotencyKey: "new-card-mandate-001"})
	require.NoError(t, err)
}
