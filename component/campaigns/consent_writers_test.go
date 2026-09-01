package campaigns

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// consent_writers_test.go -- the four production writers of
// v1:campaigns:consentEvent (memql#4820, design D15).
//
// # Why this file exists at all
//
// The concept, its five mutations, its two export queries and the Go helpers
// in consent.go all shipped in memql#4141 with NO PRODUCTION CALLER. Every
// deployment's consent stream was empty, and the export answered "no record"
// for people who had explicitly opted out and for addresses the cluster was
// actively refusing to mail. Nothing failed; the feature was inert, which is
// the worst of the two ways a feature can be wrong.
//
// So each writer is pinned at its SITE rather than through consent.go's pure
// helpers, because "the helper works" was already true.
//
// # The three source values are not decoration
//
//	one_click  the RECIPIENT acted, through the RFC 8058 endpoint
//	provider   a THIRD PARTY reported, in a payload the operator allowlisted
//	operator   an ADMIN decided, with a reason they had to supply
//	import     the list arrived with the consent asserted by whoever built it
//
// Collapsing any two of them would make the stream unable to answer the one
// question it exists for: on whose authority did this person's consent change.

// TestTheUnsubscribeEndpointAppendsAWithdrawEvent is the recipient's own act.
func TestTheUnsubscribeEndpointAppendsAWithdrawEvent(t *testing.T) {
	engine := &fakeEngine{
		campaign: campaignRow(),
		roster:   []map[string]any{recipientRow("r-1", "person@example.test", "subscribed")},
	}
	cfg := Config{UnsubscribeSecret: trackSecretA, UnsubscribeBaseURL: "https://api.example.test"}
	h := NewUnsubscribeHandler(engine, cfg, quietLogger())

	token, err := MintUnsubscribeToken(trackSecretA, testOwner, "r-1", testCampaign)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, UnsubscribePath+"?token="+token, nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d", rw.Code)
	}

	if !wroteContaining(engine, "mutation recordConsentWithdraw", `source: "one_click"`) {
		t.Fatalf("no one-click consent withdraw was appended. The suppression row says the address is on "+
			"a do-not-mail list and the recipient row says the membership is unsubscribed; NEITHER says "+
			"when the person withdrew or by what means, which is what an audit asks.\ncalls:\n%s",
			strings.Join(callsWithPrefix(engine, "mutation recordConsent"), "\n"))
	}
	// Under the OWNER's actor, which the signed token is the only source of
	// -- the request itself carries none.
	engine.mu.Lock()
	defer engine.mu.Unlock()
	for _, c := range engine.calls {
		if strings.HasPrefix(c.query, "mutation recordConsentWithdraw(") && c.actorID != testOwner {
			t.Errorf("the consent event was written under actor %q, want the campaign owner %q", c.actorID, testOwner)
		}
	}
}

// TestTheUnsubscribeEndpointResolvesTheAddressWithoutTheCampaign is the
// correctness half of the by-id read (memql#4829's second gap, reached from
// the other side).
//
// The address used to be resolved by reading the CAMPAIGN, taking its
// audience and walking that roster. So a click on a link whose campaign no
// longer resolves -- a deleted campaign, or one of the synthetic ids the
// single-recipient send stamps -- left the address unknown, fell to the
// row-level opt-out, and NEVER REACHED THE CLUSTER SUPPRESSION LIST. The
// person was removed from the audience that mailed them and stayed mailable
// by every other one: the exact thing the cluster list exists to prevent,
// silently, on the path a regulator looks at.
func TestTheUnsubscribeEndpointResolvesTheAddressWithoutTheCampaign(t *testing.T) {
	engine := &fakeEngine{
		// NO campaign row at all -- campaignById answers nothing.
		roster: []map[string]any{recipientRow("r-1", "person@example.test", "subscribed")},
	}
	cfg := Config{UnsubscribeSecret: trackSecretA, UnsubscribeBaseURL: "https://api.example.test"}
	h := NewUnsubscribeHandler(engine, cfg, quietLogger())

	token, err := MintUnsubscribeToken(trackSecretA, testOwner, "r-1", "rule-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, UnsubscribePath+"?token="+token, nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d", rw.Code)
	}

	digest := EmailDigest("person@example.test")
	if !wroteContaining(engine, "mutation recordSuppression", `emailDigest: "`+digest+`"`) {
		t.Errorf("the address never reached the CLUSTER suppression list, so the person stays mailable "+
			"by every other audience.\ncalls:\n%s", strings.Join(callsWithPrefix(engine, "mutation "), "\n"))
	}
	if len(callsWithPrefix(engine, "query audienceRosterForSend")) != 0 {
		t.Error("the handler walked an audience roster to find one recipient. That cost a whole-roster " +
			"read per click and was the reason the address was resolvable only when the campaign was")
	}
	if len(callsWithPrefix(engine, "query recipientById")) != 1 {
		t.Error("the recipient was not read by id")
	}
}

// TestFeedbackIngestAppendsBounceAndComplaintEvents is the provider's report.
func TestFeedbackIngestAppendsBounceAndComplaintEvents(t *testing.T) {
	cases := map[string]string{
		"hard_bounce": ConsentBounce,
		"complaint":   ConsentComplaint,
	}
	for reportKind, consentKind := range cases {
		engine := &fakeEngine{jobs: []map[string]any{jobRow()}}
		w := newTestWorker(t, engine, &recordingSender{})

		applied, err := w.applyFeedbackReport(context.TODO(), FeedbackReport{
			Email: "person@example.test", Kind: reportKind, CampaignID: testCampaign,
		})
		if err != nil || !applied {
			t.Fatalf("%s: applied=%v err=%v", reportKind, applied, err)
		}
		if !wroteContaining(engine, "mutation "+consentMutations[consentKind], `source: "provider"`) {
			t.Errorf("%s appended no %s consent event.\ncalls:\n%s", reportKind, consentKind,
				strings.Join(callsWithPrefix(engine, "mutation recordConsent"), "\n"))
		}
	}
}

// TestASoftBounceAppendsNoConsentEvent: it does not suppress and it does not
// change anybody's consent. An event for one would make the stream say a
// subscriber's permission changed because their mailbox was full.
func TestASoftBounceAppendsNoConsentEvent(t *testing.T) {
	engine := &fakeEngine{jobs: []map[string]any{jobRow()}}
	w := newTestWorker(t, engine, &recordingSender{})

	if _, err := w.applyFeedbackReport(context.TODO(), FeedbackReport{
		Email: "person@example.test", Kind: "soft_bounce", CampaignID: testCampaign,
	}); err != nil {
		t.Fatalf("applyFeedbackReport: %v", err)
	}
	if n := len(callsWithPrefix(engine, "mutation recordConsent")); n != 0 {
		t.Errorf("a soft bounce appended %d consent events", n)
	}
}

// TestAReportWithNoCampaignAppendsNothing is the stated limit. consentEvent
// is owner-tier and this path has no actor; without a campaign there is no
// owner to file the event under, and a row written under the automation's
// actor would be readable by nobody -- worse than the gap, because it looks
// like data.
func TestAReportWithNoCampaignAppendsNothing(t *testing.T) {
	engine := &fakeEngine{jobs: []map[string]any{jobRow()}}
	w := newTestWorker(t, engine, &recordingSender{})

	if _, err := w.applyFeedbackReport(context.TODO(), FeedbackReport{
		Email: "person@example.test", Kind: "hard_bounce",
	}); err != nil {
		t.Fatalf("applyFeedbackReport: %v", err)
	}
	if n := len(callsWithPrefix(engine, "mutation recordConsent")); n != 0 {
		t.Errorf("a report naming no campaign appended %d consent events", n)
	}
	// The SUPPRESSION still happens either way -- it is the audit line that
	// is missing, not the protection.
	if len(callsWithPrefix(engine, "mutation recordSuppression")) == 0 {
		t.Error("the address was not suppressed")
	}
}

// TestCampaignSuppressAppendsAnOperatorEventWithItsReason is the admin's
// decision. `reason` is required on this kind alone, because an operator
// adding an address to a cluster-wide list is the one consent transition with
// no external evidence behind it.
func TestCampaignSuppressAppendsAnOperatorEventWithItsReason(t *testing.T) {
	engine := &fakeEngine{}
	w := newTestWorker(t, engine, &recordingSender{})

	if _, err := w.handleSuppress(adminCtx(auth.RoleAdmin), map[string]any{
		"email": "person@example.test", "reason": "complaint",
	}, 0); err != nil {
		t.Fatalf("suppress: %v", err)
	}
	if !wroteContaining(engine, "mutation recordConsentSuppress", `source: "operator"`) {
		t.Fatalf("no operator consent event was appended.\ncalls:\n%s",
			strings.Join(callsWithPrefix(engine, "mutation recordConsent"), "\n"))
	}
	if !wroteContaining(engine, "mutation recordConsentSuppress", `reason: "complaint"`) {
		t.Error("the suppress event carries no reason. It is required on this kind precisely because " +
			"this is somebody's judgement rather than a report, and the stream has to record what it was")
	}
	// Under the CALLER'S OWN actor, which is the only honest owner available:
	// the suppression is cluster-wide and belongs to no campaign, so there is
	// no campaign owner to file it under, and the engine's own synthetic
	// operator would make the row readable by cluster owners alone --
	// invisible to the admin who is answerable for it.
	engine.mu.Lock()
	defer engine.mu.Unlock()
	for _, c := range engine.calls {
		if strings.HasPrefix(c.query, "mutation recordConsentSuppress(") && c.actorID != "admin-1" {
			t.Errorf("the suppress event was written under actor %q, want the admin who made the call", c.actorID)
		}
	}
}

// TestASuppressWithNoReasonIsRefusedBeforeTheEngine: the mutation would
// refuse it anyway, but refusing here names the CALLER rather than the
// construct, which is what an operator reading the log needs.
func TestASuppressWithNoReasonIsRefusedBeforeTheEngine(t *testing.T) {
	engine := &fakeEngine{}
	store := NewStore(engine)

	err := store.RecordConsent(context.TODO(), ConsentSuppress, ConsentRecord{
		EventID: "e-1", EmailDigest: strings.Repeat("a", 64), Source: "operator",
	})
	if err == nil {
		t.Fatal("a suppress event with no reason was rendered")
	}
	if n := len(callsWithPrefix(engine, "mutation recordConsentSuppress")); n != 0 {
		t.Error("the call was issued anyway")
	}
}

func TestAnUnknownConsentKindIsRefused(t *testing.T) {
	store := NewStore(&fakeEngine{})
	if err := store.RecordConsent(context.TODO(), "shrugged", ConsentRecord{EventID: "e-1"}); err == nil {
		t.Error("an unknown consent kind was accepted; the five mutations each STAMP their kind, which " +
			"is what keeps the append-only stream trustworthy without a validation step")
	}
}

// TestEveryConsentKindHasAWriter pins the map against consent.go's constants,
// so a sixth kind added to one and not the other fails here rather than at
// the first call site that needs it.
func TestEveryConsentKindHasAWriter(t *testing.T) {
	for _, kind := range []string{ConsentGrant, ConsentWithdraw, ConsentBounce, ConsentComplaint, ConsentSuppress} {
		if consentMutations[kind] == "" {
			t.Errorf("consent kind %q has no mutation", kind)
		}
	}
	if len(consentMutations) != 5 {
		t.Errorf("consentMutations has %d entries, want 5", len(consentMutations))
	}
}
