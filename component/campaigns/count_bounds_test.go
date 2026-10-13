package campaigns

import (
	"context"
	"github.com/znasllc-io/memql/component/memql"
	"strings"
	"testing"
)

type boundedStatsEngine struct{ *countingEngine }

func (e boundedStatsEngine) Execute(ctx context.Context, q string) (any, error) {
	res, err := e.countingEngine.Execute(ctx, q)
	if err == nil && (strings.Contains(q, `status: "sent"`) || strings.Contains(q, `status: "skipped"`) || strings.Contains(q, `kind: "open"`)) {
		for _, row := range memql.MaterializeRows(res) {
			row["atLeast"] = true
		}
	}
	return res, err
}
func TestCampaignStatsPreservesPerFieldLowerBounds(t *testing.T) {
	e := boundedStatsEngine{statsFixture()}
	w := newTestWorker(t, e, &recordingSender{})
	nodes, err := w.handleStats(importCtx(), map[string]any{"campaignId": "camp-1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	result := decodeResult(t, nodes)
	bounds := result["atLeast"].(map[string]any)
	if bounds["sent"] != true || bounds["skipped.total"] != true || bounds["opens.total"] != true {
		t.Fatalf("bounds %v", bounds)
	}
	if _, ok := result["skipped"].(map[string]any)["other"]; ok {
		t.Fatal("subtracted lower bounds to invent an exact remainder")
	}
}

type boundedRosterEngine struct{ *fakeEngine }

func (e boundedRosterEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "query audienceRosterSize") {
		return rowsEnvelope([]map[string]any{{"count": 2, "atLeast": true}}), nil
	}
	return e.fakeEngine.Execute(ctx, q)
}
func TestRosterCapacityCountWalksAllPagesWhenAggregateIsBounded(t *testing.T) {
	base := &fakeEngine{pageSize: 2, roster: []map[string]any{recipientRow("r1", "one@example.test", "subscribed"), recipientRow("r2", "two@example.test", "subscribed"), recipientRow("r3", "three@example.test", "subscribed")}}
	n, err := NewStore(boundedRosterEngine{base}).RosterSize(context.Background(), "a1")
	if err != nil || n != 3 {
		t.Fatalf("count %d err %v", n, err)
	}
}

type boundedProviderStats struct {
	*countingEngine
	status string
}

func (e boundedProviderStats) Execute(ctx context.Context, q string) (any, error) {
	result, err := e.countingEngine.Execute(ctx, q)
	if err == nil && strings.Contains(q, `providerStatus: "`+e.status+`"`) {
		for _, row := range memql.MaterializeRows(result) {
			row["atLeast"] = true
		}
	}
	return result, err
}
func TestProviderDeliveryBoundsOmitUnconfirmedRemainder(t *testing.T) {
	for status, field := range map[string]string{"delivered": "delivery.confirmed", "bounced": "delivery.failed", "filtered-spam": "delivery.failed", "expanded": "delivery.expanded"} {
		w := newTestWorker(t, boundedProviderStats{statsFixture(), status}, &recordingSender{})
		nodes, err := w.handleStats(importCtx(), map[string]any{"campaignId": "camp-1"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		result := decodeResult(t, nodes)
		if result["atLeast"].(map[string]any)[field] != true {
			t.Fatalf("lost bound %v", result)
		}
		if _, present := result["delivery"].(map[string]any)["unconfirmed"]; present {
			t.Fatal("invented remainder from a lower bound")
		}
	}
}
