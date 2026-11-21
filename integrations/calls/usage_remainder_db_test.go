package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

func remainderRoom(t *testing.T, cw *callWorld, k accountTestKey, people int, through time.Duration) roomRow {
	t.Helper()
	c := callrules.CallConfiguration{ParticipantLimit: people, DurationMinutes: 10, RateVersion: callrules.PreviewRateVersion}
	at := cw.clock.Now()
	rows := make([]usageInterval, people)
	for i := range rows {
		rows[i] = usageInterval{SID: fmt.Sprint("connection-", i), Identity: fmt.Sprint("seat-", i), Start: at, Through: at.Add(through)}
	}
	data, err := json.Marshal(rows)
	require.NoError(t, err)
	return roomRow{ID: id.NewShortId(), SiteID: cw.site, OwnerUserID: cw.owner, KeyID: k.result.Account.KeyID, OpenedAt: at, roomFunding: roomFunding{FundingMode: usageMeteredMode, FundingDeadline: at.Add(10 * time.Minute), FundingMaxPeople: people, UsageConfig: encodeUsageConfig(c), UsageReservedCents: callrules.PreviewReservation(c), UsageMeter: string(data)}}
}
func remainderSettle(s *Service, room roomRow) (*int64, error) {
	ctx := context.Background()
	release, err := s.gates(ctx, keyGate(room.KeyID))
	if err != nil {
		return nil, err
	}
	defer release()
	return s.settleUsage(memql.ContextWithFreshRead(ctx), room, room.FundingDeadline)
}
func remainderWallet(t *testing.T, cw *callWorld, k accountTestKey) *simulationWalletEntry {
	t.Helper()
	w, err := cw.svc.simulatedWallet(memql.ContextWithFreshRead(context.Background()), cw.owner, cw.site, k.result.Account.KeyID)
	require.NoError(t, err)
	require.NotNil(t, w)
	return w
}

func TestUsageRemainderCarriesAcrossShortCallsTopupsAndReplicaReplays(t *testing.T) {
	cw := newCallWorld(t, "usage-remainder-short", 10)
	if cw == nil {
		return
	}
	k, _ := meteredAccount(t, cw)
	for i := 0; i < 4; i++ {
		room := remainderRoom(t, cw, k, 1, time.Minute)
		cost, err := remainderSettle(cw.svc, room)
		require.NoError(t, err)
		replay, err := remainderSettle(allowanceReplica(t, cw), room)
		require.NoError(t, err)
		require.Equal(t, cost, replay)
		if i == 0 {
			fundAction(t, cw, k, topupRequest(k, 1000, "carry-preserving-topup"))
		}
		wallet := remainderWallet(t, cw, k)
		require.EqualValues(t, 2000-(i+1)/4, wallet.BalanceCents)
		require.EqualValues(t, int64((i+1)%4)*usageUnitsPerCent/4, wallet.UsageRemainderUnits)
	}
	require.Len(t, cw.rowsOf(simulationWalletConcept), 6, "four settlement receipts and two top-ups, despite repeated retries")
}

func TestUsageRemainderConcurrentSettlementAndFunding(t *testing.T) {
	cw := newCallWorld(t, "usage-remainder-concurrent", 10)
	if cw == nil {
		return
	}
	k, _ := meteredAccount(t, cw)
	// Three quarters of a cent already measured.
	_, err := remainderSettle(cw.svc, remainderRoom(t, cw, k, 1, 3*time.Minute))
	require.NoError(t, err)
	room := remainderRoom(t, cw, k, 1, time.Minute)
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, svc := range []*Service{cw.svc, allowanceReplica(t, cw)} {
		wg.Add(1)
		go func(s *Service) {
			defer wg.Done()
			cost, e := remainderSettle(s, room)
			if e == nil && *cost != 1 {
				e = fmt.Errorf("settlement debit: %d", *cost)
			}
			errs <- e
		}(svc)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, e := allowanceReplica(t, cw).Account(context.Background(), cw.ref, k.result.Session.Token, topupRequest(k, 1000, "carry-concurrent-topup"))
		errs <- e
	}()
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	wallet := remainderWallet(t, cw, k)
	require.EqualValues(t, 1999, wallet.BalanceCents)
	require.Zero(t, wallet.UsageRemainderUnits)
}

func TestUsageRemainderFullReservationNeverOverdrawsBalance(t *testing.T) {
	cw := newCallWorld(t, "usage-remainder-full-reserve", 10)
	if cw == nil {
		return
	}
	k, _ := meteredAccount(t, cw)
	previous := remainderWallet(t, cw, k)
	// Existing measured usage has consumed 990 whole cents and almost one more.
	require.NoError(t, cw.svc.store.createSimulatedWalletEntry(context.Background(), cw.owner, simulationWalletEntry{ID: id.NewShortId(), SiteID: cw.site, KeyID: k.result.Account.KeyID, Kind: "settlement", RequestDigest: strings.Repeat("a", 64), BalanceCents: 10, DebitedCents: 990, UsageRemainderUnits: usageUnitsPerCent - 1, Version: memql.VersionTimeAfter(previous.Version, cw.clock.Now())}))
	room := remainderRoom(t, cw, k, 4, 10*time.Minute)
	require.EqualValues(t, 10, room.UsageReservedCents)
	cost, err := remainderSettle(cw.svc, room)
	require.NoError(t, err)
	require.EqualValues(t, 10, *cost)
	wallet := remainderWallet(t, cw, k)
	require.Zero(t, wallet.BalanceCents)
	require.EqualValues(t, usageUnitsPerCent-1, wallet.UsageRemainderUnits)
	// Added funding must retain, rather than forgive, the pending measured fraction.
	fundAction(t, cw, k, topupRequest(k, 1000, "carry-after-zero-balance"))
	_, err = remainderSettle(cw.svc, remainderRoom(t, cw, k, 1, time.Minute))
	require.NoError(t, err)
	wallet = remainderWallet(t, cw, k)
	require.EqualValues(t, 999, wallet.BalanceCents)
	require.EqualValues(t, usageUnitsPerCent/4-1, wallet.UsageRemainderUnits)
}

func TestUsageRemainderOldReceiptMigratesWithoutRetroactiveCharge(t *testing.T) {
	cw := newCallWorld(t, "usage-remainder-migration", 10)
	if cw == nil {
		return
	}
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	enableFunding(t, cw)
	// Persist the actual old wire schema: no remainder property at all.
	entryID := id.NewShortId()
	call, err := langparser.RenderCall("createCallsSimulatedWalletEntry", map[string]any{"entryId": entryID, "siteId": cw.site, "keyId": k.result.Account.KeyID, "kind": "topup", "requestDigest": strings.Repeat("e", 64), "balanceCents": 1000, "creditedCents": 1000, "debitedCents": 0, "versionTime": versionStamp(cw.clock.Now())})
	require.NoError(t, err)
	require.NoError(t, cw.svc.store.writeAs(context.Background(), cw.owner, absent(simulationWalletConcept, entryID), "old wallet fixture", call, nil))
	wallet := remainderWallet(t, cw, k)
	require.Zero(t, wallet.UsageRemainderUnits)
	require.EqualValues(t, 1000, wallet.BalanceCents)
	cost, err := remainderSettle(cw.svc, remainderRoom(t, cw, k, 1, time.Minute))
	require.NoError(t, err)
	require.Zero(t, *cost)
	require.EqualValues(t, usageUnitsPerCent/4, remainderWallet(t, cw, k).UsageRemainderUnits)
}

func TestUsageRemainderSurvivesProviderCheckout(t *testing.T) {
	cw, k, _ := commerceFixture(t, "usage-remainder-provider")
	if cw == nil {
		return
	}
	commercePurchase(t, cw, k, "carry-provider-initial")
	_, err := remainderSettle(cw.svc, remainderRoom(t, cw, k, 1, time.Minute))
	require.NoError(t, err)
	commercePurchase(t, cw, k, "carry-provider-next")
	wallet := remainderWallet(t, cw, k)
	require.EqualValues(t, 2000, wallet.BalanceCents)
	require.EqualValues(t, usageUnitsPerCent/4, wallet.UsageRemainderUnits)
}
