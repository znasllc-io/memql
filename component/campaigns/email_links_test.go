package campaigns

import (
	"context"
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
func unitEmailGate(context.Context, string) (func(), error) { return func() {}, nil }
