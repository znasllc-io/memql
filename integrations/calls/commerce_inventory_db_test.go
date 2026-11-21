package calls

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/calls/providers"
)

func TestCommerceAccountingInventoryTraversesActualEngineCapAndRefusesOverflow(t *testing.T) {
	cw, _, c := commerceFixture(t, "commerce-inventory-complete")
	if cw == nil {
		return
	}
	ctx := context.Background()
	baseline, err := cw.svc.store.commerceAccountCount(ctx)
	require.NoError(t, err)
	cfg := callrules.CallConfiguration{ParticipantLimit: 2, DurationMinutes: 60, RateVersion: callrules.PreviewRateVersion}
	for i := range 600 {
		keyID := id.NewShortId()
		require.NoError(t, cw.svc.store.createKey(ctx, cw.owner, newKey{ID: keyID, SiteID: cw.site, Digest: fmt.Sprintf("%064x", i+1), Kind: "test", UserHandle: "fixture-user-" + keyID}, cw.clock.Now()))
		state := newCommerceState(cw.site, keyID, providers.Mock)
		state.PaidCents = 1000
		require.NoError(t, cw.svc.store.saveCommerce(ctx, cw.owner, state, cw.clock.Now()))
		require.NoError(t, cw.svc.store.createRoom(ctx, cw.owner, newRoom{ID: id.NewShortId(), SiteID: cw.site, KeyID: keyID, MediaServerID: BootstrapMediaServerID, MediaRoom: fmt.Sprint("inventory-room-", i), GuestDigest: strings.Repeat("a", 64), HostDigest: strings.Repeat("b", 64), OpenedAt: cw.clock.Now(), roomFunding: roomFunding{FundingMode: usageMeteredMode, FundingDeadline: cw.clock.Now().Add(time.Hour), FundingMaxPeople: 2, UsageConfig: encodeUsageConfig(cfg), UsageReservedCents: 30, UsageMeter: "[]"}}, cw.clock.Now()))
	}
	states, err := cw.svc.store.commerceAll(ctx)
	require.NoError(t, err)
	require.Len(t, states, 600)
	rooms, err := cw.svc.store.commerceRooms(ctx)
	require.NoError(t, err)
	require.Len(t, rooms, 600)
	count, err := cw.svc.store.commerceAccountCount(ctx)
	require.NoError(t, err)
	require.Equal(t, baseline+600, count)
	// The previous first-500 result would admit this account and understate paid
	// liabilities. The actual complete inventory must enforce existing limits.
	c.MaxAccounts = baseline + 550
	require.Equal(t, "registration_capacity", callrules.ReasonOf(cw.svc.commerceRegistrationAllowed(ctx)))
	c.AvailableCashCents = c.ProtectedCents + c.FixedCents + 550*500
	require.Equal(t, "business_reserve", callrules.ReasonOf(cw.svc.commerceCapacity(ctx, *c, 0, 0, 0, 0)))
	for range 400 {
		state := newCommerceState(cw.site, id.NewShortId(), providers.Mock)
		require.NoError(t, cw.svc.store.saveCommerce(ctx, cw.owner, state, cw.clock.Now()))
	}
	states, err = cw.svc.store.commerceAll(ctx)
	require.Equal(t, "allowance_unavailable", callrules.ReasonOf(err))
	require.Nil(t, states)
	// This invocation is an inventory check only; no hosted call or provider
	// operation is performed for any fixture account.
	require.Empty(t, cw.rowsOf(simulationWalletConcept))
}

type incompleteCommerceInventory struct{ Engine }

func (e incompleteCommerceInventory) Execute(ctx context.Context, query string) (any, error) {
	if strings.Contains(query, "callsCommerceStates(") || strings.Contains(query, "callsCommerceRooms(") || strings.Contains(query, "callsCommerceAccounts(") {
		result := memql.NewResultWithOutput([]map[string]any{})
		result.SetHasMore(true)
		return result, nil
	}
	return e.Engine.Execute(ctx, query)
}
func TestCommerceAccountingInventoryMissingContinuationNeverAuthorizes(t *testing.T) {
	cw := newCallWorld(t, "commerce-inventory-missing", 20)
	if cw == nil {
		return
	}
	store := *cw.svc.store
	store.engine = incompleteCommerceInventory{Engine: store.engine}
	ctx := context.Background()
	states, err := store.commerceAll(ctx)
	require.Equal(t, "allowance_unavailable", callrules.ReasonOf(err))
	require.Nil(t, states)
	rooms, err := store.commerceRooms(ctx)
	require.Equal(t, "allowance_unavailable", callrules.ReasonOf(err))
	require.Nil(t, rooms)
	count, err := store.commerceAccountCount(ctx)
	require.Equal(t, "allowance_unavailable", callrules.ReasonOf(err))
	require.Zero(t, count)
}
