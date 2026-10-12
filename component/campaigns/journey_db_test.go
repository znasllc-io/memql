package campaigns

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

func TestJourneyAcrossIndependentReplicas(t *testing.T) {
	f := newDigestFixture(t)
	n := f.newEmptyNewsletter(t, "journey-hop")
	e2, db2 := digestDBEngine(t)
	second := newGatedWorker(t, e2, db2, n.sender)
	for _, w := range []*Worker{n.worker, second} {
		wired := NewWorker(w.store.engine, nil, nil, quietLogger(), gatePoolFor(t))
		w.journeyGate = wired.journeyGate
	}
	n.execute(t, n.owner, call("mutation", "setAudienceConfirmation", arg{"audienceId", n.audience}, arg{"confirmation", "required"}))
	// Both mail steps use one published confirmation-safe template, so the test
	// focuses on durable progression, independently of template authoring.
	template := n.suffix + "-tpl"
	rows, err := n.store.rows(n.owner, call("query", "templateById", arg{"templateId", template}))
	if err != nil || len(rows) != 1 {
		t.Fatalf("template: %v %v", rows, err)
	}
	if err = n.worker.newsletterWrite(n.owner, "updateTemplate", map[string]any{"templateId": template, "name": "Confirm", "subject": "Confirm your subscription", "textBody": "Confirm: {{confirmUrl}}", "htmlBody": "<p><a href=\"{{confirmUrl}}\">Confirm</a></p>", "status": "ready", "versionTime": memql.VersionTimeAfter(templateTime(rows[0]["createdAt"]), time.Now()).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"journeyId": n.suffix + "-journey", "accountId": n.org, "name": "Welcome sequence", "trigger": map[string]any{"kind": "joined", "audienceId": n.audience}, "steps": []map[string]any{
		{"id": "confirm", "kind": "confirm", "templateId": template, "senderIdentityId": n.suffix + "-sender", "timeoutDays": 7},
		{"id": "welcome", "kind": "send", "templateId": template, "senderIdentityId": n.suffix + "-sender"},
		{"id": "wait", "kind": "wait", "days": 3},
		{"id": "finish", "kind": "send", "templateId": template, "senderIdentityId": n.suffix + "-sender"},
	}}
	nodes, err := n.worker.handleSaveJourney(n.owner, args, 0)
	if err != nil {
		t.Fatal(err)
	}
	receipt := decodeResult(t, nodes)
	if issues := receipt["issues"].([]any); len(issues) != 0 {
		t.Fatalf("issues=%v", issues)
	}
	_, err = n.worker.handleSetJourneyState(n.owner, map[string]any{"journeyId": args["journeyId"], "action": "activate", "expectedRevision": receipt["revision"]}, 0)
	if err != nil {
		t.Fatal(err)
	}
	recipient := n.suffix + "-recipient"
	if err = n.store.AddRecipient(n.owner, recipient, n.audience, "journey@example.test", "Journey Person", "signup", n.org, "pending", nil); err != nil {
		t.Fatal(err)
	}
	race := func() {
		t.Helper()
		var wg sync.WaitGroup
		for _, w := range []*Worker{n.worker, second} {
			wg.Add(1)
			go func(w *Worker) {
				defer wg.Done()
				err := w.runJourney(context.Background(), auth.ContextWithInternalOrigin(w.systemActorContext(context.Background())), map[string]any{"id": args["journeyId"]})
				if err != nil {
					t.Error(err)
				}
			}(w)
		}
		wg.Wait()
	}
	enrollmentID := journeyEnrollmentID(fmt.Sprint(args["journeyId"]), recipient)
	enrollment := func() map[string]any {
		t.Helper()
		r, err := n.worker.newsletterRow(n.system, "journeyEnrollmentById", "enrollmentId", enrollmentID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	race()
	if got := enrollment(); str(got, "status") != "waiting" || n.sender.count() != 1 {
		t.Fatalf("first step=%v sends=%d", got, n.sender.count())
	}
	if err = n.store.ConfirmRecipient(n.owner, recipient, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := enrollment()
	if err = n.worker.newsletterWrite(n.system, "nudgeJourneyEnrollment", map[string]any{"enrollmentId": enrollmentID, "nextAt": time.Now().Format(time.RFC3339Nano), "versionTime": memql.VersionTimeAfter(templateTime(got["createdAt"]), time.Now()).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	// A tick performs one durable step per selected enrollment. Replaying each
	// tick on the other node still submits each intended email just once.
	race()
	race()
	race()
	if got := enrollment(); str(got, "stepId") != "finish" || n.sender.count() != 2 {
		t.Fatalf("after confirm/wait=%v sends=%d", got, n.sender.count())
	}
	future := time.Now().Add(4 * 24 * time.Hour)
	for _, w := range []*Worker{n.worker, second} {
		w.now = func() time.Time { return future }
	}
	race()
	race()
	if got := enrollment(); str(got, "status") != "completed" || n.sender.count() != 3 {
		t.Fatalf("completed=%v sends=%d", got, n.sender.count())
	}
	// The completed membership cannot enroll again after a fresh replica scan.
	race()
	if n.sender.count() != 3 {
		t.Fatal("completed enrollment resent")
	}
}
