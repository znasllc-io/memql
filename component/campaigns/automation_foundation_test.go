package campaigns

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

type operationalEngine struct {
	*fakeEngine
	rule  map[string]any
	users []map[string]any
}

func (e *operationalEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "query emailRuleById") {
		return rowsEnvelope([]map[string]any{e.rule}), nil
	}
	if strings.HasPrefix(q, "query activeUsers") {
		return rowsEnvelope(e.users), nil
	}
	return e.fakeEngine.Execute(ctx, q)
}
func TestOperationalEmailUsesPublishedCopyAndActualClusterRecipient(t *testing.T) {
	for _, tc := range []struct {
		name, status, mode, target, account string
		want                                bool
	}{{"allowed", "ready", "cluster_roles", "owner@example.test", "org", true}, {"draft", "draft", "cluster_roles", "owner@example.test", "org", false}, {"external target", "ready", "cluster_roles", "outsider@example.test", "org", false}, {"marketing rule", "ready", "audience", "owner@example.test", "org", false}, {"cross organization", "ready", "cluster_roles", "owner@example.test", "elsewhere", false}} {
		t.Run(tc.name, func(t *testing.T) {
			base := &fakeEngine{template: map[string]any{"id": "t1", "accountId": tc.account, "status": tc.status, "subject": "Hello {{displayName}}", "textBody": "Plain {{displayName}} {{confirmUrl}}", "htmlBody": "<b>{{displayName}}</b>"}}
			e := &operationalEngine{fakeEngine: base, rule: map[string]any{"id": "r1", "accountId": "org", "ownerUserId": "u1", "status": "active", "templateId": "t1", "recipientMode": tc.mode}, users: []map[string]any{{"role": "owner", "primaryEmail": "owner@example.test", "displayName": "Ada <Owner>"}}}
			w := newTestWorker(t, e, &recordingSender{})
			_, err := w.handleStageOperationalEmail(importCtx(), map[string]any{"requestId": "operational-one", "templateId": "t1", "to": tc.target, "displayName": "Untrusted", "emailRuleId": "r1", "dedupeKey": "dedupe"}, 0)
			if (err == nil) != tc.want {
				t.Fatalf("error=%v want success=%v", err, tc.want)
			}
			calls := callsWithPrefix(base, "mutation stageServerOutboundRequest")
			if !tc.want {
				if len(calls) != 0 {
					t.Fatalf("refused effect: %v", calls)
				}
				return
			}
			if len(calls) != 1 || !strings.Contains(calls[0], `htmlBody: "<b>Ada &lt;Owner&gt;</b>"`) || strings.Contains(calls[0], "Unsubscribe:") || strings.Contains(calls[0], "Untrusted") {
				t.Fatalf("unexpected operational payload: %v", calls)
			}
		})
	}
}
func TestPendingRecipientsOnlyReceiveInternalConfirmationSends(t *testing.T) {
	e := recipientSendEngine()
	e.roster[0]["subscriptionStatus"] = "pending"
	e.template["textBody"] = "Please confirm: {{confirmUrl}}"
	sender := &recordingSender{}
	w := newTestWorker(t, e, sender)
	nodes, err := w.handleSendToRecipient(importCtx(), sendToRecipientArgs(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if decodeResult(t, nodes)["reason"] != "pending" || sender.count() != 0 {
		t.Fatalf("pending recipient mailed: %+v", nodes)
	}
	_, err = w.sendToRecipientOnce(importCtx(), sendToRecipientArgs(), singleSend{confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 || !strings.Contains(sender.sent[0].TextBody, "/email/confirm?token=") {
		t.Fatalf("confirmation missing: %+v", sender.sent)
	}
}
func TestJourneyDraftRefusesInvalidStructure(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"name": "Welcome", "trigger": map[string]any{"kind": "joined", "audienceId": "a1"}, "steps": []map[string]any{{"id": "s1", "kind": "send"}}}
	}
	tests := []struct {
		name   string
		change func(map[string]any)
	}{
		{"empty name", func(a map[string]any) { a["name"] = " " }},
		{"unknown trigger", func(a map[string]any) { a["trigger"] = map[string]any{"kind": "changed"} }},
		{"unknown trigger field", func(a map[string]any) { a["trigger"] = map[string]any{"kind": "joined", "evil": true} }},
		{"fractional wait", func(a map[string]any) { a["steps"] = []map[string]any{{"id": "s1", "kind": "wait", "hours": 1.5}} }},
		{"zero wait", func(a map[string]any) { a["steps"] = []map[string]any{{"id": "s1", "kind": "wait"}} }},
		{"long wait", func(a map[string]any) {
			a["steps"] = []map[string]any{{"id": "s1", "kind": "wait", "days": 365, "hours": 1}}
		}},
		{"duplicate id", func(a map[string]any) {
			a["steps"] = []map[string]any{{"id": "s1", "kind": "send"}, {"id": "s1", "kind": "send"}}
		}},
		{"unused field", func(a map[string]any) { a["steps"] = []map[string]any{{"id": "s1", "kind": "send", "days": 0}} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := base()
			tc.change(a)
			if _, err := parseJourneyDraft(a); err == nil {
				t.Fatal("invalid draft admitted")
			}
		})
	}
	a := base()
	a["steps"] = []map[string]any{{"id": "s1", "kind": "confirm"}}
	draft, err := parseJourneyDraft(a)
	if err != nil || draft.Steps[0].TimeoutDays != 7 {
		t.Fatalf("default timeout: %+v %v", draft, err)
	}
}

func TestDoubleOptInAndJourneyRevisionAgainstTheRealEngine(t *testing.T) {
	f := newDigestFixture(t)
	n := f.newEmptyNewsletter(t, "double-opt-in")
	n.execute(t, n.owner, call("mutation", "setAudienceConfirmation", arg{"audienceId", n.audience}, arg{"confirmation", "required"}))
	binding, err := n.worker.newsletterRow(n.owner, "newsletterForSite", "siteId", n.suffix+"-site")
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil {
		t.Fatal("newsletter binding missing")
	}
	args := map[string]any{"siteId": str(binding, "siteId"), "audienceId": n.audience, "consentText": str(binding, "consentText"), "enabled": true, "expectedRevision": templateTime(binding["createdAt"]).Format(time.RFC3339Nano)}
	saved, err := n.worker.handleConfigureNewsletter(n.owner, args, 0)
	if err != nil {
		t.Fatal(err)
	}
	n.args["consentRevision"] = decodeResult(t, saved)["revision"]
	if _, err = n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatal(err)
	}
	roster, err := n.store.Roster(memql.ContextWithFreshRead(n.owner), n.audience)
	if err != nil || len(roster) != 1 {
		t.Fatalf("roster=%+v err=%v", roster, err)
	}
	if roster[0].SubscriptionStatus != "pending" || roster[0].JoinedAt.IsZero() {
		t.Fatalf("new recipient: %+v", roster[0])
	}
	signup, err := n.worker.newsletterRow(n.owner, "newsletterSignupById", "signupId", newsletterSignupID(n.audience, str(n.args, "email")))
	if err != nil || str(signup, "status") != "none" {
		t.Fatalf("signup=%v err=%v", signup, err)
	}
	consent, err := n.worker.newsletterRow(n.owner, "newsletterConsentById", "eventId", sha256Hex("newsletter-consent\x00"+newsletterSignupID(n.audience, str(n.args, "email"))))
	if err != nil || consent != nil {
		t.Fatalf("unconfirmed grant=%v err=%v", consent, err)
	}
	n.worker.drainNewsletterWelcomes(n.owner, n.system)
	if n.sender.count() != 0 {
		t.Fatal("pending recipient got welcome")
	}
	wired := NewWorker(deliveryDBEngine{n.engine}, nil, nil, quietLogger(), gatePoolFor(t))
	n.worker.journeyGate = wired.journeyGate
	journeyArgs := map[string]any{"journeyId": n.suffix + "-journey", "accountId": n.org, "name": "Draft", "trigger": map[string]any{"kind": "joined", "audienceId": n.audience}, "steps": []map[string]any{{"id": "s1", "kind": "send"}}}
	result, err := n.worker.handleSaveJourney(n.owner, journeyArgs, 0)
	if err != nil {
		t.Fatal(err)
	}
	receipt := decodeResult(t, result)
	if issues, ok := receipt["issues"].([]any); !ok || len(issues) == 0 {
		t.Fatalf("missing draft issues: %v", receipt)
	}
	if _, err = n.worker.handleSetJourneyState(n.owner, map[string]any{"journeyId": journeyArgs["journeyId"], "action": "activate", "expectedRevision": receipt["revision"]}, 0); err == nil {
		t.Fatal("unready journey activated")
	}
	journeyArgs["name"] = "Changed"
	journeyArgs["expectedRevision"] = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err = n.worker.handleSaveJourney(n.owner, journeyArgs, 0); err == nil {
		t.Fatal("stale revision saved")
	}
	if _, err = n.engine.Execute(n.owner, `insert("v1:campaigns:journey", payload={"name":"bypass"})`); err == nil || !strings.Contains(err.Error(), "automation writes require") {
		t.Fatalf("raw write guard: %v", err)
	}
	// A named server-only mutation remains refused at the client boundary.
	_, err = n.engine.Execute(auth.ContextWithUserActor(context.Background(), n.user), call("mutation", "confirmRecipientSubscription", arg{"recipientId", roster[0].ID}, arg{"confirmedAt", time.Now()}))
	if err == nil {
		t.Fatal("client confirmed a recipient")
	}
}

func TestRateLimitedRequestIsDurablyDeferred(t *testing.T) {
	e := recipientSendEngine()
	sender := &recordingSender{}
	w := newTestWorker(t, e, sender)
	w.singleSendGate = func(context.Context, string) (gateHold, error) { return openGate{}, nil }
	w.limiter = newRateLimiter(1, w.now)
	if !w.allowSend() {
		t.Fatal("new limiter empty")
	}
	args := sendToRecipientArgs()
	args["requestId"] = "rule-deferred-one"
	nodes, err := w.handleSendToRecipient(importCtx(), args, 0)
	if err != nil {
		t.Fatal(err)
	}
	result := decodeResult(t, nodes)
	if result["deferred"] != true || sender.count() != 0 {
		t.Fatalf("defer=%v sent=%d", result, sender.count())
	}
	if !wroteContaining(e, "mutation recordCampaignDelivery", `status: "pending"`) || !wroteContaining(e, "mutation recordCampaignDelivery", `requestId: "rule-deferred-one"`) {
		t.Fatalf("missing retry ledger: %v", callsWithPrefix(e, "mutation recordCampaignDelivery"))
	}
	if len(callsWithPrefix(e, "mutation recordCampaignSingleSend")) > 0 {
		t.Fatal("deferred send claimed provider attempt")
	}
}
