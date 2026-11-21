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

func analyticsOverview(t *testing.T, svc *Service, host callrules.Session, days int) callrules.HistoryOverview {
	t.Helper()
	ctx := context.Background()
	held, err := svc.authenticatedKey(ctx, host)
	require.NoError(t, err)
	out := callrules.HistoryOverview{PeriodDays: days}
	require.NoError(t, svc.historyLedger(ctx, held, &out))
	return out
}

func TestHistoryAnalyticsPostedDebitsExcludeReservationsAndSurvivePurge(t *testing.T) {
	cw := newCallWorld(t, "history-analytics-settle", 120)
	if cw == nil {
		return
	}
	ctx := context.Background()
	key, host := meteredAccount(t, cw)
	q := meteredQuote(t, cw, key, 2, 60)
	started, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: q.QuoteID, IdempotencyKey: "analytics-reserved-call"})
	require.NoError(t, err)
	page := completeHistory(t, cw, host)
	require.EqualValues(t, 0, *page.Overview.SpendCents)
	require.EqualValues(t, 30, *page.Overview.ReservedCents)
	require.EqualValues(t, 0, *page.Overview.Usage.AgentHostedCalls)
	require.EqualValues(t, 0, *page.Overview.Usage.InsightsCalls)
	require.Equal(t, "complete", page.Overview.Usage.Status)
	cw.media.setPresent(started.Call.ID, MediaParticipant{SID: "analytics-host", Identity: started.Join.Identity, JoinedAt: cw.clock.Now()})
	cw.clock.Advance(20 * time.Minute)
	broken, fault := qualificationAfterCommit(cw, "createCallsSimulatedWalletEntry(")
	require.Error(t, broken.Host(ctx, host, callrules.HostActionRequest{Action: callrules.HostEnd}))
	require.True(t, fault.failed)
	ledger := analyticsOverview(t, cw.svc, host, 90)
	require.EqualValues(t, 5, *ledger.SpendCents, "a committed debit is visible even before its caller recovers")
	require.EqualValues(t, 1000, *ledger.Ledger.CreditedCents)
	require.EqualValues(t, 0, *ledger.ReservedCents)
	require.NoError(t, allowanceReplica(t, cw).Reconcile(ctx))
	page = completeHistory(t, cw, host)
	require.EqualValues(t, 5, *page.Overview.SpendCents)
	require.Len(t, page.Calls, 1)
	preview, err := cw.svc.HistoryAction(ctx, host, callrules.HistoryActionRequest{Action: "archive_preview", ID: page.Calls[0].ID})
	require.NoError(t, err)
	_, err = cw.svc.HistoryAction(ctx, host, callrules.HistoryActionRequest{Action: "archive", ID: page.Calls[0].ID, ConfirmationToken: preview.ConfirmationToken, Acknowledged: true})
	require.NoError(t, err)
	archived := completeHistory(t, cw, host)
	require.EqualValues(t, 1, *archived.Overview.HostedCalls, "unexpired archived metadata remains in retained history")
	deadline, err := time.Parse(time.RFC3339Nano, preview.DeleteAt)
	require.NoError(t, err)
	cw.clock.Advance(deadline.Sub(cw.clock.Now()))
	// This disposable database models retention's physical history delete. Its
	// suppression receipt remains, so repair cannot restore any source details.
	_, err = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept=? AND id=?", historyConcept, historyConcept+":"+page.Calls[0].ID).Exec(ctx)
	require.NoError(t, err)
	signed, err := accountAssert(t, cw, key, "", "login", "")
	require.NoError(t, err)
	var after callrules.HistoryPage
	for range 4 {
		after, err = cw.svc.History(ctx, *signed.Session, callrules.HistoryRequest{Period: 90})
		require.NoError(t, err)
	}
	require.Empty(t, after.Calls)
	require.EqualValues(t, 0, *after.Overview.HostedCalls)
	require.EqualValues(t, 0, *after.Overview.Usage.AgentHostedCalls)
	require.EqualValues(t, 5, *after.Overview.SpendCents, "financial receipts survive without restoring private call history")
	require.Len(t, cw.rowsOf(simulationWalletConcept), 2)
	other := accountRegister(t, cw, newSoftwareAuthenticator(t))
	isolated := completeHistory(t, cw, *other.result.Session)
	require.EqualValues(t, 0, *isolated.Overview.SpendCents)
	require.EqualValues(t, 0, *isolated.Overview.Ledger.CreditedCents)
	require.EqualValues(t, 0, *isolated.Overview.AvailableCents)
	require.EqualValues(t, 0, *isolated.Overview.ReservedCents)
}

func TestHistoryAnalyticsReceiptDateLegacyPassAndUnknownFeatureCounts(t *testing.T) {
	cw := newCallWorld(t, "history-analytics-period", 20)
	if cw == nil {
		return
	}
	ctx := context.Background()
	enableFunding(t, cw)
	key := accountRegister(t, cw, newSoftwareAuthenticator(t))
	broken, fault := qualificationAfterCommit(cw, "createCallsSimulatedWalletEntry(")
	_, err := broken.Account(ctx, cw.ref, key.result.Session.Token, topupRequest(key, 1000, "analytics-old-topup"))
	require.Error(t, err)
	require.True(t, fault.failed)
	require.EqualValues(t, 1000, *analyticsOverview(t, cw.svc, *key.result.Session, 7).Ledger.CreditedCents)
	fundAction(t, cw, key, topupRequest(key, 1000, "analytics-old-topup"))
	cw.clock.Advance(8 * 24 * time.Hour)
	signed, err := accountAssert(t, cw, key, "", "login", "")
	require.NoError(t, err)
	key.result.Session = signed.Session
	fundAction(t, cw, key, activateRequest(key, "day", "analytics-legacy-pass"))
	started, err := cw.svc.Start(ctx, *signed.Session, callrules.StartBody{Name: "Legacy", Confirm: confirmed})
	require.NoError(t, err)
	out := completeHistory(t, cw, *signed.Session).Overview
	require.EqualValues(t, 0, *out.Ledger.CreditedCents, "top-up outside selected period")
	require.EqualValues(t, 150, *out.Ledger.PassDebitedCents)
	require.EqualValues(t, 150, *out.SpendCents)
	require.EqualValues(t, 150, *out.Series[len(out.Series)-1].CostCents)
	require.Nil(t, out.Usage.InsightsCalls, "legacy config is unknown, not false")
	require.EqualValues(t, 0, *out.Usage.AgentHostedCalls)
	require.Equal(t, "incomplete", out.Usage.Status)
	require.NoError(t, cw.act(*signed.Session, callrules.HostEnd, ""))
	require.Equal(t, string(callrules.KindPerson), rowString(cw.row(roomConcept, started.Call.ID), "originalHostKind"))
}

func TestHistoryAnalyticsPendingCheckoutAndRefundAreNotWalletReversals(t *testing.T) {
	cw, key, _ := commerceFixture(t, "history-analytics-refund")
	if cw == nil {
		return
	}
	pending := commerceDo(t, cw, key, callrules.AccountRequest{Action: "checkout_begin", AmountCents: 1000, IdempotencyKey: "analytics-checkout", AdultDeclared: true, TermsAccepted: true})
	before := analyticsOverview(t, cw.svc, *key.result.Session, 30)
	require.EqualValues(t, 0, *before.Ledger.CreditedCents)
	commerceDo(t, cw, key, callrules.AccountRequest{Action: "mock_checkout_complete", OperationID: pending.Lifecycle.Checkout.ID, Outcome: "paid"})
	state, err := cw.svc.store.commerceState(context.Background(), cw.owner, cw.site, key.result.Account.AccountID)
	require.NoError(t, err)
	op := state.latest("checkout")
	refund := providers.Checkout{ID: "cs_analytics_refund", AccountID: key.result.Account.AccountID, OperationID: op.ID, AmountCents: op.AmountCents, Currency: "usd", Paid: true, PaymentID: "pi_analytics_refund", Refunded: true}
	require.NoError(t, cw.svc.applyCommerceCheckout(context.Background(), cw.ref, key.result.Account.AccountID, op.ID, refund))
	after := analyticsOverview(t, cw.svc, *key.result.Session, 30)
	require.True(t, *after.Ledger.FinancialReview)
	require.EqualValues(t, 1000, *after.Ledger.CreditedCents)
	require.EqualValues(t, 0, *after.SpendCents)
	require.EqualValues(t, 1000, *after.AvailableCents, "posted wallet value does not claim net cash or hosting permission")
}

type analyticsInterleaveEngine struct {
	Engine
	match   string
	hook    func()
	repeats bool
	called  int
}

func (e *analyticsInterleaveEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.Contains(q, e.match) && (e.called == 0 || e.repeats) {
		e.called++
		e.hook()
	}
	return e.Engine.Execute(ctx, q)
}
func analyticsInterleave(cw *callWorld, match string, repeats bool, hook func()) (*Service, *analyticsInterleaveEngine) {
	service := *cw.svc
	store := *service.store
	fault := &analyticsInterleaveEngine{Engine: store.engine, match: match, hook: hook, repeats: repeats}
	store.engine = fault
	service.store = &store
	return &service, fault
}

func TestHistoryAnalyticsRetriesConcurrentPostingAndNeverReturnsPartialTotals(t *testing.T) {
	cw := newCallWorld(t, "history-analytics-concurrent", 20)
	if cw == nil {
		return
	}
	key, host := meteredAccount(t, cw)
	once, _ := analyticsInterleave(cw, "callsWalletEntriesForPeriod(", false, func() { fundAction(t, cw, key, topupRequest(key, 1000, "analytics-interleave")) })
	out := analyticsOverview(t, once, host, 30)
	require.Equal(t, "complete", out.Ledger.Status)
	require.EqualValues(t, 2000, *out.Ledger.CreditedCents)
	require.EqualValues(t, 2000, *out.AvailableCents)
	i := 0
	churn, fault := analyticsInterleave(cw, "callsWalletEntriesForPeriod(", true, func() { i++; fundAction(t, cw, key, topupRequest(key, 1000, fmt.Sprintf("analytics-churn-%03d", i))) })
	out = analyticsOverview(t, churn, host, 30)
	require.Equal(t, 3, fault.called)
	require.Equal(t, "incomplete", out.Ledger.Status)
	require.Nil(t, out.SpendCents)
	require.Nil(t, out.AvailableCents)
	require.Nil(t, out.Ledger.CreditedCents)
}

func TestHistoryAnalyticsBoundedLedgerDoesNotReturnTruncatedTotals(t *testing.T) {
	cw := newCallWorld(t, "history-analytics-bounded", 20)
	if cw == nil {
		return
	}
	_, host := meteredAccount(t, cw)
	ctx := context.Background()
	wallet, err := cw.svc.simulatedWallet(ctx, cw.owner, cw.site, host.KeyID)
	require.NoError(t, err)
	// Immutable one-cent settlement receipts model many very short calls. This
	// exercises the actual DSL cap, not a mock that invents an oversized page.
	for i := range historyLedgerLimit {
		entry := simulationWalletEntry{ID: id.NewShortId(), SiteID: cw.site, KeyID: host.KeyID, Kind: "settlement", RequestDigest: strings.Repeat("a", 64), BalanceCents: int64(999 - i), DebitedCents: 1, Version: wallet.Version.Add(time.Microsecond)}
		require.NoError(t, cw.svc.store.createSimulatedWalletEntry(ctx, cw.owner, entry))
		wallet = &entry
		if i == 599 {
			complete := analyticsOverview(t, cw.svc, host, 30)
			require.Equal(t, "complete", complete.Ledger.Status)
			require.EqualValues(t, 600, *complete.SpendCents, "totals cross the engine's 500-row cap using native continuations")
			require.EqualValues(t, 400, *complete.AvailableCents)
		}
	}
	out := analyticsOverview(t, cw.svc, host, 30)

	require.Equal(t, "limited", out.Ledger.Status)
	require.Nil(t, out.SpendCents)
	require.Nil(t, out.Ledger.UsageDebitedCents)
	require.EqualValues(t, 0, *out.AvailableCents)
	require.Empty(t, out.Series)
}

func TestHistoryAnalyticsRechecksCredentialAndSessionAfterLedgerReads(t *testing.T) {
	for _, change := range []string{"credential", "session"} {
		t.Run(change, func(t *testing.T) {
			cw := newCallWorld(t, "history-analytics-auth-"+change, 20)
			if cw == nil {
				return
			}
			key, host := meteredAccount(t, cw)
			broken, _ := analyticsInterleave(cw, "callsWalletEntriesForPeriod(", false, func() {
				ctx := memql.ContextWithFreshRead(context.Background())
				if change == "credential" {
					credential, err := cw.svc.store.credentialByID(ctx, cw.owner, key.result.Credentials[0].ID)
					require.NoError(t, err)
					require.NoError(t, cw.svc.store.updateCredential(ctx, cw.owner, *credential, map[string]any{"revokedAt": versionStamp(cw.clock.Now())}, cw.clock.Now()))
				} else {
					row, _, err := cw.svc.session(ctx, cw.ref, host.Token, cw.clock.Now())
					require.NoError(t, err)
					require.NoError(t, cw.svc.store.updateAccountSession(ctx, cw.owner, row, map[string]any{"endedAt": versionStamp(cw.clock.Now())}, cw.clock.Now()))
				}
			})
			out, err := broken.History(context.Background(), host, callrules.HistoryRequest{})
			require.Error(t, err)
			require.Empty(t, out.AccountID)
			require.Nil(t, out.Overview.Ledger)
		})
	}
}

func TestHistoryAnalyticsAgentOpeningHostSurvivesHumanHandover(t *testing.T) {
	cw, key, _ := commerceFixture(t, "history-analytics-agent")
	if cw == nil {
		return
	}
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, key)
	credential, _ := delegationTestGrant(t, cw, key, delegationTestRequest(cw))
	ctx := context.Background()
	quoted, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "quote", ParticipantLimit: 2, DurationMinutes: 60, Insights: true, Resources: true})
	require.NoError(t, err)
	q := quoted.(*callrules.CallQuote)
	result, err := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "start", IdempotencyKey: "analytics-agent-opening", QuoteID: q.QuoteID, Name: "Assistant", Terms: callrules.AgentTermsVersion, Participation: "adults_only"})
	require.NoError(t, err)
	started := result.(callrules.StartResult)
	out := completeHistory(t, cw, *key.result.Session).Overview
	require.EqualValues(t, 1, *out.Usage.AgentHostedCalls)
	require.EqualValues(t, 1, *out.Usage.InsightsCalls)
	require.EqualValues(t, 1, *out.Usage.ResourcesCalls)
	// A later human co-host and a changed hostSeatId do not redefine the opening
	// host's kind. This is the same narrow native handover mutation boundary.
	guestKey := accountRegister(t, cw, newSoftwareAuthenticator(t))
	commerceVerified(t, cw, guestKey)
	guestSession := *guestKey.result.Session
	joined, guest, err := cw.svc.Join(ctx, cw.ref, &guestSession, callrules.JoinBody{Token: started.Call.GuestToken, Name: "Human co-host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, joined.Outcome)
	room, err := cw.svc.store.roomByID(ctx, cw.owner, started.Call.ID)
	require.NoError(t, err)
	view, err := cw.svc.viewRoom(ctx, *room)
	require.NoError(t, err)
	seat, err := cw.svc.store.seatByID(ctx, cw.owner, guest.SeatID)
	require.NoError(t, err)
	require.NoError(t, cw.svc.handOver(ctx, view, *seat, cw.clock.Now()))
	out = completeHistory(t, cw, *key.result.Session).Overview
	require.EqualValues(t, 1, *out.Usage.AgentHostedCalls)
	joinedHistory := completeHistory(t, cw, guest).Overview
	require.EqualValues(t, 0, *joinedHistory.HostedCalls)
	require.EqualValues(t, 1, *joinedHistory.JoinedCalls)
	require.EqualValues(t, 0, *joinedHistory.Usage.AgentHostedCalls, "co-host does not become payer/originator")
}

func TestHistoryAnalyticsMissingLegacyOriginRemainsUnknown(t *testing.T) {
	// Pure source projection: an old room has no opening-host stamp even if its
	// current host pointer is populated. Do not infer its past from that pointer.
	svc := &Service{}
	site := callrules.SiteRef{SiteID: "site", OwnerUserID: "owner"}
	row, err := svc.roomHistory(context.Background(), site, "key", roomRow{ID: "room", SiteID: "site", OwnerUserID: "owner", KeyID: "key", HostSeatID: "current-host", OpenedAt: time.Now()}, []byte("pepper"))
	require.NoError(t, err)
	require.Nil(t, row.AgentHosted)
}

func TestHistoryAnalyticsRejectsRevokedAgentAfterLedgerRead(t *testing.T) {
	for _, change := range []string{"revoked", "scope", "approval"} {
		t.Run(change, func(t *testing.T) {
			cw, key, _ := commerceFixture(t, "history-analytics-agent-"+change)
			if cw == nil {
				return
			}
			cleanupDelegationTestRows(t, cw)
			commerceVerified(t, cw, key)
			credential, grantID := delegationTestGrant(t, cw, key, delegationTestRequest(cw))
			service, _ := analyticsInterleave(cw, "callsWalletEntriesForPeriod(", false, func() {
				ctx := memql.ContextWithFreshRead(context.Background())
				grant, err := cw.svc.store.delegation(ctx, cw.owner, grantID)
				require.NoError(t, err)
				switch change {
				case "revoked":
					grant.State.RevokedAt = cw.clock.Now()
				case "scope":
					grant.State.Request.Scopes = []string{"host"}
				case "approval":
					grant.State.Request.Purpose = "Another approved purpose"
				}
				require.NoError(t, cw.svc.store.saveDelegation(ctx, cw.owner, grant, cw.clock.Now()))
			})
			result, err := delegationTestAgent(t, cw, service, context.Background(), cw.ref, credential, callrules.AgentRequest{Action: "history"})
			require.Error(t, err)
			if result != nil {
				require.Empty(t, result.(callrules.HistoryPage).AccountID)
			}
		})
	}
}
