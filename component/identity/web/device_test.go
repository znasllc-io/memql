package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/devicecode"
)

// The verification page (memql#3410). What is worth pinning here is not
// the HTML but the three behaviours a user's safety rests on:
//
//  1. A signed-out visitor is sent through login and comes BACK here
//     with their code, rather than being dumped on /admin/.
//  2. The approval screen names the client, the source IP and the user
//     agent -- without those a person cannot tell their own request
//     from an attacker's, and the whole grant becomes a phishing tool.
//  3. Approve and Deny reach the store under the SIGNED-IN user's
//     actor, and a code that cannot be answered never renders buttons.

type fakeDeviceAdapter struct {
	mu sync.Mutex

	row *devicecode.Row
	// lookupErr, when set, is returned instead of a row.
	lookupErr error

	approvedId string
	deniedId   string
	// actorAt records the actor.userId the engine would have seen on
	// the transition call. Empty means the context carried no actor.
	actorAt string
}

func (f *fakeDeviceAdapter) LookupByUserCodeHash(_ context.Context, hash string) (*devicecode.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	if f.row == nil || hash == "" || f.row.UserCodeHash != hash {
		return nil, nil
	}
	cp := *f.row
	return &cp, nil
}

func (f *fakeDeviceAdapter) Approve(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvedId = id
	f.actorAt = actorUserId(ctx)
	return nil
}

func (f *fakeDeviceAdapter) Deny(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deniedId = id
	f.actorAt = actorUserId(ctx)
	return nil
}

func actorUserId(ctx context.Context) string {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return ""
	}
	v, _ := auth.ActorEnvelopeValue(ac, "userId")
	s, _ := v.(string)
	return s
}

const (
	deviceTestUser     = "v1:identity:user:approver-3410"
	deviceTestUserCode = "ABCD-2345"
)

// newDeviceWebServer returns a web Server with the device flow wired, a
// real JWT issuer (so the auth gate exercises real verification), and a
// pending row keyed on deviceTestUserCode.
func newDeviceWebServer(t *testing.T) (*Server, *fakeDeviceAdapter, string) {
	t.Helper()
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	if err != nil {
		t.Fatalf("new key manager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("load keys: %v", err)
	}
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		KeyDir:      dir,
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	srv, err := NewServer(cfg, slog.Default(), nil)
	if err != nil {
		t.Fatalf("new web server: %v", err)
	}
	adapter := &fakeDeviceAdapter{
		row: &devicecode.Row{
			ID:              "v1:identity:deviceCode:dc-1",
			ClientId:        "vscode-memql",
			UserCodeHash:    devicecode.HashUserCode(deviceTestUserCode),
			Status:          devicecode.StatusPending,
			ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
			IntervalSeconds: devicecode.DefaultIntervalSeconds,
			SourceIP:        "203.0.113.9",
			UserAgent:       "memql-vscode/1.2.3 (linux)",
			CreatedAt:       time.Now().UTC(),
		},
	}
	srv.SetDeviceFlow(&DeviceFlow{
		Adapter: adapter,
		Issuer:  iss,
		ClientName: func(_ context.Context, clientId string) string {
			if clientId == "vscode-memql" {
				return "memQL for VS Code"
			}
			return ""
		},
	})

	token, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId:    deviceTestUser,
		SessionId: "sess-3410",
		Role:      "owner",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}
	return srv, adapter, token
}

func deviceGet(s *Server, token, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/device"+query, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.handleDeviceGet(rec, req)
	return rec
}

func devicePost(s *Server, token string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/device", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.handleDevicePost(rec, req)
	return rec
}

// A signed-out visitor on verification_uri_complete must come back to
// /device WITH the code, not to /admin/. return_to alone cannot do this
// -- /device is first-party, so the magic-link flow classifies the
// sign-in as an admin session and hardcodes /admin/ -- which is why the
// post-login cookie exists.
func TestDevicePageBouncesSignedOutVisitorBackToItself(t *testing.T) {
	s, _, _ := newDeviceWebServer(t)

	rec := deviceGet(s, "", "?user_code="+url.QueryEscape(deviceTestUserCode))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login?") {
		t.Fatalf("Location = %q, want a bounce to /login", loc)
	}

	var postLogin string
	for _, c := range rec.Result().Cookies() {
		if c.Name == identity.PostLoginCookieName {
			postLogin = c.Value
		}
	}
	if postLogin == "" {
		t.Fatal("no post-login cookie was set; the visitor would land on /admin/ after signing in")
	}
	want := "/device?user_code=" + url.QueryEscape(deviceTestUserCode)
	if postLogin != want {
		t.Fatalf("post-login destination = %q, want %q", postLogin, want)
	}
	// And /auth/complete's consumer must accept it.
	if identity.SafeRelativeRedirect(postLogin) != want {
		t.Fatalf("the stashed destination does not survive same-origin validation: %q", postLogin)
	}
}

// A signed-in visitor arriving with a live code gets the approval panel,
// and that panel carries the evidence needed to judge the request.
func TestDeviceApprovalPanelNamesClientSourceIPAndUserAgent(t *testing.T) {
	s, _, token := newDeviceWebServer(t)

	rec := deviceGet(s, token, "?user_code="+url.QueryEscape(deviceTestUserCode))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"memQL for VS Code",          // the client's human name
		"vscode-memql",               // and the id the session actually binds to
		"203.0.113.9",                // the source IP
		"memql-vscode/1.2.3 (linux)", // the user agent
		deviceTestUserCode,           // the code, to compare against the device
		"Approve",
		"Deny",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("approval page does not mention %q\n---\n%s", want, body)
		}
	}
}

// The lookup tolerates the code being typed the way a person types it.
func TestDeviceLookupIsCaseAndSeparatorInsensitive(t *testing.T) {
	s, _, token := newDeviceWebServer(t)

	for _, typed := range []string{"abcd-2345", "ABCD2345", " abcd 2345 "} {
		rec := devicePost(s, token, url.Values{
			"user_code": {typed},
			"action":    {"lookup"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("typed %q: status = %d, want 200", typed, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Approve") {
			t.Fatalf("typed %q did not resolve to the approval panel\n---\n%s", typed, rec.Body.String())
		}
	}
}

func TestDeviceApproveAndDenyRunUnderTheSignedInActor(t *testing.T) {
	t.Run("approve", func(t *testing.T) {
		s, adapter, token := newDeviceWebServer(t)
		rec := devicePost(s, token, url.Values{
			"user_code": {deviceTestUserCode},
			"action":    {"approve"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if adapter.approvedId != "v1:identity:deviceCode:dc-1" {
			t.Fatalf("approved id = %q, want the resolved row", adapter.approvedId)
		}
		if adapter.deniedId != "" {
			t.Fatal("approve also denied the row")
		}
		// The mutation stamps approvedByUserId from actor.userId, so an
		// unstamped context would record an empty approver.
		if adapter.actorAt != deviceTestUser {
			t.Fatalf("actor.userId at the transition = %q, want %q", adapter.actorAt, deviceTestUser)
		}
		if !strings.Contains(rec.Body.String(), "Device approved") {
			t.Fatalf("expected the approved confirmation\n---\n%s", rec.Body.String())
		}
	})

	t.Run("deny", func(t *testing.T) {
		s, adapter, token := newDeviceWebServer(t)
		rec := devicePost(s, token, url.Values{
			"user_code": {deviceTestUserCode},
			"action":    {"deny"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if adapter.deniedId != "v1:identity:deviceCode:dc-1" {
			t.Fatalf("denied id = %q, want the resolved row", adapter.deniedId)
		}
		if adapter.approvedId != "" {
			t.Fatal("deny also approved the row")
		}
		if adapter.actorAt != deviceTestUser {
			t.Fatalf("actor.userId at the transition = %q, want %q", adapter.actorAt, deviceTestUser)
		}
		if !strings.Contains(rec.Body.String(), "Request denied") {
			t.Fatalf("expected the denied confirmation\n---\n%s", rec.Body.String())
		}
	})
}

// An unanswerable code renders the entry form with a reason, never
// buttons that would do nothing. The four reasons stay distinct because
// they send the human to four different next steps.
func TestDeviceUnanswerableCodesRenderAReasonNotButtons(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*devicecode.Row)
		wantText string
	}{
		{"expired", func(r *devicecode.Row) { r.ExpiresAt = time.Now().UTC().Add(-time.Minute) }, "expired"},
		{"already approved", func(r *devicecode.Row) { r.Status = devicecode.StatusApproved }, "already approved"},
		{"already denied", func(r *devicecode.Row) { r.Status = devicecode.StatusDenied }, "already denied"},
		{"already used", func(r *devicecode.Row) { r.Status = devicecode.StatusRedeemed }, "already been used"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, adapter, token := newDeviceWebServer(t)
			c.mutate(adapter.row)

			rec := devicePost(s, token, url.Values{
				"user_code": {deviceTestUserCode},
				"action":    {"approve"},
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, c.wantText) {
				t.Fatalf("page does not explain %q\n---\n%s", c.wantText, body)
			}
			if adapter.approvedId != "" || adapter.deniedId != "" {
				t.Fatal("an unanswerable code was still transitioned")
			}
		})
	}
}

func TestDeviceUnknownCodeIsRefused(t *testing.T) {
	s, adapter, token := newDeviceWebServer(t)

	// Well-formed but not ours.
	rec := devicePost(s, token, url.Values{"user_code": {"ZZZZ-9999"}, "action": {"approve"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "recognize that code") {
		t.Fatalf("expected a not-recognized message\n---\n%s", rec.Body.String())
	}
	// Not even well-formed.
	rec = devicePost(s, token, url.Values{"user_code": {"nope"}, "action": {"approve"}})
	if !strings.Contains(rec.Body.String(), "look like a valid code") {
		t.Fatalf("expected a malformed-code message\n---\n%s", rec.Body.String())
	}
	if adapter.approvedId != "" || adapter.deniedId != "" {
		t.Fatal("an unknown code was transitioned")
	}
}

// Both device endpoints are per-IP rate limited. On the page that
// matters because the page is a code ORACLE: each submission reports
// whether a user_code exists and what state it is in, so unbounded
// attempts turn the 40-bit code into a guessing game.
func TestDevicePageIsRateLimitedPerIP(t *testing.T) {
	t.Setenv(envDeviceVerifyPerHour, "3")
	s, _, token := newDeviceWebServer(t)

	for i := 0; i < 3; i++ {
		if rec := deviceGet(s, token, ""); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 while inside the budget", i, rec.Code)
		}
	}
	rec := deviceGet(s, token, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the budget is spent", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 must carry Retry-After so a client knows when to come back")
	}
}
