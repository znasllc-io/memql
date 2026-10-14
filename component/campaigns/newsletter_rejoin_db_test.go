package campaigns

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

func TestExpiredSignupStartsOneNewMembershipAcrossReplicas(t *testing.T) {
	f := newDigestFixture(t)
	n := f.newEmptyNewsletter(t, "rejoin")
	e2, db2 := digestDBEngine(t)
	second := newGatedWorker(t, e2, db2, n.sender)
	for _, w := range []*Worker{n.worker, second} {
		wired := NewWorker(w.store.engine, nil, nil, quietLogger(), gatePoolFor(t))
		w.journeyGate = wired.journeyGate
	}
	n.execute(t, n.owner, call("mutation", "setAudienceConfirmation", arg{"audienceId", n.audience}, arg{"confirmation", "required"}))
	binding, err := n.worker.newsletterRow(n.owner, "newsletterForSite", "siteId", n.suffix+"-site")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := n.worker.handleConfigureNewsletter(n.owner, map[string]any{"siteId": str(binding, "siteId"), "audienceId": n.audience, "consentText": str(binding, "consentText"), "enabled": true, "expectedRevision": templateTime(binding["createdAt"]).Format(time.RFC3339Nano)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	n.args["consentRevision"] = decodeResult(t, saved)["revision"]
	template := n.suffix + "-tpl"
	rows, err := n.store.rows(n.owner, call("query", "templateById", arg{"templateId", template}))
	if err != nil {
		t.Fatal(err)
	}
	if err = n.worker.newsletterWrite(n.owner, "updateTemplate", map[string]any{"templateId": template, "name": "Confirm", "subject": "Confirm", "textBody": "Confirm: {{confirmUrl}}", "htmlBody": "<p><a href=\"{{confirmUrl}}\">Confirm</a></p>", "status": "ready", "versionTime": memql.VersionTimeAfter(templateTime(rows[0]["createdAt"]), time.Now()).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	journeyID := n.suffix + "-journey"
	result, err := n.worker.handleSaveJourney(n.owner, map[string]any{"journeyId": journeyID, "accountId": n.org, "name": "Confirm membership", "trigger": map[string]any{"kind": "joined", "audienceId": n.audience}, "steps": []map[string]any{{"id": "confirm", "kind": "confirm", "templateId": template, "senderIdentityId": n.suffix + "-sender", "timeoutDays": 7}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.worker.handleSetJourneyState(n.owner, map[string]any{"journeyId": journeyID, "action": "activate", "expectedRevision": decodeResult(t, result)["revision"]}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatal(err)
	}
	member := func() Recipient {
		t.Helper()
		rows, err := n.store.Roster(memql.ContextWithFreshRead(n.owner), n.audience)
		if err != nil || len(rows) != 1 {
			t.Fatalf("roster %v %v", rows, err)
		}
		return rows[0]
	}
	first := member()
	run := func(w *Worker) {
		t.Helper()
		if err := w.runJourney(context.Background(), n.system, map[string]any{"id": journeyID}); err != nil {
			t.Fatal(err)
		}
	}
	run(n.worker)
	if n.sender.count() != 1 {
		t.Fatalf("confirmation sends=%d", n.sender.count())
	}
	future := time.Now().Add(8 * 24 * time.Hour)
	n.worker.now = func() time.Time { return future }
	second.now = n.worker.now
	run(second)
	id := journeyEnrollmentID(journeyID, first.ID, first.JoinedAt)
	expired, err := n.worker.newsletterRow(n.system, "journeyEnrollmentById", "enrollmentId", id)
	if err != nil || str(expired, "status") != "exited" || str(expired, "exitReason") != "not confirmed" {
		t.Fatalf("expiration %v %v", expired, err)
	}
	// A network retry of the original form never creates a renewed membership.
	if _, err = n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatal(err)
	}
	if !member().JoinedAt.Equal(first.JoinedAt) {
		t.Fatal("original submission renewed membership")
	}
	n.args["submissionId"] = "new-opt-in"
	var wg sync.WaitGroup
	for _, w := range []*Worker{n.worker, second} {
		wg.Add(1)
		go func(w *Worker) {
			defer wg.Done()
			if _, err := w.handleSubscribe(n.owner, n.args, 0); err != nil {
				t.Error(err)
			}
		}(w)
	}
	wg.Wait()
	renewed := member()
	if !renewed.JoinedAt.After(first.JoinedAt) || renewed.SubscriptionStatus != "pending" {
		t.Fatalf("renewed member %+v", renewed)
	}
	if _, err = n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatal(err)
	}
	if !member().JoinedAt.Equal(renewed.JoinedAt) {
		t.Fatal("retry renewed membership twice")
	}
	run(second)
	run(n.worker)
	if n.sender.count() != 2 {
		t.Fatalf("new membership confirmations=%d", n.sender.count())
	}
	if id == journeyEnrollmentID(journeyID, renewed.ID, renewed.JoinedAt) {
		t.Fatal("new membership reused old receipt")
	}
	// A later lapse does not authorize mailing an opted-out organization mailbox.
	future = future.Add(8 * 24 * time.Hour)
	run(second)
	if err = n.store.RecordOrganizationSuppression(n.owner, n.org, EmailDigest(renewed.Email), "unsubscribed", "example.test", "", ""); err != nil {
		t.Fatal(err)
	}
	n.args["submissionId"] = "after-opt-out"
	if _, err = n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatal(err)
	}
	if !member().JoinedAt.Equal(renewed.JoinedAt) {
		t.Fatal("suppressed mailbox renewed")
	}
}
