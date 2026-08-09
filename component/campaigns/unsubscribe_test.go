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

// unsubscribe_test.go -- the opt-out path, including the two properties
// that are security rather than behaviour:
//
//   - a GET NEVER unsubscribes (link scanners and mail clients prefetch,
//     which is exactly why RFC 8058 specifies POST);
//   - the impersonated identity comes out of the SIGNED token, so no
//     query parameter can aim it at another operator's rows.

const testSecret = "unsubscribe-signing-secret-for-tests"

func handlerFor(engine Engine) *UnsubscribeHandler {
	w := &Worker{}
	return &UnsubscribeHandler{
		store:  NewStore(engine),
		cfg:    Config{UnsubscribeSecret: testSecret, UnsubscribeBaseURL: "https://example.test"},
		logger: quietLogger(),
		system: w.systemActorContext,
		now:    time.Now,
	}
}

func unsubscribeFixture() *fakeEngine {
	return &fakeEngine{
		campaign: campaignRow(),
		roster:   []map[string]any{recipientRow("r-1", "leaving@example.test", "subscribed")},
	}
}

func validToken(t *testing.T) string {
	t.Helper()
	tok, err := MintUnsubscribeToken(testSecret, testOwner, "r-1", testCampaign)
	if err != nil {
		t.Fatalf("MintUnsubscribeToken: %v", err)
	}
	return tok
}

func TestGetRendersAndDoesNotUnsubscribe(t *testing.T) {
	engine := unsubscribeFixture()
	h := handlerFor(engine)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/unsubscribe?token="+url.QueryEscape(validToken(t)), nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "<form method=\"POST\"") {
		t.Error("the confirmation page has no POST form, so a person clicking the link has nothing to confirm with")
	}
	if n := len(engine.mutations("recordSuppression")) + len(engine.mutations("setRecipientSubscription")); n != 0 {
		t.Fatalf("a GET performed %d writes. Mail clients and security appliances PREFETCH links, so a GET with a "+
			"side effect silently unsubscribes people who never clicked", n)
	}
}

func TestPostUnsubscribesAndSuppressesClusterWide(t *testing.T) {
	engine := unsubscribeFixture()
	h := handlerFor(engine)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/unsubscribe?token="+url.QueryEscape(validToken(t)), nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	sup := engine.mutations("recordSuppression")
	if len(sup) != 1 {
		t.Fatalf("want one cluster-wide suppression, got %d", len(sup))
	}
	if got := argOf(sup[0].query, "emailDigest"); got != EmailDigest("leaving@example.test") {
		t.Errorf("suppression digest = %q, want the digest of the address resolved from the recipient row", got)
	}
	if argOf(sup[0].query, "reason") != "unsubscribed" {
		t.Errorf("reason = %q, want unsubscribed", argOf(sup[0].query, "reason"))
	}
	if !sup[0].isOwner || sup[0].actorID != systemCampaignsActor {
		t.Errorf("the suppression was written under %q (clusterOwner=%v); it must be the engine identity, because the "+
			"list is clusterOwner-tier", sup[0].actorID, sup[0].isOwner)
	}

	rec := engine.mutations("setRecipientSubscription")
	if len(rec) != 1 || argOf(rec[0].query, "subscriptionStatus") != "unsubscribed" {
		t.Fatalf("the operator's own recipient row was not updated: %+v", rec)
	}
	if rec[0].actorID != testOwner {
		t.Errorf("the recipient row was written under %q, want the owner %q -- only the owner may write the owner's row",
			rec[0].actorID, testOwner)
	}
}

func TestPostAcceptsTheTokenInTheFormBody(t *testing.T) {
	engine := unsubscribeFixture()
	h := handlerFor(engine)

	body := strings.NewReader("token=" + url.QueryEscape(validToken(t)))
	req := httptest.NewRequest(http.MethodPost, "/unsubscribe", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if len(engine.mutations("recordSuppression")) != 1 {
		t.Fatal("the confirmation page's own button (which posts a form body) did not work")
	}
}

func TestTamperedTokenIsRefusedAndWritesNothing(t *testing.T) {
	for _, name := range []string{"flipped id", "truncated tag", "empty", "garbage"} {
		engine := unsubscribeFixture()
		h := handlerFor(engine)
		var tok string
		switch name {
		case "flipped id":
			// Re-encode with a DIFFERENT owner, keeping the original tag:
			// the shape an attacker would try to aim the impersonation.
			good := validToken(t)
			parts := strings.Split(good, ".")
			forged, _ := MintUnsubscribeToken(testSecret, "someone-else", "r-1", testCampaign)
			tok = strings.Join(append(strings.Split(forged, ".")[:4], parts[4]), ".")
		case "truncated tag":
			good := validToken(t)
			tok = good[:len(good)-1]
		case "empty":
			tok = ""
		default:
			tok = "u1.aaa.bbb.ccc.ddd"
		}

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/unsubscribe?token="+url.QueryEscape(tok), nil))

		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rr.Code)
		}
		if n := len(engine.mutations("recordSuppression")) + len(engine.mutations("setRecipientSubscription")); n != 0 {
			t.Errorf("%s: an unverified token produced %d writes", name, n)
		}
	}
}

// TestTokenBindsAllThreeIds -- every field is covered by the tag, so
// none of them can be swapped in transit. campaignId matters as much as
// the other two: it is what attributes a complaint to a send.
func TestTokenBindsAllThreeIds(t *testing.T) {
	tok, err := MintUnsubscribeToken(testSecret, "owner-a", "rec-a", "camp-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	owner, recipient, campaign, err := ParseUnsubscribeToken(testSecret, tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if owner != "owner-a" || recipient != "rec-a" || campaign != "camp-a" {
		t.Fatalf("round trip lost a field: %q %q %q", owner, recipient, campaign)
	}
	if _, _, _, err := ParseUnsubscribeToken("a-different-secret", tok); err == nil {
		t.Error("a token verified under the wrong secret")
	}
}

// TestTokenSeparatorCannotBeSmuggled -- the ids are base64url-encoded
// rather than escaped, so an id containing the "." separator cannot make
// two different triples produce one token body. Same injectivity argument
// the DSL's composite-id derivation makes about its separator.
func TestTokenSeparatorCannotBeSmuggled(t *testing.T) {
	a, err := MintUnsubscribeToken(testSecret, "x.y", "z", "c")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	b, err := MintUnsubscribeToken(testSecret, "x", "y.z", "c")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if a == b {
		t.Fatal("two different (owner, recipient) pairs produced the same token")
	}
	owner, recipient, _, err := ParseUnsubscribeToken(testSecret, a)
	if err != nil || owner != "x.y" || recipient != "z" {
		t.Fatalf("a separator-bearing id did not round-trip: %q %q %v", owner, recipient, err)
	}
}

// TestUnsubscribeWithNoResolvableAddressStillOptsOut -- the recipient row
// may be gone. The cluster list cannot be updated (no address, no
// digest), but the person must still leave the audience that mailed them.
func TestUnsubscribeWithNoResolvableAddressStillOptsOut(t *testing.T) {
	engine := &fakeEngine{campaign: campaignRow(), roster: nil}
	h := handlerFor(engine)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/unsubscribe?token="+url.QueryEscape(validToken(t)), nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if len(engine.mutations("setRecipientSubscription")) != 1 {
		t.Error("no row-level opt-out happened, so the recipient stays in the audience that mailed them")
	}
}

// TestUnsubscribePageIsFramingSafe -- this page is reached from an email,
// which is the classic setup for clickjacking an irreversible action.
func TestUnsubscribePageIsFramingSafe(t *testing.T) {
	h := handlerFor(unsubscribeFixture())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/unsubscribe?token="+url.QueryEscape(validToken(t)), nil))

	csp := rr.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP %q does not forbid framing", csp)
	}
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP %q does not deny by default", csp)
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Error("the page is cacheable; a shared cache would serve one recipient's token page to another")
	}
}

func TestUnsupportedMethodIsRefused(t *testing.T) {
	h := handlerFor(unsubscribeFixture())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/unsubscribe", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

var _ = context.Background
