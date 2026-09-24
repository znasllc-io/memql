package campaigns

import (
	"context"
	"strings"
	"testing"
)

func organizationSendEngine() *fakeEngine {
	e := recipientSendEngine()
	e.template["accountId"] = "client-a"
	e.roster[0]["accountId"] = "client-a"
	e.emailRules[testEmailRule]["accountId"] = "client-a"
	e.senderIdentities = map[string]map[string]any{"sender": {"id": "sender", "accountId": "client-a", "address": "hello@client-a.test", "fromName": "Client A", "status": "active"}}
	return e
}

func TestRecipientSendCannotMixOrganizationsEvenWhenEveryResourceIsReadable(t *testing.T) {
	for _, resource := range []string{"template", "recipient", "rule", "sender", "missing-rule", "default-sender"} {
		t.Run(resource, func(t *testing.T) {
			e := organizationSendEngine()
			args := sendToRecipientArgs()
			args["senderIdentityId"] = "sender"
			switch resource {
			case "template":
				e.template["accountId"] = "client-b"
			case "recipient":
				e.roster[0]["accountId"] = "client-b"
			case "rule":
				e.emailRules[testEmailRule]["accountId"] = "client-b"
			case "sender":
				e.senderIdentities["sender"]["accountId"] = "client-b"
			case "missing-rule":
				delete(e.emailRules, testEmailRule)
			case "default-sender":
				delete(args, "senderIdentityId")
			}
			sender := &recordingSender{}
			w := newTestWorker(t, e, sender)
			if _, err := w.handleSendToRecipient(importCtx(), args, 0); err == nil {
				t.Fatal("cross-organization send accepted")
			}
			if sender.count() != 0 {
				t.Fatal("a message was sent before ownership was checked")
			}
		})
	}
}

func TestRecipientSendPersistsOrganizationOnDelivery(t *testing.T) {
	e := organizationSendEngine()
	args := sendToRecipientArgs()
	args["senderIdentityId"] = "sender"
	sender := &recordingSender{}
	w := newTestWorker(t, e, sender)
	if _, err := w.handleSendToRecipient(importCtx(), args, 0); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 {
		t.Fatal("authorized organization send did not deliver")
	}
	for _, call := range e.calls {
		if strings.HasPrefix(call.query, "mutation recordCampaignDelivery") && call.actorID != testOwner {
			t.Fatalf("delivery lost the initiating owner: %q", call.actorID)
		}
	}
	if !wroteContaining(e, "mutation recordCampaignDelivery", `accountId: "client-a"`) {
		t.Fatalf("delivery lost its organization: %s", strings.Join(callsWithPrefix(e, "mutation recordCampaignDelivery"), "\n"))
	}
}

func TestCampaignSenderAndTemplateCannotCrossOrganization(t *testing.T) {
	w := newTestWorker(t, organizationSendEngine(), &recordingSender{})
	if err := w.validateCampaignOrganization(context.Background(), Campaign{AccountID: "client-a"}, Template{AccountID: "client-b"}); err == nil {
		t.Fatal("foreign template accepted")
	}
	if _, refusal := w.resolveSendIdentity(context.Background(), Campaign{AccountID: "client-b", SenderIdentityID: "sender"}); !refusal.refused() {
		t.Fatal("foreign sender accepted")
	}
	if _, refusal := w.resolveSendIdentity(context.Background(), Campaign{AccountID: "client-a"}); !refusal.refused() {
		t.Fatal("client sent as cluster default")
	}
	if _, refusal := w.resolveSendIdentity(context.Background(), Campaign{AccountID: "self"}); refusal.refused() {
		t.Fatal(refusal.Reason)
	}
}

func TestWorkerRefusesRecipientFromAnotherOrganizationBeforeMail(t *testing.T) {
	sender := &recordingSender{}
	w := newTestWorker(t, organizationSendEngine(), sender)
	stop, err := w.processRecipient(context.Background(), context.Background(), importCtx(), &SendJob{}, Campaign{AccountID: "client-a"}, Template{AccountID: "client-a"}, resolvedIdentity{}, batchItem{recipient: Recipient{AccountID: "client-b", Email: "someone@example.test"}})
	if !stop || err == nil || sender.count() != 0 {
		t.Fatal("worker did not refuse foreign recipient before delivering")
	}
}

// Reads remain available while action permission is revoked. This reproduces
// the important boundary: read authorization must never suffice to send mail.
type readOnlyOrganizationEngine struct {
	*fakeEngine
	checkedAccounts []string
}

func (e *readOnlyOrganizationEngine) OrganizationCapable(_ context.Context, account, _, _ string) bool {
	e.checkedAccounts = append(e.checkedAccounts, account)
	return false
}

func TestReadableOrganizationCannotSendWithoutCurrentWriteAuthority(t *testing.T) {
	for _, action := range []string{"test", "single-recipient", "preflight", "queued-recipient"} {
		t.Run(action, func(t *testing.T) {
			base := organizationSendEngine()
			base.campaign = campaignRow()
			base.campaign["accountId"] = "client-a"
			engine := &readOnlyOrganizationEngine{fakeEngine: base}
			sender := &recordingSender{}
			w := newTestWorker(t, engine, sender)
			var err error
			switch action {
			case "test":
				_, err = w.handleTestSend(importCtx(), map[string]any{"campaignId": "campaign", "to": "test@example.test"}, 0)
			case "single-recipient":
				_, err = w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0)
			case "preflight":
				_, err = w.preflight(importCtx(), "startSend", Campaign{AccountID: "client-a"})
			case "queued-recipient":
				var stop bool
				stop, err = w.processRecipient(context.Background(), context.Background(), importCtx(), &SendJob{}, Campaign{AccountID: "client-a"}, Template{AccountID: "client-a"}, resolvedIdentity{}, batchItem{recipient: Recipient{AccountID: "client-a", Email: "test@example.test"}})
				if !stop {
					t.Fatal("worker continued after write authority was revoked")
				}
			}
			if err == nil || !strings.Contains(err.Error(), "write permission") {
				t.Fatalf("expected current write permission refusal, got %v", err)
			}
			if sender.count() != 0 {
				t.Fatal("mail sent without write permission")
			}
			if len(engine.checkedAccounts) != 1 || engine.checkedAccounts[0] != "client-a" {
				t.Fatalf("action checked the wrong organization: %v", engine.checkedAccounts)
			}
		})
	}
}
