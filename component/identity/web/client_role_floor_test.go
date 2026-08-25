package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/devicecode"
	"github.com/znasllc-io/memql/component/identity/magiclink"
)

// client_role_floor_test.go -- memql#4516.
//
// The editor is a management surface, so the built-in editor client declares a
// role floor: owner, admin and developer complete sign-in; writer and reader
// are refused. The rule lives in ONE function
// (identity.CheckClientRoleFloor); these tests pin the two places a signed-in
// person is known at the moment a credential would be minted -- the device
// approval, and the /authorize SSO fast path -- plus the fact that a client
// which declares no floor is untouched.
//
// The magic-link and passkey halves of the code flow are pinned beside their
// own code (magiclink/role_floor_test.go, http/webauthn_role_floor_test.go).

// find returns the first recorded event with the given action, or nil. The type
// itself lives in enroll_test.go -- one capture sink for the package.
func (a *capturingAudit) find(action string) *identity.AuditEvent {
	for i := range a.events {
		if a.events[i].Action == action {
			return &a.events[i]
		}
	}
	return nil
}

const roleFloorUserCode = "WXYZ-6789"

// newRoleFloorDeviceServer is newDeviceWebServer parametrised on the two
// things this file varies: the client the device grant names, and the role the
// approver holds.
func newRoleFloorDeviceServer(t *testing.T, clientId, role string) (*Server, *fakeDeviceAdapter, *capturingAudit, string) {
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
			ID:              "v1:identity:deviceCode:dc-floor",
			ClientId:        clientId,
			UserCodeHash:    devicecode.HashUserCode(roleFloorUserCode),
			Status:          devicecode.StatusPending,
			ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
			IntervalSeconds: devicecode.DefaultIntervalSeconds,
			SourceIP:        "203.0.113.9",
			UserAgent:       "memql-vscode/1.2.3 (linux)",
			CreatedAt:       time.Now().UTC(),
		},
	}
	audit := &capturingAudit{}
	srv.SetDeviceFlow(&DeviceFlow{Adapter: adapter, Issuer: iss, Audit: audit})

	token, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId:    "v1:identity:user:floor-" + role,
		SessionId: "sess-floor",
		Role:      role,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}
	return srv, adapter, audit, token
}

func approveRoleFloorDevice(s *Server, token string) *httptest.ResponseRecorder {
	return devicePost(s, token, url.Values{
		"user_code": {roleFloorUserCode},
		"action":    {"approve"},
	})
}

func TestDeviceApproval_AdmitsDeveloperAndAbove(t *testing.T) {
	for _, role := range []string{"owner", "admin", "developer"} {
		t.Run(role, func(t *testing.T) {
			s, adapter, _, token := newRoleFloorDeviceServer(t, identity.BuiltinClientVSCode, role)
			rec := approveRoleFloorDevice(s, token)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			adapter.mu.Lock()
			approved, denied := adapter.approvedId, adapter.deniedId
			adapter.mu.Unlock()
			if approved != "v1:identity:deviceCode:dc-floor" {
				t.Fatalf("role %q was not approved (approvedId=%q, deniedId=%q)", role, approved, denied)
			}
			if denied != "" {
				t.Fatalf("role %q was also denied: %q", role, denied)
			}
		})
	}
}

func TestDeviceApproval_RefusesBelowDeveloper(t *testing.T) {
	for _, role := range []string{"writer", "reader"} {
		t.Run(role, func(t *testing.T) {
			s, adapter, audit, token := newRoleFloorDeviceServer(t, identity.BuiltinClientVSCode, role)
			rec := approveRoleFloorDevice(s, token)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (the page renders the refusal); body=%s", rec.Code, rec.Body.String())
			}

			adapter.mu.Lock()
			approved, denied := adapter.approvedId, adapter.deniedId
			adapter.mu.Unlock()
			if approved != "" {
				t.Fatalf("role %q was APPROVED; the editor floor is developer and above", role)
			}
			// The grant is denied rather than left pending: the device is
			// polling, and a decision it never hears about reads to the user
			// as a timeout.
			if denied != "v1:identity:deviceCode:dc-floor" {
				t.Fatalf("a refused grant must be denied so the device stops polling; deniedId=%q", denied)
			}

			// The page names the role and the requirement.
			body := rec.Body.String()
			for _, want := range []string{role, "developer"} {
				if !strings.Contains(body, want) {
					t.Errorf("refusal page does not mention %q; body=%s", want, body)
				}
			}

			ev := audit.find(identity.AuditActionRoleFloorRefused)
			if ev == nil {
				t.Fatalf("no %q audit event was written", identity.AuditActionRoleFloorRefused)
			}
			if ev.Outcome != identity.AuditOutcomeBlocked {
				t.Errorf("audit outcome = %q, want %q", ev.Outcome, identity.AuditOutcomeBlocked)
			}
			if ev.ActorRole != role {
				t.Errorf("audit ActorRole = %q, want %q", ev.ActorRole, role)
			}
			if ev.Detail["clientId"] != identity.BuiltinClientVSCode {
				t.Errorf("audit detail clientId = %v, want %q", ev.Detail["clientId"], identity.BuiltinClientVSCode)
			}
			if ev.Detail["requiredRole"] != "developer" || ev.Detail["actualRole"] != role {
				t.Errorf("audit detail = %+v", ev.Detail)
			}
		})
	}
}

func TestDeviceApproval_NonFlooredClientIsUnaffected(t *testing.T) {
	// A statically-configured or self-registered client -- anything that is
	// not a built-in declaring a floor -- keeps behaving exactly as before.
	s, adapter, audit, token := newRoleFloorDeviceServer(t, "mcp_abc123", "reader")
	rec := approveRoleFloorDevice(s, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	adapter.mu.Lock()
	approved := adapter.approvedId
	adapter.mu.Unlock()
	if approved != "v1:identity:deviceCode:dc-floor" {
		t.Fatalf("a reader must still approve a grant for a client that declares no floor; approvedId=%q", approved)
	}
	if ev := audit.find(identity.AuditActionRoleFloorRefused); ev != nil {
		t.Fatalf("a non-floored client wrote a role-floor refusal: %+v", ev)
	}
}

func TestDeviceDenial_IsNeverBlockedByTheFloor(t *testing.T) {
	// Saying NO must always work. Routing a denial through the floor would
	// leave a reader unable to reject a grant somebody started in their name.
	s, adapter, _, token := newRoleFloorDeviceServer(t, identity.BuiltinClientVSCode, "reader")
	rec := devicePost(s, token, url.Values{
		"user_code": {roleFloorUserCode},
		"action":    {"deny"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	adapter.mu.Lock()
	denied := adapter.deniedId
	adapter.mu.Unlock()
	if denied != "v1:identity:deviceCode:dc-floor" {
		t.Fatalf("deny must reach the store; deniedId=%q", denied)
	}
}

// -----------------------------------------------------------------------------
// The /authorize SSO fast path
// -----------------------------------------------------------------------------

func TestSSOFastPath_RefusesBelowDeveloperWithAnOAuthErrorRedirect(t *testing.T) {
	s, token, audit := newSSOFloorServer(t, "reader")

	rec := ssoAuthorizeRequest(s, token)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (the OAuth error redirect); body=%s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if !strings.HasPrefix(loc.String(), "http://127.0.0.1:54321/callback") {
		t.Fatalf("Location = %q, want the client's own redirect URI", loc)
	}
	if got := loc.Query().Get("error"); got != "access_denied" {
		t.Fatalf("error = %q, want access_denied", got)
	}
	desc := loc.Query().Get("error_description")
	for _, want := range []string{"reader", "developer"} {
		if !strings.Contains(desc, want) {
			t.Errorf("error_description = %q, missing %q -- the editor prints this verbatim", desc, want)
		}
	}
	if got := loc.Query().Get("code"); got != "" {
		t.Fatalf("a refused sign-in must not carry an authorization code, got %q", got)
	}
	if ev := audit.find(identity.AuditActionRoleFloorRefused); ev == nil {
		t.Fatalf("no %q audit event was written", identity.AuditActionRoleFloorRefused)
	}
}

func TestSSOFastPath_AdmitsDeveloperAndAbove(t *testing.T) {
	for _, role := range []string{"owner", "admin", "developer"} {
		t.Run(role, func(t *testing.T) {
			s, token, _ := newSSOFloorServer(t, role)
			rec := ssoAuthorizeRequest(s, token)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want 303 (the SSO code redirect); body=%s", rec.Code, rec.Body.String())
			}
			loc, err := url.Parse(rec.Header().Get("Location"))
			if err != nil {
				t.Fatalf("parse Location: %v", err)
			}
			if loc.Query().Get("code") == "" {
				t.Fatalf("Location = %q, want an authorization code", loc)
			}
			if loc.Query().Get("error") != "" {
				t.Fatalf("Location = %q, want no error", loc)
			}
		})
	}
}

// newSSOFloorServer wires the SSO fast path against the built-in editor client
// and returns a session token for a user holding `role`.
func newSSOFloorServer(t *testing.T, role string) (*Server, string, *capturingAudit) {
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
	audit := &capturingAudit{}
	srv.mlAudit = audit
	srv.meTokens = &MeTokens{Issuer: iss}
	srv.SetMintSSOAuthCode(func(_ context.Context, _ MintSSOAuthCodeInput) (MintSSOAuthCodeResult, error) {
		return MintSSOAuthCodeResult{Code: "sso-code-1"}, nil
	})

	token, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId:    "v1:identity:user:sso-" + role,
		SessionId: "sess-sso",
		Role:      role,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}
	return srv, token, audit
}

// ssoAuthorizeRequest drives redirectIfAuthenticated exactly as GET /authorize
// does for a browser that already holds a session.
func ssoAuthorizeRequest(s *Server, token string) *httptest.ResponseRecorder {
	q := url.Values{
		"client_id":             {identity.BuiltinClientVSCode},
		"redirect_uri":          {"http://127.0.0.1:54321/callback"},
		"response_type":         {"code"},
		"state":                 {"st-1"},
		"code_challenge":        {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"},
		"code_challenge_method": {"S256"},
	}
	req := httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler := s.redirectIfAuthenticated("/portal", func(w http.ResponseWriter, _ *http.Request) {
		// The wrapped handler is the login FORM. Reaching it on a
		// role-floor path would be the bug: the person is already signed
		// in, so a form asks for something they have and explains nothing.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("LOGIN FORM RENDERED"))
	})
	handler(rec, req)
	return rec
}

// -----------------------------------------------------------------------------
// The magic-link handler's surfacing of a refusal
// -----------------------------------------------------------------------------

// floorVerifier is a MagicLinkVerifier that always refuses with the role floor.
// The DECISION is pinned beside the verifier (magiclink/role_floor_test.go);
// what is pinned here is that the handler turns it into an OAuth error redirect
// rather than an HTML page -- the difference between the editor printing a
// sentence and the editor waiting for a callback that never arrives.
type floorVerifier struct{ redirectURI string }

func (f *floorVerifier) Inspect(_ context.Context, _, _, _ string) (*identity.MagicLinkRow, error) {
	return nil, nil
}

func (f *floorVerifier) Finish(_ context.Context, _ magiclink.FinishInput) (*magiclink.VerifyResult, error) {
	return nil, &magiclink.RoleFloorError{
		Refusal: identity.RoleFloorRefusal{
			ClientId:   identity.BuiltinClientVSCode,
			ClientName: "MemQL for VS Code",
			Required:   "developer",
			Actual:     "reader",
		},
		RedirectURI: f.redirectURI,
		State:       "st-1",
	}
}

func TestMagicLinkHandler_RefusalLeavesAsAnOAuthErrorRedirect(t *testing.T) {
	srv, err := NewServer(identity.Config{BaseURL: "https://identity.test"},
		slog.Default(), nil)
	if err != nil {
		t.Fatalf("new web server: %v", err)
	}
	srv.mlVerifier = &floorVerifier{redirectURI: "http://127.0.0.1:54321/callback"}
	srv.mlAudit = &capturingAudit{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/magic-link/finish", nil)
	srv.finishSignIn(rec, req, &identity.MagicLinkRow{ID: "v1:identity:magiclink:ml-1"}, true)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Scheme != "http" || loc.Host != "127.0.0.1:54321" || loc.Path != "/callback" {
		t.Fatalf("Location = %q, want the editor's own loopback callback", loc)
	}
	if loc.Query().Get("error") != "access_denied" {
		t.Errorf("error = %q, want access_denied", loc.Query().Get("error"))
	}
	desc := loc.Query().Get("error_description")
	for _, want := range []string{"reader", "developer"} {
		if !strings.Contains(desc, want) {
			t.Errorf("error_description = %q, missing %q", desc, want)
		}
	}
	if loc.Query().Get("state") != "st-1" {
		t.Errorf("state = %q, want st-1", loc.Query().Get("state"))
	}
	if loc.Query().Get("code") != "" {
		t.Error("a refused sign-in must carry no authorization code")
	}
}

func TestMagicLinkHandler_RefusalWithNoRedirectRendersAPage(t *testing.T) {
	// A refusal on a flow that carries no relying party has nowhere to
	// redirect. The person still gets the sentence rather than a generic
	// "something went wrong".
	srv, err := NewServer(identity.Config{BaseURL: "https://identity.test"},
		slog.Default(), nil)
	if err != nil {
		t.Fatalf("new web server: %v", err)
	}
	srv.mlVerifier = &floorVerifier{redirectURI: ""}
	srv.mlAudit = &capturingAudit{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/magic-link/finish", nil)
	srv.finishSignIn(rec, req, &identity.MagicLinkRow{ID: "v1:identity:magiclink:ml-1"}, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the rendered page); body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"reader", "developer"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not mention %q; body=%s", want, body)
		}
	}
}
