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
	// Pressing "Subscribe again" a second time, or refreshing it, confirms no one.
	for i := 0; i < 2; i++ {
		if rr := pageRequest(h, "POST", PreferencesPath, token, "subscribe"); rr.Code != 200 || !strings.Contains(rr.Body.String(), "Check your inbox to confirm.") {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	if again := storedRecipient(t, n.store, n.owner, rid); str(again, "subscriptionStatus") != "pending" || !templateTime(again["joinedAt"]).Equal(templateTime(renewed["joinedAt"])) {
		t.Fatal("a replayed Subscribe again changed the membership", renewed, again)
	}
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

// A person who signed up and never confirmed, past the window to confirm, asks
// their preferences page for a new confirmation email. It starts a new pending
// membership, the audience's joined automation mails them a fresh confirmation,
// and asking again sends nothing more.
func TestAnExpiredSignUpGetsANewConfirmationEmailWhenItAsks(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "renew-expired", confirmFirst)
	a.drain(t)
	if a.azure.count() != 1 {
		t.Fatalf("the first confirmation: %d emails", a.azure.count())
	}
	rid := a.suffix + "-member"
	original := storedRecipient(t, a.store, a.owner, rid)
	// The confirmation is never pressed: the run gives up after its timeout, and
	// later the window to confirm closes.
	a.later(confirmLinkLifetime + time.Hour)
	a.drain(t)
	if row := a.enrollment(t, a.member); str(row, "status") != "exited" || str(row, "exitReason") != "not confirmed" {
		t.Fatalf("the unconfirmed run is %v", row)
	}
	page := NewEmailLinkHandler(deliveryDBEngine{a.engine}, a.worker.cfg, quietLogger(), gatePoolFor(t))
	page.now = func() time.Time { return a.now }
	token, err := MintLinkToken(a.worker.cfg.UnsubscribeSecret, LinkPreferences, LinkPayload{OwnerUserID: a.user, RecipientID: rid, AccountID: a.org, EmailDigest: EmailDigest("member@example.test")})
	if err != nil {
		t.Fatal(err)
	}
	if body := pageRequest(page, "GET", PreferencesPath, token, "").Body.String(); !strings.Contains(body, "Send me a new confirmation email") {
		t.Fatal(body)
	}
	if rr := pageRequest(page, "POST", PreferencesPath, token, "renew"); rr.Code != 200 || !strings.Contains(rr.Body.String(), "Check your inbox to confirm.") {
		t.Fatal(rr.Code, rr.Body.String())
	}
	renewed := storedRecipient(t, a.store, a.owner, rid)
	if str(renewed, "subscriptionStatus") != "pending" || !templateTime(renewed["joinedAt"]).After(templateTime(original["joinedAt"])) {
		t.Fatalf("asking for a new confirmation left %v, want a new pending membership after %v", renewed, original["joinedAt"])
	}
	// Asking again, or reloading what was posted, writes nothing.
	if rr := pageRequest(page, "POST", PreferencesPath, token, "renew"); rr.Code != 200 || !strings.Contains(rr.Body.String(), "Check your inbox to confirm.") {
		t.Fatal(rr.Code, rr.Body.String())
	}
	if again := storedRecipient(t, a.store, a.owner, rid); !templateTime(again["joinedAt"]).Equal(templateTime(renewed["joinedAt"])) {
		t.Fatalf("a second press restarted the membership again: %v, then %v", renewed["joinedAt"], again["joinedAt"])
	}
	a.later(time.Minute)
	a.drain(t)
	a.drain(t)
	if a.azure.count() != 2 {
		t.Fatalf("emails sent = %d, want the first confirmation and one new one", a.azure.count())
	}
	fresh := a.enrollment(t, journeyEnrollmentID(a.journeyID, rid, templateTime(renewed["joinedAt"])))
	if str(fresh, "status") != "waiting" || str(fresh, "stepId") != "confirm" {
		t.Fatalf("the new membership's run is %v, want it waiting for the new confirmation", fresh)
	}
}
