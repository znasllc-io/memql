package campaigns

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// A person who unsubscribed from a confirmation-required audience and comes back
// through their preferences page is a new, unconfirmed member: the audience's
// joined automation must actually send them a confirmation, and nothing else
// reaches them until they confirm.
func TestSubscribingAgainToAConfirmFirstAudienceSendsAConfirmation(t *testing.T) {
	f := newDigestFixture(t)
	n := f.newEmptyNewsletter(t, "resubscribe-confirm")
	wired := NewWorker(n.worker.store.engine, nil, nil, quietLogger(), gatePoolFor(t))
	n.worker.journeyGate = wired.journeyGate
	n.execute(t, n.owner, call("mutation", "setAudienceConfirmation", arg{"audienceId", n.audience}, arg{"confirmation", "required"}))

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

	rid, address := n.suffix+"-recipient", "comes-back@example.test"
	if err := n.store.AddRecipient(n.owner, rid, n.audience, address, "Reader", "manual", n.org, "subscribed", nil); err != nil {
		t.Fatal(err)
	}
	original := storedRecipient(t, n.store, n.owner, rid)
	h := NewEmailLinkHandler(deliveryDBEngine{n.engine}, n.worker.cfg, quietLogger(), gatePoolFor(t))
	token, err := MintLinkToken(n.worker.cfg.UnsubscribeSecret, LinkPreferences, LinkPayload{OwnerUserID: n.user, RecipientID: rid, AccountID: n.org, EmailDigest: EmailDigest(address)})
	if err != nil {
		t.Fatal(err)
	}
	if rr := pageRequest(h, "POST", PreferencesPath, token, "unsubscribe"); rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	rr := pageRequest(h, "POST", PreferencesPath, token, "subscribe")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Check your inbox to confirm.") || strings.Contains(rr.Body.String(), "You're subscribed.") {
		t.Fatal(rr.Code, rr.Body.String())
	}
	renewed := storedRecipient(t, n.store, n.owner, rid)
	if str(renewed, "subscriptionStatus") != "pending" || !templateTime(renewed["joinedAt"]).After(templateTime(original["joinedAt"])) {
		t.Fatal("subscribing again did not begin a new pending membership", original, renewed)
	}
	if _, blocked, err := n.store.SuppressionForSend(n.system, n.org, EmailDigest(address)); err != nil || blocked {
		t.Fatalf("the opt-out was not lifted: blocked=%v err=%v", blocked, err)
	}
	if n.sender.count() != 0 {
		t.Fatalf("mail went out before the person confirmed: %d", n.sender.count())
	}

	run := func() {
		t.Helper()
		if err := n.worker.runJourney(context.Background(), n.system, map[string]any{"id": journeyID}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	run()
	if n.sender.count() != 1 {
		t.Fatalf("confirmation emails sent = %d, want exactly one", n.sender.count())
	}
	n.sender.mu.Lock()
	text := n.sender.sent[0].TextBody
	n.sender.mu.Unlock()
	if !strings.Contains(text, ConfirmPath+"?token=") {
		t.Fatalf("the email carries no confirm link: %q", text)
	}
}
