package campaigns

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

func TestJourneyPreflightFailuresStopWithoutSubmittingMail(t *testing.T) {
	f := newDigestFixture(t)
	n := f.newEmptyNewsletter(t, "journey-failure")
	wired := NewWorker(n.worker.store.engine, nil, nil, quietLogger(), gatePoolFor(t))
	n.worker.journeyGate = wired.journeyGate
	template := n.suffix + "-tpl"
	publish := func(status string) {
		t.Helper()
		rows, err := n.store.rows(memql.ContextWithFreshRead(n.owner), call("query", "templateById", arg{"templateId", template}))
		if err != nil || len(rows) != 1 {
			t.Fatalf("template %v %v", rows, err)
		}
		if err = n.worker.newsletterWrite(n.owner, "updateTemplate", map[string]any{"templateId": template, "name": "Welcome", "subject": "Welcome", "textBody": "Hello", "status": status, "versionTime": memql.VersionTimeAfter(templateTime(rows[0]["createdAt"]), time.Now()).Format(time.RFC3339Nano)}); err != nil {
			t.Fatal(err)
		}
	}
	publish("ready")
	journeyID := n.suffix + "-journey"
	nodes, err := n.worker.handleSaveJourney(n.owner, map[string]any{"journeyId": journeyID, "accountId": n.org, "name": "Welcome", "trigger": map[string]any{"kind": "joined", "audienceId": n.audience}, "steps": []map[string]any{{"id": "welcome", "kind": "send", "templateId": template, "senderIdentityId": n.suffix + "-sender"}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.worker.handleSetJourneyState(n.owner, map[string]any{"journeyId": journeyID, "action": "activate", "expectedRevision": decodeResult(t, nodes)["revision"]}, 0); err != nil {
		t.Fatal(err)
	}
	recipientID := n.suffix + "-recipient"
	if err = n.store.AddRecipient(n.owner, recipientID, n.audience, "failure@example.test", "Person", "signup", n.org, "subscribed", nil); err != nil {
		t.Fatal(err)
	}
	recipient, found, err := n.store.RecipientByID(memql.ContextWithFreshRead(n.owner), recipientID)
	if err != nil || !found {
		t.Fatalf("recipient %v %v", found, err)
	}
	// A resource can become unready after activation. Recheck it before each
	// delivery; the run must not submit an email from that now-draft template.
	publish("draft")
	now := time.Now()
	n.worker.now = func() time.Time { return now }
	enrollmentID := journeyEnrollmentID(journeyID, recipientID, recipient.JoinedAt)
	for attempt := 1; attempt <= journeyMaxAttempts; attempt++ {
		if err = n.worker.runJourney(context.Background(), n.system, map[string]any{"id": journeyID}); err != nil {
			t.Fatal(err)
		}
		row, err := n.worker.newsletterRow(n.system, "journeyEnrollmentById", "enrollmentId", enrollmentID)
		if err != nil {
			t.Fatal(err)
		}
		if integer(row, "attempts") != attempt || str(row, "lastError") == "" {
			t.Fatalf("attempt %d: %v", attempt, row)
		}
		if n.sender.count() != 0 {
			t.Fatal("unpublished template reached transport")
		}
		if attempt < journeyMaxAttempts {
			if str(row, "status") != "active" || !templateTime(row["nextAt"]).After(now) {
				t.Fatalf("retry not delayed: %v", row)
			}
		} else if str(row, "status") != "failed" {
			t.Fatalf("not terminal after retries: %v", row)
		}
		now = now.Add(24 * time.Hour)
	}
	journey, err := n.worker.newsletterRow(n.system, "journeyById", "journeyId", journeyID)
	if err != nil || str(journey, "lastError") == "" {
		t.Fatalf("journey error not exposed %v %v", journey, err)
	}
	if err = n.worker.runJourney(context.Background(), n.system, map[string]any{"id": journeyID}); err != nil {
		t.Fatal(err)
	}
	if n.sender.count() != 0 {
		t.Fatal("failed enrollment resumed automatically")
	}
}
