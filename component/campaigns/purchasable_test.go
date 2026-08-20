package campaigns

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestProductPurchasableEmptyCatalogRefuses(t *testing.T) {
	if ProductPurchasable(nil) {
		t.Fatal("empty catalog must not be purchasable")
	}
	if ProductPurchasable([]IndexProduct{}) {
		t.Fatal("empty catalog must not be purchasable")
	}
}

func TestProductPurchasableUnavailableOrRetiredRefuses(t *testing.T) {
	if ProductPurchasable([]IndexProduct{{Present: true, AvailableForSale: false}}) {
		t.Fatal("present but not for sale must refuse")
	}
	if ProductPurchasable([]IndexProduct{{Present: false, AvailableForSale: true}}) {
		t.Fatal("retired row must not count, even if availableForSale is stale-true")
	}
}

func TestProductPurchasableOneAvailablePasses(t *testing.T) {
	if !ProductPurchasable([]IndexProduct{
		{Present: true, AvailableForSale: false},
		{Present: true, AvailableForSale: true},
	}) {
		t.Fatal("one present + availableForSale product must pass")
	}
}

func TestAnyPurchasableProductReadsTheIndex(t *testing.T) {
	engine := &fakeEngine{
		shopifyProducts: []map[string]any{
			{"id": "gid://shopify/Product/1", "handle": "retired", "present": false, "availableForSale": false},
			{"id": "gid://shopify/Product/2", "handle": "hat", "present": true, "availableForSale": true},
		},
	}
	ok, err := NewStore(engine).AnyPurchasableProduct(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("want purchasable when one present product is for sale")
	}

	engine.shopifyProducts = []map[string]any{
		{"id": "gid://shopify/Product/1", "handle": "gone", "present": true, "availableForSale": false},
	}
	ok, err = NewStore(engine).AnyPurchasableProduct(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("unavailable catalog must refuse")
	}
}

func TestStartSendRefusesEmptyCatalog(t *testing.T) {
	engine := schedulingEngine()
	w := newTestWorker(t, engine, &recordingSender{})
	w.catalogPurchasable = func(context.Context) (bool, error) { return false, nil }

	_, err := w.handleStartSend(schedulingCtx(), map[string]any{"campaignId": testCampaign}, 0)
	if err == nil {
		t.Fatal("startSend accepted an empty catalog")
	}
	if !strings.Contains(err.Error(), "product_purchasable") {
		t.Fatalf("refusal must be operator-visible; got %q", err)
	}
	if n := len(engine.mutations("enqueueCampaignSend")); n != 0 {
		t.Fatalf("a job was enqueued (%d) despite the refusal", n)
	}
}

func TestScheduleSendRefusesEmptyCatalog(t *testing.T) {
	engine := schedulingEngine()
	w := newTestWorker(t, engine, &recordingSender{})
	atClock(w, time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC))
	w.catalogPurchasable = func(context.Context) (bool, error) { return false, nil }

	_, err := w.handleScheduleSend(schedulingCtx(), map[string]any{
		"campaignId":  testCampaign,
		"scheduledAt": "2026-08-14T09:00:00Z",
	}, 0)
	if err == nil {
		t.Fatal("scheduleSend accepted an empty catalog")
	}
	if !strings.Contains(err.Error(), "product_purchasable") {
		t.Fatalf("refusal must be operator-visible; got %q", err)
	}
}

func TestStartSendPassesWhenOneProductAvailable(t *testing.T) {
	engine := schedulingEngine()
	w := newTestWorker(t, engine, &recordingSender{})
	if _, err := w.handleStartSend(schedulingCtx(), map[string]any{"campaignId": testCampaign}, 0); err != nil {
		t.Fatalf("startSend with a purchasable catalog: %v", err)
	}
	if n := len(engine.mutations("enqueueCampaignSend")); n != 1 {
		t.Fatalf("want the send enqueued, got %d", n)
	}
}

func TestFireTimeRefusesEmptyCatalog(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "keep@example.test", "subscribed")},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.catalogPurchasable = func(context.Context) (bool, error) { return false, nil }

	w.DrainOnce(context.Background())

	if got := sender.recipients(); len(got) != 0 {
		t.Fatalf("fire-time must not mail against an empty catalog: mailed %v", got)
	}
	failed := engine.mutations("updateSendJob")
	found := false
	for _, c := range failed {
		if strings.Contains(c.query, "failed") && strings.Contains(c.query, "product_purchasable") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("job must fail with a visible product_purchasable reason; writes=%v", queriesOf(failed))
	}
}

func queriesOf(calls []recordedCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.query)
	}
	return out
}
