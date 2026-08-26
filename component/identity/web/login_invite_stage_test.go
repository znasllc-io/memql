package web

// login_invite_stage_test.go -- memql#4601.
//
// THE TEST THAT DID NOT EXIST. Nothing in this repository ever posted
// `form=invite`. The issue side (memql#4270) and the validate side (memql#4282)
// each had tests; the browser form that joins them had none, and so it shipped
// rendering no address field at all. Every submission reached the issuer with
// `email: ""`, resolveInvitation compared the row's address against the empty
// string, and the person holding a valid invitation was told to check their
// email address. Redemption had never once succeeded on any shipped version,
// against a green suite.
//
// These assert the seam from the OUTSIDE -- what the form renders, and what the
// handler does with what a browser would post -- because that is the only
// vantage point from which the original defect was visible.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

func newInviteStageServer(t *testing.T) (*Server, *IssueMagicLinkInput, *int) {
	t.Helper()
	s, err := NewServer(identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
	}, slog.Default(), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	captured := &IssueMagicLinkInput{}
	calls := new(int)
	s.IssueMagicLink = func(_ context.Context, in IssueMagicLinkInput) (IssueMagicLinkResult, error) {
		*captured = in
		*calls++
		return IssueMagicLinkResult{
			RequestId:    "req-1",
			BindingNonce: "nonce-1",
			ExpiresAt:    time.Now().UTC().Add(7 * time.Minute),
		}, nil
	}
	s.CountUsers = func(context.Context) (int, error) { return 1, nil }
	return s, captured, calls
}

// The whole defect in one assertion: a browser posting this form must reach the
// issuer with BOTH the token and the address it belongs to.
func TestFormInviteReachesTheIssuerWithTheAddress(t *testing.T) {
	s, captured, calls := newInviteStageServer(t)

	rec := postLogin(t, s, url.Values{
		"form":       {"invite"},
		"email":      {"colleague@example.test"},
		"invitation": {"mql_inv_" + strings.Repeat("a", 43)},
	})

	if *calls != 1 {
		t.Fatalf("the issuer was called %d time(s), want 1; status=%d body=%s", *calls, rec.Code, rec.Body.String())
	}
	if captured.Email != "colleague@example.test" {
		t.Errorf("the issuer got email %q, want the submitted address. An empty address here is the "+
			"exact defect: resolveInvitation compares it against the row and refuses "+
			"invitation_address_mismatch, then the page blames the user's email.", captured.Email)
	}
	if captured.InvitationId == "" {
		t.Error("the issuer got no invitation token, so the registration-mode gate would reject an invite-only cluster")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 to the check-email page", rec.Code)
	}
}

// The guard that keeps the template and the handler honest. If the field is
// ever dropped again, the submission must fail HERE, naming the real problem,
// rather than travelling on to be misdiagnosed as an address mismatch.
func TestFormInviteRefusesAnEmptyAddress(t *testing.T) {
	s, _, calls := newInviteStageServer(t)

	rec := postLogin(t, s, url.Values{
		"form":       {"invite"},
		"invitation": {"mql_inv_" + strings.Repeat("a", 43)},
	})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if *calls != 0 {
		t.Error("an addressless submission reached the issuer, where it can only be refused as somebody else's mismatch")
	}
	if body := rec.Body.String(); strings.Contains(strings.ToLower(body), "double-check your email") {
		t.Error("the refusal still blames the user's email address, which is the message that sent the original reporter in circles")
	}
}

// A token with no address must not be treated as a token with an address that
// happens to be blank -- the row's own address would then have to be blank to
// match, and no invitation ever is.
func TestFormInviteRefusesAWhitespaceAddress(t *testing.T) {
	s, _, calls := newInviteStageServer(t)

	rec := postLogin(t, s, url.Values{
		"form":       {"invite"},
		"email":      {"   "},
		"invitation": {"mql_inv_" + strings.Repeat("a", 43)},
	})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if *calls != 0 {
		t.Error("a whitespace-only address reached the issuer")
	}
}

// The template half of the same contract. The handler reads `email` from this
// form; the form has to render one.
func TestTheNeedsInviteStageRendersTheAddressItsHandlerReads(t *testing.T) {
	var buf bytes.Buffer
	err := webtempl.Login(webtempl.LoginData{
		Stage:        "needs_invite",
		PrefillEmail: "colleague@example.test",
	}).Render(context.Background(), &buf)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	if !strings.Contains(body, `name="email"`) {
		t.Fatal("the needs_invite form renders no email field, so every submission posts email=\"\" " +
			"and the invitation can never resolve. This is the original defect.")
	}
	if !strings.Contains(body, "colleague@example.test") {
		t.Error("the form renders an email field but does not carry the address the handler put in PrefillEmail")
	}
	if !strings.Contains(body, `name="invitation"`) {
		t.Error("the form renders no invitation field")
	}
}

// Links composed before /invitation existed point at /login?invitation=<token>
// and are sitting in mailboxes now. They must land somewhere that can actually
// redeem them.
func TestLoginRedirectsAnInvitationLinkToTheInvitationPage(t *testing.T) {
	s, _, _ := newInviteStageServer(t)

	token := "mql_inv_" + strings.Repeat("a", 43)
	req := httptest.NewRequest(http.MethodGet, "/login?invitation="+token, nil)
	rec := httptest.NewRecorder()
	s.handleLoginGet(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; an unread invitation parameter is how this whole failure started", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/invitation?code=") {
		t.Fatalf("redirected to %q, want the invitation page", loc)
	}
	if !strings.Contains(loc, token) {
		t.Errorf("the redirect %q dropped the token", loc)
	}
}

// An ordinary sign-in must not be diverted by the new branch.
func TestLoginWithoutAnInvitationStillRendersTheEmailStage(t *testing.T) {
	s, _, _ := newInviteStageServer(t)

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	s.handleLoginGet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `name="email"`) {
		t.Error("the ordinary login page lost its email field")
	}
}

// A person who arrived through /authorize and gets bounced to stage 2 must not
// lose their relying party on the way (memql#4609).
//
// Before the fix, renderLoginStage built stage 2 from the settings and the
// address alone. The stage-2 forms had rendered client_id, redirect_uri and
// state since they were written, each behind an `if != ""` that was never true,
// so completing stage 2 signed the person in as an identity admin and left them
// on /admin/ with the application that sent them still waiting.
func TestStageTwoCarriesTheOAuthContextForward(t *testing.T) {
	s, _, _ := newInviteStageServer(t)

	form := url.Values{
		"form":                  {"email"},
		"email":                 {"colleague@example.test"},
		"client_id":             {"app"},
		"redirect_uri":          {"https://app.example.com/auth/callback"},
		"state":                 {"opaque-state"},
		"code_challenge":        {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"},
		"code_challenge_method": {"S256"},
	}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatalf("ParseForm: %v", err)
	}

	got := s.renderLoginStage(req, "needs_invite", "colleague@example.test", "", "")

	if got.ClientID != "app" {
		t.Errorf("ClientID = %q, want it carried forward; without it stage 2 completes as an admin session "+
			"and the relying party never hears back", got.ClientID)
	}
	if got.RedirectURI != "https://app.example.com/auth/callback" {
		t.Errorf("RedirectURI = %q, want it carried forward", got.RedirectURI)
	}
	if got.OAuthState != "opaque-state" {
		t.Errorf("OAuthState = %q, want it carried forward", got.OAuthState)
	}
	// The PKCE pair has to travel WITH client_id, not after it: handleLoginPost
	// reads the challenge in its prologue and, since memql#4303, refuses a
	// matched client that arrives without one.
	if got.CodeChallenge == "" {
		t.Error("CodeChallenge was dropped, so carrying client_id turns stage 2 into a 400")
	}
	if got.CodeChallengeMethod != "S256" {
		t.Errorf("CodeChallengeMethod = %q, want S256", got.CodeChallengeMethod)
	}
}

// And the rendered form must actually emit them, or carrying them in the struct
// changes nothing.
func TestStageTwoFormsRenderThePKCEPair(t *testing.T) {
	for _, stage := range []string{"needs_invite", "waitlist_signup"} {
		var buf bytes.Buffer
		err := webtempl.Login(webtempl.LoginData{
			Stage:               stage,
			PrefillEmail:        "colleague@example.test",
			ClientID:            "app",
			RedirectURI:         "https://app.example.com/auth/callback",
			OAuthState:          "opaque-state",
			CodeChallenge:       "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
			CodeChallengeMethod: "S256",
		}).Render(context.Background(), &buf)
		if err != nil {
			t.Fatalf("render %s: %v", stage, err)
		}
		body := buf.String()
		for _, field := range []string{"client_id", "redirect_uri", "state", "code_challenge", "code_challenge_method"} {
			if !strings.Contains(body, `name="`+field+`"`) {
				t.Errorf("stage %s renders no %q input", stage, field)
			}
		}
	}
}
