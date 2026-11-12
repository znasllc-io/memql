package campaigns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// send_row_test.go -- the transactional row lane against the fake engine: what it
// refuses before it reads, what it holds before it asks the row's resolver, and
// what the message it sends does not carry.

// testRowConcept is a concept only these tests register. RegisterRowAddress is
// process-wide, so every registration here goes through registerTestRowAddress,
// which puts back what was there when the test ends.
const testRowConcept = "v1:test:rowLaneUnit"

// registerTestRowAddress registers resolve for concept for the length of t.
func registerTestRowAddress(t *testing.T, concept string, resolve func(context.Context, RowMessageRequest) (RowMessage, error)) {
	t.Helper()
	prior, had := rowAddressFor(concept)
	RegisterRowAddress(RowAddress{Concept: concept, Resolve: resolve})
	t.Cleanup(func() {
		rowAddresses.Lock()
		defer rowAddresses.Unlock()
		if had {
			rowAddresses.byConcept[concept] = prior
		} else {
			delete(rowAddresses.byConcept, concept)
		}
	})
}

// countingResolver answers one row and counts how often it is asked.
type countingResolver struct {
	mu     sync.Mutex
	calls  int
	asked  []RowMessageRequest
	answer func(n int) (RowMessage, error)
}

func (r *countingResolver) resolve(_ context.Context, req RowMessageRequest) (RowMessage, error) {
	r.mu.Lock()
	r.calls++
	r.asked = append(r.asked, req)
	n := r.calls
	r.mu.Unlock()
	return r.answer(n)
}

func (r *countingResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func answering(email, account string, fields map[string]string) *countingResolver {
	return &countingResolver{answer: func(int) (RowMessage, error) {
		return RowMessage{Email: email, AccountID: account, Fields: fields}, nil
	}}
}

// rowSendEngine is the order route's organization with a published
// transactional template.
func rowSendEngine() *fakeEngine {
	engine := orderSendEngine()
	engine.template["name"] = "Application received"
	engine.template["purpose"] = "transactional"
	engine.template["subject"] = "We have your application"
	engine.template["textBody"] = "Thank you, {{fields.companyName}}."
	engine.template["htmlBody"] = "<p>Thank you, {{fields.companyName}}.</p>"
	return engine
}

func rowArgs() map[string]any {
	return map[string]any{
		"rowConcept": testRowConcept, "rowId": "application-1", "accountId": "acct-1", "kind": "received",
		"templateId": testTemplate, "requestId": "lane-received-application-1-1", "senderIdentityId": "si-store",
	}
}

func rowWorker(t *testing.T, engine *fakeEngine, sender *recordingSender) *Worker {
	t.Helper()
	w := newTestWorker(t, engine, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	return w
}

func TestARowMessageCarriesNoListUnsubscribeHeaderAndNoFooter(t *testing.T) {
	engine := rowSendEngine()
	sender := &recordingSender{}
	w := rowWorker(t, engine, sender)
	resolver := answering("applicant@example.com", "acct-1", map[string]string{"companyName": "Acme Widgets"})
	registerTestRowAddress(t, testRowConcept, resolver.resolve)

	nodes, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
	if err != nil {
		t.Fatalf("sendRowMessage: %v", err)
	}
	got := decodeResult(t, nodes)
	if got["sent"] != true || sender.count() != 1 {
		t.Fatalf("result = %+v, %d messages; want one sent", got, sender.count())
	}
	msg := sender.sent[0]
	if msg.To != "applicant@example.com" {
		t.Fatalf("To = %q, want the address the row's resolver answered", msg.To)
	}
	if _, ok := msg.Headers[headerListUnsubscribe]; ok {
		t.Errorf("a row message carries List-Unsubscribe: %+v", msg.Headers)
	}
	if _, ok := msg.Headers[headerListUnsubscribePost]; ok {
		t.Errorf("a row message carries List-Unsubscribe-Post: %+v", msg.Headers)
	}
	if msg.TextBody != "Thank you, Acme Widgets." || msg.HTMLBody != "<p>Thank you, Acme Widgets.</p>" {
		t.Errorf("the message is not the template's words alone, with no footer: text %q html %q", msg.TextBody, msg.HTMLBody)
	}
	if strings.Contains(string(nodes[0].Payload), "applicant@example.com") {
		t.Errorf("the reply names the address: %s", nodes[0].Payload)
	}
	req := resolver.asked[0]
	if resolver.count() != 1 || req.Concept != testRowConcept || req.RowID != "application-1" || req.AccountID != "acct-1" || req.Kind != "received" || req.RequestID != "lane-received-application-1-1" {
		t.Fatalf("the resolver was asked %d times, last %+v", resolver.count(), req)
	}
	writes := callsWithPrefix(engine, "mutation recordCampaignDelivery")
	if len(writes) != 1 || !strings.Contains(writes[0], `rowConcept: "`+testRowConcept+`"`) || !strings.Contains(writes[0], `rowId: "application-1"`) || !strings.Contains(writes[0], `rowKind: "received"`) {
		t.Fatalf("the ledger row does not name its row: %v", writes)
	}
	if strings.Contains(writes[0], "Acme Widgets") {
		t.Fatalf("the ledger row carries the message's fields: %s", writes[0])
	}
}

func TestARowMessageRefusesACallWithoutInternalOrigin(t *testing.T) {
	engine := rowSendEngine()
	sender := &recordingSender{}
	w := rowWorker(t, engine, sender)
	resolver := answering("applicant@example.com", "acct-1", nil)
	registerTestRowAddress(t, testRowConcept, resolver.resolve)

	_, err := w.handleSendRowMessage(importCtx(), rowArgs(), 0)
	if err == nil || err.Error() != "campaigns.sendRowMessage: a message about one person's own request is sent by a trusted job, never by a client" {
		t.Fatalf("a client call: %v", err)
	}
	if len(engine.calls) != 0 || resolver.count() != 0 || sender.count() != 0 {
		t.Fatalf("a refused call read %d times, asked the resolver %d times, sent %d", len(engine.calls), resolver.count(), sender.count())
	}
}

func TestARowMessageRefusesAnUnregisteredConcept(t *testing.T) {
	engine := rowSendEngine()
	sender := &recordingSender{}
	w := rowWorker(t, engine, sender)
	args := rowArgs()
	args["rowConcept"] = "v1:test:nobodyRegisteredThis"
	if _, err := w.handleSendRowMessage(fromTrustedJob(), args, 0); err == nil || !strings.Contains(err.Error(), "is not a concept registered for the row lane") {
		t.Fatalf("an unregistered concept: %v", err)
	}
	if len(engine.calls) != 0 || sender.count() != 0 {
		t.Fatalf("an unregistered concept read %d times and sent %d", len(engine.calls), sender.count())
	}
}

func TestARowMessageRefusesARowOfAnotherOrganization(t *testing.T) {
	engine := rowSendEngine()
	sender := &recordingSender{}
	w := rowWorker(t, engine, sender)
	resolver := answering("applicant@example.com", "acct-x", nil)
	registerTestRowAddress(t, testRowConcept, resolver.resolve)

	_, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
	if err == nil || !strings.Contains(err.Error(), "the row belongs to another organization") {
		t.Fatalf("a row of another organization: %v", err)
	}
	if strings.Contains(err.Error(), "applicant@example.com") {
		t.Fatalf("the refusal names the address: %v", err)
	}
	if sender.count() != 0 || len(callsWithPrefix(engine, "mutation recordCampaignDelivery")) != 0 {
		t.Fatal("a row of another organization was mailed or ledgered")
	}
}

func TestAMarketingTemplateIsHeldOnTheLane(t *testing.T) {
	for _, purpose := range []string{"", "marketing"} {
		engine := rowSendEngine()
		engine.template["purpose"] = purpose
		sender := &recordingSender{}
		w := rowWorker(t, engine, sender)
		resolver := answering("applicant@example.com", "acct-1", nil)
		registerTestRowAddress(t, testRowConcept, resolver.resolve)

		_, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
		var held sendHeld
		if !errors.As(err, &held) || held.reason != "“Application received” is not a published transactional template." {
			t.Fatalf("purpose %q: %v", purpose, err)
		}
		if resolver.count() != 0 || sender.count() != 0 {
			t.Fatalf("purpose %q: the resolver was asked %d times, %d sent", purpose, resolver.count(), sender.count())
		}
	}
}

func TestADraftTransactionalTemplateIsHeld(t *testing.T) {
	for _, status := range []string{"draft", "archived"} {
		engine := rowSendEngine()
		engine.template["status"] = status
		sender := &recordingSender{}
		w := rowWorker(t, engine, sender)
		resolver := answering("applicant@example.com", "acct-1", nil)
		registerTestRowAddress(t, testRowConcept, resolver.resolve)

		_, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
		var held sendHeld
		if !errors.As(err, &held) || !strings.Contains(held.reason, "is not a published transactional template") {
			t.Fatalf("status %q: %v", status, err)
		}
		if resolver.count() != 0 || sender.count() != 0 {
			t.Fatalf("status %q: the resolver was asked %d times, %d sent", status, resolver.count(), sender.count())
		}
	}
}

// A transactional template published before the save-time check existed may
// still carry the unsubscribe or preferences tag. It is held, never sent with
// the tag filled empty.
func TestATransactionalTemplateThatStillCarriesAnUnsubscribeTagIsHeld(t *testing.T) {
	for _, part := range []string{"subject", "preheader", "textBody", "htmlBody"} {
		for _, tag := range []string{"{{unsubscribeUrl}}", "{{ preferencesUrl }}"} {
			engine := rowSendEngine()
			engine.template[part] = str(engine.template, part) + " " + tag
			sender := &recordingSender{}
			w := rowWorker(t, engine, sender)
			resolver := answering("applicant@example.com", "acct-1", nil)
			registerTestRowAddress(t, testRowConcept, resolver.resolve)

			_, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
			var held sendHeld
			if !errors.As(err, &held) || held.reason != "“Application received” cannot be sent. A message about one person's own request carries no unsubscribe link. Remove it." {
				t.Fatalf("%s with %s: %v", part, tag, err)
			}
			if resolver.count() != 0 || sender.count() != 0 {
				t.Fatalf("%s with %s: the resolver was asked %d times, %d sent", part, tag, resolver.count(), sender.count())
			}
		}
	}
}

// The send render refuses the tag itself, whoever calls it.
func TestTheTransactionalRenderRefusesAnUnsubscribeTag(t *testing.T) {
	tmpl := Template{Subject: "Hello", TextBody: "Manage: {{preferencesUrl}}"}
	if _, err := renderMessage(Campaign{}, tmpl, Recipient{Email: "applicant@example.com"}, RenderOptions{Transactional: true}); !errors.Is(err, errListLinkInTransactional) {
		t.Fatalf("render: %v", err)
	}
}

func TestAWithdrawnRowMessageIsSkippedWithItsReason(t *testing.T) {
	engine := rowSendEngine()
	sender := &recordingSender{}
	w := rowWorker(t, engine, sender)
	resolver := &countingResolver{answer: func(int) (RowMessage, error) {
		return RowMessage{}, RowWithdrawn{Reason: "the application was withdrawn"}
	}}
	registerTestRowAddress(t, testRowConcept, resolver.resolve)

	nodes, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeResult(t, nodes); got["skipped"] != true || got["reason"] != "the application was withdrawn" {
		t.Fatalf("result = %+v", got)
	}
	if sender.count() != 0 {
		t.Fatal("a withdrawn message was sent")
	}
	if !wroteContaining(engine, "mutation recordCampaignDelivery", `skipReason: "the application was withdrawn"`) {
		t.Fatal("the withdrawal was not ledgered with its reason")
	}
}

// A resolver error that is not a withdrawal fails the send, and nothing goes out.
func TestARowMessageWhoseResolverFailsSendsNothing(t *testing.T) {
	engine := rowSendEngine()
	sender := &recordingSender{}
	w := rowWorker(t, engine, sender)
	resolver := &countingResolver{answer: func(int) (RowMessage, error) { return RowMessage{}, errors.New("the row could not be read") }}
	registerTestRowAddress(t, testRowConcept, resolver.resolve)
	if _, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0); err == nil {
		t.Fatal("a failed resolver was a send")
	}
	if sender.count() != 0 {
		t.Fatal("a failed resolver sent mail")
	}
}

func TestRowMessageStatusRefusesACallWithoutInternalOrigin(t *testing.T) {
	engine := rowSendEngine()
	w := rowWorker(t, engine, &recordingSender{})
	_, err := w.handleRowMessageStatus(importCtx(), map[string]any{"requestIds": []any{"lane-received-application-1-1"}}, 0)
	if err == nil || err.Error() != "campaigns.rowMessageStatus: read by a trusted job, never by a client" {
		t.Fatalf("a client call: %v", err)
	}
	if len(engine.calls) != 0 {
		t.Fatalf("a refused call read %d times", len(engine.calls))
	}
}

// The hold on a transactional template stays for every other send: only the row
// lane passes it.
func TestAnOrderMessageStillHoldsATransactionalTemplate(t *testing.T) {
	engine := rowSendEngine()
	sender := &recordingSender{}
	w := rowWorker(t, engine, sender)
	_, err := w.handleSendOrderMessage(fromTrustedJob(), orderArgs(), 0)
	var held sendHeld
	if !errors.As(err, &held) || held.reason != errTransactionalTemplate.Error() || sender.count() != 0 {
		t.Fatalf("an order message with a transactional template: %v", err)
	}
}

// A resolver is a pack's code, and its error is its own words: they may name the
// row and its address. None of it reaches the error the caller gets, nor a log
// line; the lane says only that the resolver did not answer.
func TestAResolverErrorNeverReachesTheCallerOrTheLog(t *testing.T) {
	for _, cause := range []error{
		fmt.Errorf("row application-77 has no valid email applicant@example.com"),
		fmt.Errorf("reading row application-77 for applicant@example.com: %w", context.DeadlineExceeded),
	} {
		engine := rowSendEngine()
		sender := &recordingSender{}
		w := rowWorker(t, engine, sender)
		var logs bytes.Buffer
		w.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		resolver := &countingResolver{answer: func(int) (RowMessage, error) { return RowMessage{}, cause }}
		registerTestRowAddress(t, testRowConcept, resolver.resolve)

		_, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
		if err == nil {
			t.Fatal("a failed resolver was a send")
		}
		for _, leaked := range []string{"applicant@example.com", "application-77"} {
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("the error names %q: %v", leaked, err)
			}
			if strings.Contains(logs.String(), leaked) {
				t.Fatalf("the log names %q: %s", leaked, logs.String())
			}
		}
		if errors.Is(err, cause) || errors.Unwrap(err) != nil {
			t.Fatalf("the error wraps the resolver's own: %v", err)
		}
		if sender.count() != 0 {
			t.Fatal("a failed resolver sent mail")
		}
	}
}

// A resolver may answer a withdrawal as a pointer; it is a withdrawal still.
func TestAWithdrawalAnsweredAsAPointerIsAWithdrawal(t *testing.T) {
	engine := rowSendEngine()
	sender := &recordingSender{}
	w := rowWorker(t, engine, sender)
	resolver := &countingResolver{answer: func(int) (RowMessage, error) {
		return RowMessage{}, &RowWithdrawn{Reason: "the application was withdrawn"}
	}}
	registerTestRowAddress(t, testRowConcept, resolver.resolve)
	nodes, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
	if err != nil {
		t.Fatalf("a pointer withdrawal failed the send: %v", err)
	}
	if got := decodeResult(t, nodes); got["skipped"] != true || got["reason"] != "the application was withdrawn" || sender.count() != 0 {
		t.Fatalf("result = %+v", got)
	}
}
