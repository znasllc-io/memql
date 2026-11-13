package campaigns

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/email"
)

// send_order_test.go -- THE THIRD SHAPE OF AUTOMATED MAIL: what an ORDER
// entitles.
//
// The program allows the transactional outbox (this cluster's own people) and
// the marketing lane (people already on a list). A request to the buyer of an
// order -- "tell us how it went" -- is neither: the buyer is on no list, and a
// store that asked only its subscribers would be asking only the customers it
// expects to be happy. So this route takes an ORDER, never an address: it
// reads the buyer's address off the mirrored order itself, and it keeps every
// lawful-mail property of the marketing lane -- the suppression list at the
// point of send, the one-click unsubscribe pair, the organization's sender,
// the delivery ledger -- while needing no subscription. It answers only to the
// trusted job that runs it (internal origin): a client that could call it
// could mail the buyer of any order.

func orderSendEngine() *fakeEngine {
	engine := recipientSendEngine()
	engine.roster = nil // no recipient row: an order entitles the message
	engine.template["textBody"] = "Tell us how {{fields.productTitle}} went: {{fields.reviewUrl}}"
	engine.template["accountId"] = "v1:accounts:account:acct-1"
	// The organization's own sender: a send under an organization goes out
	// only as an identity that belongs to it.
	fyloSender := senderIdentityRow("si-store", "hello@store.example.test", "The Store", "active")
	fyloSender["accountId"] = "v1:accounts:account:acct-1"
	engine.senderIdentities = map[string]map[string]any{"si-store": fyloSender}
	// THE STORE'S ORGANIZATION IS ITS SERVING SITE'S. Another organization's
	// site previews the same store, which must not make it that
	// organization's store.
	engine.boundSites = []map[string]any{
		{"id": "v1:platform:site:site-1", "accountId": "v1:accounts:account:acct-1", "binding": map[string]any{"storeId": "store-live"}},
		{"id": "v1:platform:site:site-x", "accountId": "v1:accounts:account:acct-x", "previewBinding": map[string]any{"storeId": "store-live"}},
	}
	engine.orders = map[string]map[string]any{
		"gid://shopify/Order/9": {"gid": "gid://shopify/Order/9", "storeId": "store-live", "email": "buyer@example.test", "name": "#1009"},
		"gid://shopify/Order/0": {"gid": "gid://shopify/Order/0", "storeId": "store-live", "email": ""},
	}
	return engine
}

// fromTrustedJob is how the reviews pack's request job calls: a trusted
// automation's internal origin, borrowing the store's merchant.
func fromTrustedJob() context.Context {
	return auth.ContextWithInternalOrigin(importCtx())
}

func orderArgs() map[string]any {
	return map[string]any{
		"storeId": "store-live", "orderGid": "gid://shopify/Order/9", "templateId": testTemplate, "accountId": "acct-1",
		"requestId": "reviews-request-abc123", "senderIdentityId": "si-store",
		"fields": map[string]any{"productTitle": "Floss Picks",
			"reviewUrl": "https://shop.example.test/reviews/write?product=floss-picks&t=tok"},
	}
}

func TestAnOrderMessageGoesToThatOrdersBuyer(t *testing.T) {
	engine := orderSendEngine()
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	nodes, err := w.handleSendOrderMessage(fromTrustedJob(), orderArgs(), 0)
	if err != nil {
		t.Fatalf("sendOrderMessage: %v", err)
	}
	if got := decodeResult(t, nodes); got["sent"] != true {
		t.Fatalf("result = %+v, want sent", got)
	}
	if sender.count() != 1 {
		t.Fatalf("messages = %d, want one", sender.count())
	}
	msg := sender.sent[0]
	if msg.To != "buyer@example.test" {
		t.Fatalf("To = %q, want the order's own buyer", msg.To)
	}
	if !strings.Contains(msg.TextBody, "Floss Picks") || !strings.Contains(msg.TextBody, "https://shop.example.test/reviews/write?product=floss-picks&t=tok") {
		t.Fatalf("body = %q, want the request's own fields", msg.TextBody)
	}
	if msg.Headers[headerListUnsubscribe] == "" || msg.Headers[headerListUnsubscribePost] != oneClickValue {
		t.Errorf("the RFC 8058 header pair is missing: %+v", msg.Headers)
	}
	if !wroteContaining(engine, "mutation recordCampaignDelivery", `status: "sent"`) {
		t.Error("the outcome was not ledgered")
	}
}

// AN ORDER, NEVER AN ADDRESS: an address passed beside the order is not who
// the message goes to.
func TestAnOrderMessageTakesNoAddress(t *testing.T) {
	engine := orderSendEngine()
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	args := orderArgs()
	args["email"] = "somebody-else@example.test"
	args["to"] = "somebody-else@example.test"
	if _, err := w.handleSendOrderMessage(fromTrustedJob(), args, 0); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 || sender.sent[0].To != "buyer@example.test" {
		t.Fatalf("the message went to %v; only the order's buyer may be mailed", sender.sent)
	}
}

func TestAnOrderMessageRefusesAChangedBoundBuyer(t *testing.T) {
	engine := orderSendEngine()
	sender := &recordingSender{}
	w := withOpenGate(newTestWorker(t, engine, sender))
	args := orderArgs()
	args["expectedEmailDigest"] = EmailDigest("buyer@example.test")
	engine.orders["gid://shopify/Order/9"]["email"] = "changed@example.test"
	if _, err := w.handleSendOrderMessage(fromTrustedJob(), args, 0); err == nil || !strings.Contains(err.Error(), "buyer changed") {
		t.Fatalf("changed buyer was not refused: %v", err)
	}
	if sender.count() != 0 || wroteContaining(engine, "mutation recordCampaignSingleSend", "") {
		t.Fatal("recipient mismatch reached transport or wrote an attempt receipt")
	}
}

func TestAnOrderMessageAcceptsTheOriginalBoundBuyer(t *testing.T) {
	engine := orderSendEngine()
	sender := &recordingSender{}
	w := withOpenGate(newTestWorker(t, engine, sender))
	args := orderArgs()
	args["expectedEmailDigest"] = EmailDigest(" BUYER@EXAMPLE.TEST ")
	if _, err := w.handleSendOrderMessage(fromTrustedJob(), args, 0); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 || sender.sent[0].To != "buyer@example.test" {
		t.Fatal("the bound buyer did not receive its message")
	}
}

func TestAnOrderMessageRefusesAClient(t *testing.T) {
	engine := orderSendEngine()
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	if _, err := w.handleSendOrderMessage(importCtx(), orderArgs(), 0); err == nil {
		t.Fatal("a client call mailed an order's buyer")
	}
	if sender.count() != 0 {
		t.Fatal("a refused call sent mail")
	}
}

func TestAnOrderMessageHonoursTheSuppressionList(t *testing.T) {
	engine := orderSendEngine()
	engine.suppression = map[string]map[string]any{
		EmailDigest("buyer@example.test"): {"id": EmailDigest("buyer@example.test"), "reason": "unsubscribed"},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	nodes, err := w.handleSendOrderMessage(fromTrustedJob(), orderArgs(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeResult(t, nodes); got["skipped"] != true || got["reason"] != "unsubscribed" {
		t.Fatalf("result = %+v, want skipped: unsubscribed", got)
	}
	if sender.count() != 0 {
		t.Fatal("a suppressed buyer was mailed")
	}
}

func TestAnOrderWithNoAddressOrNoOrderMailsNobody(t *testing.T) {
	for _, gid := range []string{"gid://shopify/Order/0", "gid://shopify/Order/404"} {
		engine := orderSendEngine()
		sender := &recordingSender{}
		w := newTestWorker(t, engine, sender)
		w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
		args := orderArgs()
		args["orderGid"] = gid
		if _, err := w.handleSendOrderMessage(fromTrustedJob(), args, 0); err == nil {
			t.Errorf("%s: no error for an order with nobody to mail", gid)
		}
		if sender.count() != 0 {
			t.Errorf("%s: mail was sent", gid)
		}
	}
}

// THE ORGANIZATION IS THE STORE'S, CHECKED, NEVER TAKEN. It scopes the
// suppression list and the unsubscribe a buyer may use, so a send under any
// organization but the one whose site serves the store is refused -- as is a
// send that names none.
func TestAnOrderMessageIsSentOnlyUnderTheStoresOwnOrganization(t *testing.T) {
	for name, account := range map[string]any{"no organization": "", "a previewing organization": "acct-x", "a stranger": "acct-z"} {
		engine := orderSendEngine()
		sender := &recordingSender{}
		w := newTestWorker(t, engine, sender)
		w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
		args := orderArgs()
		args["accountId"] = account
		if _, err := w.handleSendOrderMessage(fromTrustedJob(), args, 0); err == nil {
			t.Errorf("%s: an order's buyer was mailed under it", name)
		}
		if sender.count() != 0 {
			t.Errorf("%s: mail was sent", name)
		}
	}
}

// THE CASE THE TEMPLATE CHECK CANNOT CATCH: a template, a sender and an
// organization that all agree with each other -- and a store that is not
// theirs. The buyer of another organization's store is never mailed, however
// consistent the rest of the call is.
func TestAnOrderMessageNeverMailsAnotherOrganizationsBuyers(t *testing.T) {
	engine := orderSendEngine()
	engine.template["accountId"] = "v1:accounts:account:acct-x"
	other := senderIdentityRow("si-x", "hello@other.example.test", "Other", "active")
	other["accountId"] = "v1:accounts:account:acct-x"
	engine.senderIdentities["si-x"] = other
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	args := orderArgs()
	args["accountId"] = "acct-x"
	args["senderIdentityId"] = "si-x"
	if _, err := w.handleSendOrderMessage(fromTrustedJob(), args, 0); err == nil {
		t.Fatal("the buyer of a store another organization serves was mailed")
	}
	if sender.count() != 0 {
		t.Fatal("mail was sent")
	}
}

// THE INSTALLATION'S OWN SENDER MAY STAND IN, WHEN THE JOB SAYS SO. An
// organization with no mailbox of its own can have its order mail leave from
// the installation's sender -- the one whose organization is "self" -- when the
// trusted job that runs the send says an operator chose it. The message still
// belongs to the store's organization: its suppression list decides, and its
// unsubscribe is the one the buyer gets. Only the transport is the
// installation's.
func houseSender() map[string]any {
	house := senderIdentityRow("si-house", "noreply@house.example.test", "House", "active")
	house["accountId"] = "self"
	return house
}

func TestAnOrderMessageMayLeaveFromTheInstallationsSenderWhenTheJobSaysSo(t *testing.T) {
	engine := orderSendEngine()
	engine.senderIdentities["si-house"] = houseSender()
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	args := orderArgs()
	args["senderIdentityId"] = "si-house"
	args["installationSender"] = true
	nodes, err := w.handleSendOrderMessage(fromTrustedJob(), args, 0)
	if err != nil {
		t.Fatalf("sendOrderMessage: %v", err)
	}
	if got := decodeResult(t, nodes); got["sent"] != true {
		t.Fatalf("result = %+v, want sent", got)
	}
	if sender.count() != 1 {
		t.Fatalf("messages = %d, want one", sender.count())
	}
	as := sender.sentAs[0]
	if bare(as.AccountID) != "self" || as.Address != "noreply@house.example.test" {
		t.Fatalf("sent as %+v, want the installation's own sender and connection", as)
	}
	if sender.sent[0].To != "buyer@example.test" {
		t.Fatalf("To = %q, want the order's own buyer", sender.sent[0].To)
	}
	if sender.sent[0].Headers[headerListUnsubscribe] == "" {
		t.Error("the buyer was handed no unsubscribe")
	}
}

func TestTheInstallationsSenderStillHonoursTheStoresSuppressionList(t *testing.T) {
	engine := orderSendEngine()
	engine.senderIdentities["si-house"] = houseSender()
	engine.suppression = map[string]map[string]any{
		EmailDigest("buyer@example.test"): {"id": EmailDigest("buyer@example.test"), "reason": "unsubscribed"},
	}
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	args := orderArgs()
	args["senderIdentityId"] = "si-house"
	args["installationSender"] = true
	nodes, err := w.handleSendOrderMessage(fromTrustedJob(), args, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeResult(t, nodes); got["skipped"] != true {
		t.Fatalf("result = %+v, want skipped", got)
	}
	if sender.count() != 0 {
		t.Fatal("a buyer the store's organization suppressed was mailed")
	}
}

func TestTheInstallationsSenderIsRefusedUnlessTheJobChoseIt(t *testing.T) {
	engine := orderSendEngine()
	engine.senderIdentities["si-house"] = houseSender()
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	args := orderArgs()
	args["senderIdentityId"] = "si-house"
	if _, err := w.handleSendOrderMessage(fromTrustedJob(), args, 0); err == nil {
		t.Fatal("an organization's order mail left from the installation's sender nobody chose")
	}
	if sender.count() != 0 {
		t.Fatal("mail was sent")
	}
}

func TestOnlyTheInstallationsSenderMayStandInForAnOrganizations(t *testing.T) {
	engine := orderSendEngine()
	other := senderIdentityRow("si-x", "hello@other.example.test", "Other", "active")
	other["accountId"] = "v1:accounts:account:acct-x"
	engine.senderIdentities["si-x"] = other
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	args := orderArgs()
	args["senderIdentityId"] = "si-x"
	args["installationSender"] = true
	if _, err := w.handleSendOrderMessage(fromTrustedJob(), args, 0); err == nil {
		t.Fatal("an organization's order mail left from another organization's mailbox")
	}
	if sender.count() != 0 {
		t.Fatal("mail was sent")
	}
}

// A BUYER SUBSCRIBED TO NOTHING. An order message (a review request) goes to
// the person an order entitles, who is on no list, so its footer must not tell
// them they subscribed; it says they bought from the sender. Before this the
// footer read "You are receiving this because you subscribed to ..." on every
// review request.
func TestAnOrderMessageFooterSaysTheBuyerBoughtNotSubscribed(t *testing.T) {
	engine := orderSendEngine()
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	if _, err := w.handleSendOrderMessage(fromTrustedJob(), orderArgs(), 0); err != nil {
		t.Fatalf("sendOrderMessage: %v", err)
	}
	if sender.count() != 1 {
		t.Fatalf("messages = %d, want one", sender.count())
	}
	msg := sender.sent[0]
	for part, body := range map[string]string{"text": msg.TextBody, "html": msg.HTMLBody} {
		if body == "" {
			continue
		}
		if strings.Contains(body, "you subscribed to") {
			t.Errorf("%s footer tells a buyer they subscribed: %q", part, body)
		}
		if !strings.Contains(body, "you bought from") {
			t.Errorf("%s footer: want \"you bought from\" in %q", part, body)
		}
	}
}

func TestOrderProviderErrorNeverPersistsEchoedPrivateFields(t *testing.T) {
	const token = "private-review-bearer-0123456789abcdef"
	const url = "https://shop.example.test/reviews/write?t=" + token
	for _, kind := range []string{"refused", "uncertain", "held earlier"} {
		t.Run(kind, func(t *testing.T) {
			engine := orderSendEngine()
			sender := &recordingSender{}
			failure := &email.SendError{Detail: "provider echoed " + token + " and " + url, NotAccepted: kind == "refused", HeldEarlier: kind == "held earlier"}
			sender.fail = func(int) error { return failure }
			w := withOpenGate(newTestWorker(t, engine, sender))
			args := orderArgs()
			args["fields"].(map[string]any)["reviewUrl"] = url
			_, err := w.handleSendOrderMessage(fromTrustedJob(), args, 0)
			if err == nil {
				t.Fatal("provider failure was reported sent")
			}
			if !errors.Is(err, failure) || email.IsNotAccepted(err) != (kind == "refused") || email.HeldEarlier(err) != (kind == "held earlier") || UncertainSend(err) != (kind != "refused") {
				t.Fatal("original typed provider classification was not preserved")
			}
			for _, call := range engine.calls {
				if strings.HasPrefix(call.query, "mutation ") && (strings.Contains(call.query, token) || strings.Contains(call.query, url)) {
					t.Fatal("provider echo persisted in a delivery or receipt mutation")
				}
			}
			if kind == "held earlier" {
				if wroteContaining(engine, "mutation recordCampaignDelivery", `status: "failed"`) {
					t.Fatal("held earlier rewrote the earlier delivery")
				}
			} else if !wroteContaining(engine, "mutation recordCampaignDelivery", "order message") {
				t.Fatal("safe provider explanation was not recorded")
			}
			if got := wroteContaining(engine, "mutation recordCampaignSingleSend", `status: "failed"`); got != (kind == "refused") {
				t.Fatal("provider classification changed receipt retry semantics")
			}
		})
	}
}
