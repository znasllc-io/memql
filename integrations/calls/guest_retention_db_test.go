package calls

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

func retirementGuest(t *testing.T, cw *callWorld, age time.Duration, handle string) string {
	t.Helper()
	keyID := id.NewShortId()
	at := cw.clock.Now().Add(-age)
	require.NoError(t, cw.svc.store.createKey(context.Background(), cw.owner, newKey{ID: keyID, SiteID: cw.site, Digest: strings.Repeat("b", 64), Kind: "guest", UserHandle: handle, IssuedAt: at}, at))
	return keyID
}
func retirementStatus(t *testing.T, cw *callWorld, keyID string) string {
	t.Helper()
	k, err := cw.svc.store.keyByID(memql.ContextWithFreshRead(context.Background()), cw.owner, keyID)
	require.NoError(t, err)
	require.NotNil(t, k)
	return k.Status
}

func TestGuestRetirementAgeBoundaryAndIdempotentReplica(t *testing.T) {
	cw := newCallWorld(t, "guest-retirement", 10)
	if cw == nil {
		return
	}
	ctx := context.Background()
	old := retirementGuest(t, cw, guestInactivityPeriod+time.Second, "")
	recent := retirementGuest(t, cw, guestInactivityPeriod-time.Second, "")
	claimed := retirementGuest(t, cw, 2*guestInactivityPeriod, "passkey-user-handle")
	var wg sync.WaitGroup
	for _, svc := range []*Service{cw.svc, allowanceReplica(t, cw)} {
		wg.Add(1)
		go func(s *Service) { defer wg.Done(); require.NoError(t, s.retireGuestKey(ctx, cw.owner, old)) }(svc)
	}
	wg.Wait()
	require.Equal(t, "retired", retirementStatus(t, cw, old))
	require.NoError(t, cw.svc.retireGuestKey(ctx, cw.owner, recent))
	require.NoError(t, cw.svc.retireGuestKey(ctx, cw.owner, claimed))
	require.Equal(t, "active", retirementStatus(t, cw, recent))
	require.Equal(t, "active", retirementStatus(t, cw, claimed))
	k, err := cw.svc.store.keyByID(ctx, cw.owner, old)
	require.NoError(t, err)
	before := k.Version
	require.NoError(t, cw.svc.retireGuestKey(ctx, cw.owner, old))
	k, err = cw.svc.store.keyByID(memql.ContextWithFreshRead(ctx), cw.owner, old)
	require.NoError(t, err)
	require.Equal(t, before, k.Version)
	// Retirement is the credential boundary even for a still-present old row.
	row, err := cw.svc.store.checkedSessionRow(ctx, cw.owner, &sessionRow{KeyID: old, SiteID: cw.site, Kind: callrules.SessionGuest})
	require.NoError(t, err)
	require.Nil(t, row)
}

func TestGuestRetirementKeepsRecentSessionsAndFinancialRecords(t *testing.T) {
	cw := newCallWorld(t, "guest-retirement-protected", 10)
	if cw == nil {
		return
	}
	ctx := context.Background()
	for _, mode := range []string{"recent", "live", "funded"} {
		key := retirementGuest(t, cw, 2*guestInactivityPeriod, "")
		if mode == "funded" {
			require.NoError(t, cw.svc.store.createSimulatedWalletEntry(ctx, cw.owner, simulationWalletEntry{ID: id.NewShortId(), SiteID: cw.site, KeyID: key, Kind: "topup", RequestDigest: strings.Repeat("c", 64), BalanceCents: 1000, CreditedCents: 1000, Version: cw.clock.Now().Add(-40 * 24 * time.Hour)}))
		} else {
			at := cw.clock.Now().Add(-time.Hour)
			expires := cw.clock.Now().Add(-time.Minute)
			if mode == "live" {
				at = cw.clock.Now().Add(-40 * 24 * time.Hour)
				expires = cw.clock.Now().Add(time.Hour)
			}
			require.NoError(t, cw.svc.store.createSession(ctx, cw.owner, newSession{ID: id.NewShortId(), SiteID: cw.site, KeyID: key, TokenDigest: strings.Repeat("d", 64), Kind: callrules.SessionGuest, ExpiresAt: expires}, at))
		}
		require.NoError(t, cw.svc.retireGuestKey(ctx, cw.owner, key))
		require.Equal(t, "active", retirementStatus(t, cw, key), mode)
	}
}

func TestGuestRetirementKeepsActiveCallAndBooking(t *testing.T) {
	cw := newCallWorld(t, "guest-retirement-media", 10)
	if cw == nil {
		return
	}
	ctx := context.Background()
	host, err := cw.svc.Unlock(ctx, cw.ref, cw.mintKey("active"))
	require.NoError(t, err)
	started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed})
	require.NoError(t, err)
	_, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	cw.clock.Advance(31 * 24 * time.Hour)
	require.NoError(t, cw.svc.retireGuestKey(ctx, cw.owner, guest.KeyID))
	require.Equal(t, "active", retirementStatus(t, cw, guest.KeyID), "a still-open media seat prevents retirement")
	key := retirementGuest(t, cw, 2*guestInactivityPeriod, "")
	require.NoError(t, cw.svc.store.createBooking(ctx, cw.owner, newBooking{ID: id.NewShortId(), SiteID: cw.site, KeyID: key, Start: cw.clock.Now().Add(time.Hour), End: cw.clock.Now().Add(2 * time.Hour), GuestDigest: strings.Repeat("e", 64), HostDigest: strings.Repeat("f", 64), CalendarUID: "retirement-test"}, cw.clock.Now().Add(-40*24*time.Hour)))
	require.NoError(t, cw.svc.retireGuestKey(ctx, cw.owner, key))
	require.Equal(t, "active", retirementStatus(t, cw, key), "upcoming bookings protect their key")
}

func TestGuestRetirementBoundedScanVisitsNextPage(t *testing.T) {
	cw := newCallWorld(t, "guest-retirement-pages", 10)
	if cw == nil {
		return
	}
	ctx := context.Background()
	var keys []string
	for i := 0; i < guestRetirementBatch+2; i++ {
		keys = append(keys, retirementGuest(t, cw, 2*guestInactivityPeriod, ""))
	}
	require.NoError(t, cw.svc.ReconcileGuestKeys(ctx))
	retired := 0
	for _, key := range keys {
		if retirementStatus(t, cw, key) == "retired" {
			retired++
		}
	}
	require.Equal(t, guestRetirementBatch, retired)
	require.NoError(t, cw.svc.ReconcileGuestKeys(ctx))
	for _, key := range keys {
		require.Equal(t, "retired", retirementStatus(t, cw, key))
	}
	require.Empty(t, cw.svc.guestRetirement.cursor)
}

func TestGuestRetirementRechecksClaimUnderAccountGate(t *testing.T) {
	cw := newCallWorld(t, "guest-retirement-claim-race", 10)
	if cw == nil {
		return
	}
	ctx := context.Background()
	key := retirementGuest(t, cw, 2*guestInactivityPeriod, "")
	release, err := cw.svc.gates(ctx, accountGate(key))
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- allowanceReplica(t, cw).retireGuestKey(ctx, cw.owner, key) }()
	cw.asOwnerMutate(keyConcept, key, "claimCallsKey", map[string]any{"keyId": key, "digest": strings.Repeat("a", 64), "userHandle": "claimed-before-retirement-lock", "keySuffix": "abcd"})
	release()
	require.NoError(t, <-done)
	require.Equal(t, "active", retirementStatus(t, cw, key))
}

func TestGuestRetirementPolicyMayLengthenButNeverShortenWindow(t *testing.T) {
	cw := newCallWorld(t, "guest-retirement-policy", 10)
	if cw == nil {
		return
	}
	ctx := context.Background()
	key := retirementGuest(t, cw, 31*24*time.Hour, "")
	_, err := cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "guestInactiveDays": 29})
	require.Error(t, err)
	_, err = cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "guestInactiveDays": 45})
	require.NoError(t, err)
	require.NoError(t, cw.svc.retireGuestKey(ctx, cw.owner, key))
	require.Equal(t, "active", retirementStatus(t, cw, key))
	cw.clock.Advance(14 * 24 * time.Hour)
	require.NoError(t, cw.svc.retireGuestKey(ctx, cw.owner, key))
	require.Equal(t, "retired", retirementStatus(t, cw, key))
}
