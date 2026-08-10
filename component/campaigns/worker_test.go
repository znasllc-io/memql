package campaigns

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/email"
)

// worker_test.go -- the four behaviours memql#3348 has to get right, each
// driven through DrainOnce against a fake engine so it is provable without
// a database (the outbound worker's test precedent):
//
//	suppression outranks an audience   TestSuppressionOutranksTheAudience
//	a resumed send does not re-mail    TestResumedSendDoesNotReMail
//	rate limits, ours and theirs       TestOurRateLimitBoundsABatch,
//	                                   TestProviderThrottleParksTheJob
//	hard bounce vs soft bounce         TestHardBounceSuppresses...
//
// plus the one that is the point of the issue: the delivery ledger is
// written under the CAMPAIGN OWNER'S actor, which is what stamps
// ownerUserId and clears the owner-gate exemption
// (TestDeliveryIsWrittenUnderTheCampaignOwnersActor).

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

const (
	testOwner    = "user-owner-1"
	testCampaign = "camp-1"
	testAudience = "aud-1"
	testTemplate = "tmpl-1"
)

// recordedCall is one engine call plus the identity it ran under. The
// actor is captured because "which actor issued this write" is the
// central claim of the design, and a test that only checked the query
// string would pass just as well against a worker that reached past the
// owner tier.
type recordedCall struct {
	query   string
	actorID string
	isOwner bool
}

type fakeEngine struct {
	mu sync.Mutex

	jobs        []map[string]any
	campaign    map[string]any
	template    map[string]any
	roster      []map[string]any
	ledger      []map[string]any
	suppression map[string]map[string]any // digest -> row

	calls []recordedCall
}

func (e *fakeEngine) Execute(ctx context.Context, q string) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ac, _ := auth.AccessFromContext(ctx)
	rec := recordedCall{query: q}
	if ac != nil {
		rec.actorID = ac.UserId
		rec.isOwner = ac.IsClusterOwner()
	}
	e.calls = append(e.calls, rec)

	switch {
	case strings.HasPrefix(q, "query drainableSendJobs"):
		return rowsEnvelope(e.jobs), nil
	case strings.HasPrefix(q, "query sendJobById"):
		return rowsEnvelope(e.jobs), nil
	case strings.HasPrefix(q, "query campaignById"):
		return rowsEnvelope([]map[string]any{e.campaign}), nil
	case strings.HasPrefix(q, "query templateById"):
		return rowsEnvelope([]map[string]any{e.template}), nil
	case strings.HasPrefix(q, "query audienceRosterForSend"):
		return rowsEnvelope(e.roster), nil
	case strings.HasPrefix(q, "query deliveryLedgerForCampaign"):
		return rowsEnvelope(e.ledger), nil
	case strings.HasPrefix(q, "query suppressionByDigest"):
		digest := argOf(q, "emailDigest")
		if row, ok := e.suppression[digest]; ok {
			return rowsEnvelope([]map[string]any{row}), nil
		}
		return rowsEnvelope(nil), nil
	default:
		return nil, nil
	}
}

func (e *fakeEngine) mutations(name string) []recordedCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []recordedCall
	for _, c := range e.calls {
		if strings.HasPrefix(c.query, "mutation "+name+"(") {
			out = append(out, c)
		}
	}
	return out
}

func rowsEnvelope(rows []map[string]any) any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		if r != nil {
			out = append(out, r)
		}
	}
	return map[string]any{"output": out}
}

// argOf pulls a quoted argument value back out of a rendered call. Crude
// on purpose: the test asserts on the call the worker actually built, so
// re-implementing the renderer here would only prove it agrees with
// itself.
func argOf(q, name string) string {
	marker := name + `: "`
	i := strings.Index(q, marker)
	if i < 0 {
		return ""
	}
	rest := q[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// recordingSender captures every message instead of delivering it, and
// can be told to fail.
type recordingSender struct {
	mu   sync.Mutex
	sent []email.Message
	fail func(n int) error
}

func (s *recordingSender) Send(_ context.Context, msg email.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.sent)
	if s.fail != nil {
		if err := s.fail(n); err != nil {
			return err
		}
	}
	s.sent = append(s.sent, msg)
	return nil
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *recordingSender) recipients() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sent))
	for _, m := range s.sent {
		out = append(out, m.To)
	}
	return out
}

// --- fixtures -----------------------------------------------------------

func jobRow() map[string]any {
	return map[string]any{
		"id":                  testCampaign,
		"campaignId":          "v1:campaigns:campaign:" + testCampaign,
		"campaignOwnerUserId": "v1:identity:user:" + testOwner,
		"audienceId":          "v1:campaigns:audience:" + testAudience,
		"templateId":          "v1:campaigns:template:" + testTemplate,
		"status":              "queued",
	}
}

func campaignRow() map[string]any {
	return map[string]any{
		"id":          "v1:campaigns:campaign:" + testCampaign,
		"ownerUserId": "v1:identity:user:" + testOwner,
		"name":        "August update",
		"audienceId":  "v1:campaigns:audience:" + testAudience,
		"templateId":  "v1:campaigns:template:" + testTemplate,
		"status":      "sending",
	}
}

func templateRow() map[string]any {
	return map[string]any{
		"id":       "v1:campaigns:template:" + testTemplate,
		"subject":  "Hello {{displayName}}",
		"textBody": "Body copy.",
		"status":   "ready",
	}
}

func recipientRow(id, addr, status string) map[string]any {
	return map[string]any{
		"id":                 "v1:campaigns:recipient:" + id,
		"email":              addr,
		"subscriptionStatus": status,
	}
}

func newTestWorker(t *testing.T, engine Engine, sender email.Sender) *Worker {
	t.Helper()
	w := &Worker{
		store:   NewStore(engine),
		claimer: nil, // single "replica" in a test; the claim is exercised separately
		resolve: func() email.Sender { return sender },
		logger:  quietLogger(),
		cfg: Config{
			Enabled:            true,
			BatchSize:          50,
			SendRatePerMinute:  1000,
			MaxAttempts:        3,
			MaxAudience:        5000,
			DefaultThrottle:    90 * time.Second,
			SendTimeout:        5 * time.Second,
			UnsubscribeSecret:  "test-signing-secret-not-a-credential",
			UnsubscribeBaseURL: "https://example.test",
		},
		now:     time.Now,
		readyCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	w.limiter = newRateLimiter(w.cfg.SendRatePerMinute, w.now)
	return w
}

// --- the four behaviours ------------------------------------------------

// TestSuppressionOutranksTheAudience is the "outranks every audience"
// claim, stated as the case that distinguishes it from filtering at
// audience-build time: the recipient row says `subscribed`, and the
// address is on the cluster list anyway. That is exactly what a
// re-imported CSV produces, and it is the shape a build-time filter
// cannot catch.
func TestSuppressionOutranksTheAudience(t *testing.T) {
	suppressed := "gone@example.test"
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster: []map[string]any{
			recipientRow("r-1", "keep@example.test", "subscribed"),
			// subscribed on the row, suppressed cluster-wide.
			recipientRow("r-2", suppressed, "subscribed"),
		},
		suppression: map[string]map[string]any{
			EmailDigest(suppressed): {
				"id":           EmailDigest(suppressed),
				"reason":       "unsubscribed",
				"suppressedAt": time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	w.DrainOnce(context.Background())

	if got := sender.recipients(); len(got) != 1 || got[0] != "keep@example.test" {
		t.Fatalf("suppression did not outrank the audience: mailed %v, want only keep@example.test", got)
	}

	var skipped, sent bool
	for _, c := range engine.mutations("recordCampaignDelivery") {
		switch argOf(c.query, "status") {
		case "skipped":
			skipped = true
			if reason := argOf(c.query, "skipReason"); reason != "unsubscribed" {
				t.Errorf("skipReason = %q, want the suppression's own reason %q", reason, "unsubscribed")
			}
		case "sent":
			sent = true
		}
	}
	if !skipped {
		t.Error("no `skipped` delivery row was recorded; a suppressed recipient must leave an outcome, not a silence")
	}
	if !sent {
		t.Error("the unsuppressed recipient produced no `sent` delivery row")
	}

	// And the operator's own row converges onto the cluster verdict, so
	// the audience stops showing the address as sendable.
	conv := engine.mutations("setRecipientSubscription")
	if len(conv) != 1 || argOf(conv[0].query, "subscriptionStatus") != "unsubscribed" {
		t.Errorf("recipient row was not converged onto the cluster verdict: %+v", conv)
	}
}

// TestResumedSendDoesNotReMail drives the ledger diff: two recipients
// already terminal, one never attempted. Only the third may be mailed,
// and nothing about the worker's state carries that over -- the ledger
// alone decides, which is why a restart behaves identically.
func TestResumedSendDoesNotReMail(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster: []map[string]any{
			recipientRow("r-1", "already-sent@example.test", "subscribed"),
			recipientRow("r-2", "already-skipped@example.test", "subscribed"),
			recipientRow("r-3", "fresh@example.test", "subscribed"),
		},
		ledger: []map[string]any{
			{"recipientId": "v1:campaigns:recipient:r-1", "status": "sent"},
			{"recipientId": "v1:campaigns:recipient:r-2", "status": "skipped"},
		},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	w.DrainOnce(context.Background())

	got := sender.recipients()
	if len(got) != 1 || got[0] != "fresh@example.test" {
		t.Fatalf("resumed send re-mailed somebody: sent to %v, want only fresh@example.test", got)
	}
}

// TestResumedSendHonoursARetryDeadline covers the other half of the
// ledger rule: a `pending` row is retryable, but not before its
// nextAttemptAt. Without this the backoff would exist on the row and be
// ignored by the reader.
func TestResumedSendHonoursARetryDeadline(t *testing.T) {
	future := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	past := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster: []map[string]any{
			recipientRow("r-1", "not-due@example.test", "subscribed"),
			recipientRow("r-2", "due@example.test", "subscribed"),
		},
		ledger: []map[string]any{
			{"recipientId": "v1:campaigns:recipient:r-1", "status": "pending", "attempts": 1, "nextAttemptAt": future},
			{"recipientId": "v1:campaigns:recipient:r-2", "status": "pending", "attempts": 1, "nextAttemptAt": past},
		},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	w.DrainOnce(context.Background())

	got := sender.recipients()
	if len(got) != 1 || got[0] != "due@example.test" {
		t.Fatalf("retry deadlines not honoured: sent to %v, want only due@example.test", got)
	}
}

// TestOurRateLimitBoundsABatch -- the self-imposed ceiling. Three
// addresses, a bucket holding two: the batch ends rather than blocking,
// and the third is simply still outstanding next tick.
func TestOurRateLimitBoundsABatch(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster: []map[string]any{
			recipientRow("r-1", "a@example.test", "subscribed"),
			recipientRow("r-2", "b@example.test", "subscribed"),
			recipientRow("r-3", "c@example.test", "subscribed"),
		},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	// A frozen clock so the bucket cannot refill mid-batch: the assertion
	// is about the cap, not about how fast the test ran.
	frozen := time.Now()
	w.now = func() time.Time { return frozen }
	w.limiter = newRateLimiter(2, w.now)

	w.DrainOnce(context.Background())

	if sender.count() != 2 {
		t.Fatalf("sent %d messages, want exactly the 2 the bucket allowed", sender.count())
	}
	// The unsent recipient must have NO delivery row -- absence is what
	// puts it in the next batch. A `failed` row here would drop it.
	if n := len(engine.mutations("recordCampaignDelivery")); n != 2 {
		t.Fatalf("recorded %d delivery rows, want 2; the rate-limited recipient must be left with no row at all", n)
	}
}

// TestProviderThrottleParksTheJob -- the provider's ceiling. A 429 with a
// Retry-After parks the whole job until the stated deadline and ends the
// batch, rather than moving on to the next address (which is what turns
// one throttle into a storm and the storm into duplicates).
func TestProviderThrottleParksTheJob(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster: []map[string]any{
			recipientRow("r-1", "a@example.test", "subscribed"),
			recipientRow("r-2", "b@example.test", "subscribed"),
			recipientRow("r-3", "c@example.test", "subscribed"),
		},
	}
	sender := &recordingSender{
		fail: func(n int) error {
			if n == 1 {
				return &email.SendError{StatusCode: 429, Throttled: true, RetryAfter: 120 * time.Second, Detail: "throttled"}
			}
			return nil
		},
	}
	w := newTestWorker(t, engine, sender)
	frozen := time.Now().UTC()
	w.now = func() time.Time { return frozen }

	w.DrainOnce(context.Background())

	if sender.count() != 1 {
		t.Fatalf("sent %d messages, want 1: the batch must stop at the throttle rather than trying the next address", sender.count())
	}
	parked := engine.mutations("updateSendJob")
	var until string
	for _, c := range parked {
		if v := argOf(c.query, "throttledUntil"); v != "" {
			until = v
		}
	}
	if until == "" {
		t.Fatal("the job was not parked: no throttledUntil was stamped, so the next tick would hammer the provider again")
	}
	want := frozen.Add(120 * time.Second).Format(time.RFC3339)
	if until != want {
		t.Errorf("throttledUntil = %q, want the provider's own Retry-After deadline %q", until, want)
	}

	// The throttled recipient stays retryable -- a throttle is not that
	// message's failure.
	var sawPending bool
	for _, c := range engine.mutations("recordCampaignDelivery") {
		if argOf(c.query, "status") == "pending" {
			sawPending = true
		}
		if argOf(c.query, "status") == "failed" {
			t.Error("a throttled recipient was recorded as FAILED; the message was never judged on its merits")
		}
	}
	if !sawPending {
		t.Error("the throttled recipient left no retryable row")
	}
}

// TestThrottleWithoutRetryAfterStillParks -- a 429 with no header is
// still a throttle. Reading the absent header as "retry now" is the
// hammering case.
func TestThrottleWithoutRetryAfterStillParks(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "a@example.test", "subscribed")},
	}
	sender := &recordingSender{fail: func(int) error {
		return &email.SendError{StatusCode: 429, Throttled: true, Detail: "throttled"}
	}}
	w := newTestWorker(t, engine, sender)
	frozen := time.Now().UTC()
	w.now = func() time.Time { return frozen }

	w.DrainOnce(context.Background())

	want := frozen.Add(w.cfg.DefaultThrottle).Format(time.RFC3339)
	var got string
	for _, c := range engine.mutations("updateSendJob") {
		if v := argOf(c.query, "throttledUntil"); v != "" {
			got = v
		}
	}
	if got != want {
		t.Errorf("throttledUntil = %q, want the configured default %q", got, want)
	}
}

// TestPermanentFailureIsNotRetried -- a refused message must go terminal
// immediately. Retrying it re-refuses on every batch for the life of the
// campaign.
func TestPermanentFailureIsNotRetried(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "bad@example.test", "subscribed")},
	}
	sender := &recordingSender{fail: func(int) error {
		return &email.SendError{StatusCode: 400, Permanent: true, Detail: "refused"}
	}}
	w := newTestWorker(t, engine, sender)

	w.DrainOnce(context.Background())

	rows := engine.mutations("recordCampaignDelivery")
	if len(rows) != 1 || argOf(rows[0].query, "status") != "failed" {
		t.Fatalf("permanent failure did not go terminal: %+v", rows)
	}
}

// TestRetryableFailureSchedulesABackoff -- the complement. A failure with
// no classification is retryable, and lands as `pending` with a future
// deadline rather than as a terminal row.
func TestRetryableFailureSchedulesABackoff(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "flaky@example.test", "subscribed")},
	}
	sender := &recordingSender{fail: func(int) error { return fmt.Errorf("connection reset") }}
	w := newTestWorker(t, engine, sender)
	frozen := time.Now().UTC()
	w.now = func() time.Time { return frozen }

	w.DrainOnce(context.Background())

	rows := engine.mutations("recordCampaignDelivery")
	if len(rows) != 1 {
		t.Fatalf("want one delivery row, got %d", len(rows))
	}
	if got := argOf(rows[0].query, "status"); got != "pending" {
		t.Fatalf("status = %q, want pending", got)
	}
	next := argOf(rows[0].query, "nextAttemptAt")
	parsed, err := time.Parse(time.RFC3339, next)
	if err != nil {
		t.Fatalf("nextAttemptAt %q is not RFC3339: %v", next, err)
	}
	if !parsed.After(frozen) {
		t.Errorf("nextAttemptAt %s is not in the future; the retry would fire immediately", next)
	}
}

// TestAttemptsExhaustGoTerminal -- the retry budget is bounded. A row
// already at MaxAttempts-1 goes `failed` on its next failure rather than
// retrying forever.
func TestAttemptsExhaustGoTerminal(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "flaky@example.test", "subscribed")},
		ledger: []map[string]any{
			{"recipientId": "v1:campaigns:recipient:r-1", "status": "pending", "attempts": 2, "nextAttemptAt": past},
		},
	}
	sender := &recordingSender{fail: func(int) error { return fmt.Errorf("connection reset") }}
	w := newTestWorker(t, engine, sender)
	w.cfg.MaxAttempts = 3

	w.DrainOnce(context.Background())

	rows := engine.mutations("recordCampaignDelivery")
	if len(rows) != 1 || argOf(rows[0].query, "status") != "failed" {
		t.Fatalf("attempt 3 of 3 did not go terminal: %+v", rows)
	}
}

// TestDeliveryIsWrittenUnderTheCampaignOwnersActor is the assertion
// memql#3348 exists for. `recordCampaignDelivery` stamps
// `ownerUserId: actor.userId`, so the value on the row is whatever
// identity issued the call -- and the ONLY correct identity is the
// campaign's owner. A worker that wrote it under its own system identity
// would satisfy the DSL gate (the field is still server-stamped) and
// produce rows the operator cannot read.
func TestDeliveryIsWrittenUnderTheCampaignOwnersActor(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "a@example.test", "subscribed")},
	}
	w := newTestWorker(t, engine, &recordingSender{})

	w.DrainOnce(context.Background())

	rows := engine.mutations("recordCampaignDelivery")
	if len(rows) != 1 {
		t.Fatalf("want one delivery row, got %d", len(rows))
	}
	if rows[0].actorID != testOwner {
		t.Errorf("delivery written under actor %q, want the campaign owner %q -- ownerUserId is stamped from actor.userId, "+
			"so any other actor produces a row the owner cannot read", rows[0].actorID, testOwner)
	}
	if rows[0].isOwner {
		t.Error("the delivery write ran as a CLUSTER OWNER. The worker must borrow exactly the campaign owner's authority; " +
			"anything more means the owner tier is not what bounds it")
	}
}

// TestSuppressionIsReadUnderTheEngineIdentity -- the other half of the
// two-identity split. The suppression list must NOT be read under the
// campaign owner's actor: it is clusterOwner-tier precisely so that no
// operator's authority decides whether it applies to them.
func TestSuppressionIsReadUnderTheEngineIdentity(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "a@example.test", "subscribed")},
	}
	w := newTestWorker(t, engine, &recordingSender{})

	w.DrainOnce(context.Background())

	engine.mu.Lock()
	defer engine.mu.Unlock()
	var seen bool
	for _, c := range engine.calls {
		if strings.HasPrefix(c.query, "query suppressionByDigest") {
			seen = true
			if c.actorID != systemCampaignsActor {
				t.Errorf("suppression read under actor %q, want the engine identity %q", c.actorID, systemCampaignsActor)
			}
			if !c.isOwner {
				t.Error("suppression read under a non-cluster-owner actor; the clusterOwner tier would return nothing, " +
					"and an empty result reads as `not suppressed`")
			}
		}
	}
	if !seen {
		t.Fatal("no suppression lookup happened at all -- the list is not being consulted at the point of send")
	}
}

// TestSuppressionLookupFailureStopsTheBatch -- fail CLOSED. A lookup that
// errored must not be read as "not suppressed": a delayed message is
// recoverable, a message to somebody who opted out is not.
func TestSuppressionLookupFailureStopsTheBatch(t *testing.T) {
	engine := &failingSuppressionEngine{fakeEngine: fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "a@example.test", "subscribed")},
	}}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	w.DrainOnce(context.Background())

	if sender.count() != 0 {
		t.Fatalf("sent %d messages after a FAILED suppression lookup; an unanswerable list must block the send, not be assumed empty", sender.count())
	}
}

type failingSuppressionEngine struct{ fakeEngine }

func (e *failingSuppressionEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "query suppressionByDigest") {
		return nil, fmt.Errorf("database unavailable")
	}
	return e.fakeEngine.Execute(ctx, q)
}

// TestUnsubscribeHeadersArePresent -- RFC 8058 is delivered as two
// headers, and a message missing either is a bulk send the large mailbox
// providers treat as defective.
func TestUnsubscribeHeadersArePresent(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "a@example.test", "subscribed")},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	w.DrainOnce(context.Background())

	if sender.count() != 1 {
		t.Fatalf("want one message, got %d", sender.count())
	}
	msg := sender.sent[0]
	uri := msg.Headers["List-Unsubscribe"]
	if !strings.HasPrefix(uri, "<https://example.test/unsubscribe?token=") || !strings.HasSuffix(uri, ">") {
		t.Errorf("List-Unsubscribe = %q; RFC 2369 requires the URI in angle brackets and it must point at the one-click endpoint", uri)
	}
	if got := msg.Headers["List-Unsubscribe-Post"]; got != "List-Unsubscribe=One-Click" {
		t.Errorf("List-Unsubscribe-Post = %q, want %q", got, "List-Unsubscribe=One-Click")
	}
	if !strings.Contains(msg.TextBody, "Unsubscribe: https://example.test/unsubscribe?token=") {
		t.Error("the visible body carries no unsubscribe link; the header serves the provider's UI, a person needs the link")
	}
}

// TestSendIsRefusedWithoutUnsubscribeConfig -- fail closed on the
// precondition. Sending without a working opt-out is not a degraded send.
func TestSendIsRefusedWithoutUnsubscribeConfig(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "a@example.test", "subscribed")},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.cfg.UnsubscribeSecret = ""

	w.DrainOnce(context.Background())

	if sender.count() != 0 {
		t.Fatalf("sent %d messages with no unsubscribe secret configured; the send must be refused", sender.count())
	}
	var failed bool
	for _, c := range engine.mutations("updateSendJob") {
		if argOf(c.query, "status") == "failed" {
			failed = true
		}
	}
	if !failed {
		t.Error("the job was not failed, so the refusal is invisible to the operator")
	}
}

// TestCampaignStatusIsAuthoritativeOverTheJob -- an operator pausing the
// CAMPAIGN must stop the send even though they never touched the job row.
func TestCampaignStatusIsAuthoritativeOverTheJob(t *testing.T) {
	campaign := campaignRow()
	campaign["status"] = "paused"
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaign,
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "a@example.test", "subscribed")},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	w.DrainOnce(context.Background())

	if sender.count() != 0 {
		t.Fatalf("sent %d messages for a PAUSED campaign", sender.count())
	}
	var paused bool
	for _, c := range engine.mutations("updateSendJob") {
		if argOf(c.query, "status") == "paused" {
			paused = true
		}
	}
	if !paused {
		t.Error("the job did not follow the campaign into paused")
	}
}

// TestCompletionStampsBothRows -- when every roster member is terminal
// the job completes and the campaign reads `sent`.
func TestCompletionStampsBothRows(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "a@example.test", "subscribed")},
		ledger: []map[string]any{
			{"recipientId": "v1:campaigns:recipient:r-1", "status": "sent"},
		},
	}
	w := newTestWorker(t, engine, &recordingSender{})

	w.DrainOnce(context.Background())

	var jobDone, campaignSent bool
	for _, c := range engine.mutations("updateSendJob") {
		if argOf(c.query, "status") == "completed" {
			jobDone = true
		}
	}
	for _, c := range engine.mutations("updateCampaignProgress") {
		if argOf(c.query, "status") == "sent" {
			campaignSent = true
			if c.actorID != testOwner {
				t.Errorf("campaign progress written under %q, want the owner %q", c.actorID, testOwner)
			}
		}
	}
	if !jobDone || !campaignSent {
		t.Errorf("completion did not stamp both rows: jobCompleted=%v campaignSent=%v", jobDone, campaignSent)
	}
}

// TestSelectBatchIsPureAndOrdered pins the resume rule directly, without
// an engine. Roster order is preserved so a resumed send continues rather
// than re-shuffling, and the limit is respected.
func TestSelectBatchIsPureAndOrdered(t *testing.T) {
	now := time.Now()
	roster := []Recipient{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
	}
	ledger := map[string]LedgerEntry{
		"b": {Status: "sent"},
		"c": {Status: "pending", NextAttemptAt: now.Add(-time.Minute)},
	}
	got := selectBatch(roster, ledger, now, 10)
	var ids []string
	for _, it := range got {
		ids = append(ids, it.recipient.ID)
	}
	want := []string{"a", "c", "d"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("selectBatch = %v, want %v", ids, want)
	}
	if len(selectBatch(roster, ledger, now, 2)) != 2 {
		t.Error("selectBatch ignored its limit")
	}
}
