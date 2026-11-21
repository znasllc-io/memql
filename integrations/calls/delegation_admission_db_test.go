package calls

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

func TestDelegationStartValidationPrecedesReservationAndAutomaticTopup(t *testing.T) {
	cw, k, _ := commerceFixture(t, "delegation-start-validation")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	ctx := context.Background()
	grant := delegationTestRequest(cw)
	grant.MaxCallCents, grant.PeriodLimitCents, grant.MaxDurationMinutes = 2000, 2000, 1440
	grant.Scopes = append(grant.Scopes, "topup")
	grant.Topup = callrules.AutoTopupLimit{Enabled: true, AmountCents: 1000, ThresholdCents: 1000, PerChargeLimitCents: 1000, PeriodLimitCents: 1000, LifetimeLimitCents: 1000}
	credential, grantID := delegationTestGrant(t, cw, k, grant)
	quoted, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "quote", ParticipantLimit: 4, DurationMinutes: 1440})
	require.NoError(t, err)
	quote := quoted.(*callrules.CallQuote)
	before, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.Greater(t, quote.MaxCostCents, *before.Account.AvailableCents, "the valid request must need automatic funding")
	request := callrules.AgentRequest{Action: "start", IdempotencyKey: "agent-validation-before-cost", QuoteID: quote.QuoteID, Name: "Assistant", Terms: callrules.AgentTermsVersion, Participation: "adults_only"}
	for _, tc := range []struct {
		name, reason string
		change       func(*callrules.AgentRequest)
	}{
		{"terms", "delegation_participation_scope", func(r *callrules.AgentRequest) { r.Terms = "different-terms" }},
		{"participation", "delegation_participation_scope", func(r *callrules.AgentRequest) { r.Participation = "children_present" }},
		{"name", "", func(r *callrules.AgentRequest) { r.Name = "" }},
		{"title", "", func(r *callrules.AgentRequest) { r.Title = strings.Repeat("a", 81) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rejected := request
			tc.change(&rejected)
			_, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, rejected)
			if tc.reason != "" {
				require.ErrorContains(t, err, tc.reason)
			} else {
				require.ErrorIs(t, err, callrules.ErrInvalid)
			}
			stored, err := cw.svc.store.delegation(memql.ContextWithFreshRead(ctx), cw.owner, grantID)
			require.NoError(t, err)
			require.Empty(t, stored.State.Operations, "a deterministic refusal must not reserve usage or top-up authority")
			funds, err := cw.svc.store.commerceState(memql.ContextWithFreshRead(ctx), cw.owner, cw.site, k.result.Account.AccountID)
			require.NoError(t, err)
			for _, op := range funds.Operations {
				require.NotEqual(t, "auto_topup", op.Kind, "a deterministic refusal must not submit a saved-method charge")
			}
			after, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, nil)
			require.NoError(t, err)
			require.Equal(t, *before.Account.BalanceCents, *after.Account.BalanceCents)
			require.Equal(t, *before.Account.AvailableCents, *after.Account.AvailableCents)
			require.Empty(t, cw.media.rooms())
		})
	}
	if t.Failed() {
		return
	}
	// Legacy supervised declarations remain readable, but cannot fund a start
	// that the dedicated family-admission policy will deterministically refuse.
	legacy := grant
	legacy.Participation, legacy.AuthorityAccepted = "children_present", true
	legacyCredential, legacyID := delegationTestGrant(t, cw, k, legacy)
	legacyRequest := request
	legacyRequest.Participation = "children_present"
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, legacyCredential, legacyRequest)
	require.ErrorContains(t, err, "family_consent_required")
	legacyStored, err := cw.svc.store.delegation(memql.ContextWithFreshRead(ctx), cw.owner, legacyID)
	require.NoError(t, err)
	require.Empty(t, legacyStored.State.Operations)
	unchanged, err := cw.svc.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.Equal(t, *before.Account.BalanceCents, *unchanged.Account.BalanceCents)
	require.Equal(t, *before.Account.AvailableCents, *unchanged.Account.AvailableCents)
	// A same-account quote created by a human does not authorize agent family
	// participation, and must not trigger funding before that refusal either.
	t.Setenv("MEMQL_CALLS_FAMILY_MODE", "mock")
	familyQuote := commerceDo(t, cw, k, callrules.AccountRequest{Action: "quote_call", ParticipantLimit: 4, DurationMinutes: 1440, ParticipationMode: familyMode})
	familyRequest := request
	familyRequest.QuoteID = familyQuote.Quote.QuoteID
	_, err = delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, familyRequest)
	require.ErrorContains(t, err, "family_features_unavailable")
	unfunded, err := cw.svc.store.delegation(memql.ContextWithFreshRead(ctx), cw.owner, grantID)
	require.NoError(t, err)
	require.Empty(t, unfunded.State.Operations)
	unchanged, err = cw.svc.Account(ctx, cw.ref, k.result.Session.Token, nil)
	require.NoError(t, err)
	require.Equal(t, *before.Account.BalanceCents, *unchanged.Account.BalanceCents)
	require.Equal(t, *before.Account.AvailableCents, *unchanged.Account.AvailableCents)
	// Refusal consumed neither the nonce nor the top-up mandate: the corrected
	// request can fund and start once, and an exact retry cannot charge twice.
	result, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, request)
	require.NoError(t, err)
	started := result.(callrules.StartResult)
	replay, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, request)
	require.NoError(t, err)
	require.Equal(t, started.Call.ID, replay.(callrules.StartResult).Call.ID)
	stored, err := cw.svc.store.delegation(memql.ContextWithFreshRead(ctx), cw.owner, grantID)
	require.NoError(t, err)
	require.Len(t, stored.State.Operations, 2)
	_, topups := delegationUsage(stored, "topup", cw.clock.Now())
	require.EqualValues(t, 1000, topups)
}
