package campaigns

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Campaigns have no catalog binding. Neither an empty store nor sold-out
// inventory elsewhere in the cluster can veto an organization's newsletter.
func TestCampaignSendsDoNotDependOnClusterShopifyInventory(t *testing.T) {
	for _, inventory := range []struct {
		name     string
		products []map[string]any
	}{
		{"empty", nil},
		{"sold-out", []map[string]any{{"deleted": false, "availableForSale": false}}},
	} {
		for _, operation := range []string{"start", "schedule", "scheduled-fire", "worker"} {
			t.Run(inventory.name+"/"+operation, func(t *testing.T) {
				engine := schedulingEngine()
				engine.shopifyStores = []map[string]any{{"id": "unrelated-store", "accountId": "another-client"}}
				engine.shopifyProducts = inventory.products
				sender := &recordingSender{}
				w := newTestWorker(t, engine, sender)
				var err error
				switch operation {
				case "start":
					_, err = w.handleStartSend(schedulingCtx(), map[string]any{"campaignId": testCampaign}, 0)
				case "schedule":
					atClock(w, time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC))
					_, err = w.handleScheduleSend(schedulingCtx(), map[string]any{"campaignId": testCampaign, "scheduledAt": "2026-08-14T09:00:00Z"}, 0)
				case "scheduled-fire":
					due := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
					engine.campaign = scheduledCampaignRow(due)
					engine.scheduledJobs = []map[string]any{scheduledJobRow(due)}
					atClock(w, due.Add(time.Minute))
					w.promoteDueSchedules(context.Background(), w.systemActorContext(context.Background()))
					updates := engine.mutations("updateSendJob")
					if len(updates) != 1 || argOf(updates[0].query, "status") != "queued" {
						t.Fatalf("scheduled campaign did not become runnable: %v", updates)
					}
				case "worker":
					// Reconstruct a worker from only the shared queued rows: it has
					// no originating request or caller-local setup state.
					engine.campaign["status"] = "sending"
					engine.jobs = []map[string]any{jobRow()}
					newTestWorker(t, engine, sender).DrainOnce(context.Background())
					if sender.count() != 1 {
						t.Fatalf("queued campaign did not send: %v", engine.mutations("updateSendJob"))
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if (operation == "start" || operation == "schedule") && len(engine.mutations("enqueueCampaignSend")) != 1 {
					t.Fatal("campaign was not enqueued")
				}
				for _, call := range engine.calls {
					if strings.HasPrefix(call.query, "query stores") || strings.HasPrefix(call.query, "query purchasableVariants") {
						t.Fatalf("unrelated Shopify inventory was consulted: %s", call.query)
					}
				}
			})
		}
	}
}

func TestCampaignWithoutCatalogStillRequiresReviewedTemplate(t *testing.T) {
	engine := schedulingEngine()
	engine.shopifyStores = []map[string]any{{"id": "empty-store"}}
	engine.template["status"] = "draft"
	w := newTestWorker(t, engine, &recordingSender{})
	_, err := w.handleStartSend(schedulingCtx(), map[string]any{"campaignId": testCampaign}, 0)
	if err == nil || !strings.Contains(err.Error(), "Mark it ready") {
		t.Fatalf("expected template review refusal, got %v", err)
	}
	if len(engine.mutations("enqueueCampaignSend")) != 0 {
		t.Fatal("draft template was enqueued")
	}
}
