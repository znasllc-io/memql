package calls

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// An agent's automatic top-up has nobody present to type the tester password,
// so while payments are simulated it must never fabricate a paid payment.
func TestDelegationMockTopupRefusedWithoutReservation(t *testing.T) {
	cw, k, _ := commerceFixture(t, "delegation-mock-topup")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	ctx := context.Background()
	req := delegationTestRequest(cw)
	req.Scopes = append(req.Scopes, "topup")
	req.Topup = callrules.AutoTopupLimit{Enabled: true, AmountCents: 1000, ThresholdCents: 1000, PerChargeLimitCents: 1000, PeriodLimitCents: 1000, LifetimeLimitCents: 1000}
	credential, grant := delegationTestGrant(t, cw, k, req)

	_, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "topup", IdempotencyKey: "mock-topup-refused-01"})
	require.Equal(t, callrules.ReasonAutomaticTopupUnavailable, callrules.ReasonOf(err))
	require.ErrorIs(t, err, callrules.ErrForbidden)
	stored, err := cw.svc.store.delegation(memql.ContextWithFreshRead(ctx), cw.owner, grant)
	require.NoError(t, err)
	require.Empty(t, stored.State.Operations, "a refused top-up reserves no mandate")
	_, err = cw.svc.commerceAutoTopup(ctx, cw.ref, k.result.Session.Token, k.result.Account.AccountID, 1000, "mock-topup-direct-0001")
	require.Equal(t, callrules.ReasonAutomaticTopupUnavailable, callrules.ReasonOf(err))

	for _, op := range testerState(t, cw, k).Operations {
		require.NotEqual(t, "auto_topup", op.Kind)
	}
	require.EqualValues(t, 1000, testerBalance(t, cw, k))
}

func TestDelegationMockStartSkipsAutomaticTopup(t *testing.T) {
	cw, k, _ := commerceFixture(t, "delegation-mock-start")
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
	quote := func(people, minutes int) *callrules.CallQuote {
		t.Helper()
		out, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "quote", ParticipantLimit: people, DurationMinutes: minutes})
		require.NoError(t, err)
		return out.(*callrules.CallQuote)
	}

	large := quote(4, 1440)
	require.Greater(t, large.MaxCostCents, int64(1000), "the call must need more than the balance")
	_, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "start", IdempotencyKey: "mock-start-too-large", QuoteID: large.QuoteID, Name: "Assistant", Terms: callrules.AgentTermsVersion, Participation: "adults_only"})
	require.ErrorContains(t, err, "insufficient_balance")
	stored, err := cw.svc.store.delegation(memql.ContextWithFreshRead(ctx), cw.owner, grantID)
	require.NoError(t, err)
	_, topups := delegationUsage(stored, "topup", cw.clock.Now())
	require.Zero(t, topups)
	for _, op := range testerState(t, cw, k).Operations {
		require.NotEqual(t, "auto_topup", op.Kind)
	}
	require.EqualValues(t, 1000, testerBalance(t, cw, k))

	small := quote(2, 10)
	require.LessOrEqual(t, small.MaxCostCents, int64(1000))
	started, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "start", IdempotencyKey: "mock-start-fits-0001", QuoteID: small.QuoteID, Name: "Assistant", Terms: callrules.AgentTermsVersion, Participation: "adults_only"})
	require.NoError(t, err)
	require.NotEmpty(t, started.(callrules.StartResult).Call.ID)
}
