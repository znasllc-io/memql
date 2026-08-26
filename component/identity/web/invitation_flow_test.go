package web

// GET /invitation + POST /invitation/accept -- the redeem pair (memql#4601).
//
// THE TEST THAT DID NOT EXIST. Before this file, nothing in the repository
// exercised user-invitation redemption from a browser's point of view: the
// issue side (memql#4270) and the validate side (memql#4282) each had tests,
// and the seam between them -- the page a human actually uses -- had none. That
// is why redemption shipped broken on every version, and why the failure was
// invisible to a green suite.
//
// These tests are written against the OUTSIDE of the flow on purpose. They post
// what a browser posts and read what it renders, so a future refactor that
// keeps the internals tidy while dropping a field the handler needs fails here.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// invitationHarness is a mounted server whose three seams answer from
// test-controlled state, and which records what was asked of them.
type invitationHarness struct {
	mux   *http.ServeMux
	srv   *Server
	audit *capturingAudit

	// bound records every (invitationId, bindingHash) the page stamped.
	bound []string
	// accepted counts spends. The GET must never move this.
	accepted int
	// acceptErr, when set, is what the accept seam returns.
	acceptErr error
	// bindErr, when set, is what the bind seam returns.
	bindErr error
	// enrolmentCode is what a successful accept hands back.
	enrolmentCode string
}

func newInvitationHarness(t *testing.T, res InvitationResolution) *invitationHarness {
	t.Helper()
	srv, err := NewServer(identity.Config{BaseURL: "https://identity.example.test", BrandName: "MemQL"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	h := &invitationHarness{
		srv:           srv,
		audit:         &capturingAudit{},
		enrolmentCode: "mql_enr_" + strings.Repeat("b", 43),
	}
	srv.SetInvitationFlow(
		func(context.Context, string) (InvitationResolution, error) { return res, nil },
		func(_ context.Context, invitationId, bindingHash string) error {
			if h.bindErr != nil {
				return h.bindErr
			}
			h.bound = append(h.bound, invitationId+":"+bindingHash)
			return nil
		},
		func(context.Context, string, string) (InvitationAcceptResult, error) {
			if h.acceptErr != nil {
				return InvitationAcceptResult{}, h.acceptErr
			}
			h.accepted++
			return InvitationAcceptResult{EnrolmentCode: h.enrolmentCode, Email: res.InviteeEmail}, nil
		},
		h.audit,
	)
	mux := http.NewServeMux()
	srv.Mount(mux)
	h.mux = mux
	return h
}

const liveInvitation = "mql_inv_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func validResolution() InvitationResolution {
	return InvitationResolution{
		State:        InvitationValid,
		InvitationId: "inv-1",
		InviteeEmail: "colleague@example.test",
		InviterName:  "owner@example.test",
		Role:         "developer",
		ExpiresAt:    time.Now().UTC().Add(48 * time.Hour),
	}
}

// getInvitation issues a TLS-fronted GET, which is how every real request
// arrives (ingress terminates TLS and forwards plaintext with
// X-Forwarded-Proto).
func (h *invitationHarness) get(query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/invitation"+query, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", "198.51.100.9")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// csrfCookie primes a token the way a browser does: the middleware mints one on
// any GET and the form renders it back into the hidden _csrf field.
//
// /invitation/accept is deliberately NOT added to the CSRF carve-out that
// /login and /setup take. Those are exempt because nothing renders them on our
// behalf; this form IS ours to render, so exempting it would trade a real
// protection for a line of template.
func (h *invitationHarness) csrfCookie(t *testing.T) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/invitation?code="+liveInvitation, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == CSRFCookieName {
			return c
		}
	}
	t.Fatal("the CSRF middleware minted no cookie, so no POST in this suite could ever succeed")
	return nil
}

// accept posts the form a browser would, carrying the CSRF pair plus whatever
// binding cookie the caller wants to simulate.
func (h *invitationHarness) accept(t *testing.T, code string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	csrf := h.csrfCookie(t)

	form := url.Values{}
	form.Set("code", code)
	form.Set(CSRFFormField, csrf.Value)
	req := httptest.NewRequest(http.MethodPost, "/invitation/accept", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", "198.51.100.9")
	req.AddCookie(csrf)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// acceptWithoutCSRF is the negative control for the paragraph above.
func (h *invitationHarness) acceptWithoutCSRF(code string) *httptest.ResponseRecorder {
	form := url.Values{}
	form.Set("code", code)
	req := httptest.NewRequest(http.MethodPost, "/invitation/accept", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// The GET renders and does not spend
// ---------------------------------------------------------------------------

// A GET that consumed the invitation would be spent by a mail scanner before
// the recipient ever clicked, and they would be told the link was already used.
// This is the single most important property of the pair.
func TestTheGetNeverSpendsTheInvitation(t *testing.T) {
	h := newInvitationHarness(t, validResolution())
	rec := h.get("?code=" + liveInvitation)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	if h.accepted != 0 {
		t.Fatalf("the GET spent the invitation %d time(s); a mail scanner would burn every link", h.accepted)
	}
}

// The page must tell the holder who they are rather than ask. An editable email
// input is the shape of the old broken flow, and its absence is the fix.
func TestTheLivePageShowsTheInvitedAddressAndDoesNotAskForIt(t *testing.T) {
	res := validResolution()
	h := newInvitationHarness(t, res)
	body := h.get("?code=" + liveInvitation).Body.String()

	if !strings.Contains(body, res.InviteeEmail) {
		t.Errorf("the page does not show the invited address %q", res.InviteeEmail)
	}
	if !strings.Contains(body, res.Role) {
		t.Errorf("the page does not name the role %q the holder will land with", res.Role)
	}
	if !strings.Contains(body, res.InviterName) {
		t.Errorf("the page does not attribute the invitation to %q", res.InviterName)
	}
	if strings.Contains(body, `name="email"`) {
		t.Error("the page renders an email input; the invitation already names one address and asking for it invites a mismatch")
	}
	if !strings.Contains(body, `name="code"`) {
		t.Error("the accept form carries no code field, so the POST could not re-resolve the invitation")
	}
}

// Links composed before this page existed are sitting in mailboxes and point at
// ?invitation=. Dropping that spelling would break exactly the people this
// change exists to rescue.
func TestTheOldInvitationParameterStillResolves(t *testing.T) {
	h := newInvitationHarness(t, validResolution())
	rec := h.get("?invitation=" + liveInvitation)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?invitation= status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `data-invitation-state="valid"`) {
		t.Error("a link using the old parameter did not render the live page")
	}
}

// ---------------------------------------------------------------------------
// Each refusal is its own page
// ---------------------------------------------------------------------------

// Five states, five next steps. Easy to satisfy on the day it is written and
// easy to lose by factoring the branches into one, so the bodies are compared
// against EACH OTHER rather than against strings somebody typed twice.
func TestEachInvitationRejectionStateRendersItsOwnMessage(t *testing.T) {
	states := []InvitationState{
		InvitationInvalid,
		InvitationExpired,
		InvitationAlreadyUsed,
		InvitationRevoked,
		InvitationWrongKind,
	}
	bodies := make(map[string]InvitationState, len(states))
	for _, st := range states {
		h := newInvitationHarness(t, InvitationResolution{State: st, InvitationId: "inv-1"})
		rec := h.get("?code=" + liveInvitation)
		if rec.Code == http.StatusOK {
			t.Errorf("state %q rendered 200; a refusal must not read as success to a machine", st)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `data-invitation-state="`+string(st)+`"`) {
			t.Errorf("state %q is not machine-readable on the page", st)
		}
		if prev, dup := bodies[body]; dup {
			t.Errorf("states %q and %q render an identical page, so two different problems give the holder the same next step", prev, st)
		}
		bodies[body] = st
	}
}

// A burst of already-used is a replay attempt; a burst of invalid is a scanner.
// One collapsed reason makes those indistinguishable in the trail.
func TestEachRefusalCarriesItsOwnAuditReason(t *testing.T) {
	seen := map[string]bool{}
	for _, st := range []InvitationState{InvitationExpired, InvitationAlreadyUsed, InvitationRevoked, InvitationWrongKind} {
		h := newInvitationHarness(t, InvitationResolution{State: st, InvitationId: "inv-1"})
		h.get("?code=" + liveInvitation)
		reason := h.audit.last().FailureReason
		if reason == "" {
			t.Errorf("state %q produced no audit failure reason", st)
		}
		if seen[reason] {
			t.Errorf("reason %q is reused across states", reason)
		}
		seen[reason] = true
	}
}

// ---------------------------------------------------------------------------
// The accept spends, once, and hands off in the same window
// ---------------------------------------------------------------------------

func TestAcceptSpendsAndRedirectsToEnrolment(t *testing.T) {
	h := newInvitationHarness(t, validResolution())
	rec := h.accept(t, liveInvitation, nil)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("accept status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if h.accepted != 1 {
		t.Fatalf("accept spent the invitation %d time(s), want exactly 1", h.accepted)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/enroll?code=") {
		t.Fatalf("accept redirected to %q, want the enrolment page -- the invitee must not have to navigate anywhere themselves", loc)
	}
	if !strings.Contains(loc, h.enrolmentCode) {
		t.Errorf("the redirect %q does not carry the minted enrolment code", loc)
	}
}

// A refusal state must not be spendable, whatever the holder posts.
func TestAcceptRefusesANonLiveInvitation(t *testing.T) {
	for _, st := range []InvitationState{InvitationExpired, InvitationAlreadyUsed, InvitationRevoked, InvitationInvalid} {
		h := newInvitationHarness(t, InvitationResolution{State: st, InvitationId: "inv-1"})
		rec := h.accept(t, liveInvitation, nil)
		if rec.Code == http.StatusSeeOther {
			t.Errorf("state %q was accepted", st)
		}
		if h.accepted != 0 {
			t.Errorf("state %q was spent", st)
		}
	}
}

// ---------------------------------------------------------------------------
// First-touch binding
// ---------------------------------------------------------------------------

// The GET stamps a binding on an unbound row and hands the browser the nonce.
func TestTheGetBindsAnUnboundRowAndSetsTheCookie(t *testing.T) {
	h := newInvitationHarness(t, validResolution())
	rec := h.get("?code=" + liveInvitation)

	if len(h.bound) != 1 {
		t.Fatalf("the page stamped %d bindings, want 1", len(h.bound))
	}
	var found *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == invitationCookieName {
			found = c
		}
	}
	if found == nil {
		t.Fatal("no binding cookie was set, so the accept could not tell this browser from any other")
	}
	if !found.HttpOnly {
		t.Error("the binding cookie is readable from script")
	}
	if strings.Contains(strings.Join(h.bound, " "), found.Value) {
		t.Error("the plaintext nonce was stamped on the row; only its digest may be persisted")
	}
}

// A row that already carries a binding is not re-bound. Re-binding would hand
// the invitation to the LAST browser that opened it, which is the opposite of
// the control.
func TestTheGetDoesNotRebindARowThatIsAlreadyBound(t *testing.T) {
	res := validResolution()
	res.BindingHash = hashBindingNonce("somebody-elses-nonce")
	h := newInvitationHarness(t, res)
	h.get("?code=" + liveInvitation)
	if len(h.bound) != 0 {
		t.Fatalf("an already-bound row was re-bound %d time(s)", len(h.bound))
	}
}

// The accept must come from the browser that opened the link. This is what
// stops a forwarded invitation from being redeemed by the forwardee.
func TestAcceptRequiresTheBindingCookie(t *testing.T) {
	res := validResolution()
	nonce := "the-first-toucher"
	res.BindingHash = hashBindingNonce(nonce)

	h := newInvitationHarness(t, res)
	if rec := h.accept(t, liveInvitation, nil); rec.Code != http.StatusForbidden {
		t.Errorf("accept with NO cookie = %d, want 403", rec.Code)
	}
	if h.accepted != 0 {
		t.Fatal("a bound invitation was spent by a browser holding no binding")
	}

	h2 := newInvitationHarness(t, res)
	wrong := []*http.Cookie{{Name: invitationCookieName, Value: "a-different-browser"}}
	if rec := h2.accept(t, liveInvitation, wrong); rec.Code != http.StatusForbidden {
		t.Errorf("accept with the WRONG cookie = %d, want 403", rec.Code)
	}
	if h2.accepted != 0 {
		t.Fatal("a bound invitation was spent by a browser holding the wrong binding")
	}

	h3 := newInvitationHarness(t, res)
	right := []*http.Cookie{{Name: invitationCookieName, Value: nonce}}
	if rec := h3.accept(t, liveInvitation, right); rec.Code != http.StatusSeeOther {
		t.Errorf("accept with the RIGHT cookie = %d, want 303", rec.Code)
	}
	if h3.accepted != 1 {
		t.Fatal("the first toucher could not spend their own invitation")
	}
}

// Rows issued before this control existed carry no digest. Refusing them would
// invalidate every invitation in flight at deploy time.
func TestAnUnboundRowIsStillAcceptable(t *testing.T) {
	h := newInvitationHarness(t, validResolution()) // BindingHash empty
	if rec := h.accept(t, liveInvitation, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("an unbound (pre-existing) invitation was refused: %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Step-up for privileged roles
// ---------------------------------------------------------------------------

// An admin/owner invitation buys a magic link, not an account. It must NOT be
// spent here: burning it now would leave somebody who never received the mail
// holding a dead invitation and no account.
func TestStepUpIssuesAMagicLinkAndDoesNotSpendTheInvitation(t *testing.T) {
	res := validResolution()
	res.Role = "admin"
	res.StepUp = true
	h := newInvitationHarness(t, res)

	var issuedTo, issuedInvitation string
	h.srv.IssueMagicLink = func(_ context.Context, in IssueMagicLinkInput) (IssueMagicLinkResult, error) {
		issuedTo = in.Email
		issuedInvitation = in.InvitationId
		return IssueMagicLinkResult{RequestId: "req-1", BindingNonce: "nonce-1", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}

	rec := h.accept(t, liveInvitation, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("step-up accept = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/check-email") {
		t.Errorf("step-up redirected to %q, want the check-email page", loc)
	}
	if issuedTo != res.InviteeEmail {
		t.Errorf("the magic link went to %q, want the INVITED address %q", issuedTo, res.InviteeEmail)
	}
	if issuedInvitation == "" {
		t.Error("the magic link carries no invitation id, so the issuer's mode gate would reject it on an invite-only cluster")
	}
	if h.accepted != 0 {
		t.Error("step-up spent the invitation; it must be spent when the link is consumed, not before")
	}
}

// The live page warns about the extra step before the button is pressed. An
// unannounced extra step reads as the flow failing.
func TestTheStepUpPageSaysTheExtraStepIsComing(t *testing.T) {
	res := validResolution()
	res.Role = "admin"
	res.StepUp = true
	h := newInvitationHarness(t, res)

	plain := newInvitationHarness(t, validResolution()).get("?code=" + liveInvitation).Body.String()
	stepped := h.get("?code=" + liveInvitation).Body.String()
	if plain == stepped {
		t.Error("the step-up page is identical to the ordinary one, so the holder is not told an email is coming")
	}
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// The code is a bearer in a URL. Over plaintext it lands in proxy logs.
func TestPlaintextIsRefusedOnBothRoutes(t *testing.T) {
	h := newInvitationHarness(t, validResolution())

	req := httptest.NewRequest(http.MethodGet, "/invitation?code="+liveInvitation, nil)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("plaintext GET = %d, want 403", rec.Code)
	}

	form := url.Values{}
	form.Set("code", liveInvitation)
	req2 := httptest.NewRequest(http.MethodPost, "/invitation/accept", strings.NewReader(form.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec2 := httptest.NewRecorder()
	h.mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("plaintext POST = %d, want 403", rec2.Code)
	}
	if h.accepted != 0 {
		t.Error("a plaintext POST spent the invitation")
	}
}

// Nothing mounts unless the whole flow is wired: a redeem surface that renders
// but cannot complete is worse than one that is absent.
func TestTheRoutesDoNotMountWhenTheFlowIsNotWired(t *testing.T) {
	srv, err := NewServer(identity.Config{BaseURL: "https://identity.example.test"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	srv.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/invitation?code="+liveInvitation, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unwired /invitation = %d, want 404", rec.Code)
	}
}

// The accept form is CSRF-protected. /login and /setup are carved out of the
// middleware because nothing renders them on our behalf; this form is ours, so
// it takes the protection rather than the exemption.
func TestAcceptRequiresTheCSRFToken(t *testing.T) {
	h := newInvitationHarness(t, validResolution())
	rec := h.acceptWithoutCSRF(liveInvitation)
	if rec.Code != http.StatusForbidden {
		t.Errorf("accept with no CSRF token = %d, want 403", rec.Code)
	}
	if h.accepted != 0 {
		t.Error("an invitation was spent by a request carrying no CSRF token")
	}
}

// Losing the first-touch race is the control WORKING. It must not be recorded
// as the same event as a broken write: a run of the first on one invitation is
// a forwarded email being opened by several people, and a run of the second is
// infrastructure. An operator greps the trail for exactly that difference.
func TestLosingTheBindingRaceIsRecordedApartFromABrokenWrite(t *testing.T) {
	lost := newInvitationHarness(t, validResolution())
	lost.bindErr = identity.ErrInvitationBoundElsewhere
	lostRec := lost.get("?code=" + liveInvitation)

	broke := newInvitationHarness(t, validResolution())
	broke.bindErr = errors.New("the database fell over")
	brokeRec := broke.get("?code=" + liveInvitation)

	lostEv, brokeEv := lost.audit.events, broke.audit.events
	if len(lostEv) == 0 || len(brokeEv) == 0 {
		t.Fatal("one of the two paths emitted no audit event at all")
	}
	lostReason := lostEv[0].FailureReason
	brokeReason := brokeEv[0].FailureReason
	if lostReason == brokeReason {
		t.Errorf("both outcomes recorded the same reason %q, so a forwarding incident is indistinguishable from a database wobble", lostReason)
	}
	if lostEv[0].Outcome == identity.AuditOutcomeFailure {
		t.Error("losing the race was recorded as a failure; the control worked")
	}

	// Neither may hand this browser a binding cookie -- it does not hold one.
	for name, rec := range map[string]*httptest.ResponseRecorder{"bound-elsewhere": lostRec, "write-failed": brokeRec} {
		for _, c := range rec.Result().Cookies() {
			if c.Name == invitationCookieName && strings.TrimSpace(c.Value) != "" {
				t.Errorf("%s handed out a binding cookie for a binding it does not hold", name)
			}
		}
	}

	// Both still render the page. Somebody who opened their own link twice
	// should see the invitation, not an error.
	if lostRec.Code != http.StatusOK || brokeRec.Code != http.StatusOK {
		t.Errorf("statuses were %d and %d, want 200 for both", lostRec.Code, brokeRec.Code)
	}
}
