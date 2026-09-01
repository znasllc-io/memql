package campaigns

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// stats_test.go -- the outcome breakdown (memql#4823, design D12).
//
// The two tests that matter most are about ABSENCE. `unique` is left out when
// the read behind it hit its bound, and `bounces.soft` is left out always --
// and in both cases the wrong answer is a ZERO, which is indistinguishable
// from a correct one at every surface downstream. Emitting a plausible number
// nobody can question is how the browser's page-capped counting
// under-reported every large campaign for a year.

// countingEngine answers `count` queries from a table keyed by a substring of
// the rendered call, and engagement-ref reads from a slice.
//
// Its own fixture rather than fakeEngine's, because what is being tested is
// which QUERIES the aggregation issues and what it does with their answers --
// a fake that served rows would make every bucket the same read.
type countingEngine struct {
	mu       sync.Mutex
	counts   map[string]int
	refs     map[string][]map[string]any
	campaign map[string]any
	roster   int
	queries  []string
}

func (e *countingEngine) Execute(_ context.Context, q string) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queries = append(e.queries, q)

	switch {
	case strings.HasPrefix(q, "query campaignById"):
		return rowsEnvelope([]map[string]any{e.campaign}), nil
	case strings.HasPrefix(q, "query audienceRosterSize"):
		return rowsEnvelope([]map[string]any{{"count": e.roster}}), nil
	case strings.HasPrefix(q, "query campaignEngagementRefs"):
		return rowsEnvelope(e.refs[argOf(q, "kind")]), nil
	}
	for needle, n := range e.counts {
		if strings.Contains(q, needle) {
			return rowsEnvelope([]map[string]any{{"count": n}}), nil
		}
	}
	return rowsEnvelope([]map[string]any{{"count": 0}}), nil
}

func statsFixture() *countingEngine {
	return &countingEngine{
		campaign: campaignRow(),
		roster:   100,
		counts: map[string]int{
			`campaignDeliveryCountByStatus(campaignId: "camp-1", status: "pending")`: 4,
			`campaignDeliveryCountByStatus(campaignId: "camp-1", status: "sent")`:    80,
			`campaignDeliveryCountByStatus(campaignId: "camp-1", status: "failed")`:  6,
			`campaignDeliveryCountByStatus(campaignId: "camp-1", status: "skipped")`: 10,
			// The two named skip buckets, plus an unnamed remainder.
			`campaignSkipCountByReason(campaignId: "camp-1", skipReasons: ["hard_bounce"`:   5,
			`campaignSkipCountByReason(campaignId: "camp-1", skipReasons: ["unsubscribed"]`: 3,
			`campaignConsentCountByKind(campaignId: "camp-1", kind: "bounce")`:              7,
			`campaignConsentCountByKind(campaignId: "camp-1", kind: "complaint")`:           2,
			`campaignConsentCountByKind(campaignId: "camp-1", kind: "withdraw")`:            9,
			`campaignEngagementCountByKind(campaignId: "camp-1", kind: "open")`:             40,
			`campaignEngagementCountByKind(campaignId: "camp-1", kind: "click")`:            12,
		},
		refs: map[string][]map[string]any{
			// Three hits from two deliveries: total 40 (from the count),
			// unique 2 (from the fold).
			"open": {
				{"deliveryId": "v1:campaigns:delivery:d-1"},
				{"deliveryId": "v1:campaigns:delivery:d-1"},
				{"deliveryId": "v1:campaigns:delivery:d-2"},
			},
			"click": {{"deliveryId": "v1:campaigns:delivery:d-1"}},
		},
	}
}

func runStats(t *testing.T, engine *countingEngine) map[string]any {
	t.Helper()
	w := newTestWorker(t, engine, &recordingSender{})
	nodes, err := w.handleStats(importCtx(), map[string]any{"campaignId": testCampaign}, 0)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	return decodeResult(t, nodes)
}

func TestStatsReportsEveryBucket(t *testing.T) {
	got := runStats(t, statsFixture())

	for key, want := range map[string]float64{
		"recipients":   100,
		"pending":      4,
		"sent":         80,
		"failed":       6,
		"complaints":   2,
		"unsubscribed": 9,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}

	skipped, _ := got["skipped"].(map[string]any)
	if skipped["total"] != float64(10) || skipped["suppressed"] != float64(5) ||
		skipped["unsubscribed"] != float64(3) || skipped["other"] != float64(2) {
		t.Errorf("skipped = %+v, want total 10 / suppressed 5 / unsubscribed 3 / other 2. `other` is a "+
			"REMAINDER taken from the same source as the total, so a skip reason nobody thought of lands "+
			"there visibly rather than vanishing", skipped)
	}

	bounces, _ := got["bounces"].(map[string]any)
	if bounces["hard"] != float64(7) {
		t.Errorf("bounces.hard = %v, want 7", bounces["hard"])
	}
	// SOFT BOUNCES ARE ABSENT, and that is the assertion.
	if _, present := bounces["soft"]; present {
		t.Error("bounces.soft is present. Nothing measures soft bounces per campaign -- a soft bounce is " +
			"transient, does not suppress, and is recorded against the sending identity's reputation " +
			"rather than the campaign. Emitting a zero would be a claim, and a reader cannot tell a " +
			"claim from a count")
	}

	opens, _ := got["opens"].(map[string]any)
	if opens["total"] != float64(40) {
		t.Errorf("opens.total = %v, want 40 (the exact count, not the length of the ref read)", opens["total"])
	}
	if opens["unique"] != float64(2) {
		t.Errorf("opens.unique = %v, want 2 -- three hits from two deliveries", opens["unique"])
	}
	clicks, _ := got["clicks"].(map[string]any)
	if clicks["total"] != float64(12) || clicks["unique"] != float64(1) {
		t.Errorf("clicks = %+v, want total 12 / unique 1", clicks)
	}
}

// TestUniqueIsUnmeasuredAtTheBound is the honesty assertion.
//
// A fold over a truncated page gives a unique count that is LOWER than the
// truth and indistinguishable from a correct one. Reporting it -- or
// reporting zero -- is the exact failure replacing the browser's page-capped
// counting was supposed to remove.
func TestUniqueIsUnmeasuredAtTheBound(t *testing.T) {
	engine := statsFixture()
	atBound := make([]map[string]any, engagementRefsBound)
	for i := range atBound {
		atBound[i] = map[string]any{"deliveryId": "v1:campaigns:delivery:d-1"}
	}
	engine.refs["open"] = atBound

	got := runStats(t, engine)
	opens, _ := got["opens"].(map[string]any)

	if _, present := opens["unique"]; present {
		t.Errorf("opens.unique = %v is present at the read's bound. It must be ABSENT, not zero and not "+
			"the truncated fold: both are numbers a client renders without question", opens["unique"])
	}
	if opens["uniqueUnmeasured"] != true {
		t.Error("nothing says WHY unique is missing, so a client cannot tell 'unmeasured' from 'the key " +
			"was not implemented'")
	}
	if opens["total"] != float64(40) {
		t.Errorf("opens.total = %v -- the TOTAL is an exact count and is unaffected by the ref read's bound", opens["total"])
	}
}

// TestEveryExactBucketIsACountQuery: the mechanism behind the numbers, so a
// future change that folds a bucket in Go from a bounded read fails here
// rather than silently under-reporting.
func TestEveryExactBucketIsACountQuery(t *testing.T) {
	engine := statsFixture()
	runStats(t, engine)

	engine.mu.Lock()
	defer engine.mu.Unlock()
	issued := strings.Join(engine.queries, "\n")
	for _, want := range []string{
		"query campaignDeliveryCountByStatus",
		"query campaignSkipCountByReason",
		"query campaignConsentCountByKind",
		"query campaignEngagementCountByKind",
		"query audienceRosterSize",
	} {
		if !strings.Contains(issued, want) {
			t.Errorf("%s was never issued. Every bucket that CAN be an exact count is one: a count is "+
				"exact at any audience size, while the length of a bounded read is a truncation wearing "+
				"a number's clothes.\nissued:\n%s", want, issued)
		}
	}
	// The ONE bounded read, and only one.
	if n := strings.Count(issued, "query campaignEngagementRefs"); n != 2 {
		t.Errorf("campaignEngagementRefs was issued %d times, want 2 (one per engagement kind)", n)
	}
	// deliveriesForCampaign is the page-capped read the browser used. It must
	// not appear here.
	if strings.Contains(issued, "query deliveriesForCampaign") {
		t.Error("the stats path reads the page-capped delivery list. That is exactly the client-side " +
			"counting this builtin replaces")
	}
}

func TestStatsRefusesACampaignTheCallerCannotRead(t *testing.T) {
	engine := statsFixture()
	engine.campaign = nil
	w := newTestWorker(t, engine, &recordingSender{})

	if _, err := w.handleStats(importCtx(), map[string]any{"campaignId": testCampaign}, 0); err == nil {
		t.Fatal("stats answered for a campaign the caller cannot read. The composite-tier read is the " +
			"authorization, exactly as it is for every other campaign-scoped builtin")
	}
}

func TestSkipReasonBucketsCoverBothEnums(t *testing.T) {
	// The worker writes the SUPPRESSION ROW's reason for a cluster-list skip
	// and the RECIPIENT ROW's subscription status for a per-audience one.
	// Those two enums overlap in meaning and not in spelling, and a list
	// carrying only one family files half the suppressions under "other".
	for _, want := range []string{"hard_bounce", "complaint", "manual", "bounced", "complained"} {
		found := false
		for _, have := range suppressedSkipReasons {
			if have == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not in suppressedSkipReasons", want)
		}
	}
	for _, r := range suppressedSkipReasons {
		if r == unsubscribedSkipReason {
			t.Errorf("%q is in BOTH the suppressed and the unsubscribed bucket, so the two would "+
				"double-count and `other` would go negative", r)
		}
	}
}

// TestSkippedCountReachesTheCampaignRow is the other half of design D12, and
// the smaller half only in lines of code.
//
// The worker has computed skippedCount per JOB since memql#3348 and had
// nowhere on the campaign to put it -- so the one number an operator most
// needs, the gap between the audience and what actually left, lived only on
// v1:campaigns:sendJob: a clusterOwner-tier row a browser cannot read.
// recipientCount minus sentCount minus this minus failedCount is what is
// still outstanding, and without it that subtraction has no third term.
func TestSkippedCountReachesTheCampaignRow(t *testing.T) {
	suppressed := "gone@example.test"
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster: []map[string]any{
			recipientRow("r-1", "keep@example.test", "subscribed"),
			recipientRow("r-2", suppressed, "subscribed"),
		},
		suppression: map[string]map[string]any{
			EmailDigest(suppressed): {"id": EmailDigest(suppressed), "reason": "hard_bounce"},
		},
	}
	w := newTestWorker(t, engine, &recordingSender{})
	w.DrainOnce(context.Background())

	if !wroteContaining(engine, "mutation updateSendJob", "skippedCount: 1") {
		t.Fatalf("the JOB did not carry the skip count.\ncalls:\n%s",
			strings.Join(callsWithPrefix(engine, "mutation updateSendJob"), "\n"))
	}
	if !wroteContaining(engine, "mutation updateCampaignProgress", "skippedCount: 1") {
		t.Errorf("the CAMPAIGN row did not carry the skip count. The two patches were deliberately "+
			"identical apart from this one field, which is how the number an operator most needs came "+
			"to be the one number their browser could not read.\ncalls:\n%s",
			strings.Join(callsWithPrefix(engine, "mutation updateCampaignProgress"), "\n"))
	}
}
