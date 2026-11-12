package campaigns

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// A terminal flag is not a quiescence barrier: an already submitted transport
// call can return on a different replica after the operator cancels the job.
func TestLateCampaignSendRequeuesTerminalCounterRepair(t *testing.T) {
	f := newDigestFixture(t)
	e2, db2 := digestDBEngine(t)
	for _, terminal := range []string{"cancelled", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			n := f.newEmptyNewsletter(t, "late-counter-"+terminal)
			other := newGatedWorker(t, e2, db2, n.sender)
			campaignID := n.suffix + "-campaign"
			n.execute(t, n.owner, call("mutation", "createCampaign", arg{"campaignId", campaignID}, arg{"name", "Late completion"}, arg{"audienceId", n.audience}, arg{"templateId", n.suffix + "-tpl"}, arg{"accountId", n.org}))
			if err := n.store.SetCampaignStatus(n.owner, "startCampaign", campaignID); err != nil {
				t.Fatal(err)
			}
			if err := n.store.EnqueueSend(n.system, SendJob{CampaignID: campaignID, CampaignOwnerUserID: n.user, CampaignAccountID: n.org, AudienceID: n.audience, TemplateID: n.suffix + "-tpl", Status: "running"}); err != nil {
				t.Fatal(err)
			}
			proof, late := n.suffix+"-proof", n.suffix+"-late"
			for _, id := range []string{proof, late} {
				n.execute(t, n.owner, call("mutation", "addRecipient", arg{"recipientId", id}, arg{"audienceId", n.audience}, arg{"accountId", n.org}, arg{"email", id + "@example.test"}))
			}
			if err := n.store.RecordDelivery(n.owner, Delivery{CampaignID: campaignID, RecipientID: proof, Email: proof + "@example.test", AccountID: n.org, Status: "failed", Attempts: 1}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			if err := n.worker.newsletterWrite(n.system, "recordProviderDelivery", map[string]any{"deliveryId": deliveryRowID(campaignID, proof), "providerStatus": "delivered", "providerAt": now.Format(time.RFC3339Nano), "sentAt": now.Format(time.RFC3339Nano), "providerCountersPending": true, "versionTime": now.Add(time.Microsecond).Format(time.RFC3339Nano)}); err != nil {
				t.Fatal(err)
			}
			campaign, found, err := other.store.CampaignByID(n.owner, campaignID)
			if err != nil || !found {
				t.Fatalf("campaign %v", err)
			}
			tmpl, found, err := other.store.TemplateByID(n.owner, n.suffix+"-tpl")
			if err != nil || !found {
				t.Fatalf("template %v", err)
			}
			job, found, err := other.store.JobByID(n.system, campaignID)
			if err != nil || !found {
				t.Fatalf("job %v", err)
			}
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			other.sendHook = func(context.Context, email.Sender, email.Message, email.SendAs) error {
				close(started)
				<-release
				return nil
			}
			identity := resolvedIdentity{SendAs: email.SendAs{AccountID: n.org, FromName: "Client"}, Label: "hello@example.test"}
			go func() {
				_, err := other.processRecipient(context.Background(), n.system, n.owner, &job, campaign, tmpl, identity, batchItem{recipient: Recipient{ID: late, Email: late + "@example.test", AccountID: n.org, SubscriptionStatus: "subscribed"}})
				done <- err
			}()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("send did not reach transport: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("send did not start")
			}
			if err := n.store.UpdateJob(n.system, campaignID, SendJobPatch{Status: &terminal}); err != nil {
				close(release)
				t.Fatal(err)
			}
			if err := n.store.UpdateCampaignProgress(n.owner, campaignID, CampaignProgress{Status: &terminal}); err != nil {
				close(release)
				t.Fatal(err)
			}
			selected := map[string]any{"campaignId": campaignID, "ownerUserId": n.user, "accountId": n.org}
			if err := n.worker.repairProviderCampaign(context.Background(), selected); err != nil {
				close(release)
				t.Fatal(err)
			}
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("late send did not finish")
			}
			lateRow, err := n.worker.newsletterRow(n.system, "providerDeliveryById", "deliveryId", deliveryRowID(campaignID, late))
			if err != nil || str(lateRow, "status") != "sent" {
				t.Fatalf("late row: %v %v", lateRow, err)
			}
			queue, err := n.worker.store.rows(n.system, call("query", "providerCounterLedger", arg{"campaignId", campaignID}, arg{"ownerUserId", n.user}, arg{"accountId", n.org}))
			if err != nil {
				t.Fatal(err)
			}
			pending := false
			for _, row := range queue {
				pending = pending || boolean(row, "providerCountersPending")
			}
			if !pending {
				t.Fatal("late send lost durable counter repair")
			}
			n.worker.repairProviderCounters(context.Background())
			// An old batch may also try to overwrite an already proven provider row.
			if err := other.recordWorkerDelivery(n.owner, Delivery{CampaignID: campaignID, RecipientID: proof, Email: proof + "@example.test", AccountID: n.org, Status: "failed", Attempts: 2}); err != nil {
				t.Fatal(err)
			}
			proven, err := n.worker.newsletterRow(n.system, "providerDeliveryById", "deliveryId", deliveryRowID(campaignID, proof))
			if err != nil || str(proven, "status") != "sent" || str(proven, "providerStatus") != "delivered" {
				t.Fatalf("provider proof overwritten: %v %v", proven, err)
			}
			n.worker.repairProviderCounters(context.Background())
			other.stampProgress(n.system, n.owner, job)
			fresh, found, err := n.store.JobByID(memql.ContextWithFreshRead(n.system), campaignID)
			if err != nil || !found || fresh.SentCount != 2 || fresh.FailedCount != 0 || fresh.Status != terminal {
				t.Fatalf("late repair: %+v %v", fresh, err)
			}
		})
	}
}
