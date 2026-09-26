package http

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubconnect"
)

func TestGitHubReturnCompletesOnTheHostHoldingTheBrowserSession(t *testing.T) {
	for _, section := range []string{"settings", "deployables"} {
		t.Run(section, func(t *testing.T) {
			eng := &githubFakeEngine{state: liveState()}
			gh := newFakeGitHub()
			s, _, _ := newGitHubCallbackServer(t, eng, gh)
			eng.state["returnPath"] = "/?connect=" + section
			eng.state["flowId"] = "originating-flow"
			jar, _ := cookiejar.New(nil)
			osURL, _ := url.Parse("https://os.example.test")
			jar.SetCookies(osURL, []*http.Cookie{{Name: refreshCookieName, Value: "browser-refresh", Path: "/", Secure: true, HttpOnly: true}})
			providerReturn := callbackRequest("code=the-code&state=" + testStateValue + "&return_to=https://untrusted.example")
			providerReturn.Header.Del("Cookie")
			for _, cookie := range jar.Cookies(providerReturn.URL) {
				providerReturn.AddCookie(cookie)
			}
			if len(providerReturn.Cookies()) != 0 {
				t.Fatal("the OS host-only cookie must not reach the Identity hostname")
			}
			relay := httptest.NewRecorder()
			s.handleGitHubReturn(relay, providerReturn)
			dest, err := url.Parse(relay.Header().Get("Location"))
			if err != nil || dest.Host != osURL.Host || dest.Path != githubconnect.CompletePath || dest.Query().Get("return_to") != "" {
				t.Fatal("callback must relay only onto the configured OS completion route")
			}
			if eng.consumes != 0 || eng.creates != 0 || gh.tokenHits != 0 {
				t.Fatal("the cookieless provider hop must not consume state, exchange the code or create a grant")
			}
			if relay.Header().Get("Cache-Control") != "no-store" || relay.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("the authorization-code redirect must not be cached or used as a referrer")
			}
			completion := httptest.NewRequest(http.MethodGet, dest.String(), nil)
			for _, cookie := range jar.Cookies(dest) {
				completion.AddCookie(cookie)
			}
			result := httptest.NewRecorder()
			s.handleGitHubCallback(result, completion)
			final, _ := url.Parse(result.Header().Get("Location"))
			if final.Query().Get("connect") != section || final.Query().Get("github") != "connected" || final.Query().Get("githubFlowId") != "originating-flow" || final.Query().Get("githubCredentialId") == "" {
				t.Fatalf("connection did not return to %s with a verified account: %s", section, final)
			}
			if eng.consumes != 1 || eng.creates != 1 || gh.tokenHits != 1 {
				t.Fatal("the OS session must admit exactly one code exchange and grant")
			}
			assertNoUnknownConstructs(t, eng)
		})
	}
}

func TestGitHubCallbackRequiresTheInitiatingLiveBrowserSession(t *testing.T) {
	for _, mode := range []string{"missing cookie", "different session", "different user", "revoked session", "expired session", "missing stored binding", "missing PKCE"} {
		t.Run(mode, func(t *testing.T) {
			eng := &githubFakeEngine{state: liveState()}
			gh := newFakeGitHub()
			s, _, _ := newGitHubCallbackServer(t, eng, gh)
			eng.session = map[string]string{"id": "browser-session", "userId": "v1:identity:user:asked", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
			req := callbackRequest("code=code&state=" + testStateValue)
			switch mode {
			case "missing cookie":
				req.Header.Del("Cookie")
			case "different session":
				eng.session["id"] = "another-session"
			case "different user":
				eng.session["userId"] = "another-user"
			case "revoked session":
				eng.session["revokedAt"] = time.Now().UTC().Format(time.RFC3339)
			case "expired session":
				eng.session["expiresAt"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
			case "missing stored binding":
				eng.state["sessionId"] = ""
			case "missing PKCE":
				eng.state["pkceVerifier"] = ""
			}
			rec := httptest.NewRecorder()
			s.handleGitHubCallback(rec, req)
			if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "github=connect_state_invalid") {
				t.Fatalf("unexpected callback outcome: %d", rec.Code)
			}
			if gh.tokenHits != 0 || eng.creates != 0 || eng.updates != 0 {
				t.Fatal("unbound callback contacted token endpoint or wrote a grant")
			}
		})
	}
}

func TestTargetedGitHubReconnectRefusesAnotherIdentityAndReturnsCorrelation(t *testing.T) {
	eng := &githubFakeEngine{state: liveState(), grant: map[string]string{"id": "v1:platform:sourceCredential:existing", "kind": "github_app", "externalId": "583231"}}
	gh := newFakeGitHub()
	gh.userBody = `{"login":"other-account","id":9001}`
	s, _, _ := newGitHubCallbackServer(t, eng, gh)
	eng.state["credentialId"] = "existing"
	eng.state["expectedExternalId"] = "583231"
	eng.state["flowId"] = "client-flow"
	rec := run(t, s, "code=code&state="+testStateValue)
	dest, _ := url.Parse(rec.Header().Get("Location"))
	if dest.Query().Get("github") != "github_account_mismatch" || dest.Query().Get("githubFlowId") != "client-flow" {
		t.Fatal("wrong account did not return a correlated refusal")
	}
	if eng.creates != 0 || eng.updates != 0 || gh.instHits != 0 {
		t.Fatal("wrong GitHub identity reached grant persistence or installation discovery")
	}
}

func TestTargetedGitHubReconnectKeepsTheSelectedGrant(t *testing.T) {
	eng := &githubFakeEngine{state: liveState(), grant: map[string]string{"id": "v1:platform:sourceCredential:existing", "kind": "github_app", "externalId": "583231", "status": "revoked", "revokedAt": "2026-01-01T00:00:00Z"}}
	gh := newFakeGitHub()
	s, _, _ := newGitHubCallbackServer(t, eng, gh)
	eng.state["credentialId"] = "existing"
	eng.state["expectedExternalId"] = "583231"
	eng.state["targetRevokedAt"] = "2026-01-01T00:00:00Z"
	eng.state["flowId"] = "client-flow"
	rec := run(t, s, "code=code&state="+testStateValue)
	dest, _ := url.Parse(rec.Header().Get("Location"))
	if dest.Query().Get("github") != "reconnected" || dest.Query().Get("githubCredentialId") != "existing" || dest.Query().Get("githubFlowId") != "client-flow" {
		t.Fatal("successful reconnect did not identify its selected grant")
	}
	if eng.creates != 0 || eng.updates != 1 {
		t.Fatalf("creates=%d updates=%d", eng.creates, eng.updates)
	}
}

func TestDisconnectCancelsReconnectEvenWhenItWinsImmediatelyBeforeGrantWrite(t *testing.T) {
	eng := &githubFakeEngine{state: liveState(), grant: map[string]string{"id": "v1:platform:sourceCredential:existing", "kind": "github_app", "externalId": "583231"}}
	gh := newFakeGitHub()
	s, _, _ := newGitHubCallbackServer(t, eng, gh)
	eng.state["credentialId"] = "existing"
	eng.state["expectedExternalId"] = "583231"
	eng.state["targetRevokedAt"] = ""
	eng.state["flowId"] = "pending-reconnect"
	s.Store.GithubGate = func(ctx context.Context, key string, fn func(context.Context) error) error {
		if strings.HasPrefix(key, "grant:") {
			eng.grant["revokedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
			eng.grant["status"] = "revoked"
		}
		return fn(ctx)
	}
	rec := run(t, s, "code=code&state="+testStateValue)
	dest, _ := url.Parse(rec.Header().Get("Location"))
	if dest.Query().Get("github") != "connect_state_invalid" || dest.Query().Get("githubFlowId") != "pending-reconnect" {
		t.Fatal("cancelled reconnect did not return its correlated refusal")
	}
	if eng.creates != 0 || eng.updates != 0 {
		t.Fatal("late reconnect reactivated a disconnected grant")
	}
}
