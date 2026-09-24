package http

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestShopifyReturnUsesTheOSSessionAndPreservesTheSignedQuery(t *testing.T) {
	eng := &githubFakeEngine{state: shopifyLiveState()}
	hook := &fakeShopifyHook{verify: true, writeResult: "connected"}
	first, _, _ := newShopifyCallbackServer(t, eng, hook)
	// Completion lands on another identity replica with no local flow state.
	second, _, _ := newShopifyCallbackServer(t, eng, hook)
	jar, _ := cookiejar.New(nil)
	osURL, _ := url.Parse("https://os.example.test")
	jar.SetCookies(osURL, []*http.Cookie{{Name: refreshCookieName, Value: "browser-refresh", Path: "/", Secure: true, HttpOnly: true}})
	raw := shopifyQuery() + "&extra=a%2Bb&extra=x%20y"
	req := httptest.NewRequest("GET", "https://identity.example.test/auth/shopify/callback?"+raw, nil)
	for _, cookie := range jar.Cookies(req.URL) {
		req.AddCookie(cookie)
	}
	if len(req.Cookies()) != 0 {
		t.Fatal("host-only session leaked to identity")
	}
	relay := httptest.NewRecorder()
	first.handleShopifyReturn(relay, req)
	dest, err := url.Parse(relay.Header().Get("Location"))
	if err != nil || dest.Host != osURL.Host || dest.Path != "/auth/shopify/complete" || dest.RawQuery != raw {
		t.Fatalf("signed callback was not preserved: %s", dest)
	}
	if eng.consumes != 0 || hook.order() != "" {
		t.Fatal("cookieless hop touched the connection")
	}
	if relay.Header().Get("Cache-Control") != "no-store" || relay.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("callback relay may disclose the code")
	}
	finish := httptest.NewRequest("GET", dest.String(), nil)
	for _, cookie := range jar.Cookies(dest) {
		finish.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	second.handleShopifyCallback(response, finish)
	final, _ := url.Parse(response.Header().Get("Location"))
	if final.Query().Get("shopify") != "connected" || final.Query().Get("site") != "site-1" || eng.consumes != 1 {
		t.Fatalf("connection did not complete: %s", final)
	}
	if len(hook.gotQuery["extra"]) != 2 {
		t.Fatal("signed query multiplicity was lost")
	}
}
