package campaigns

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCatalogRefusalUnconfiguredEmptyPasses(t *testing.T) {
	if got := CatalogRefusal(false, nil); got != "" {
		t.Fatalf("zero rows + unconfigured must pass; got %q", got)
	}
	if got := CatalogRefusal(false, []IndexProduct{}); got != "" {
		t.Fatalf("zero rows + unconfigured must pass; got %q", got)
	}
}

func TestCatalogRefusalFailsClosedWhenIndexInPlay(t *testing.T) {
	if got := CatalogRefusal(true, nil); got != ErrNoPurchasableProduct {
		t.Fatalf("configured + zero rows must refuse; got %q", got)
	}
	if got := CatalogRefusal(false, []IndexProduct{{Present: false, AvailableForSale: false}}); got != ErrNoPurchasableProduct {
		t.Fatalf("retired row means the index is in play; got %q", got)
	}
	if got := CatalogRefusal(true, []IndexProduct{{Present: true, AvailableForSale: false}}); got != ErrNoPurchasableProduct {
		t.Fatalf("unavailable catalog must refuse; got %q", got)
	}
}

func TestCatalogRefusalOneAvailablePasses(t *testing.T) {
	if got := CatalogRefusal(true, []IndexProduct{
		{Present: true, AvailableForSale: false},
		{Present: true, AvailableForSale: true},
	}); got != "" {
		t.Fatalf("one present + availableForSale must pass; got %q", got)
	}
}

func TestProductPurchasableEmptyCatalogIsNotPurchasable(t *testing.T) {
	if ProductPurchasable(nil) || ProductPurchasable([]IndexProduct{}) {
		t.Fatal("empty catalog is not purchasable (in-play check is CatalogRefusal)")
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

func TestShopifyIndexReadsPresentAndRetired(t *testing.T) {
	engine := &fakeEngine{
		shopifyProducts: []map[string]any{
			{"id": "gid://shopify/Product/1", "handle": "retired", "present": false, "availableForSale": false},
			{"id": "gid://shopify/Product/2", "handle": "hat", "present": true, "availableForSale": true},
		},
	}
	products, err := NewStore(engine).ShopifyIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(products) != 2 {
		t.Fatalf("want both present and retired rows, got %d", len(products))
	}
	if CatalogRefusal(false, products) != "" {
		t.Fatal("one available product must pass even when env is unconfigured")
	}
}

func TestStartSendPassesWhenIndexNotInPlay(t *testing.T) {
	engine := schedulingEngine()
	w := newTestWorker(t, engine, &recordingSender{})
	if _, err := w.handleStartSend(schedulingCtx(), map[string]any{"campaignId": testCampaign}, 0); err != nil {
		t.Fatalf("unconfigured + empty index must not block campaigns: %v", err)
	}
	if n := len(engine.mutations("enqueueCampaignSend")); n != 1 {
		t.Fatalf("want the send enqueued, got %d", n)
	}
}

func TestStartSendRefusesWhenConfiguredAndEmpty(t *testing.T) {
	engine := schedulingEngine()
	w := newTestWorker(t, engine, &recordingSender{})
	w.shopifyConfigured = func() bool { return true }

	_, err := w.handleStartSend(schedulingCtx(), map[string]any{"campaignId": testCampaign}, 0)
	if err == nil {
		t.Fatal("startSend accepted a configured empty catalog")
	}
	if !strings.Contains(err.Error(), "product_purchasable") {
		t.Fatalf("refusal must be operator-visible; got %q", err)
	}
	if n := len(engine.mutations("enqueueCampaignSend")); n != 0 {
		t.Fatalf("a job was enqueued (%d) despite the refusal", n)
	}
}

func TestScheduleSendRefusesWhenConfiguredAndEmpty(t *testing.T) {
	engine := schedulingEngine()
	w := newTestWorker(t, engine, &recordingSender{})
	atClock(w, time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC))
	w.shopifyConfigured = func() bool { return true }

	_, err := w.handleScheduleSend(schedulingCtx(), map[string]any{
		"campaignId":  testCampaign,
		"scheduledAt": "2026-08-14T09:00:00Z",
	}, 0)
	if err == nil {
		t.Fatal("scheduleSend accepted a configured empty catalog")
	}
	if !strings.Contains(err.Error(), "product_purchasable") {
		t.Fatalf("refusal must be operator-visible; got %q", err)
	}
}

func TestStartSendPassesWhenOneProductAvailable(t *testing.T) {
	engine := schedulingEngine()
	engine.shopifyProducts = []map[string]any{
		{"id": "gid://shopify/Product/1", "handle": "hat", "present": true, "availableForSale": true},
	}
	w := newTestWorker(t, engine, &recordingSender{})
	w.shopifyConfigured = func() bool { return true }
	if _, err := w.handleStartSend(schedulingCtx(), map[string]any{"campaignId": testCampaign}, 0); err != nil {
		t.Fatalf("startSend with a purchasable catalog: %v", err)
	}
	if n := len(engine.mutations("enqueueCampaignSend")); n != 1 {
		t.Fatalf("want the send enqueued, got %d", n)
	}
}

func TestFireTimeRefusesWhenConfiguredAndEmpty(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "keep@example.test", "subscribed")},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.shopifyConfigured = func() bool { return true }

	w.DrainOnce(context.Background())

	if got := sender.recipients(); len(got) != 0 {
		t.Fatalf("fire-time must not mail against an empty in-play catalog: mailed %v", got)
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
