package campaigns

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// send_to_recipient_test.go -- the marketing lane's primitive (memql#4829,
// program P5).
//
// Every assertion here is about a property that makes marketing mail lawful,
// and each one is silent if it regresses: a suppressed address mailed anyway,
// a message with no unsubscribe pair, an outcome that never reached the
// ledger. None of them shows up at any surface afterwards.

const testEmailRule = "rule-1"

func recipientSendEngine() *fakeEngine {
	engine := &fakeEngine{
		template: map[string]any{
			"id":       "v1:campaigns:template:" + testTemplate,
			"subject":  "Welcome {{displayName}}",
			"textBody": "Hello {{displayName}}.",
			"status":   "ready",
		},
		roster: []map[string]any{
			recipientRow("r-1", "person@example.test", "subscribed"),
		},
		emailRules: map[string]map[string]any{
			testEmailRule: {
				"id":         "v1:campaigns:emailRule:" + testEmailRule,
				"audienceId": "v1:campaigns:audience:" + testAudience,
			},
		},
	}
	return engine
}

func sendToRecipientArgs() map[string]any {
	return map[string]any{
		"templateId":  testTemplate,
		"recipientId": "r-1",
		"emailRuleId": testEmailRule,
	}
}

func TestSendToRecipientMailsAndLedgersTheOutcome(t *testing.T) {
	engine := recipientSendEngine()
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	nodes, err := w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0)
	if err != nil {
		t.Fatalf("sendToRecipient: %v", err)
	}
	got := decodeResult(t, nodes)
	if got["sent"] != true {
		t.Fatalf("result = %+v, want sent", got)
	}
	if sender.count() != 1 {
		t.Fatalf("expected one message, got %d", sender.count())
	}
	msg := sender.sent[0]
	if msg.To != "person@example.test" {
		t.Errorf("To = %q", msg.To)
	}
	// THE UNSUBSCRIBE PAIR. This is the marketing lane; a message without it
	// is the thing the deployment refuses to emit.
	if msg.Headers[headerListUnsubscribe] == "" || msg.Headers[headerListUnsubscribePost] != oneClickValue {
		t.Errorf("the RFC 8058 header pair is missing: %+v", msg.Headers)
	}
	if !strings.Contains(msg.TextBody, "Unsubscribe:") {
		t.Error("the visible unsubscribe footer is missing")
	}
	// The outcome is LEDGERED, exactly as a campaign send's is.
	if !wroteContaining(engine, "mutation recordCampaignDelivery", `status: "sent"`) {
		t.Errorf("no delivery row was written.\ncalls:\n%s",
			strings.Join(callsWithPrefix(engine, "mutation recordCampaignDelivery"), "\n"))
	}
}

// TestSendToRecipientHonoursTheClusterSuppressionList is the point-of-send
// check. An address re-imported after a bounce has a recipient row saying
// `subscribed`, and the cluster list is what still refuses it.
func TestSendToRecipientHonoursTheClusterSuppressionList(t *testing.T) {
	engine := recipientSendEngine()
	engine.suppression = map[string]map[string]any{
		EmailDigest("person@example.test"): {"id": EmailDigest("person@example.test"), "reason": "hard_bounce"},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	nodes, err := w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0)
	if err != nil {
		t.Fatalf("sendToRecipient: %v", err)
	}
	got := decodeResult(t, nodes)
	if got["skipped"] != true || got["reason"] != "hard_bounce" {
		t.Errorf("result = %+v, want skipped for hard_bounce", got)
	}
	if sender.count() != 0 {
		t.Fatal("a suppressed address was mailed. The list is consulted at the POINT OF SEND precisely " +
			"because an audience assembled last month cannot know about last week's bounce")
	}
	if !wroteContaining(engine, "mutation recordCampaignDelivery", `status: "skipped"`) {
		t.Error("the skip was not ledgered; a skip is an outcome the operator is owed rather than a silence")
	}
}

// TestSendToRecipientRefusesWhenTheSuppressionListCannotBeRead: a failed
// lookup must NOT read as "not suppressed". A delayed message is recoverable;
// one sent to somebody who opted out is not.
func TestSendToRecipientRefusesWhenTheSuppressionListCannotBeRead(t *testing.T) {
	engine := &suppressionFailEngine{fakeEngine: recipientSendEngine()}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	if _, err := w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0); err == nil {
		t.Fatal("the send proceeded with the suppression list unread")
	}
	if sender.count() != 0 {
		t.Error("a message went out while the do-not-mail list was unreadable")
	}
}

// suppressionFailEngine makes exactly the suppression read fail.
type suppressionFailEngine struct{ *fakeEngine }

func (e *suppressionFailEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "query suppressionByDigest") {
		return nil, errors.New("engine not ready")
	}
	return e.fakeEngine.Execute(ctx, q)
}

func TestSendToRecipientSkipsAnUnsubscribedRecipient(t *testing.T) {
	engine := recipientSendEngine()
	engine.roster = []map[string]any{recipientRow("r-1", "person@example.test", "unsubscribed")}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	nodes, err := w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0)
	if err != nil {
		t.Fatalf("sendToRecipient: %v", err)
	}
	if got := decodeResult(t, nodes); got["skipped"] != true {
		t.Errorf("result = %+v, want skipped", got)
	}
	if sender.count() != 0 {
		t.Error("an unsubscribed recipient was mailed")
	}
}

func TestSendToRecipientAppliesTheResolvedIdentity(t *testing.T) {
	engine := recipientSendEngine()
	engine.senderIdentities = map[string]map[string]any{
		"si-acme": senderIdentityRow("si-acme", "news@acme.test", "Acme News", "active"),
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	args := sendToRecipientArgs()
	args["senderIdentityId"] = "si-acme"
	if _, err := w.handleSendToRecipient(importCtx(), args, 0); err != nil {
		t.Fatalf("sendToRecipient: %v", err)
	}
	if as := sender.identities()[0]; as.Address != "news@acme.test" {
		t.Errorf("the message went out as %q, want the named identity", as.Address)
	}
}

func TestSendToRecipientRefusesADisabledIdentity(t *testing.T) {
	engine := recipientSendEngine()
	engine.senderIdentities = map[string]map[string]any{
		"si-retired": senderIdentityRow("si-retired", "old@acme.test", "Acme", "disabled"),
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	args := sendToRecipientArgs()
	args["senderIdentityId"] = "si-retired"
	if _, err := w.handleSendToRecipient(importCtx(), args, 0); err == nil {
		t.Fatal("a send from a disabled identity was accepted")
	}
	if sender.count() != 0 {
		t.Error("a message went out anyway")
	}
}

// TestSendToRecipientResolvesTheAudienceFromTheRule is the production path:
// one extra read rather than a scan.
func TestSendToRecipientResolvesTheAudienceFromTheRule(t *testing.T) {
	engine := recipientSendEngine()
	w := newTestWorker(t, engine, &recordingSender{})

	if _, err := w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0); err != nil {
		t.Fatalf("sendToRecipient: %v", err)
	}
	if len(callsWithPrefix(engine, "query emailRuleById")) == 0 {
		t.Error("the rule's audience was not consulted")
	}
	if len(callsWithPrefix(engine, "query audiences(")) != 0 {
		t.Error("the fallback audience scan ran even though the rule named its audience. The scan is " +
			"the manual path and costs a roster walk per audience")
	}
}

func TestSendToRecipientRefusesAnUnresolvableRecipient(t *testing.T) {
	engine := recipientSendEngine()
	engine.roster = nil
	w := newTestWorker(t, engine, &recordingSender{})

	if _, err := w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0); err == nil {
		t.Fatal("a recipient that is in no readable audience was accepted")
	}
}

func TestSendToRecipientRequiresBothIds(t *testing.T) {
	w := newTestWorker(t, recipientSendEngine(), &recordingSender{})
	for _, args := range []map[string]any{
		{"recipientId": "r-1"},
		{"templateId": testTemplate},
		{},
	} {
		if _, err := w.handleSendToRecipient(importCtx(), args, 0); err == nil {
			t.Errorf("args %+v were accepted", args)
		}
	}
}

// TestSendToRecipientLedgersAFailure: a transport failure is an outcome the
// ledger has to carry, or "which of these people did we actually reach" has
// no answer.
func TestSendToRecipientLedgersAFailure(t *testing.T) {
	engine := recipientSendEngine()
	sender := &recordingSender{fail: func(int) error { return errors.New("provider refused") }}
	w := newTestWorker(t, engine, sender)

	if _, err := w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0); err == nil {
		t.Fatal("a refused send reported success")
	}
	if !wroteContaining(engine, "mutation recordCampaignDelivery", `status: "failed"`) {
		t.Errorf("the failure was not ledgered.\ncalls:\n%s",
			strings.Join(callsWithPrefix(engine, "mutation recordCampaignDelivery"), "\n"))
	}
}
