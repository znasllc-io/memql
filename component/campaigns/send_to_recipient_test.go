package campaigns

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
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

// TestSendToRecipientReadsTheRecipientByIdAndNeverScans pins the shape the
// bounded scan was replaced with.
//
// The scan was a correct answer that got slower as an operator succeeded, and
// refused past its bound at exactly the point somebody had enough audiences
// to care. A regression to it would still PASS every other test in this file
// -- the recipient is found either way -- so the assertion has to be about
// which reads are issued.
func TestSendToRecipientReadsTheRecipientByIdAndNeverScans(t *testing.T) {
	engine := recipientSendEngine()
	w := newTestWorker(t, engine, &recordingSender{})

	if _, err := w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0); err != nil {
		t.Fatalf("sendToRecipient: %v", err)
	}
	if len(callsWithPrefix(engine, "query recipientById")) != 1 {
		t.Errorf("the recipient was not read by id exactly once.\ncalls:\n%s",
			strings.Join(callsWithPrefix(engine, "query "), "\n"))
	}
	for _, scan := range []string{"query audiences(", "query emailRuleById", "query audienceRosterForSend"} {
		if len(callsWithPrefix(engine, scan)) != 0 {
			t.Errorf("%s was issued. Nothing on this path needs the audience itself any more: the "+
				"audience was a SEARCH KEY for the recipient, never a check, and the query's own tier "+
				"conjunct is what gates the read", scan)
		}
	}
}

// TestARecipientTheCallerDoesNotOwnIsNotFound is the reason an unscoped by-id
// read is safe to expose at all.
//
// The composite tier decides it INSIDE the query, so the row simply does not
// come back -- the same answer campaignById gives. The refusal must not
// distinguish "no such row" from "not yours" either, or the builtin becomes
// an existence oracle over every operator's recipients.
func TestARecipientTheCallerDoesNotOwnIsNotFound(t *testing.T) {
	engine := recipientSendEngine()
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)

	// A different, perfectly valid caller. The recipient row exists and is
	// readable by its owner; this caller is not its owner.
	stranger := auth.ContextWithUserActor(context.Background(), "user-stranger")
	_, err := w.handleSendToRecipient(stranger, sendToRecipientArgs(), 0)
	if err == nil {
		t.Fatal("a recipient in an audience the caller does not own was mailed")
	}
	if !strings.Contains(err.Error(), "not readable") {
		t.Errorf("the refusal reads %v; it must be the same sentence a genuinely absent recipient gets", err)
	}
	if sender.count() != 0 {
		t.Error("a message went out")
	}
	if len(callsWithPrefix(engine, "query audiences(")) != 0 {
		t.Error("the handler fell back to scanning audiences when the by-id read came back empty. " +
			"Not-found and not-yours are one answer here, and neither is a reason to go looking")
	}
}

func TestSendToRecipientRefusesAnUnresolvableRecipient(t *testing.T) {
	engine := recipientSendEngine()
	engine.roster = nil
	w := newTestWorker(t, engine, &recordingSender{})

	if _, err := w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0); err == nil {
		t.Fatal("a recipient that does not exist was accepted")
	}
}

// TestARuleDrivenSendStampsTheRuleOnTheLedgerRow is the first half of
// memql#4829's ledger question. `emailRuleId` was threaded as far as the
// builtin's REPLY and no further, so "which rule mailed this person" had an
// answer for the length of one response.
func TestARuleDrivenSendStampsTheRuleOnTheLedgerRow(t *testing.T) {
	engine := recipientSendEngine()
	w := newTestWorker(t, engine, &recordingSender{})

	if _, err := w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0); err != nil {
		t.Fatalf("sendToRecipient: %v", err)
	}
	if !wroteContaining(engine, "mutation recordCampaignDelivery", `emailRuleId: "`+testEmailRule+`"`) {
		t.Errorf("the delivery row does not name the rule that produced it.\ncalls:\n%s",
			strings.Join(callsWithPrefix(engine, "mutation recordCampaignDelivery"), "\n"))
	}
}

// TestASendWithNoRuleStampsNoRuleId is the same question from the other side.
// A send made by hand has no rule behind it, and a stamped one would be a
// false attribution no reader could detect.
func TestASendWithNoRuleStampsNoRuleId(t *testing.T) {
	engine := recipientSendEngine()
	w := newTestWorker(t, engine, &recordingSender{})

	args := sendToRecipientArgs()
	delete(args, "emailRuleId")
	if _, err := w.handleSendToRecipient(importCtx(), args, 0); err != nil {
		t.Fatalf("sendToRecipient: %v", err)
	}
	for _, q := range callsWithPrefix(engine, "mutation recordCampaignDelivery") {
		if strings.Contains(q, "emailRuleId:") {
			t.Errorf("a send with no rule stamped one anyway: %s", q)
		}
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

// TestAnOrdinaryCampaignDeliveryCarriesNoRuleId is the half that catches a
// stamp applied too widely.
//
// It drives the DRAIN WORKER -- a plain campaign send, the overwhelming
// majority of every delivery row this engine will ever write -- and asserts
// the field is absent. A stamp that leaked here would attribute thousands of
// campaign messages to whichever rule id happened to be in scope, and the
// only reader of `emailRuleId` is somebody asking a question it would then
// answer confidently and wrongly.
func TestAnOrdinaryCampaignDeliveryCarriesNoRuleId(t *testing.T) {
	engine := &fakeEngine{
		jobs:     []map[string]any{jobRow()},
		campaign: campaignRow(),
		template: templateRow(),
		roster:   []map[string]any{recipientRow("r-1", "person@example.test", "subscribed")},
	}
	w := newTestWorker(t, engine, &recordingSender{})
	w.DrainOnce(context.Background())

	written := callsWithPrefix(engine, "mutation recordCampaignDelivery")
	if len(written) == 0 {
		t.Fatal("the drain wrote no delivery row, so this test is checking nothing")
	}
	for _, q := range written {
		if strings.Contains(q, "emailRuleId:") {
			t.Errorf("an ordinary campaign delivery carries a rule id: %s", q)
		}
	}
}
