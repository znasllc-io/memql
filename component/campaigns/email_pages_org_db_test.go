package campaigns

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// In production an organization and its brand kit are often created by someone
// other than the person who owns its automations: an operator, a colleague, a
// package deploy. A page reached from an email borrows the automation owner's
// identity, which is a bare user with no rank, and the account concept shows an
// account only to its owner, an admin or the cluster owner. The page must still
// name the organization its verified token names, and show that organization's
// brand.
func TestEmailPagesShowAnOrganizationTheAutomationOwnerDidNotCreate(t *testing.T) {
	f := newDigestFixture(t)
	n := f.newEmptyNewsletter(t, "page-org")

	operatorID := n.suffix + "-operator"
	n.execute(t, n.system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Operator","role":"owner","active":true})`, langparser.QuoteString(operatorID), langparser.QuoteString(operatorID+"@example.test")))
	operator := digestOwner(operatorID)

	org, audience := n.suffix+"-operated-org", n.suffix+"-operated-aud"
	n.execute(t, operator, call("mutation", "createClientAccount", arg{"accountId", org}, arg{"name", "Brightside"}))
	n.worker.brandGate = NewWorker(n.worker.store.engine, nil, nil, quietLogger(), gatePoolFor(t)).brandGate
	if _, err := n.worker.handleSaveBrandKit(operator, map[string]any{"accountId": org, "kit": testBrandKit(t), "expectedRevision": ""}, 0); err != nil {
		t.Fatal(err)
	}

	// The automation owner works in that organization: its audience and its
	// people are theirs.
	n.execute(t, n.owner, call("mutation", "createAudience", arg{"audienceId", audience}, arg{"accountId", org}, arg{"name", "Newsletter"}))
	rid, address := n.suffix+"-member", "member@example.test"
	if err := n.store.AddRecipient(n.owner, rid, audience, address, "Reader", "manual", org, "subscribed", nil); err != nil {
		t.Fatal(err)
	}

	h := NewEmailLinkHandler(deliveryDBEngine{n.engine}, n.worker.cfg, quietLogger(), gatePoolFor(t))
	token, err := MintLinkToken(n.worker.cfg.UnsubscribeSecret, LinkPreferences, LinkPayload{OwnerUserID: n.user, RecipientID: rid, AccountID: org, EmailDigest: EmailDigest(address)})
	if err != nil {
		t.Fatal(err)
	}
	rr := pageRequest(h, "GET", PreferencesPath, token, "")
	body := rr.Body.String()
	if rr.Code != 200 {
		t.Fatalf("preferences page: %d %s", rr.Code, body)
	}
	if !strings.Contains(body, "<title>Brightside email preferences</title>") {
		t.Errorf("the page does not name the organization in its title:\n%s", body)
	}
	if !strings.Contains(body, `<img`) || !strings.Contains(body, `alt="Brightside"`) {
		t.Errorf("the page does not show the organization's logo:\n%s", body)
	}
	if !strings.Contains(body, "Unsubscribe from all Brightside emails") {
		t.Errorf("the unsubscribe button does not name the organization:\n%s", body)
	}
}

// An automation's email that names its organization ({{accountName}}) names it
// even when the automation's owner did not create that organization: a send
// borrows the owner's identity just as a page does.
func TestAnAutomationEmailNamesAnOrganizationItsOwnerDidNotCreate(t *testing.T) {
	f := newDigestFixture(t)
	n := f.newEmptyNewsletter(t, "send-org")
	n.worker.journeyGate = NewWorker(n.worker.store.engine, nil, nil, quietLogger(), gatePoolFor(t)).journeyGate

	operatorID := n.suffix + "-operator"
	n.execute(t, n.system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":"Operator","role":"owner","active":true})`, langparser.QuoteString(operatorID), langparser.QuoteString(operatorID+"@example.test")))
	operator := digestOwner(operatorID)
	org, audience := n.suffix+"-operated-org", n.suffix+"-operated-aud"
	n.execute(t, operator, call("mutation", "createClientAccount", arg{"accountId", org}, arg{"name", "Brightside"}))

	n.execute(t, n.owner, call("mutation", "createAudience", arg{"audienceId", audience}, arg{"accountId", org}, arg{"name", "Newsletter"}))
	sender := n.suffix + "-operated-sender"
	n.execute(t, n.owner, call("mutation", "createSenderIdentity", arg{"senderIdentityId", sender}, arg{"accountId", org}, arg{"address", "hello@brightside.test"}, arg{"fromName", "Brightside"}))
	template := n.suffix + "-operated-tpl"
	content := map[string]any{"subject": "Welcome", "textBody": "Welcome to {{accountName}}.", "htmlBody": "<p>Welcome to {{accountName}}.</p>"}
	if _, err := n.worker.handleSaveTemplate(n.owner, map[string]any{"templateId": template, "accountId": org, "name": "Welcome", "content": content}, 0); err != nil {
		t.Fatal(err)
	}
	rows, err := n.store.rows(n.owner, call("query", "templateById", arg{"templateId", template}))
	if err != nil || len(rows) != 1 {
		t.Fatalf("templateById: %d rows, %v", len(rows), err)
	}
	if err = n.worker.newsletterWrite(n.owner, "updateTemplate", map[string]any{"templateId": template, "name": "Welcome", "subject": "Welcome", "textBody": content["textBody"], "htmlBody": content["htmlBody"], "status": "ready", "versionTime": memql.VersionTimeAfter(templateTime(rows[0]["createdAt"]), time.Now()).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	journeyID := n.suffix + "-operated-journey"
	result, err := n.worker.handleSaveJourney(n.owner, map[string]any{"journeyId": journeyID, "accountId": org, "name": "Welcome", "trigger": map[string]any{"kind": "joined", "audienceId": audience}, "steps": []map[string]any{{"id": "welcome", "kind": "send", "templateId": template, "senderIdentityId": sender}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.worker.handleSetJourneyState(n.owner, map[string]any{"journeyId": journeyID, "action": "activate", "expectedRevision": decodeResult(t, result)["revision"]}, 0); err != nil {
		t.Fatal(err)
	}
	if err := n.store.AddRecipient(n.owner, n.suffix+"-member", audience, "member@example.test", "Reader", "manual", org, "subscribed", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := n.worker.runJourney(context.Background(), n.system, map[string]any{"id": journeyID}); err != nil {
			t.Fatal(err)
		}
	}
	if n.sender.count() != 1 {
		t.Fatalf("emails sent = %d, want 1", n.sender.count())
	}
	n.sender.mu.Lock()
	text := n.sender.sent[0].TextBody
	n.sender.mu.Unlock()
	if !strings.Contains(text, "Welcome to Brightside.") {
		t.Fatalf("the email does not name its organization: %q", text)
	}
}
