package campaigns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

type counterRepairEngine struct {
	fakeEngine
	fail    string
	pages   int
	onWrite func(context.Context, string) error
}

func (e *counterRepairEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "mutation ") && e.onWrite != nil {
		if err := e.onWrite(ctx, q); err != nil {
			return nil, err
		}
	}
	if e.fail != "" && strings.HasPrefix(q, e.fail) {
		return nil, errors.New("synthetic write unavailable")
	}
	switch {
	case strings.HasPrefix(q, "query providerDeliveryById"):
		for _, row := range e.ledger {
			if bare(str(row, "id")) == argOf(q, "deliveryId") {
				return rowsEnvelope([]map[string]any{row}), nil
			}
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query providerCounterCampaignById"):
		return rowsEnvelope([]map[string]any{e.campaign}), nil
	case strings.HasPrefix(q, "query providerCountersToRepair"):
		var pending []map[string]any
		for _, r := range e.ledger {
			if boolean(r, "providerCountersPending") {
				pending = append(pending, r)
			}
		}
		return rowsEnvelope(pending), nil
	case strings.HasPrefix(q, "query providerCounterLedger"):
		e.pages++
		// An intentionally smaller page than the ledger proves repair follows the
		// engine cursor instead of treating the first bounded page as the total.
		page := fakeEngine{roster: e.ledger, pageSize: 2}
		return page.rosterPage(memql.CursorFromContext(ctx)), nil
	case strings.HasPrefix(q, "mutation clearProviderCounterRepair"):
		for _, r := range e.ledger {
			if bare(str(r, "id")) == argOf(q, "deliveryId") {
				r["providerCountersPending"] = false
			}
		}
	}
	return e.fakeEngine.Execute(ctx, q)
}

func counterFixture(t *testing.T) (*Worker, *counterRepairEngine, map[string]any) {
	t.Helper()
	e := &counterRepairEngine{fakeEngine: fakeEngine{jobs: []map[string]any{jobRow()}, campaign: campaignRow()}}
	e.jobs[0]["status"] = "completed"
	e.jobs[0]["campaignAccountId"] = "account"
	e.jobs[0]["createdAt"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	e.campaign["accountId"] = "account"
	e.campaign["createdAt"] = time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	for i, status := range []string{"sent", "skipped", "failed", "sent", "sent"} {
		e.ledger = append(e.ledger, map[string]any{"id": fmt.Sprintf("delivery-%d", i), "campaignId": testCampaign, "accountId": "account", "ownerUserId": testOwner, "status": status, "providerCountersPending": i == 0, "createdAt": time.Now().UTC().Format(time.RFC3339Nano)})
	}
	w := newTestWorker(t, e, &recordingSender{})
	w.newsletterGate = func(context.Context, ...string) (gateHold, error) { return openGate{}, nil }
	return w, e, e.ledger[0]
}

func TestProviderCounterRepairPagesAndRetriesEveryWrite(t *testing.T) {
	for _, failure := range []string{"mutation updateSendJob", "mutation repairProviderCampaignCounters", "mutation clearProviderCounterRepair"} {
		t.Run(failure, func(t *testing.T) {
			w, e, row := counterFixture(t)
			e.fail = failure
			if err := w.repairProviderCampaign(context.Background(), row); err == nil {
				t.Fatal("failed write lost its retry")
			}
			if !boolean(row, "providerCountersPending") {
				t.Fatal("marker cleared before both rows were repaired")
			}
			e.fail = ""
			if err := w.repairProviderCampaign(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			if boolean(row, "providerCountersPending") {
				t.Fatal("repair was not acknowledged")
			}
			for _, name := range []string{"updateSendJob", "repairProviderCampaignCounters"} {
				calls := e.mutations(name)
				if len(calls) == 0 {
					t.Fatalf("no %s write", name)
				}
				q := calls[len(calls)-1].query
				for _, count := range []string{"sentCount: 3", "skippedCount: 1", "failedCount: 1"} {
					if !strings.Contains(q, count) {
						t.Fatalf("wrong totals: %s", q)
					}
				}
				prior := templateTime(e.jobs[0]["createdAt"])
				if name == "repairProviderCampaignCounters" {
					prior = templateTime(e.campaign["createdAt"])
				}
				if !templateTime(argOf(q, "versionTime")).After(prior) {
					t.Fatal("repair version did not advance past the peer's clock")
				}
			}
			if e.pages < 3 {
				t.Fatalf("only %d pages", e.pages)
			}
			before := len(e.mutations("repairProviderCampaignCounters"))
			if err := w.repairProviderCampaign(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			if len(e.mutations("repairProviderCampaignCounters")) != before {
				t.Fatal("replay rewrote acknowledged counters")
			}
		})
	}
}

func TestProviderCounterRepairWaitsForTerminalAndRejectsAttributionChanges(t *testing.T) {
	for _, status := range []string{"scheduled", "queued", "running", "paused"} {
		t.Run(status, func(t *testing.T) {
			w, e, row := counterFixture(t)
			e.jobs[0]["status"] = status
			if err := w.repairProviderCampaign(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			if len(e.mutations("updateSendJob")) != 0 || !boolean(row, "providerCountersPending") {
				t.Fatal("live job was repaired")
			}
		})
	}
	for _, field := range []string{"campaignAccountId", "campaignOwnerUserId", "campaignId"} {
		t.Run(field, func(t *testing.T) {
			w, e, row := counterFixture(t)
			e.jobs[0][field] = "another"
			if err := w.repairProviderCampaign(context.Background(), row); err == nil {
				t.Fatal("cross-attribution repair allowed")
			}
			if len(e.mutations("updateSendJob")) != 0 {
				t.Fatal("mismatched job was written")
			}
		})
	}
}

func TestStaleWorkerCannotOverwriteTerminalProviderRepair(t *testing.T) {
	w, e, _ := counterFixture(t)
	stale := sendJobFromRow(e.jobs[0])
	stale.Status = "running"
	stale.FailedCount = 5
	system := w.systemActorContext(context.Background())
	owner := w.ownerActorContext(system, testOwner)
	w.stampProgress(system, owner, stale)
	w.completeJob(system, owner, stale, Campaign{ID: testCampaign})
	if len(e.mutations("updateSendJob")) != 0 || len(e.mutations("updateCampaignProgress")) != 0 {
		t.Fatal("stale worker overwrote terminal repair")
	}
}

func TestProviderCounterRepairRefusesLostGate(t *testing.T) {
	w, e, row := counterFixture(t)
	w.newsletterGate = func(_ context.Context, keys ...string) (gateHold, error) {
		if len(keys) != 2 || keys[0] != campaignProgressLock(testCampaign) || keys[1] != providerLedgerLock(testCampaign) {
			t.Fatalf("repair did not acquire both locks in order on one connection: %v", keys)
		}
		return lostEmailGate{}, nil
	}
	if err := w.repairProviderCampaign(context.Background(), row); err == nil {
		t.Fatal("lost gate allowed counter repair")
	}
	if len(e.mutations("updateSendJob")) != 0 || !boolean(row, "providerCountersPending") {
		t.Fatal("lost gate wrote or acknowledged repair")
	}
}

type cancellationCounterGate struct{ ctx context.Context }

func (g cancellationCounterGate) Release() {}
func (g cancellationCounterGate) Verify(ctx context.Context) error {
	if err := g.ctx.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

func TestProviderCounterRepairKeepsGateThroughCancelledCommit(t *testing.T) {
	for _, boundary := range []string{"mutation updateSendJob", "mutation repairProviderCampaignCounters", "mutation clearProviderCounterRepair"} {
		t.Run(boundary, func(t *testing.T) {
			w, e, row := counterFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w.newsletterGate = func(ctx context.Context, _ ...string) (gateHold, error) { return cancellationCounterGate{ctx}, nil }
			canceled := false
			e.onWrite = func(commit context.Context, q string) error {
				if strings.HasPrefix(q, boundary) {
					cancel()
					canceled = true
				}
				return commit.Err()
			}
			if err := w.repairProviderCampaign(ctx, row); err != nil {
				t.Fatal(err)
			}
			if !canceled || boolean(row, "providerCountersPending") {
				t.Fatal("cancelled commit failed to finish under its held gate")
			}
		})
	}
}

func TestNormalCampaignCompletionKeepsGateThroughCancelledCommit(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			w, e, _ := counterFixture(t)
			e.jobs[0]["status"] = "running"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w.progressGate = func(ctx context.Context, _ string) (gateHold, error) { return cancellationCounterGate{ctx}, nil }
			e.onWrite = func(commit context.Context, q string) error {
				if strings.HasPrefix(q, "mutation updateSendJob") {
					cancel()
				}
				return commit.Err()
			}
			job := sendJobFromRow(e.jobs[0])
			job.SentCount = 3
			system := w.systemActorContext(ctx)
			owner := w.ownerActorContext(ctx, testOwner)
			if complete {
				w.completeJob(system, owner, job, Campaign{ID: testCampaign})
			} else {
				w.stampProgress(system, owner, job)
			}
			if len(e.mutations("updateSendJob")) != 1 || len(e.mutations("updateCampaignProgress")) != 1 {
				t.Fatal("cancellation lost a counter write")
			}
		})
	}
}

func TestLateWorkerDeliveryRefusesChangedOwner(t *testing.T) {
	for _, changed := range []string{"job", "delivery"} {
		t.Run(changed, func(t *testing.T) {
			w, e, row := counterFixture(t)
			row["id"] = deliveryRowID(testCampaign, "recipient")
			row["providerStatus"] = "delivered"
			if changed == "job" {
				e.jobs[0]["campaignOwnerUserId"] = "another"
			} else {
				row["ownerUserId"] = "another"
			}
			err := w.recordWorkerDelivery(w.ownerActorContext(context.Background(), testOwner), Delivery{CampaignID: testCampaign, RecipientID: "recipient", AccountID: "account", Status: "failed", Email: "recipient@example.test"})
			if err == nil {
				t.Fatal("same-organization ownership change was accepted")
			}
			if len(e.mutations("markProviderCounterRepair")) != 0 || len(e.mutations("recordCampaignDelivery")) != 0 {
				t.Fatal("ownership mismatch wrote ledger")
			}
		})
	}
}
