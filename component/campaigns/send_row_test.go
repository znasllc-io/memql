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
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
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
	// REFUSED BY ITS CODE, in the lane's sentence for a person: the lane's own
	// error text stays in its log (TestEveryRefusalAndFailureOfARowSendAnswersACodeAndASentence).
	if code, sentence, failed := RowSendFailure(err); !failed || code != RowSendRowOtherOrg || err.Error() != sentence ||
		!strings.Contains(sentence, "belongs to another organization") {
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

// A transactional template published before ruling 45 may carry a
// view-in-browser or confirmation link, which the lane would send with an
// empty address. It is held with the template's name and the link's sentence,
// before the resolver is asked; one carrying several of the engine's links is
// told each.
func TestATransactionalTemplateThatStillCarriesAViewOrConfirmLinkIsHeld(t *testing.T) {
	const (
		view    = "A message about one person's own request has no web copy. Remove the view-in-browser link."
		confirm = "A message about one person's own request has nothing to confirm. Remove the confirmation link."
		list    = "A message about one person's own request carries no unsubscribe link. Remove it."
	)
	for _, c := range []struct{ tags, want string }{
		{"{{viewUrl}}", view},
		{"{{ confirmUrl }}", confirm},
		{"{{confirmUrl}} {{unsubscribeUrl}} {{viewUrl}}", list + " " + view + " " + confirm},
	} {
		for _, part := range []string{"subject", "preheader", "textBody", "htmlBody"} {
			engine := rowSendEngine()
			engine.template[part] = str(engine.template, part) + " " + c.tags
			sender := &recordingSender{}
			w := rowWorker(t, engine, sender)
			resolver := answering("applicant@example.com", "acct-1", map[string]string{"companyName": "Acme Widgets"})
			registerTestRowAddress(t, testRowConcept, resolver.resolve)

			_, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
			var held sendHeld
			if !errors.As(err, &held) || held.reason != "“Application received” cannot be sent. "+c.want {
				t.Fatalf("%s with %s: %v", part, c.tags, err)
			}
			if resolver.count() != 0 || sender.count() != 0 {
				t.Fatalf("%s with %s: the resolver was asked %d times, %d sent", part, c.tags, resolver.count(), sender.count())
			}
			if strings.Contains(err.Error(), "applicant@example.com") || strings.Contains(err.Error(), "Acme Widgets") {
				t.Fatalf("%s with %s: the refusal carries the row's words: %v", part, c.tags, err)
			}
		}
	}
	// The send render refuses either tag itself, whoever calls it, in the same words.
	for tag, want := range map[string]string{"{{viewUrl}}": view, "{{confirmUrl}}": confirm} {
		tmpl := Template{Subject: "Hello", HTMLBody: `<p><a href="` + tag + `">Open</a></p>`}
		if _, err := renderMessage(Campaign{}, tmpl, Recipient{Email: "applicant@example.com"}, RenderOptions{Transactional: true}); err == nil || err.Error() != want {
			t.Fatalf("render with %s: %v", tag, err)
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

// A HOLD IS READ BY ITS SENTENCE outside this package, through a wrap, and
// nothing else is a hold: what a pack records as skipped rather than failed.
// UncertainSend reads an uncertain hand-over through a caller's wrap, and
// nothing else: not a hold, not a refusal, not nothing.
func TestUncertainSendReadsAnUncertainHandOverThroughAWrapAndNothingElse(t *testing.T) {
	uncertain := uncertainSend{error: errors.New("campaigns.sendToRecipient: the connection closed")}
	if !UncertainSend(fmt.Errorf("a caller's wrap: %w", uncertain)) {
		t.Fatal("an uncertain hand-over was not read as one through a wrap")
	}
	for name, err := range map[string]error{"a hold": sendHeld{reason: "Publish it."}, "a resolver failure": errResolverDidNotAnswer, "nothing": nil} {
		if UncertainSend(err) {
			t.Fatalf("%s was read as an uncertain hand-over", name)
		}
	}
}

func TestHeldReasonReadsAHoldThroughAWrapAndNothingElse(t *testing.T) {
	engine := rowSendEngine()
	engine.template["status"] = "draft"
	w := rowWorker(t, engine, &recordingSender{})
	registerTestRowAddress(t, testRowConcept, answering("applicant@example.com", "acct-1", nil).resolve)
	_, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
	reason, held := HeldReason(fmt.Errorf("a caller's wrap: %w", err))
	if !held || !strings.Contains(reason, "is not a published transactional template") {
		t.Fatalf("HeldReason(%v) = %q, %v", err, reason, held)
	}
	if _, held := HeldReason(errResolverDidNotAnswer); held {
		t.Fatal("a resolver failure is not a hold")
	}
	if _, held := HeldReason(nil); held {
		t.Fatal("no error is not a hold")
	}
}

// THE ROW LANE'S SENDER, AS A ROW SEND RESOLVES IT: the chosen identity of the
// organization, with its name and address; another organization's, one turned
// off and one that does not exist each refused with the sentence the send
// would give; none chosen, for an organization that is not the installation,
// refused as the send refuses it. A client's call is refused before reading.
func TestTheRowSenderIsResolvedAsARowSendResolvesIt(t *testing.T) {
	engine := rowSendEngine()
	other := senderIdentityRow("si-other", "news@other.example.test", "Other", "active")
	other["accountId"] = "v1:accounts:account:acct-x"
	off := senderIdentityRow("si-off", "old@store.example.test", "Old", "disabled")
	off["accountId"] = "v1:accounts:account:acct-1"
	engine.senderIdentities["si-other"], engine.senderIdentities["si-off"] = other, off
	w := rowWorker(t, engine, &recordingSender{})
	ask := func(sender string) map[string]any {
		t.Helper()
		nodes, err := w.handleRowSender(fromTrustedJob(), map[string]any{"accountId": "acct-1", "senderIdentityId": sender}, 0)
		if err != nil {
			t.Fatalf("rowSender(%q): %v", sender, err)
		}
		return decodeResult(t, nodes)
	}
	if got := ask("si-store"); got["ready"] != true || got["address"] != "hello@store.example.test" || got["fromName"] != "The Store" || got["reason"] != "" {
		t.Fatalf("the chosen sender answered %v", got)
	}
	for sender, want := range map[string]string{
		"si-other": "The chosen sender belongs to another organization.",
		"si-off":   "The sender old@store.example.test is turned off. Turn it on to send this email.",
		"si-none":  "The sender of this email no longer exists. Choose another sender to send it.",
		"":         "No sender is chosen for this organization.",
	} {
		if got := ask(sender); got["ready"] != false || got["reason"] != want || got["address"] != "" {
			t.Fatalf("sender %q answered %v, want refused with %q", sender, got, want)
		}
	}
	if _, err := w.handleRowSender(context.Background(), map[string]any{"accountId": "acct-1"}, 0); err == nil {
		t.Fatal("a client's call was answered")
	}
	// A TRUSTED CALLER WITHOUT AUTHORITY IN THE ORGANIZATION -- the send's own
	// check, requireSendAuthority -- is answered exactly as for an organization
	// that does not exist.
	unauthorized := func(account string) map[string]any {
		t.Helper()
		nodes, err := w.handleRowSender(auth.ContextWithInternalOrigin(context.Background()),
			map[string]any{"accountId": account, "senderIdentityId": "si-store"}, 0)
		if err != nil {
			t.Fatalf("rowSender without authority: %v", err)
		}
		got := decodeResult(t, nodes)
		delete(got, "accountId")
		return got
	}
	without, missing := unauthorized("acct-1"), unauthorized("acct-none")
	if fmt.Sprint(without) != fmt.Sprint(missing) || without["ready"] != false || without["address"] != "" ||
		without["reason"] != "No sender can be read for this organization." {
		t.Fatalf("without authority the sender reads %v; for an organization that does not exist %v", without, missing)
	}
}

// EVERY SKIP REASON THE LANE WRITES HAS A SENTENCE A PERSON READS, written
// here where the codes are: the suppression list's reasons (hard_bounce,
// complaint, manual, unsubscribed) and the recipient row's statuses (bounced,
// complained, unsubscribed, pending). A code with no words of its own, and
// anything the lane does not know, reads as a plain sentence -- never as the
// code.
func TestEverySkipReasonTheLaneWritesHasASentence(t *testing.T) {
	for reason, want := range map[string]string{
		"hard_bounce":  "An earlier email to this address bounced, so the email service no longer sends to it.",
		"bounced":      "An earlier email to this address bounced, so the email service no longer sends to it.",
		"complaint":    "The recipient marked an earlier email as spam, so the email service no longer sends to them.",
		"complained":   "The recipient marked an earlier email as spam, so the email service no longer sends to them.",
		"unsubscribed": "The recipient unsubscribed from this sender's email.",
		"manual":       "This address is on the installation's do-not-send list.",
		"pending":      "The email service skipped this send.",
		"soft_bounce":  "The email service skipped this send.",
		"":             "The email service skipped this send.",
	} {
		if got := SkipSentence(reason); got != want {
			t.Errorf("SkipSentence(%q) = %q, want %q", reason, got, want)
		}
	}
	// THE LANE'S OWN LISTS OF ITS CODES: each has words of its own, and every
	// reason that blocks a row send is among them.
	codes := append(append([]string{}, suppressedSkipReasons...), unsubscribedSkipReason)
	for _, reason := range []string{"hard_bounce", "complaint", "manual", "unsubscribed"} {
		if blocksATransactionalMessage(reason) {
			codes = append(codes, reason)
		}
		if status := recipientStatusFor(reason); status != "" {
			codes = append(codes, status)
		}
	}
	for _, code := range codes {
		got := SkipSentence(code)
		if got == SkipSentence("") || got == code {
			t.Errorf("the skip reason %q has no sentence of its own: %q", code, got)
		}
		if !strings.HasSuffix(got, ".") || got[:1] != strings.ToUpper(got[:1]) || strings.Contains(got, "_") {
			t.Errorf("the sentence for %q is %q, want a whole sentence a person reads", code, got)
		}
	}
}

// A ROW SEND THAT IS SKIPPED SAYS WHY IN A SENTENCE, beside its reason: the
// row's own withdrawal sentence, or the lane's words for the suppression
// code. The reason is as it was.
func TestASkippedRowMessageAnswersASentenceBesideItsReason(t *testing.T) {
	engine := rowSendEngine()
	w := rowWorker(t, engine, &recordingSender{})
	withdrawn := &countingResolver{answer: func(int) (RowMessage, error) {
		return RowMessage{}, RowWithdrawn{Reason: "The application was withdrawn."}
	}}
	registerTestRowAddress(t, testRowConcept, withdrawn.resolve)
	nodes, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeResult(t, nodes); got["skipped"] != true || got["reason"] != "The application was withdrawn." ||
		got["skipSentence"] != "The application was withdrawn." {
		t.Fatalf("a withdrawn row send answered %+v", got)
	}

	for _, reason := range []string{"hard_bounce", "complaint", "manual"} {
		engine := rowSendEngine()
		engine.suppression = map[string]map[string]any{EmailDigest("applicant@example.com"): {"reason": reason}}
		sender := &recordingSender{}
		w := rowWorker(t, engine, sender)
		registerTestRowAddress(t, testRowConcept, answering("applicant@example.com", "acct-1", nil).resolve)
		nodes, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
		if err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		got := decodeResult(t, nodes)
		if got["skipped"] != true || got["reason"] != reason || got["skipSentence"] != SkipSentence(reason) || sender.count() != 0 {
			t.Fatalf("a row send to a %s address answered %+v after %d sent", reason, got, sender.count())
		}
		if got["skipSentence"] == reason || got["skipSentence"] == SkipSentence("") {
			t.Fatalf("the %s skip's sentence is %q", reason, got["skipSentence"])
		}
	}
}

// ledgerEngine answers the row lane's status read from rows a test scripts.
type ledgerEngine struct {
	*fakeEngine
	deliveries []map[string]any
}

func (e *ledgerEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "query deliveriesByRequestIds") {
		return rowsEnvelope(e.deliveries), nil
	}
	return e.fakeEngine.Execute(ctx, q)
}

// THE STATUS READ ANSWERS A SKIP'S SENTENCE BESIDE ITS REASON: the lane's words
// for a code, the row's own withdrawal sentence for a row send, a plain
// sentence when the row says nothing, and nothing for a message that was not
// skipped. The reason stays the lane's own record.
func TestRowMessageStatusAnswersASkipsSentenceBesideItsReason(t *testing.T) {
	row := func(request, status, reason string) map[string]any {
		return map[string]any{"requestId": request, "status": status, "skipReason": reason, "rowConcept": testRowConcept,
			"rowId": "application-1", "rowKind": "received", "accountId": "v1:accounts:account:acct-1"}
	}
	engine := &ledgerEngine{fakeEngine: rowSendEngine(), deliveries: []map[string]any{
		row("lane-status-bounce-1", "skipped", "hard_bounce"),
		row("lane-status-spam-1", "skipped", "complaint"),
		row("lane-status-block-1", "skipped", "manual"),
		row("lane-status-withdrawn-1", "skipped", "The application is no longer waiting for terms."),
		row("lane-status-silent-1", "skipped", ""),
		row("lane-status-sent-1", "sent", ""),
	}}
	w := newTestWorker(t, engine, &recordingSender{})
	ids := []any{"lane-status-bounce-1", "lane-status-spam-1", "lane-status-block-1", "lane-status-withdrawn-1",
		"lane-status-silent-1", "lane-status-sent-1", "lane-status-never-1"}
	nodes, err := w.handleRowMessageStatus(fromTrustedJob(), map[string]any{"requestIds": ids, "accountId": "acct-1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ reason, sentence string }{
		{"hard_bounce", "An earlier email to this address bounced, so the email service no longer sends to it."},
		{"complaint", "The recipient marked an earlier email as spam, so the email service no longer sends to them."},
		{"manual", "This address is on the installation's do-not-send list."},
		{"The application is no longer waiting for terms.", "The application is no longer waiting for terms."},
		{"", "The email service skipped this send."},
		{"", ""},
		{"", ""},
	}
	if len(nodes) != len(want) {
		t.Fatalf("%d answers for %d request ids", len(nodes), len(want))
	}
	for i := range nodes {
		got := decodeResult(t, nodes[i:i+1])
		if got["requestId"] != ids[i] || got["skipReason"] != want[i].reason || got["skipSentence"] != want[i].sentence {
			t.Errorf("answer %d: %v, want reason %q and sentence %q", i, got, want[i].reason, want[i].sentence)
		}
	}
}

// THE ROW SENDER SAYS IN FIELDS OF ITS OWN WHAT ITS SENTENCE SAYS, so a caller
// decides on a field and never on the words: readFailed for a sender that
// could not be read just now, noneChosen for an organization with none chosen.
// Every other answer carries neither, and a caller without the send's
// authority still reads exactly what an organization that does not exist
// answers.
func TestTheRowSenderSaysInFieldsWhatItsSentenceSays(t *testing.T) {
	engine := rowSendEngine()
	off := senderIdentityRow("si-off", "old@store.example.test", "Old", "disabled")
	off["accountId"] = "v1:accounts:account:acct-1"
	other := senderIdentityRow("si-other", "news@other.example.test", "Other", "active")
	other["accountId"] = "v1:accounts:account:acct-x"
	engine.senderIdentities["si-off"], engine.senderIdentities["si-other"] = off, other
	w := rowWorker(t, engine, &recordingSender{})
	ask := func(ctx context.Context, account, sender string) map[string]any {
		t.Helper()
		nodes, err := w.handleRowSender(ctx, map[string]any{"accountId": account, "senderIdentityId": sender}, 0)
		if err != nil {
			t.Fatalf("rowSender(%q): %v", sender, err)
		}
		return decodeResult(t, nodes)
	}
	for sender, want := range map[string][2]bool{
		"si-store": {false, false}, // ready
		"":         {false, true},  // none chosen
		"si-off":   {false, false}, // held: turned off
		"si-none":  {false, false}, // held: gone
		"si-other": {false, false}, // another organization's
	} {
		got := ask(fromTrustedJob(), "acct-1", sender)
		if got["readFailed"] != want[0] || got["noneChosen"] != want[1] {
			t.Errorf("sender %q answered %v, want readFailed %v and noneChosen %v", sender, got, want[0], want[1])
		}
	}
	engine.identityReadErr = errors.New("the database is slow")
	if got := ask(fromTrustedJob(), "acct-1", "si-store"); got["ready"] != false || got["readFailed"] != true || got["noneChosen"] != false ||
		got["reason"] != "The sender could not be read just now." {
		t.Fatalf("a sender that could not be read answered %v", got)
	}
	engine.identityReadErr = nil
	unauthorized := auth.ContextWithInternalOrigin(context.Background())
	without, missing := ask(unauthorized, "acct-1", ""), ask(unauthorized, "acct-none", "")
	delete(without, "accountId")
	delete(missing, "accountId")
	if fmt.Sprint(without) != fmt.Sprint(missing) || without["readFailed"] != false || without["noneChosen"] != false {
		t.Fatalf("without authority the sender reads %v; for an organization that does not exist %v", without, missing)
	}
}

// THE DEFAULT SEND TIMEOUT A PACK MAY NAME is the one the lane's configuration
// starts from.
func TestTheDefaultSendTimeoutIsTheConfigurationsDefault(t *testing.T) {
	t.Setenv("MEMQL_CAMPAIGNS_SEND_TIMEOUT_SECONDS", "")
	if got := LoadConfig().SendTimeout; got != DefaultSendTimeout || DefaultSendTimeout <= 0 {
		t.Fatalf("the configuration starts from %s, DefaultSendTimeout is %s", got, DefaultSendTimeout)
	}
}

// ---------------------------------------------------------------------------
// A refusal and a failure say why in a sentence, beside a code
// ---------------------------------------------------------------------------

// scriptedQueries is the fake engine with every query of one name answered by
// a script: an error (a read the database could not make), or rows.
type scriptedQueries struct {
	*fakeEngine
	prefix string
	err    error
	rows   []map[string]any
}

func (e *scriptedQueries) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, e.prefix) {
		if e.err != nil {
			return nil, e.err
		}
		return rowsEnvelope(e.rows), nil
	}
	return e.fakeEngine.Execute(ctx, q)
}

// withoutWritePermission is a trusted job acting as a signed-in person who
// holds no write permission in the organization.
func withoutWritePermission() context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "reader-1", Role: auth.RoleReader})
	return auth.ContextWithInternalOrigin(auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "reader-1"}))
}

// rowAddressInTest is the address the row's resolver answers in these tests:
// nothing a caller, a log line or a sentence may carry.
const rowAddressInTest = "applicant@example.com"

// refusedRowSend is one way a row send is refused or fails.
type refusedRowSend struct {
	name, code string
	// ctx is who calls; nil is a trusted job holding the send's authority.
	ctx func() context.Context
	// engine arranges the fake engine and may wrap it.
	engine func(*fakeEngine) Engine
	args   func(map[string]any)
	// resolve is the row's resolver; nil answers the address, in the organization.
	resolve func(int) (RowMessage, error)
	// deliver is the provider's answer; noProvider leaves the node with none.
	deliver    func(int) error
	noProvider bool
	// raw is a part of the lane's own error: its log keeps it, its answer does not.
	raw string
	// provider is a part of the provider's own words: the lane's ledger keeps
	// them (lastError), and neither its answer nor its log does.
	provider string
}

func rowRefusals() []refusedRowSend {
	slow := errors.New("the database is slow")
	return []refusedRowSend{
		{name: "the template is another organization's", code: RowSendTemplateOtherOrg,
			engine: func(e *fakeEngine) Engine { e.template["accountId"] = "v1:accounts:account:acct-x"; return e },
			raw:    "the template must belong to the row's organization"},
		{name: "the caller holds no write permission", code: RowSendNoAuthority, ctx: withoutWritePermission,
			raw: "campaign send requires write permission in this organization"},
		{name: "the template cannot be read", code: RowSendTemplateUnreadable,
			engine: func(e *fakeEngine) Engine { e.template = nil; return e }, raw: "is not readable"},
		{name: "no sender is chosen", code: RowSendSenderNoneChosen,
			args: func(a map[string]any) { delete(a, "senderIdentityId") }, raw: "choose a sending identity belonging to this organization"},
		{name: "the sender is another organization's", code: RowSendSenderOtherOrg,
			engine: func(e *fakeEngine) Engine {
				other := senderIdentityRow("si-other", "news@other.example.test", "Other", "active")
				other["accountId"] = "v1:accounts:account:acct-x"
				e.senderIdentities["si-other"] = other
				return e
			},
			args: func(a map[string]any) { a["senderIdentityId"] = "si-other" }, raw: "campaign and sending identity must belong to the same organization"},
		{name: "the sender could not be read", code: RowSendSenderUnread,
			engine: func(e *fakeEngine) Engine { e.identityReadErr = slow; return e }, raw: "could not read sending identity"},
		{name: "the row's resolver did not answer", code: RowSendRowUnanswered,
			resolve: func(int) (RowMessage, error) {
				return RowMessage{}, errors.New("row application-77 of " + rowAddressInTest + " could not be read")
			},
			raw: "the row's resolver did not answer"},
		{name: "the row is another organization's", code: RowSendRowOtherOrg,
			resolve: func(int) (RowMessage, error) { return RowMessage{Email: rowAddressInTest, AccountID: "acct-x"}, nil },
			raw:     "the row belongs to another organization"},
		{name: "the row names no address", code: RowSendNoAddress,
			resolve: func(int) (RowMessage, error) { return RowMessage{Email: " ", AccountID: "acct-1"}, nil },
			raw:     "the row names no usable address"},
		{name: "the suppression list could not be read", code: RowSendSuppressionUnread,
			engine: func(e *fakeEngine) Engine {
				return &scriptedQueries{fakeEngine: e, prefix: "query suppressionByDigest", err: slow}
			},
			raw: "the suppression list could not be consulted"},
		{name: "the provider refused the message", code: RowSendProviderRefused,
			deliver: func(int) error {
				return &email.SendError{StatusCode: 550, Permanent: true, NotAccepted: true,
					Detail: "550 5.1.1 <" + rowAddressInTest + ">: Recipient address rejected"}
			},
			provider: "Recipient address rejected"},
		{name: "the provider is limiting what it takes", code: RowSendProviderBusy,
			deliver: func(int) error {
				return &email.SendError{StatusCode: 429, Throttled: true, NotAccepted: true, RetryAfter: 30 * time.Second,
					Detail: "too many messages for " + rowAddressInTest}
			},
			provider: "too many messages"},
		{name: "the node has no provider", code: RowSendNoProvider, noProvider: true, raw: "no email sender is registered on this node"},
		{name: "a read the send needed failed", code: RowSendNotNow,
			engine: func(e *fakeEngine) Engine {
				return &scriptedQueries{fakeEngine: e, prefix: "query templateById", err: slow}
			},
			raw: "the database is slow"},
		{name: "anything with no kind of its own", code: RowSendFailed,
			engine: func(e *fakeEngine) Engine {
				// The request id already names another message: a receipt of another
				// fingerprint that the provider took.
				return &scriptedQueries{fakeEngine: e, prefix: "query campaignSingleSendById", rows: []map[string]any{
					{"fingerprint": "another-message", "status": receiptComplete, "accountId": "acct-1"}}}
			},
			raw: "this request identifier already names a different message"},
	}
}

// send runs the row send the case describes and answers its error, the lane's
// log, the engine and the provider.
func (c refusedRowSend) send(t *testing.T) (error, string, *fakeEngine, *recordingSender) {
	t.Helper()
	base := rowSendEngine()
	var engine Engine = base
	if c.engine != nil {
		engine = c.engine(base)
	}
	sender := &recordingSender{fail: c.deliver}
	w := newTestWorker(t, engine, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	if c.noProvider {
		w.resolve = func() email.Sender { return nil }
	}
	var logs bytes.Buffer
	w.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	resolve := c.resolve
	if resolve == nil {
		resolve = func(int) (RowMessage, error) { return RowMessage{Email: rowAddressInTest, AccountID: "acct-1"}, nil }
	}
	registerTestRowAddress(t, testRowConcept, (&countingResolver{answer: resolve}).resolve)
	args := rowArgs()
	if c.args != nil {
		c.args(args)
	}
	ctx := fromTrustedJob()
	if c.ctx != nil {
		ctx = c.ctx()
	}
	nodes, err := w.handleSendRowMessage(ctx, args, 0)
	if err == nil {
		t.Fatalf("%s: the send answered %s, want it refused", c.name, nodes[0].Payload)
	}
	return err, logs.String(), base, sender
}

// EVERY REFUSAL AND FAILURE OF A ROW SEND ANSWERS A CODE AND A SENTENCE FOR A
// PERSON, AND NOTHING OF THE LANE'S OWN ERROR. For each way a row send is
// refused or fails: the answer is read, through a caller's wrap, as the kind's
// code and the lane's sentence for it; the answer's whole text is that sentence
// -- no function's name, no engine's or provider's message, no address -- and it
// wraps nothing; the lane's own error stays with the lane, in its log, or for a
// message the provider did not take in the ledger row of the delivery
// (lastError), where the provider's words are kept and from where they reach
// neither the answer nor a log line.
func TestEveryRefusalAndFailureOfARowSendAnswersACodeAndASentence(t *testing.T) {
	for _, c := range rowRefusals() {
		t.Run(c.name, func(t *testing.T) {
			err, logs, engine, sender := c.send(t)
			code, sentence, failed := RowSendFailure(fmt.Errorf("a caller's wrap: %w", err))
			if !failed || code != c.code {
				t.Fatalf("the answer %q is read as failed %v with the code %q, want %q", err, failed, code, c.code)
			}
			if sentence == "" || sentence != RowSendSentence(c.code) || err.Error() != sentence {
				t.Fatalf("the answer reads %q with the sentence %q, want the lane's sentence for %s, %q, and nothing else",
					err, sentence, c.code, RowSendSentence(c.code))
			}
			if errors.Unwrap(err) != nil {
				t.Fatalf("the answer wraps the lane's own error: %v", errors.Unwrap(err))
			}
			for _, kept := range []string{c.raw, c.provider, "campaigns.", rowAddressInTest, "application-77"} {
				if kept != "" && strings.Contains(err.Error(), kept) {
					t.Fatalf("the answer carries %q: %v", kept, err)
				}
			}
			if !strings.Contains(logs, "code="+c.code) {
				t.Fatalf("the lane's log does not say the send was refused as %s:\n%s", c.code, logs)
			}
			if c.raw != "" && !strings.Contains(logs, c.raw) {
				t.Fatalf("the lane's log does not keep its own error (%q):\n%s", c.raw, logs)
			}
			for _, never := range []string{rowAddressInTest, "application-77", c.provider} {
				if never != "" && strings.Contains(logs, never) {
					t.Fatalf("the lane's log carries %q:\n%s", never, logs)
				}
			}
			if c.provider != "" {
				// THE PROVIDER'S OWN WORDS STAY ON THE LANE'S RECORD OF THE DELIVERY.
				if !wroteContaining(engine, "mutation recordCampaignDelivery", c.provider) {
					t.Fatalf("the ledger does not keep the provider's answer: %v", callsWithPrefix(engine, "mutation recordCampaignDelivery"))
				}
			}
			if sender.count() != 0 {
				t.Fatalf("%d messages left after a refused send", sender.count())
			}
		})
	}
}

// EVERY CODE HAS A SENTENCE A PERSON READS: a capital first and a full stop
// last; no function's or package's name, no error chain and no code in it; and
// no promise of a retry nobody makes. A code the lane does not know reads the
// general sentence, never the code. And the cases above cover every code: one
// added without a case fails here.
func TestEveryRowSendCodeHasASentenceAPersonReads(t *testing.T) {
	codes := RowSendCodes()
	if len(codes) < 12 {
		t.Fatalf("the lane lists %d codes: %v", len(codes), codes)
	}
	// template_invalid is the one code no case reaches: a template that cannot be
	// rendered is held before the render (linkHeld), so it is checked by its
	// classification below.
	covered := map[string]bool{RowSendTemplateInvalid: true}
	for _, c := range rowRefusals() {
		covered[c.code] = true
	}
	for _, code := range codes {
		if !covered[code] {
			t.Errorf("the code %q has no case in rowRefusals", code)
		}
		sentence := RowSendSentence(code)
		first, _ := utf8.DecodeRuneInString(sentence)
		if sentence == "" || !unicode.IsUpper(first) || !strings.HasSuffix(sentence, ".") {
			t.Errorf("the sentence for %s is %q, want a whole sentence", code, sentence)
		}
		for _, never := range []string{"campaigns", "sendRowMessage", "sendToRecipient", "_", ": ", "will retry", "retried", code} {
			if strings.Contains(sentence, never) {
				t.Errorf("the sentence for %s carries %q: %q", code, never, sentence)
			}
		}
	}
	if got := RowSendSentence("a_code_from_a_later_build"); got == "" || got != RowSendSentence(RowSendFailed) {
		t.Errorf("an unknown code reads %q, want the general sentence", got)
	}
	// THE WORDS THEMSELVES, as a person reads them today: a rewording is made
	// here too, on purpose.
	words := map[string]string{
		"no_authority": "The person on whose behalf this email is sent does not have write permission in this organization, so it was not sent. " +
			"An administrator of the organization can give them that permission.",
		"template_unreadable": "The template chosen for this email no longer exists, or the person on whose behalf it is sent cannot read it, " +
			"so it was not sent. Once another template is chosen, or that person can read this one, it can be sent again.",
		"template_other_organization": "The template chosen for this email belongs to another organization, so it was not sent. " +
			"Once one of this organization's templates is chosen, it can be sent again.",
		"template_invalid": "The template chosen for this email cannot be sent as it is written, so the email was not sent. " +
			"Once the template is corrected, it can be sent again.",
		"sender_none_chosen": "No sender is chosen for this organization, so this email was not sent. Once a sender is chosen, it can be sent again.",
		"sender_other_organization": "The sender chosen for this email belongs to another organization, so it was not sent. " +
			"Once one of this organization's senders is chosen, it can be sent again.",
		"sender_unread": "The sender of this email could not be read just now, so it was not sent. It can be sent again.",
		"row_unanswered": "What this email needs from the record it is about could not be read, so it was not sent. " +
			"It can be sent again; if it is still not sent, the cluster's operators can say why.",
		"row_other_organization": "The record this email is about belongs to another organization, so it was not sent. The cluster's operators can say why.",
		"no_address":             "The record this email is about names no email address a message can go to, so it was not sent.",
		"suppression_unread":     "The email service could not check its do-not-send list just now, so this email was not sent. It can be sent again.",
		"no_provider":            "No email provider is connected to this installation, so this email was not sent. The cluster's operators connect one.",
		"provider_refused": "The email provider did not accept this email, so it was not sent. " +
			"The cluster's operators can read the provider's answer in the email service's record of it.",
		"provider_busy": "The email provider is limiting how much email it accepts just now, so this email was not sent. It can be sent again later.",
		"not_now":       "The email service could not finish this just now, and nothing was sent. It can be sent again.",
		"failed":        "The email service could not send this email. It can be sent again; if it is still not sent, the cluster's operators can say why.",
	}
	if len(words) != len(codes) {
		t.Errorf("%d codes and %d sentences written here", len(codes), len(words))
	}
	for code, want := range words {
		if got := RowSendSentence(code); got != want {
			t.Errorf("RowSendSentence(%q) = %q, want %q", code, got, want)
		}
	}
	// THE CLASSIFICATION, for what no case above reaches through a send.
	for want, err := range map[string]error{
		RowSendTemplateInvalid: rowRefused(RowSendTemplateInvalid, errors.New("campaigns.sendRowMessage: recipient links cannot appear in HTML comments")),
		RowSendProviderRefused: errPriorSendNotAccepted,
		RowSendNotNow:          undecided(errors.New("the receipt could not be read")),
		RowSendFailed:          errors.New("anything else"),
	} {
		if got := rowSendCode(err); got != want {
			t.Errorf("rowSendCode(%v) = %q, want %q", err, got, want)
		}
	}
}

// AN UNCERTAIN HAND-OVER IS STILL READ AS ONE, AND CARRIES NONE OF THE
// TRANSPORT'S WORDS: the answer is the lane's own sentence, the transport's
// text -- which may quote the address -- stays on the ledger row, and no log
// line carries it.
func TestAnUncertainRowSendAnswersNoneOfTheTransportsWords(t *testing.T) {
	c := refusedRowSend{name: "the connection dropped", deliver: func(int) error {
		return errors.New("write to mail.example.test: connection reset while sending to " + rowAddressInTest)
	}}
	err, logs, engine, _ := c.send(t)
	if !UncertainSend(fmt.Errorf("a caller's wrap: %w", err)) {
		t.Fatalf("a hand-over with no answer is not read as uncertain: %v", err)
	}
	if _, _, failed := RowSendFailure(err); failed {
		t.Fatalf("an uncertain hand-over is read as a failure: %v", err)
	}
	for _, never := range []string{rowAddressInTest, "connection reset", "mail.example.test"} {
		if strings.Contains(err.Error(), never) || strings.Contains(logs, never) {
			t.Fatalf("the answer %q or the log carries %q:\n%s", err, never, logs)
		}
	}
	if strings.Contains(err.Error(), "campaigns.") {
		t.Fatalf("the answer names the function that failed: %v", err)
	}
	if first, _ := utf8.DecodeRuneInString(err.Error()); !unicode.IsUpper(first) || !strings.HasSuffix(err.Error(), ".") {
		t.Fatalf("the uncertain answer reads %q, want a whole sentence", err)
	}
	if !wroteContaining(engine, "mutation recordCampaignDelivery", "connection reset") {
		t.Fatalf("the ledger does not keep the transport's answer: %v", callsWithPrefix(engine, "mutation recordCampaignDelivery"))
	}
}

// A HOLD AND A BUSY RECEIPT LOCK ARE ANSWERED AS THEY WERE: a hold's text is its
// sentence, written for the organization's people, and a busy lock says nothing
// of the message. Neither is a failure with a code.
func TestAHoldAndABusyLockAreNotRowSendFailures(t *testing.T) {
	engine := rowSendEngine()
	engine.template["status"] = "draft"
	w := rowWorker(t, engine, &recordingSender{})
	registerTestRowAddress(t, testRowConcept, answering(rowAddressInTest, "acct-1", nil).resolve)
	_, err := w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
	if reason, held := HeldReason(err); !held || err.Error() != reason || !strings.Contains(reason, "is not a published transactional template") {
		t.Fatalf("a hold answered %v", err)
	}
	if _, _, failed := RowSendFailure(err); failed {
		t.Fatalf("a hold is read as a failure: %v", err)
	}
	w = rowWorker(t, rowSendEngine(), &recordingSender{})
	w.singleSendGate = func(context.Context, string) (gateHold, error) {
		return nil, fmt.Errorf("%w: the wait for the lock was over", memql.ErrWriteGateBusy)
	}
	_, err = w.handleSendRowMessage(fromTrustedJob(), rowArgs(), 0)
	if !memql.IsWriteGateBusy(err) {
		t.Fatalf("a busy receipt lock answered %v", err)
	}
	if _, _, failed := RowSendFailure(err); failed {
		t.Fatalf("a busy receipt lock is read as a failure: %v", err)
	}
}

// THE ROW SENDER ANSWERS A CODE BESIDE ITS SENTENCE, the code a send refused
// for the same thing answers: none when it is ready; no_authority for a caller
// without the send's authority -- and for an organization that does not exist,
// which reads exactly the same --; and one for each of the others.
func TestTheRowSenderAnswersACodeBesideItsSentence(t *testing.T) {
	engine := rowSendEngine()
	off := senderIdentityRow("si-off", "old@store.example.test", "Old", "disabled")
	off["accountId"] = "v1:accounts:account:acct-1"
	other := senderIdentityRow("si-other", "news@other.example.test", "Other", "active")
	other["accountId"] = "v1:accounts:account:acct-x"
	engine.senderIdentities["si-off"], engine.senderIdentities["si-other"] = off, other
	w := rowWorker(t, engine, &recordingSender{})
	ask := func(ctx context.Context, account, sender string) map[string]any {
		t.Helper()
		nodes, err := w.handleRowSender(ctx, map[string]any{"accountId": account, "senderIdentityId": sender}, 0)
		if err != nil {
			t.Fatalf("rowSender(%q): %v", sender, err)
		}
		return decodeResult(t, nodes)
	}
	for sender, want := range map[string]string{
		"si-store": "",
		"":         RowSendSenderNoneChosen,
		"si-off":   RowSenderHeld,
		"si-none":  RowSenderHeld,
		"si-other": RowSendSenderOtherOrg,
	} {
		if got := ask(fromTrustedJob(), "acct-1", sender); got["code"] != want {
			t.Errorf("sender %q answered %v, want the code %q", sender, got, want)
		}
	}
	engine.identityReadErr = errors.New("the database is slow")
	if got := ask(fromTrustedJob(), "acct-1", "si-store"); got["code"] != RowSendSenderUnread || got["readFailed"] != true {
		t.Errorf("a sender that could not be read answered %v", got)
	}
	engine.identityReadErr = nil
	without, missing := ask(withoutWritePermission(), "acct-1", "si-store"), ask(withoutWritePermission(), "acct-none", "si-store")
	delete(without, "accountId")
	delete(missing, "accountId")
	if without["code"] != RowSendNoAuthority || fmt.Sprint(without) != fmt.Sprint(missing) {
		t.Errorf("without authority the sender reads %v; for an organization that does not exist %v; want both no_authority and alike", without, missing)
	}
}
