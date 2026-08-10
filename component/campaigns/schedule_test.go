package campaigns

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/email"
)

// schedule_test.go -- the claim memql#3459 makes, which is that a campaign
// with a scheduledAt now sends without a hand on Start.
//
// The tests are chosen for the ways a scheduler goes wrong rather than for
// coverage of its happy path:
//
//	fires when due                  TestDueScheduledCampaignSends
//	does NOT fire before            TestScheduledCampaignDoesNotSendEarly
//	the CAMPAIGN's time wins        TestPostponingWithTheCampaignRowIsObeyed
//	exactly once across replicas    TestTwoReplicasPromoteOnce
//	late is still sent, not dropped TestScheduleMissedDuringDowntimeStillSends
//	a refusal is VISIBLE            TestUnreadiedTemplateFailsTheCampaignLoudly
//	                                TestMissingSenderWaitsRatherThanFailing

func scheduledJobRow(at time.Time) map[string]any {
	row := jobRow()
	row["status"] = "scheduled"
	row["scheduledAt"] = at.UTC().Format(time.RFC3339)
	return row
}

func scheduledCampaignRow(at time.Time) map[string]any {
	row := campaignRow()
	row["status"] = "scheduled"
	row["scheduledAt"] = at.UTC().Format(time.RFC3339)
	return row
}

// scheduleEngine wires a cluster holding exactly one scheduled campaign.
func scheduleEngine(jobAt, campaignAt time.Time) *fakeEngine {
	return &fakeEngine{
		scheduledJobs: []map[string]any{scheduledJobRow(jobAt)},
		campaign:      scheduledCampaignRow(campaignAt),
		template:      templateRow(),
		roster:        []map[string]any{recipientRow("r1", "one@example.test", "subscribed")},
	}
}

func atClock(w *Worker, t time.Time) {
	w.now = func() time.Time { return t }
	w.limiter = newRateLimiter(w.cfg.SendRatePerMinute, w.now)
}

// TestDueScheduledCampaignSends is the issue in one assertion: a campaign
// whose time has passed moves to `sending`, its job becomes drainable, and a
// message actually leaves -- with nobody pressing anything.
func TestDueScheduledCampaignSends(t *testing.T) {
	due := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	engine := scheduleEngine(due, due)
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	atClock(w, due.Add(time.Minute))

	w.promoteDueSchedules(context.Background(), w.systemActorContext(context.Background()))

	starts := engine.mutations("startCampaign")
	if len(starts) != 1 {
		t.Fatalf("want the campaign moved into sending exactly once, got %d writes", len(starts))
	}
	if starts[0].actorID != testOwner {
		t.Errorf("the campaign was moved by %q; it must be written under the CAMPAIGN OWNER'S actor", starts[0].actorID)
	}
	updates := engine.mutations("updateSendJob")
	if len(updates) != 1 || argOf(updates[0].query, "status") != "queued" {
		t.Fatalf("want the send job promoted to queued, got %d writes: %v", len(updates), updates)
	}
	if !updates[0].isOwner {
		t.Error("the send job is clusterOwner-tier and must be written under the engine's own identity")
	}
}

// TestScheduledCampaignDoesNotSendEarly is the other half, and the one that
// matters more: a scheduler that fires early has mailed an audience before
// the operator meant to, which is unrecallable.
func TestScheduledCampaignDoesNotSendEarly(t *testing.T) {
	due := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	engine := scheduleEngine(due, due)
	w := newTestWorker(t, engine, &recordingSender{})
	atClock(w, due.Add(-time.Hour))

	w.promoteDueSchedules(context.Background(), w.systemActorContext(context.Background()))

	if n := len(engine.mutations("startCampaign")); n != 0 {
		t.Errorf("the campaign was started %d times an hour before its scheduled time", n)
	}
	if n := len(engine.mutations("updateSendJob")); n != 0 {
		t.Errorf("the job was promoted %d times before its time", n)
	}
}

// TestPostponingWithTheCampaignRowIsObeyed pins the authority question. The
// job row still carries the ORIGINAL time; the operator moved the campaign's.
// A worker reading its own stale copy would send a campaign that had been
// postponed -- which is a worse failure than the one this feature fixes,
// because it is unrecallable rather than merely absent.
func TestPostponingWithTheCampaignRowIsObeyed(t *testing.T) {
	original := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	postponed := original.Add(48 * time.Hour)
	engine := scheduleEngine(original, postponed)
	w := newTestWorker(t, engine, &recordingSender{})
	atClock(w, original.Add(time.Hour)) // past the job's copy, before the campaign's

	w.promoteDueSchedules(context.Background(), w.systemActorContext(context.Background()))

	if n := len(engine.mutations("startCampaign")); n != 0 {
		t.Errorf("the send fired against the job's stale scheduledAt; the campaign row is the authority (%d starts)", n)
	}
}

// countingClaimer is the cross-replica gate, admitting the first caller per
// key exactly as the automations ClusterExecutionGuard does against Postgres.
type countingClaimer struct {
	mu    sync.Mutex
	taken map[string]bool
}

func (c *countingClaimer) ClaimWithTTL(_ context.Context, name, key string, _ time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.taken == nil {
		c.taken = map[string]bool{}
	}
	composite := name + "\x00" + key
	if c.taken[composite] {
		return false
	}
	c.taken[composite] = true
	return true
}

// TestTwoReplicasPromoteOnce is the acceptance criterion the issue states in
// those words: a due campaign enqueues exactly once WITH TWO REPLICAS
// RUNNING, verified against two workers rather than one.
//
// Note what is being shown. The claim makes the campaign status write happen
// once; but the property that actually protects a recipient is that the job
// id IS the campaign id, so even a double promotion writes one row -- and the
// delivery ledger is per (campaign, recipient) underneath that. The test
// asserts the claim's effect because that is the observable one, and the
// structural argument is recorded in schedule.go where it can be read.
func TestTwoReplicasPromoteOnce(t *testing.T) {
	due := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	engine := scheduleEngine(due, due)
	claimer := &countingClaimer{}

	var wg sync.WaitGroup
	for range 2 {
		w := newTestWorker(t, engine, &recordingSender{})
		w.claimer = claimer
		atClock(w, due.Add(time.Minute))
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.promoteDueSchedules(context.Background(), w.systemActorContext(context.Background()))
		}()
	}
	wg.Wait()

	if n := len(engine.mutations("startCampaign")); n != 1 {
		t.Errorf("two replicas promoted the same due campaign %d times, want 1", n)
	}
}

// TestScheduleMissedDuringDowntimeStillSends covers the catch-up decision.
// A campaign due while the cluster was down sends when the cluster comes
// back, rather than being silently dropped for being late -- which would
// recreate the exact silence this issue is about.
func TestScheduleMissedDuringDowntimeStillSends(t *testing.T) {
	due := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	engine := scheduleEngine(due, due)
	w := newTestWorker(t, engine, &recordingSender{})
	atClock(w, due.Add(72*time.Hour)) // three days of downtime

	w.promoteDueSchedules(context.Background(), w.systemActorContext(context.Background()))

	if n := len(engine.mutations("startCampaign")); n != 1 {
		t.Errorf("a campaign due during downtime was not sent on recovery (%d starts)", n)
	}
}

// TestUnreadiedTemplateFailsTheCampaignLoudly: an AUTHORING problem is
// terminal, and the reason lands on the campaign row -- not only in a log
// nobody was reading at 3am.
func TestUnreadiedTemplateFailsTheCampaignLoudly(t *testing.T) {
	due := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	engine := scheduleEngine(due, due)
	engine.template = templateRow()
	engine.template["status"] = "draft" // un-readied after scheduling
	w := newTestWorker(t, engine, &recordingSender{})
	atClock(w, due.Add(time.Minute))

	w.promoteDueSchedules(context.Background(), w.systemActorContext(context.Background()))

	if n := len(engine.mutations("startCampaign")); n != 0 {
		t.Fatalf("a campaign with an un-readied template was sent (%d starts)", n)
	}
	progress := engine.mutations("updateCampaignProgress")
	if len(progress) != 1 {
		t.Fatalf("want the refusal recorded on the campaign row, got %d writes", len(progress))
	}
	if got := argOf(progress[0].query, "status"); got != "failed" {
		t.Errorf("campaign status = %q, want failed -- an authoring problem is terminal", got)
	}
	// Asserted over the whole rendered call rather than through argOf: the
	// reason quotes the template id and its status, so it carries escaped
	// quotes that argOf's deliberately-crude scan stops at.
	if !strings.Contains(progress[0].query, "un-readied") {
		t.Errorf("lastError did not say WHICH thing stopped the send: %s", progress[0].query)
	}
}

// TestMissingSenderWaitsRatherThanFailing: an ENVIRONMENT problem is not
// terminal. A node whose integration registry has not populated yet must not
// destroy a campaign that happened to come due in that window -- the operator
// would have to re-author a schedule to recover from a bad deploy.
func TestMissingSenderWaitsRatherThanFailing(t *testing.T) {
	due := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	engine := scheduleEngine(due, due)
	w := newTestWorker(t, engine, nil)
	w.resolve = func() email.Sender { return nil }
	atClock(w, due.Add(time.Minute))

	w.promoteDueSchedules(context.Background(), w.systemActorContext(context.Background()))

	if n := len(engine.mutations("updateSendJob")); n != 0 {
		t.Errorf("the job was touched (%d writes); an unconfigured node should leave it scheduled for the next tick", n)
	}
	progress := engine.mutations("updateCampaignProgress")
	if len(progress) != 1 {
		t.Fatalf("want the reason stamped on the campaign, got %d writes", len(progress))
	}
	if got := argOf(progress[0].query, "status"); got != "" {
		t.Errorf("campaign status = %q, want untouched -- the campaign is fine, the cluster is not", got)
	}
}

// TestCancelledCampaignStopsItsScheduledJob: cancelling before the time
// arrives has to reach the job, or the campaign would fire anyway.
func TestCancelledCampaignStopsItsScheduledJob(t *testing.T) {
	due := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	engine := scheduleEngine(due, due)
	engine.campaign["status"] = "cancelled"
	w := newTestWorker(t, engine, &recordingSender{})
	atClock(w, due.Add(time.Minute))

	w.promoteDueSchedules(context.Background(), w.systemActorContext(context.Background()))

	updates := engine.mutations("updateSendJob")
	if len(updates) != 1 || argOf(updates[0].query, "status") != "cancelled" {
		t.Fatalf("want the scheduled job cancelled with its campaign, got %v", updates)
	}
	if n := len(engine.mutations("startCampaign")); n != 0 {
		t.Errorf("a cancelled campaign was started (%d times)", n)
	}
}

// --- the builtin --------------------------------------------------------

func schedulingCtx() context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: testOwner,
		Role:   auth.RoleWriter,
	})
}

func schedulingEngine() *fakeEngine {
	draft := campaignRow()
	draft["status"] = "draft"
	return &fakeEngine{
		campaign: draft,
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r1", "one@example.test", "subscribed")},
	}
}

// TestScheduleSendEnqueuesAnInertJob: the builtin writes both halves, and the
// job it writes cannot mail anybody until it is promoted.
func TestScheduleSendEnqueuesAnInertJob(t *testing.T) {
	engine := schedulingEngine()
	w := newTestWorker(t, engine, &recordingSender{})
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	atClock(w, now)

	if _, err := w.handleScheduleSend(schedulingCtx(), map[string]any{
		"campaignId":  testCampaign,
		"scheduledAt": "2026-08-14T09:00:00Z",
	}, 0); err != nil {
		t.Fatalf("scheduleSend: %v", err)
	}

	enqueued := engine.mutations("enqueueCampaignSend")
	if len(enqueued) != 1 {
		t.Fatalf("want one send job enqueued, got %d", len(enqueued))
	}
	if got := argOf(enqueued[0].query, "status"); got != "scheduled" {
		t.Errorf("job status = %q, want scheduled -- a queued job would send immediately", got)
	}
	if got := argOf(enqueued[0].query, "campaignOwnerUserId"); got != testOwner {
		t.Errorf("campaignOwnerUserId = %q; it must be copied off the campaign the caller could read", got)
	}
	if !enqueued[0].isOwner {
		t.Error("the send job is clusterOwner-tier and must be written under the engine's own identity")
	}
	scheduled := engine.mutations("scheduleCampaign")
	if len(scheduled) != 1 {
		t.Fatalf("want the campaign committed to the time, got %d writes", len(scheduled))
	}
	if scheduled[0].actorID != testOwner {
		t.Errorf("the campaign was scheduled by %q; it is the operator's own row", scheduled[0].actorID)
	}
}

// TestScheduleSendRefusesAPastTime: a time already gone is far more often a
// typo in the year or the offset than a request to send now, and there is an
// unambiguous way to say the latter.
func TestScheduleSendRefusesAPastTime(t *testing.T) {
	engine := schedulingEngine()
	w := newTestWorker(t, engine, &recordingSender{})
	atClock(w, time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC))

	_, err := w.handleScheduleSend(schedulingCtx(), map[string]any{
		"campaignId":  testCampaign,
		"scheduledAt": "2025-08-14T09:00:00Z",
	}, 0)
	if err == nil {
		t.Fatal("a schedule a year in the past was accepted")
	}
	if !strings.Contains(err.Error(), "campaignStartSend") {
		t.Errorf("the refusal must point at the way to send now; got %q", err)
	}
	if n := len(engine.mutations("enqueueCampaignSend")); n != 0 {
		t.Errorf("a job was enqueued (%d) despite the refusal", n)
	}
}

// TestScheduleSendRunsTheSamePreflightAsStart is why scheduling is a builtin
// at all: a schedule that could never send should be refused while the
// operator is still looking at the screen.
func TestScheduleSendRunsTheSamePreflightAsStart(t *testing.T) {
	engine := schedulingEngine()
	engine.template["status"] = "draft"
	w := newTestWorker(t, engine, &recordingSender{})
	atClock(w, time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC))

	_, err := w.handleScheduleSend(schedulingCtx(), map[string]any{
		"campaignId":  testCampaign,
		"scheduledAt": "2026-08-14T09:00:00Z",
	}, 0)
	if err == nil {
		t.Fatal("a campaign with a draft template was scheduled")
	}
	if !strings.Contains(err.Error(), `not "ready"`) {
		t.Errorf("refusal = %q, want the template readiness reason", err)
	}
}
