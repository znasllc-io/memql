package campaigns

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAForgedConfirmLinkChangesNothing(t *testing.T) {
	e := unsubscribeFixture()
	h := NewEmailLinkHandler(e, Config{UnsubscribeSecret: testSecret}, quietLogger(), nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, ConfirmPath+"?token=forged", nil))
	if rr.Code != 400 || len(e.calls) != 0 {
		t.Fatalf("status=%d calls=%v", rr.Code, e.calls)
	}
}

func emailPageToken(t *testing.T, kind LinkKind, p LinkPayload) string {
	t.Helper()
	token, err := MintLinkToken(testSecret, kind, p)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
func emailPagePayload() LinkPayload {
	return LinkPayload{OwnerUserID: testOwner, RecipientID: "r-1", AccountID: "org-1", EmailDigest: EmailDigest("leaving@example.test"), ExpiresAt: time.Now().Add(time.Hour).Unix(), TemplateID: testTemplate, TemplateRevision: "2026-10-01T00:00:00Z", SenderName: "Original sender", CampaignName: "Original campaign"}
}
func emailPageFixture() *fakeEngine {
	e := unsubscribeFixture()
	e.roster[0]["accountId"] = "org-1"
	e.accounts = map[string]map[string]any{"org-1": {"name": "Example & Co"}}
	return e
}
func pageRequest(h http.Handler, method, path, token, action string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path+"?token="+url.QueryEscape(token), strings.NewReader(url.Values{"action": {action}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}
func TestConfirmGetNeverWrites(t *testing.T) {
	for _, status := range []string{"pending", "subscribed", "unsubscribed", "bounced", "complained"} {
		t.Run(status, func(t *testing.T) {
			e := emailPageFixture()
			e.roster[0]["subscriptionStatus"] = status
			h := NewEmailLinkHandler(e, Config{UnsubscribeSecret: testSecret}, quietLogger(), nil)
			rr := pageRequest(h, "GET", ConfirmPath, emailPageToken(t, LinkConfirm, emailPagePayload()), "")
			if rr.Code != 200 {
				t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
			}
			for _, c := range e.calls {
				if strings.HasPrefix(c.query, "mutation ") {
					t.Fatal(c)
				}
			}
			if strings.Contains(rr.Body.String(), "<form") != (status == "pending") {
				t.Fatalf("wrong action for %s: %s", status, rr.Body.String())
			}
		})
	}
}
func TestAnExpiredConfirmLinkChangesNothing(t *testing.T) {
	e := emailPageFixture()
	h := NewEmailLinkHandler(e, Config{UnsubscribeSecret: testSecret}, quietLogger(), nil)
	p := emailPagePayload()
	p.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	rr := pageRequest(h, "POST", ConfirmPath, emailPageToken(t, LinkConfirm, p), "")
	if rr.Code != 410 {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	for _, c := range e.calls {
		if strings.HasPrefix(c.query, "mutation ") {
			t.Fatal(c)
		}
	}
}
func TestAViewTokenCannotConfirm(t *testing.T) {
	e := emailPageFixture()
	h := NewEmailLinkHandler(e, Config{UnsubscribeSecret: testSecret}, quietLogger(), nil)
	rr := pageRequest(h, "POST", ConfirmPath, emailPageToken(t, LinkView, emailPagePayload()), "")
	if rr.Code != 400 || len(e.calls) != 0 {
		t.Fatalf("status=%d calls=%v", rr.Code, e.calls)
	}
}
func TestConfirmPagesCarryNoScript(t *testing.T) {
	h := NewEmailLinkHandler(emailPageFixture(), Config{UnsubscribeSecret: testSecret}, quietLogger(), nil)
	rr := pageRequest(h, "GET", ConfirmPath, emailPageToken(t, LinkConfirm, emailPagePayload()), "")
	if strings.Contains(strings.ToLower(rr.Body.String()), "<script") || !strings.Contains(rr.Header().Get("Content-Security-Policy"), "default-src 'none'") || rr.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal(rr.Header(), rr.Body.String())
	}
}

// A unit gate is deliberately explicit. Database cases exercise the constructor's
// real dedicated pool and cross-replica advisory lock.
func unitEmailGate(context.Context, string, string, string) (gateHold, error) { return openGate{}, nil }

type emailPageEngine struct {
	*fakeEngine
	waiting    []map[string]any
	grants     map[string]bool
	historical map[string]any
	journey    map[string]any
	fail       string
}

func (e *emailPageEngine) Execute(ctx context.Context, q string) (any, error) {
	result, err := e.fakeEngine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	if e.fail != "" && strings.HasPrefix(q, e.fail) {
		return nil, errors.New("storage unavailable")
	}
	switch {
	case strings.HasPrefix(q, "query journeyById"):
		if e.journey != nil {
			return rowsEnvelope([]map[string]any{e.journey}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query journeyEnrollmentById"):
		for _, r := range e.waiting {
			if bare(str(r, "id")) == argOf(q, "enrollmentId") {
				return rowsEnvelope([]map[string]any{r}), nil
			}
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query waitingEnrollmentsForRecipient"):
		return rowsEnvelope(e.waiting), nil
	case strings.HasPrefix(q, "query newsletterConsentById"):
		if e.grants[argOf(q, "eventId")] {
			return rowsEnvelope([]map[string]any{{"id": argOf(q, "eventId")}}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "query templateAtRevision"):
		if e.historical != nil {
			return rowsEnvelope([]map[string]any{e.historical}), nil
		}
		return rowsEnvelope(nil), nil
	case strings.HasPrefix(q, "mutation recordConsentGrant"):
		if e.grants == nil {
			e.grants = map[string]bool{}
		}
		e.grants[argOf(q, "eventId")] = true
	case strings.HasPrefix(q, "mutation confirmRecipientSubscription"):
		e.roster[0]["subscriptionStatus"] = "subscribed"
	case strings.HasPrefix(q, "mutation restartRecipientMembership"):
		e.roster[0]["subscriptionStatus"] = argOf(q, "subscriptionStatus")
		e.roster[0]["joinedAt"] = argOf(q, "joinedAt")
	case strings.HasPrefix(q, "mutation setRecipientSubscription"):
		e.roster[0]["subscriptionStatus"] = argOf(q, "subscriptionStatus")
	case strings.HasPrefix(q, "mutation liftOrganizationSuppression"):
		e.suppression[argOf(q, "suppressionId")]["liftedAt"] = argOf(q, "liftedAt")
	case strings.HasPrefix(q, "mutation recordOrganizationSuppression"):
		if e.suppression == nil {
			e.suppression = map[string]map[string]any{}
		}
		e.suppression[argOf(q, "suppressionId")] = map[string]any{"accountId": argOf(q, "accountId"), "reason": "unsubscribed", "suppressedAt": time.Now().Format(time.RFC3339Nano), "liftedAt": ""}
	}
	return result, nil
}
func newEmailPageEngine(status string) (*emailPageEngine, *EmailLinkHandler) {
	e := &emailPageEngine{fakeEngine: emailPageFixture()}
	e.roster[0]["subscriptionStatus"] = status
	h := NewEmailLinkHandler(e, Config{UnsubscribeSecret: testSecret, UnsubscribeBaseURL: "https://example.test"}, quietLogger(), nil)
	h.gate = unitEmailGate
	h.enrollments = unitEmailGate
	return e, h
}
func TestConfirmPostConfirmsAndNudges(t *testing.T) {
	e, h := newEmailPageEngine("pending")
	e.roster[0]["joinedAt"] = "2026-10-01T00:00:00Z"
	e.journey = map[string]any{"id": "journey", "ownerUserId": testOwner, "accountId": "org-1", "status": "active", "trigger": map[string]any{"kind": "joined", "audienceId": testAudience}, "steps": []map[string]any{{"id": "confirm", "kind": "confirm", "timeoutDays": 7}, {"id": "welcome", "kind": "send"}}}
	e.waiting = []map[string]any{{"id": journeyEnrollmentID("journey", "r-1", parseTime("2026-10-01T00:00:00Z")), "journeyId": "journey", "ownerUserId": testOwner, "accountId": "org-1", "recipientId": "r-1", "emailDigest": emailPagePayload().EmailDigest, "status": "waiting", "stepId": "confirm", "stepIndex": 0, "createdAt": "2026-10-01T00:00:00Z"}}
	token := emailPageToken(t, LinkConfirm, emailPagePayload())
	for i := 0; i < 2; i++ {
		rr := pageRequest(h, "POST", ConfirmPath, token, "")
		if rr.Code != 200 {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
	}
	var writes []string
	for _, c := range e.calls {
		if strings.HasPrefix(c.query, "mutation ") {
			writes = append(writes, strings.Fields(c.query)[1])
		}
	}
	if len(e.mutations("recordConsentGrant")) != 1 || len(e.mutations("confirmRecipientSubscription")) != 1 || len(e.mutations("nudgeJourneyEnrollment")) != 2 {
		t.Fatal(writes)
	}
	if !strings.HasPrefix(writes[0], "recordConsentGrant(") || !strings.HasPrefix(writes[1], "confirmRecipientSubscription(") || argOf(e.mutations("recordConsentGrant")[0].query, "source") != ConsentSourceDoubleOptIn {
		t.Fatal(writes)
	}
}
func TestConfirmRetryAfterTheGrantDoesNotGrantTwice(t *testing.T) {
	e, h := newEmailPageEngine("pending")
	e.fail = "mutation confirmRecipientSubscription"
	token := emailPageToken(t, LinkConfirm, emailPagePayload())
	rr := pageRequest(h, "POST", ConfirmPath, token, "")
	if rr.Code != 503 || rr.Header().Get("Retry-After") != "30" {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	e.fail = ""
	rr = pageRequest(h, "POST", ConfirmPath, token, "")
	if rr.Code != 200 || len(e.mutations("recordConsentGrant")) != 1 {
		t.Fatal(rr.Code, e.calls)
	}
}
func TestPreferencesGetShowsTheStateWithoutWriting(t *testing.T) {
	for _, status := range []string{"pending", "subscribed", "unsubscribed", "bounced", "complained"} {
		t.Run(status, func(t *testing.T) {
			e, h := newEmailPageEngine(status)
			e.suppression = map[string]map[string]any{organizationSuppressionID("org-1", emailPagePayload().EmailDigest): {"accountId": "org-1", "reason": "unsubscribed", "suppressedAt": "2026-10-01T00:00:00Z"}}
			if status != "unsubscribed" {
				e.suppression = nil
			}
			rr := pageRequest(h, "GET", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "")
			if rr.Code != 200 {
				t.Fatal(rr.Code, rr.Body.String())
			}
			for _, c := range e.calls {
				if strings.HasPrefix(c.query, "mutation ") {
					t.Fatal(c)
				}
			}
			if strings.Contains(rr.Body.String(), "<form") != (status == "subscribed" || status == "pending" || status == "unsubscribed") {
				t.Fatal(rr.Body.String())
			}
		})
	}
}
func TestPreferencesUnsubscribe(t *testing.T) {
	e, h := newEmailPageEngine("subscribed")
	rr := pageRequest(h, "POST", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "unsubscribe")
	if rr.Code != 200 || len(e.mutations("recordOrganizationSuppression")) != 1 || len(e.mutations("recordConsentWithdraw")) != 1 || argOf(e.mutations("recordConsentWithdraw")[0].query, "source") != ConsentSourcePreferences || str(e.roster[0], "subscriptionStatus") != "unsubscribed" {
		t.Fatal(rr.Code, e.calls)
	}
}
func TestABounceCannotBeUndoneFromThePage(t *testing.T) {
	for _, status := range []string{"pending", "unsubscribed", "bounced", "complained"} {
		e, h := newEmailPageEngine(status)
		digest := emailPagePayload().EmailDigest
		e.suppression = map[string]map[string]any{digest: {"reason": "hard_bounce"}, organizationSuppressionID("org-1", digest): {"accountId": "org-1", "reason": "unsubscribed"}}
		rr := pageRequest(h, "POST", PreferencesPath, emailPageToken(t, LinkPreferences, emailPagePayload()), "subscribe")
		if rr.Code != 200 || strings.Contains(rr.Body.String(), "<form") {
			t.Fatal(rr.Code, rr.Body.String())
		}
		for _, c := range e.calls {
			if strings.HasPrefix(c.query, "mutation ") {
				t.Fatal(c)
			}
		}
	}
}
func TestAForgedPreferencesLinkChangesNothing(t *testing.T) {
	e, h := newEmailPageEngine("unsubscribed")
	rr := pageRequest(h, "POST", PreferencesPath, "forged", "subscribe")
	if rr.Code != 400 || len(e.calls) != 0 {
		t.Fatal(rr.Code, e.calls)
	}
}
func TestEmailPagesRefuseAChangedMailboxOrOrganization(t *testing.T) {
	for _, path := range []string{ConfirmPath, PreferencesPath, ViewPath} {
		for _, field := range []string{"email", "accountId"} {
			e, h := newEmailPageEngine("pending")
			e.roster[0][field] = "replacement"
			kind := LinkConfirm
			if path == PreferencesPath {
				kind = LinkPreferences
			}
			if path == ViewPath {
				kind = LinkView
			}
			rr := pageRequest(h, "GET", path, emailPageToken(t, kind, emailPagePayload()), "")
			if rr.Code != 400 {
				t.Fatal(path, field, rr.Code)
			}
		}
	}
}
func TestViewShowsTheMessageAsSent(t *testing.T) {
	e, h := newEmailPageEngine("subscribed")
	e.roster[0]["displayName"] = "Reader <One>"
	e.template = map[string]any{"textBody": "new text"}
	e.historical = map[string]any{"id": testTemplate, "accountId": "org-1", "createdAt": emailPagePayload().TemplateRevision, "subject": "Old subject", "htmlBody": "<p>Old content {{displayName}} / {{campaignName}}</p>"}
	rr := pageRequest(h, "GET", ViewPath, emailPageToken(t, LinkView, emailPagePayload()), "")
	body := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, "Old content Reader &lt;One&gt; / Original campaign") || !strings.Contains(body, "Original sender") || strings.Contains(body, "/t/o/") || strings.Contains(body, "new text") {
		t.Fatal(rr.Code, body)
	}
	for _, c := range e.calls {
		if strings.HasPrefix(c.query, "mutation ") {
			t.Fatal(c)
		}
	}
	if !strings.Contains(rr.Header().Get("Content-Security-Policy"), "img-src data: https:") {
		t.Fatal(rr.Header())
	}
}
func TestViewEscapesTextAndRefusesPost(t *testing.T) {
	e, h := newEmailPageEngine("subscribed")
	e.historical = map[string]any{"id": testTemplate, "accountId": "org-1", "createdAt": emailPagePayload().TemplateRevision, "textBody": "<script>alert(1)</script>"}
	token := emailPageToken(t, LinkView, emailPagePayload())
	rr := pageRequest(h, "GET", ViewPath, token, "")
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "<script>") || !strings.Contains(rr.Body.String(), "<pre>") {
		t.Fatal(rr.Code, rr.Body.String())
	}
	before := len(e.calls)
	rr = pageRequest(h, "POST", ViewPath, token, "")
	if rr.Code != 405 || len(e.calls) != before {
		t.Fatal(rr.Code, e.calls)
	}
}
func TestAConfirmTokenCannotView(t *testing.T) {
	e, h := newEmailPageEngine("subscribed")
	rr := pageRequest(h, "GET", ViewPath, emailPageToken(t, LinkConfirm, emailPagePayload()), "")
	if rr.Code != 400 || len(e.calls) != 0 {
		t.Fatal(rr.Code, e.calls)
	}
}
func TestPreferencesRetryAfterLiftRetainsConsentAndFinishes(t *testing.T) {
	e, h := newEmailPageEngine("unsubscribed")
	digest := emailPagePayload().EmailDigest
	e.suppression = map[string]map[string]any{organizationSuppressionID("org-1", digest): {"accountId": "org-1", "reason": "unsubscribed", "suppressedAt": "2026-10-01T00:00:00Z"}}
	e.fail = "mutation restartRecipientMembership"
	token := emailPageToken(t, LinkPreferences, emailPagePayload())
	rr := pageRequest(h, "POST", PreferencesPath, token, "subscribe")
	if rr.Code != 503 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	e.fail = ""
	rr = pageRequest(h, "POST", PreferencesPath, token, "subscribe")
	if rr.Code != 200 || len(e.mutations("recordConsentGrant")) != 1 || str(e.roster[0], "subscriptionStatus") != "subscribed" {
		t.Fatal(rr.Code, e.calls)
	}
}

type lostEmailGate struct{ openGate }

func (lostEmailGate) Verify(context.Context) error { return errors.New("held session was lost") }
func TestEmailChoiceRefusesWritesAfterLosingItsGate(t *testing.T) {
	e, h := newEmailPageEngine("pending")
	h.gate = func(context.Context, string, string, string) (gateHold, error) { return lostEmailGate{}, nil }
	rr := pageRequest(h, "POST", ConfirmPath, emailPageToken(t, LinkConfirm, emailPagePayload()), "")
	if rr.Code != 503 {
		t.Fatal(rr.Code)
	}
	for _, c := range e.calls {
		if strings.HasPrefix(c.query, "mutation ") {
			t.Fatal(c)
		}
	}
}
func TestEmailChoiceRequiresItsDedicatedGatePool(t *testing.T) {
	e := emailPageFixture()
	e.roster[0]["subscriptionStatus"] = "pending"
	h := NewEmailLinkHandler(e, Config{UnsubscribeSecret: testSecret}, quietLogger(), nil)
	rr := pageRequest(h, "POST", ConfirmPath, emailPageToken(t, LinkConfirm, emailPagePayload()), "")
	if rr.Code != 503 || len(e.calls) != 0 {
		t.Fatal(rr.Code, e.calls)
	}
}

func TestLateConfirmationNeverReopensAnOldMembership(t *testing.T) {
	e, h := newEmailPageEngine("subscribed")
	oldJoin := parseTime("2026-10-01T00:00:00Z")
	e.roster[0]["joinedAt"] = oldJoin.Add(time.Hour).Format(time.RFC3339Nano)
	e.journey = map[string]any{"id": "journey", "ownerUserId": testOwner, "accountId": "org-1", "status": "active", "trigger": map[string]any{"kind": "joined", "audienceId": testAudience}, "steps": []map[string]any{{"id": "confirm", "kind": "confirm", "timeoutDays": 7}, {"id": "welcome", "kind": "send"}}}
	e.waiting = []map[string]any{{"id": journeyEnrollmentID("journey", "r-1", oldJoin), "journeyId": "journey", "ownerUserId": testOwner, "accountId": "org-1", "recipientId": "r-1", "emailDigest": emailPagePayload().EmailDigest, "status": "exited", "exitReason": "not confirmed", "stepId": "confirm", "stepIndex": 0, "createdAt": "2026-10-01T00:00:00Z"}}
	rr := pageRequest(h, "POST", ConfirmPath, emailPageToken(t, LinkConfirm, emailPagePayload()), "")
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	if len(e.mutations("advanceJourneyEnrollment")) != 0 || len(e.mutations("nudgeJourneyEnrollment")) != 0 {
		t.Fatal("old joinedAt enrollment reopened", e.calls)
	}
}
