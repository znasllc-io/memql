package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

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
