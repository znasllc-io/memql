package campaigns

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
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

	jobs []map[string]any
	// scheduledJobs backs `scheduledSendJobs` (memql#3459). A separate slice
	// rather than a status filter over `jobs`, because the two queries answer
	// different questions and a test that conflated them could not tell a
	// scheduled job that was correctly left alone from one the drain path
	// wrongly picked up.
	scheduledJobs []map[string]any
	campaign      map[string]any
	template      map[string]any
	roster        []map[string]any
	ledger        []map[string]any
	suppression   map[string]map[string]any // digest -> row
	// pageSize bounds one roster page. Zero means "everything in one page",
	// which is the pre-paging shape most tests still want.
	pageSize int

	// shopifyProducts backs purchasableVariants for #4140. Nil is an empty
	// catalog. Rows are mirrored VARIANTS since memql#4389.
	shopifyProducts []map[string]any
	// shopifyStores backs the `stores` read that decides whether a Shopify
	// catalog is in play at all.
	shopifyStores []map[string]any

	// senderIdentities backs senderIdentityById (memql#4821), keyed by BARE
	// id -- the form the campaign row's senderIdentityId is bare-ified to
	// before it becomes a query argument.
	senderIdentities map[string]map[string]any
	// identityReadErr makes senderIdentityById FAIL rather than answer
	// empty. The two are different outcomes and the difference is the whole
	// environment-versus-authoring split: an empty answer is an authoring
	// mistake and terminal, a failure is the cluster's and waits.
	identityReadErr error
	// accounts backs clientAccountById, keyed by bare id.
	accounts map[string]map[string]any

	// audiences backs audienceById, keyed by the id as the query carries it. An
	// organization's campaign is checked against its audience's organization
	// before it sends.
	audiences map[string]map[string]any

	// emailRules backs emailRuleById (memql#4829), keyed by bare id. Its one
	// consumer is sendToRecipient resolving which audience a rule mails.
	emailRules map[string]map[string]any

	// libraryArtifacts / libraryFiles back the CSV import's two owner-gated
	// reads (memql#4822), keyed by bare id. Absent means "not visible to
	// this caller", which is the same answer the real queries give and is
	// what makes the "an artifact id is not a capability" assertion possible.
	libraryArtifacts map[string]map[string]any
	libraryFiles     map[string]map[string]any

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
	case strings.HasPrefix(q, "query scheduledSendJobs"):
		return rowsEnvelope(e.scheduledJobs), nil
	case strings.HasPrefix(q, "query recentSendJobs"):
		return rowsEnvelope(e.jobs), nil
	case strings.HasPrefix(q, "query sendJobById"):
		return rowsEnvelope(e.jobs), nil
	case strings.HasPrefix(q, "query campaignById"):
		return rowsEnvelope([]map[string]any{e.campaign}), nil
	case strings.HasPrefix(q, "query templateById"):
		return rowsEnvelope([]map[string]any{e.template}), nil
	case strings.HasPrefix(q, "query audienceRosterForSend"):
		// PAGED, because the send walks it (memql#3460). A fake that
		// returned the whole roster at once would make every paging test
		// vacuous -- the walk would exhaust the audience on its first read
		// and no cursor would ever be exercised.
		return e.rosterPage(memql.CursorFromContext(ctx)), nil
	case strings.HasPrefix(q, "query audienceRosterSize"):
		return rowsEnvelope([]map[string]any{{"count": len(e.roster)}}), nil
	case strings.HasPrefix(q, "query deliveriesForRecipients"):
		return rowsEnvelope(e.ledgerFor(idListOf(q, "recipientIds"))), nil
	case strings.HasPrefix(q, "query deliveryLedgerForCampaign"):
		return rowsEnvelope(e.ledger), nil
	case strings.HasPrefix(q, "query suppressionForOrganization"):
		if row, ok := e.suppression[argOf(q, "suppressionId")]; ok && bare(str(row, "accountId")) == bare(argOf(q, "accountId")) {
			return rowsEnvelope([]map[string]any{row}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query suppressionByDigest"):
		digest := argOf(q, "emailDigest")
		if row, ok := e.suppression[digest]; ok {
			return rowsEnvelope([]map[string]any{row}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query purchasableVariants"):
		return rowsEnvelope(e.shopifyProducts), nil
	case strings.HasPrefix(q, "query stores"):
		return rowsEnvelope(e.shopifyStores), nil
	case strings.HasPrefix(q, "query senderIdentityById"):
		if e.identityReadErr != nil {
			return nil, e.identityReadErr
		}
		if row, ok := e.senderIdentities[argOf(q, "senderIdentityId")]; ok {
			return rowsEnvelope([]map[string]any{row}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query audienceById"):
		if row, ok := e.audiences[argOf(q, "audienceId")]; ok {
			return rowsEnvelope([]map[string]any{row}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query clientAccountById"):
		if row, ok := e.accounts[argOf(q, "accountId")]; ok {
			return rowsEnvelope([]map[string]any{row}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query recipientById"):
		// MODELS THE TIER, which is the whole point of the query: the row
		// comes back only to its owner or a cluster owner. A fake that
		// answered regardless would make "a recipient the caller does not
		// own is not found" untestable while looking covered -- and that
		// sentence is the entire safety argument for exposing an unscoped
		// by-id read.
		if ac == nil || (ac.UserId != testOwner && !ac.IsClusterOwner()) {
			return rowsEnvelope(nil), nil
		}
		wanted := argOf(q, "recipientId")
		for _, row := range e.roster {
			if memql.BareShortId(str(row, "id")) == wanted {
				return rowsEnvelope([]map[string]any{row}), nil
			}
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query emailRuleById"):
		if row, ok := e.emailRules[argOf(q, "emailRuleId")]; ok {
			return rowsEnvelope([]map[string]any{row}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query audiences("):
		rows := make([]map[string]any, 0, 1)
		if len(e.roster) > 0 {
			rows = append(rows, map[string]any{"id": "v1:campaigns:audience:" + testAudience})
		}
		return rowsEnvelope(rows), nil
	case strings.HasPrefix(q, "query libraryArtifactById"):
		if row, ok := e.libraryArtifacts[argOf(q, "artifactId")]; ok {
			return rowsEnvelope([]map[string]any{row}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query libraryFileById"):
		if row, ok := e.libraryFiles[argOf(q, "fileId")]; ok {
			return rowsEnvelope([]map[string]any{row}), nil
		}
		return rowsEnvelope(nil), nil
	default:
		return nil, nil
	}
}

// rosterPage serves one page of the roster from a numeric cursor. The real
// cursor is an opaque keyset token; the fake's is the offset, because what
// the walk is being tested on is that it CARRIES the token it was given and
// stops when there is none -- not the codec.
func (e *fakeEngine) rosterPage(cursor string) any {
	size := e.pageSize
	if size <= 0 {
		size = len(e.roster) + 1
	}
	from := 0
	if cursor != "" {
		_, _ = fmt.Sscanf(cursor, "%d", &from)
	}
	if from > len(e.roster) {
		from = len(e.roster)
	}
	to := from + size
	if to > len(e.roster) {
		to = len(e.roster)
	}
	envelope := map[string]any{"output": toAnySlice(e.roster[from:to])}
	if to < len(e.roster) {
		envelope["cursor"] = fmt.Sprintf("%d", to)
	}
	return envelope
}

// ledgerFor narrows the ledger to the canonical recipient ids named, the way
// deliveriesForRecipients does.
func (e *fakeEngine) ledgerFor(canonicalIDs []string) []map[string]any {
	want := make(map[string]bool, len(canonicalIDs))
	for _, id := range canonicalIDs {
		want[id] = true
	}
	out := make([]map[string]any, 0, len(canonicalIDs))
	for _, row := range e.ledger {
		if id, ok := row["recipientId"].(string); ok && want[id] {
			out = append(out, row)
		}
	}
	return out
}

func toAnySlice(rows []map[string]any) []any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return out
}

// idListOf pulls a rendered `name: ["a", "b"]` argument back out of a call.
func idListOf(q, name string) []string {
	marker := name + ": ["
	i := strings.Index(q, marker)
	if i < 0 {
		return nil
	}
	rest := q[i+len(marker):]
	j := strings.Index(rest, "]")
	if j < 0 {
		return nil
	}
	var out []string
	for _, part := range strings.Split(rest[:j], ",") {
		if trimmed := strings.Trim(strings.TrimSpace(part), `"`); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
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
//
// It records the SendAs beside the message (memql#4821). The identity is a
// PARAMETER rather than a header on the message, so a fake that dropped it
// would leave every identity-resolution assertion reaching for a value it
// cannot see -- which is how "the campaign sends as the right mailbox"
// becomes untestable while looking covered.
type recordingSender struct {
	mu     sync.Mutex
	sent   []email.Message
	sentAs []email.SendAs
	fail   func(n int) error
}

func (s *recordingSender) Send(_ context.Context, msg email.Message, as email.SendAs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.sent)
	if s.fail != nil {
		if err := s.fail(n); err != nil {
			return err
		}
	}
	s.sent = append(s.sent, msg)
	s.sentAs = append(s.sentAs, as)
	return nil
}

// identities returns the SendAs each captured message went out under, in
// order.
func (s *recordingSender) identities() []email.SendAs {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]email.SendAs, len(s.sentAs))
	copy(out, s.sentAs)
	return out
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

// senderIdentityRow is one v1:campaigns:senderIdentity as the engine hands it
// back (memql#4821).
func senderIdentityRow(id, address, fromName, status string) map[string]any {
	return map[string]any{
		"id":          "v1:campaigns:senderIdentity:" + id,
		"ownerUserId": "v1:identity:user:" + testOwner,
		"address":     address,
		"fromName":    fromName,
		"status":      status,
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
		now:        time.Now,
		readyCh:    make(chan struct{}),
		doneCh:     make(chan struct{}),
		reputation: newReputationCollector("sender@example.test", "n1"),
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

// linkTokenIn pulls the token out of the first <prefix>?token=<token> URL in a
// body, decoded the way a mail client's request would carry it.
func linkTokenIn(t *testing.T, body, prefix string) string {
	t.Helper()
	marker := prefix + "?token="
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no %s link in the body:\n%s", prefix, body)
	}
	rest := body[i+len(marker):]
	if j := strings.IndexAny(rest, " \t\r\n\"'<>"); j >= 0 {
		rest = rest[:j]
	}
	token, err := url.QueryUnescape(rest)
	if err != nil {
		t.Fatalf("the token in %s does not decode: %v", prefix, err)
	}
	return token
}

// TestCampaignSendsCarryPreferencesAndViewLinks -- every campaign message can
// point at its preferences page and at itself in a browser, and the view link
// carries what was SENT, not what the template says by the time somebody
// clicks it: the template revision the job froze at queue time, and the names
// the footer and {{campaignName}} showed.
func TestCampaignSendsCarryPreferencesAndViewLinks(t *testing.T) {
	const revision = "2026-10-11T09:30:00.123456789Z"
	job := jobRow()
	job["templateSnapshot"] = map[string]any{
		"id": testTemplate, "subject": "Reviewed copy", "status": "ready", "revision": revision,
		"textBody": "Manage: {{preferencesUrl}}\nView: {{viewUrl}}\n",
		"htmlBody": `<p><a href="{{preferencesUrl}}">Manage</a> <a href="{{viewUrl}}">View</a></p>`,
	}
	campaign := campaignRow()
	campaign["fromName"] = "Acme News"
	// The template has moved on since the job was queued.
	edited := templateRow()
	edited["textBody"] = "Edited later"
	engine := &fakeEngine{
		jobs: []map[string]any{job}, campaign: campaign, template: edited,
		roster: []map[string]any{recipientRow("r-1", "reader@example.test", "subscribed")},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	w.DrainOnce(context.Background())

	if sender.count() != 1 {
		t.Fatalf("want one message, got %d", sender.count())
	}
	msg := sender.sent[0]
	keys := w.cfg.UnsubscribeKeys()
	now := time.Now()

	preferences, err := ParseLinkToken(keys, LinkPreferences, linkTokenIn(t, msg.TextBody, "https://example.test"+PreferencesPath), now)
	if err != nil {
		t.Fatalf("the preferences link does not verify for its own page: %v", err)
	}
	if preferences.OwnerUserID != testOwner || preferences.RecipientID != "r-1" || preferences.CampaignID != testCampaign ||
		preferences.EmailDigest != EmailDigest("reader@example.test") {
		t.Errorf("preferences payload = %+v", preferences)
	}

	view, err := ParseLinkToken(keys, LinkView, linkTokenIn(t, msg.TextBody, "https://example.test"+ViewPath), now)
	if err != nil {
		t.Fatalf("the view link does not verify for its own page: %v", err)
	}
	if view.RecipientID != "r-1" || view.EmailDigest != EmailDigest("reader@example.test") {
		t.Errorf("view payload names the wrong person: %+v", view)
	}
	if view.TemplateID != testTemplate || view.TemplateRevision != revision {
		t.Errorf("the view link carries template %q at %q, want the job's frozen snapshot %q at %q",
			view.TemplateID, view.TemplateRevision, testTemplate, revision)
	}
	if view.CampaignName != "August update" || view.SenderName != "Acme News" {
		t.Errorf("the view link carries campaign %q and sender %q, want what the message showed", view.CampaignName, view.SenderName)
	}

	// The same two links are in the HTML part, and none of them is tracked.
	hrefs := anchorHrefs(t, msg.HTMLBody)
	if len(hrefs) < 2 || !strings.Contains(hrefs[0], PreferencesPath) || !strings.Contains(hrefs[1], ViewPath) {
		t.Errorf("the HTML part's links = %v", hrefs)
	}
	// The footer names the sender, and a campaign send never asks for a confirm link.
	if !strings.Contains(msg.TextBody, "you subscribed to Acme News.") {
		t.Errorf("the footer does not name the sender: %s", msg.TextBody)
	}
	if strings.Contains(msg.TextBody, ConfirmPath) {
		t.Errorf("a campaign message carries a confirm link: %s", msg.TextBody)
	}
}

// TestTheViewLinkCarriesTheSenderTheFooterNamed -- the view page shows the
// message as it was sent, footer included, so the token carries the name the
// footer SHOWED: the sender's, else the organization's, else "this mailing
// list". Carrying the identity's name as it was stored would leave the page to
// guess when there was none, and print a different footer.
func TestTheViewLinkCarriesTheSenderTheFooterNamed(t *testing.T) {
	const organization = "client-a"
	for name, tc := range map[string]struct {
		campaignFromName string // the campaign's own override
		identityFromName string // the sending identity's, for an organization's campaign
		organizationName string // "" for a campaign that belongs to none
		want             string
	}{
		"the campaign's own name":                   {"Acme News", "", "", "Acme News"},
		"the identity's name over the organization": {"", "Acme Identity", "Acme Org", "Acme Identity"},
		"the organization when there is no sender":  {"", "", "Acme Org", "Acme Org"},
		"neither":                 {"", "", "", "this mailing list"},
		"a name that spans lines": {"Acme\n  News", "", "", "Acme News"},
	} {
		job := jobRow()
		job["templateSnapshot"] = map[string]any{
			"id": testTemplate, "subject": "Reviewed copy", "status": "ready", "revision": "2026-10-11T09:30:00.123456789Z",
			"textBody": "View: {{viewUrl}}\n", "htmlBody": `<p><a href="{{viewUrl}}">View</a></p>`,
		}
		campaign := campaignRow()
		campaign["fromName"] = tc.campaignFromName
		recipient := recipientRow("r-1", "reader@example.test", "subscribed")
		engine := &fakeEngine{jobs: []map[string]any{job}, campaign: campaign, template: templateRow(), roster: []map[string]any{recipient}}
		if tc.organizationName != "" {
			// An organization's campaign sends as one of its sender identities, and
			// every resource it touches belongs to the organization.
			job["templateSnapshot"].(map[string]any)["accountId"] = organization
			job["campaignAccountId"] = organization
			campaign["accountId"], campaign["senderIdentityId"] = organization, "sender"
			recipient["accountId"] = organization
			engine.senderIdentities = map[string]map[string]any{"sender": {
				"id": "sender", "accountId": organization, "address": "hello@client-a.test", "fromName": tc.identityFromName, "status": "active",
			}}
			engine.accounts = map[string]map[string]any{organization: {"name": tc.organizationName}}
			engine.audiences = map[string]map[string]any{testAudience: {"id": "v1:campaigns:audience:" + testAudience, "accountId": organization}}
		}
		sender := &recordingSender{}
		w := newTestWorker(t, engine, sender)

		w.DrainOnce(context.Background())

		if sender.count() != 1 {
			t.Errorf("%s: want one message, got %d", name, sender.count())
			continue
		}
		msg := sender.sent[0]
		view, err := ParseLinkToken(w.cfg.UnsubscribeKeys(), LinkView, linkTokenIn(t, msg.TextBody, "https://example.test"+ViewPath), time.Now())
		if err != nil {
			t.Errorf("%s: the view link does not verify: %v", name, err)
			continue
		}
		if view.SenderName != tc.want {
			t.Errorf("%s: the view link carries the sender %q, want %q", name, view.SenderName, tc.want)
		}
		if footer := "you subscribed to " + tc.want + "."; !strings.Contains(msg.TextBody, footer) {
			t.Errorf("%s: the footer does not say %q.\n%s", name, footer, msg.TextBody)
		}
	}
}

// TestAnOrganizationNameTooLongForTheViewLinkIsClippedNotDropped -- the footer
// can name the organization, and an organization's name has no cap of its own,
// unlike a sender's. The view link's token has a size limit, and a name that
// would break it must not cost the message its view link (a link that cannot be
// sealed is left out): the name the link carries is cut to the sender cap, and
// the footer still says all of it. Every "&" is six bytes in the token's JSON.
func TestAnOrganizationNameTooLongForTheViewLinkIsClippedNotDropped(t *testing.T) {
	const organization = "client-a"
	long := strings.Repeat("&", 800)
	job := jobRow()
	job["templateSnapshot"] = map[string]any{
		"id": testTemplate, "subject": "Reviewed copy", "status": "ready", "revision": "2026-10-11T09:30:00.123456789Z",
		"textBody": "View: {{viewUrl}}\n", "htmlBody": `<p><a href="{{viewUrl}}">View</a></p>`, "accountId": organization,
	}
	job["campaignAccountId"] = organization
	campaign := campaignRow()
	campaign["accountId"], campaign["senderIdentityId"] = organization, "sender"
	recipient := recipientRow("r-1", "reader@example.test", "subscribed")
	recipient["accountId"] = organization
	engine := &fakeEngine{jobs: []map[string]any{job}, campaign: campaign, template: templateRow(), roster: []map[string]any{recipient}}
	engine.senderIdentities = map[string]map[string]any{"sender": {
		"id": "sender", "accountId": organization, "address": "hello@client-a.test", "fromName": "", "status": "active",
	}}
	engine.accounts = map[string]map[string]any{organization: {"name": long}}
	engine.audiences = map[string]map[string]any{testAudience: {"id": "v1:campaigns:audience:" + testAudience, "accountId": organization}}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	w.DrainOnce(context.Background())

	if sender.count() != 1 {
		t.Fatalf("want one message, got %d", sender.count())
	}
	msg := sender.sent[0]
	if !strings.Contains(msg.TextBody, "you subscribed to "+long+".") {
		t.Errorf("the footer does not name the whole organization")
	}
	view, err := ParseLinkToken(w.cfg.UnsubscribeKeys(), LinkView, linkTokenIn(t, msg.TextBody, "https://example.test"+ViewPath), time.Now())
	if err != nil {
		t.Fatalf("the message has no view link that verifies: %v", err)
	}
	if want := strings.Repeat("&", maxSenderNameRunes); view.SenderName != want {
		t.Errorf("the view link carries a sender of %d characters, want it cut to %d", utf8.RuneCountInString(view.SenderName), maxSenderNameRunes)
	}
}

// TestACampaignWhoseTemplateCannotCarryItsFooterFailsWithAPlainReason -- a
// template whose HTML ends inside a comment or a tag, is a frameset, leaves an
// Outlook section open where the footer goes, or has a spot a client's parser
// would not show, cannot be sent: no message goes out, the job fails, and the
// reason the author reads on the campaign is the sentence that says what to fix,
// not a wrapper around it.
func TestACampaignWhoseTemplateCannotCarryItsFooterFailsWithAPlainReason(t *testing.T) {
	for name, tc := range map[string]struct{ html, want string }{
		"a comment that never ends":          {"<p>Hi</p><!--", wantEndsOpen},
		"a frameset":                         {"<frameset><frame src=x></frameset>", wantFrames},
		"an Outlook section left open":       {"<html><body><p>Hi</p><![if !mso]><p>modern</p></body></html>", wantOutlookOpen},
		"a comment only a tree builder sees": {"<html><body><p>Hi</p><noscript><!--</noscript></body></html>", wantEndsOpen},
		"an svg style that opens a comment":  {"<html><body><p>Hi</p><svg><style><!--</style></svg></body></html>", wantEndsOpen},
	} {
		template := templateRow()
		template["htmlBody"] = tc.html
		engine := &fakeEngine{
			jobs: []map[string]any{jobRow()}, campaign: campaignRow(), template: template,
			roster: []map[string]any{recipientRow("r-1", "reader@example.test", "subscribed")},
		}
		sender := &recordingSender{}
		w := newTestWorker(t, engine, sender)

		w.DrainOnce(context.Background())

		if sender.count() != 0 {
			t.Errorf("%s: a message went out", name)
		}
		failed := false
		for _, c := range engine.mutations("updateSendJob") {
			if argOf(c.query, "status") == "failed" {
				failed = true
				if got := argOf(c.query, "lastError"); got != tc.want {
					t.Errorf("%s: the job's reason is %q, want the sentence %q", name, got, tc.want)
				}
			}
		}
		if !failed {
			t.Errorf("%s: the job was not failed, so nobody learns why the campaign sent nothing", name)
		}
		// The campaign carries the reason too, and the test must not pass when it
		// does not: a write that never happened, or one with no reason, says nothing.
		onCampaign := false
		for _, c := range engine.mutations("updateCampaignProgress") {
			if argOf(c.query, "status") != "failed" {
				continue
			}
			onCampaign = true
			if got := argOf(c.query, "lastError"); got != tc.want {
				t.Errorf("%s: the campaign's reason is %q, want the sentence %q", name, got, tc.want)
			}
		}
		if !onCampaign {
			t.Errorf("%s: the campaign was not marked failed, so its author reads no reason there", name)
		}
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

// TestAFailedJobMarksItsCampaignFailed is the failure-side twin of
// TestCompletionStampsBothRows (memql#6022). A send job that went terminal
// `failed` used to leave its campaign reading `sending` for good: the operator,
// who reads the campaign and never the clusterOwner-tier job, saw a send still
// in flight behind a job nothing would ever drain again. So the failure and its
// reason land on the campaign too, under the campaign OWNER'S actor -- the same
// borrowed identity the completion write uses, and not the engine's own.
//
// The template read answering nothing is the cheapest real refusal that
// reaches failJob after the owner has been established.
func TestAFailedJobMarksItsCampaignFailed(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		// No template: templateById answers empty, which is the "template is
		// not readable" refusal.
		roster: []map[string]any{recipientRow("r-1", "a@example.test", "subscribed")},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	frozen := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	atClock(w, frozen)

	w.DrainOnce(context.Background())

	if sender.count() != 0 {
		t.Fatalf("sent %d messages for a job whose template could not be read", sender.count())
	}
	if !wroteContaining(engine, "mutation updateSendJob", `status: "failed"`) {
		t.Fatal("the job was not failed, so this test is not reaching the path it is about")
	}

	progress := engine.mutations("updateCampaignProgress")
	if len(progress) != 1 {
		t.Fatalf("want exactly one campaign write for the failed job, got %d: %+v", len(progress), progress)
	}
	// Asserted over the whole rendered call rather than through argOf: the
	// reason is a sentence, and the point is that the OPERATOR can read it.
	for _, want := range []string{
		`campaignId: "` + testCampaign + `"`,
		`status: "failed"`,
		`lastError: "template is not readable; refusing to send"`,
		`completedAt: "` + frozen.Format(time.RFC3339) + `"`,
	} {
		if !strings.Contains(progress[0].query, want) {
			t.Errorf("campaign write is missing %s:\n%s", want, progress[0].query)
		}
	}
	if progress[0].actorID != testOwner {
		t.Errorf("campaign failure written under actor %q, want the campaign owner %q -- an owned-tier "+
			"write under any other identity is refused or lands on a row the owner cannot read",
			progress[0].actorID, testOwner)
	}
	if progress[0].isOwner {
		t.Error("the campaign failure ran as a CLUSTER OWNER. The worker borrows exactly the campaign " +
			"owner's authority for owned-tier rows; anything more means the owner tier bounds nothing")
	}
}

// TestAJobWithNoOwnerFailsWithoutWritingTheCampaign is the other side of the
// same write. The campaign is an OWNED-tier row, so reaching it takes the
// owner's identity -- and a job that names no owner has none to borrow. The job
// still fails (that is the clusterOwner-tier row, written under the engine's own
// identity), but the worker must not fall through to writing the campaign as
// somebody else: ownerActorContext returns its context UNCHANGED for a blank
// owner, which is the engine's system identity, and that is exactly the
// attribution this refuses to make.
func TestAJobWithNoOwnerFailsWithoutWritingTheCampaign(t *testing.T) {
	job := jobRow()
	job["campaignOwnerUserId"] = ""
	engine := &fakeEngine{
		jobs:     []map[string]any{job},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "a@example.test", "subscribed")},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	w.DrainOnce(context.Background())

	if sender.count() != 0 {
		t.Fatalf("sent %d messages for a job with no owner to run the send under", sender.count())
	}
	if !wroteContaining(engine, "mutation updateSendJob", `status: "failed"`) {
		t.Fatal("a job with no campaign owner was not failed")
	}
	if progress := engine.mutations("updateCampaignProgress"); len(progress) != 0 {
		t.Errorf("the campaign was written for a job with no owner, under %q: %+v", progress[0].actorID, progress)
	}
}

// TestWalkAppliesTheResumeRule pins the resume rule through the walk that
// now owns it: never-attempted and due-retry recipients are batched, terminal
// ones are not, and roster order is preserved so a resumed send continues
// rather than re-shuffling.
func TestWalkAppliesTheResumeRule(t *testing.T) {
	now := time.Now()
	engine := &fakeEngine{
		roster: []map[string]any{
			recipientRow("a", "a@example.test", "subscribed"),
			recipientRow("b", "b@example.test", "subscribed"),
			recipientRow("c", "c@example.test", "subscribed"),
			recipientRow("d", "d@example.test", "subscribed"),
		},
		ledger: []map[string]any{
			ledgerRow("b", "sent", 1, time.Time{}),
			ledgerRow("c", "pending", 1, now.Add(-time.Minute)),
		},
	}
	w := newTestWorker(t, engine, &recordingSender{})

	walk, err := w.walkRoster(context.Background(), SendJob{AudienceID: testAudience, CampaignID: testCampaign}, now)
	if err != nil {
		t.Fatalf("walkRoster: %v", err)
	}
	if got := batchIDs(walk); strings.Join(got, ",") != "a,c,d" {
		t.Errorf("walk batched %v, want [a c d] in roster order", got)
	}
	if !walk.exhausted {
		t.Error("a single-page roster should have been exhausted in one walk")
	}

	w.cfg.BatchSize = 2
	limited, err := w.walkRoster(context.Background(), SendJob{AudienceID: testAudience, CampaignID: testCampaign}, now)
	if err != nil {
		t.Fatalf("walkRoster: %v", err)
	}
	if len(limited.batch) != 2 {
		t.Errorf("the walk ignored its batch limit: %d items", len(limited.batch))
	}
}

func batchIDs(walk rosterWalk) []string {
	out := make([]string, 0, len(walk.batch))
	for _, it := range walk.batch {
		out = append(out, it.recipient.ID)
	}
	return out
}

func ledgerRow(recipientID, status string, attempts int, next time.Time) map[string]any {
	row := map[string]any{
		"recipientId": "v1:campaigns:recipient:" + recipientID,
		"status":      status,
		"attempts":    attempts,
	}
	if !next.IsZero() {
		row["nextAttemptAt"] = next.UTC().Format(time.RFC3339)
	}
	return row
}

// cursorOf exposes the inbound pagination cursor to a fake engine.
func cursorOf(ctx context.Context) string { return memql.CursorFromContext(ctx) }

func (e *fakeEngine) OrganizationCapable(ctx context.Context, account, verb, resource string) bool {
	subject, ok := auth.SubjectFromContext(ctx)
	return ok && auth.CapableFor(ctx, subject, verb, resource)
}

func TestQueuedSendKeepsReviewedTemplateAfterAnEditorChange(t *testing.T) {
	job := jobRow()
	job["templateSnapshot"] = map[string]any{"id": testTemplate, "subject": "Reviewed copy", "textBody": "Approved message", "htmlBody": "<p>Approved message</p>", "status": "ready"}
	edited := templateRow()
	edited["subject"] = "Unfinished changes"
	edited["textBody"] = "Do not send"
	edited["status"] = "draft"
	engine := &fakeEngine{jobs: []map[string]any{job}, campaign: campaignRow(), template: edited, roster: []map[string]any{recipientRow("r-1", "recipient@example.test", "subscribed")}}
	sender := &recordingSender{}
	worker := newTestWorker(t, engine, sender)
	worker.DrainOnce(context.Background())
	if sender.count() != 1 {
		t.Fatalf("sent %d, expected frozen reviewed copy", sender.count())
	}
	if sender.sent[0].Subject != "Reviewed copy" || !strings.Contains(sender.sent[0].TextBody, "Approved message") {
		t.Fatalf("sent mutable template: %+v", sender.sent[0])
	}
}
