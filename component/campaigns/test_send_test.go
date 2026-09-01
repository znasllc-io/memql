package campaigns

import (
	"strings"
	"testing"
)

// test_send_test.go -- the one-address preview (memql#4822, design D11).
//
// The assertions that carry weight are the NEGATIVE ones. A test send that
// wrote a delivery row would remove whoever the synthetic recipient collided
// with from the real send; one that moved a counter would make sentCount a
// lie in the direction of "more went out than did"; one that carried a real
// unsubscribe token would let a curious click opt somebody out of a campaign
// that has not been sent. Each of those is silent afterwards.

func testSendWorker(t *testing.T, engine *fakeEngine) (*Worker, *recordingSender) {
	t.Helper()
	sender := &recordingSender{}
	w := newTestWorker(t, engine, sender)
	w.cfg.SendingIdentity = "default@example.test"
	return w, sender
}

func testSendFixture() *fakeEngine {
	campaign := campaignRow()
	campaign["name"] = "Spring"
	return &fakeEngine{
		campaign: campaign,
		template: map[string]any{
			"id":       "v1:campaigns:template:" + testTemplate,
			"subject":  "Hello {{displayName}}",
			"textBody": "Hi {{displayName}} at {{fields.company}}. Your plan is {{fields.plan}}.",
			"htmlBody": "<p>Hi {{displayName}} at {{fields.company}}.</p>",
			// DRAFT, deliberately: a test send is how copy gets finished, so
			// requiring `ready` would forbid the one use the feature is for.
			"status": "draft",
		},
		roster: []map[string]any{{
			"id":                 "v1:campaigns:recipient:r-1",
			"email":              "real@example.test",
			"displayName":        "Real Person",
			"subscriptionStatus": "subscribed",
			"fields":             map[string]any{"company": "Acme"},
		}},
	}
}

func TestTestSendMailsTheNamedAddressAndReportsUnresolvedTags(t *testing.T) {
	engine := testSendFixture()
	w, sender := testSendWorker(t, engine)

	nodes, err := w.handleTestSend(importCtx(), map[string]any{
		"campaignId": testCampaign, "to": "Operator@Example.test",
	}, 0)
	if err != nil {
		t.Fatalf("testSend: %v", err)
	}
	got := decodeResult(t, nodes)

	if sender.count() != 1 {
		t.Fatalf("expected one message, got %d", sender.count())
	}
	msg := sender.sent[0]
	if msg.To != "operator@example.test" {
		t.Errorf("To = %q, want the normalized address the caller named", msg.To)
	}
	if !strings.HasPrefix(msg.Subject, testSendSubjectPrefix) {
		t.Errorf("Subject = %q, want the %q prefix so a person can tell which message is the test",
			msg.Subject, testSendSubjectPrefix)
	}
	if !strings.Contains(msg.TextBody, testSendDisplayName) {
		t.Errorf("the synthetic display name did not render: %s", msg.TextBody)
	}
	// {{fields.company}} resolves from the audience's FIRST recipient, so the
	// operator sees real shape; {{fields.plan}} has no column and is reported.
	if !strings.Contains(msg.TextBody, "Acme") {
		t.Errorf("the audience's field shape was not borrowed: %s", msg.TextBody)
	}
	tags, _ := got["unresolvedTags"].([]any)
	if len(tags) != 1 || tags[0] != "{{fields.plan}}" {
		t.Errorf("unresolvedTags = %v, want exactly {{fields.plan}}. Borrowing real field shape is what "+
			"stops the report flagging the operator's CORRECT tags as typos", tags)
	}
}

// TestTestSendWritesNothing is the whole "no side effects" claim in one
// place.
func TestTestSendWritesNothing(t *testing.T) {
	engine := testSendFixture()
	w, _ := testSendWorker(t, engine)

	if _, err := w.handleTestSend(importCtx(), map[string]any{
		"campaignId": testCampaign, "to": "operator@example.test",
	}, 0); err != nil {
		t.Fatalf("testSend: %v", err)
	}

	engine.mu.Lock()
	defer engine.mu.Unlock()
	for _, c := range engine.calls {
		if strings.HasPrefix(c.query, "mutation ") {
			t.Errorf("a test send issued a WRITE: %s\n"+
				"A delivery row would remove whoever the synthetic recipient collided with from the real "+
				"send; a counter would make sentCount a lie in the direction of 'more went out than did'.",
				c.query)
		}
	}
}

// TestTestSendCarriesAnInertUnsubscribeToken: the surface has to render --
// it is most of what an operator checks -- but a REAL token would name a real
// recipient and a real campaign.
func TestTestSendCarriesAnInertUnsubscribeToken(t *testing.T) {
	engine := testSendFixture()
	w, sender := testSendWorker(t, engine)

	if _, err := w.handleTestSend(importCtx(), map[string]any{
		"campaignId": testCampaign, "to": "operator@example.test",
	}, 0); err != nil {
		t.Fatalf("testSend: %v", err)
	}
	msg := sender.sent[0]

	header := msg.Headers[headerListUnsubscribe]
	if !strings.Contains(header, inertUnsubscribeToken) {
		t.Fatalf("List-Unsubscribe = %q, want the inert token", header)
	}
	// It must NOT verify. Anything else is an opt-out a curious click
	// performs against a campaign that has not been sent.
	if _, _, _, err := ParseUnsubscribeToken(w.cfg.UnsubscribeKeys(), inertUnsubscribeToken); err == nil {
		t.Error("the test send's unsubscribe token VERIFIES")
	}
	if !strings.Contains(msg.TextBody, "Unsubscribe:") {
		t.Error("the unsubscribe footer did not render at all; it is most of what a test send is checking")
	}
}

// TestTestSendWorksOnAnAlreadySentCampaign is the sendableStatus exemption.
// The commonest moment to want a test send is while duplicating last month's
// campaign or diagnosing a complaint about one that already went out.
func TestTestSendWorksOnAnAlreadySentCampaign(t *testing.T) {
	engine := testSendFixture()
	engine.campaign["status"] = "sent"
	w, sender := testSendWorker(t, engine)

	if _, err := w.handleTestSend(importCtx(), map[string]any{
		"campaignId": testCampaign, "to": "operator@example.test",
	}, 0); err != nil {
		t.Fatalf("a test send on an already-sent campaign was refused: %v", err)
	}
	if sender.count() != 1 {
		t.Errorf("expected the message to go out, got %d", sender.count())
	}
}

func TestTestSendRequiresAValidatedAddress(t *testing.T) {
	engine := testSendFixture()
	w, sender := testSendWorker(t, engine)

	for _, to := range []string{"", "   ", "not-an-address", "a@localhost"} {
		if _, err := w.handleTestSend(importCtx(), map[string]any{
			"campaignId": testCampaign, "to": to,
		}, 0); err == nil {
			t.Errorf("to=%q was accepted", to)
		}
	}
	if sender.count() != 0 {
		t.Error("a message went out for an invalid address")
	}
}

func TestTestSendUsesTheCampaignsResolvedIdentity(t *testing.T) {
	engine := testSendFixture()
	engine.campaign["senderIdentityId"] = "v1:campaigns:senderIdentity:si-acme"
	engine.senderIdentities = map[string]map[string]any{
		"si-acme": senderIdentityRow("si-acme", "news@acme.test", "Acme News", "active"),
	}
	w, sender := testSendWorker(t, engine)

	if _, err := w.handleTestSend(importCtx(), map[string]any{
		"campaignId": testCampaign, "to": "operator@example.test",
	}, 0); err != nil {
		t.Fatalf("testSend: %v", err)
	}
	if as := sender.identities()[0]; as.Address != "news@acme.test" {
		t.Errorf("the test went out as %q, want the campaign's declared mailbox -- a test from a "+
			"different mailbox than the campaign will use is a test of the wrong thing", as.Address)
	}
}

func TestTestSendRefusesADisabledIdentity(t *testing.T) {
	engine := testSendFixture()
	engine.campaign["senderIdentityId"] = "v1:campaigns:senderIdentity:si-retired"
	engine.senderIdentities = map[string]map[string]any{
		"si-retired": senderIdentityRow("si-retired", "old@acme.test", "Acme", "disabled"),
	}
	w, sender := testSendWorker(t, engine)

	if _, err := w.handleTestSend(importCtx(), map[string]any{
		"campaignId": testCampaign, "to": "operator@example.test",
	}, 0); err == nil {
		t.Fatal("a test send from a disabled identity was accepted")
	}
	if sender.count() != 0 {
		t.Error("a message went out anyway")
	}
}

func TestTestSendIsNotTracked(t *testing.T) {
	engine := testSendFixture()
	engine.campaign["trackOpens"] = true
	engine.campaign["trackClicks"] = true
	engine.template["htmlBody"] = `<p><a href="https://acme.test/x">Shop</a></p>`
	w, sender := testSendWorker(t, engine)

	if _, err := w.handleTestSend(importCtx(), map[string]any{
		"campaignId": testCampaign, "to": "operator@example.test",
	}, 0); err != nil {
		t.Fatalf("testSend: %v", err)
	}
	body := sender.sent[0].HTMLBody
	if strings.Contains(body, TrackingOpenPath) || strings.Contains(body, TrackingClickPath) {
		t.Errorf("the test message carries tracking. There is no delivery row to attribute a hit to, so "+
			"a pixel here records against an id nobody holds -- and the operator's own open lands in the "+
			"campaign's numbers.\ngot: %s", body)
	}
}

func TestTestSendConsumesTheRateBucket(t *testing.T) {
	engine := testSendFixture()
	w, sender := testSendWorker(t, engine)
	w.limiter = newRateLimiter(1, w.now)

	args := map[string]any{"campaignId": testCampaign, "to": "operator@example.test"}
	if _, err := w.handleTestSend(importCtx(), args, 0); err != nil {
		t.Fatalf("first test send: %v", err)
	}
	if _, err := w.handleTestSend(importCtx(), args, 0); err == nil {
		t.Error("a second test send passed an exhausted bucket. A test is a real message to a real " +
			"mailbox and the provider counts it, so a burst of them would trip a throttle a campaign is " +
			"then blamed for")
	}
	if sender.count() != 1 {
		t.Errorf("sent %d messages, want 1", sender.count())
	}
}
