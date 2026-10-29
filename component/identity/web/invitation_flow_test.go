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
//
// THE SECOND THING THIS SUITE MISSED. The page used to bind the invitation to
// whichever browser fetched it first, and every test here played one browser
// with hand-built cookies, so nothing ever asked what happens when something
// else fetches the link before the invitee does. In production that something
// was a scanner, a link preview or an email app's own browser, and the invitee
// was told to finish in "the browser that opened the invitation" -- one they
// had never used. The `browser` type below keeps one cookie jar per party, so
// the tests can say WHOSE request they are making.

import (
	"bytes"
	"context"
	"encoding/json"
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
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// invitationHarness is a mounted server whose three seams answer from
// test-controlled state, and which records what was asked of them.
type invitationHarness struct {
	mux   *http.ServeMux
	srv   *Server
	audit *capturingAudit

	// res is the row as the resolver currently reports it. The bind seam
	// writes res.BindingHash, so a later request sees the binding an earlier
	// one stamped, exactly as it would through the store.
	res InvitationResolution
	// bound records every (invitationId, bindingHash) the flow stamped.
	bound []string
	// accepted counts spends. Neither the GET nor a Continue may move this.
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
		res:           res,
		enrolmentCode: "mql_enr_" + strings.Repeat("b", 43),
	}
	srv.SetInvitationFlow(
		func(context.Context, string) (InvitationResolution, error) { return h.res, nil },
		// The store's no-overwrite rule, reproduced: an empty row is stamped,
		// the same digest again is a no-op, and a different digest is refused.
		// Store.BindUserInvitation owns that rule and invitation_binding_test.go
		// pins it there; this copy only lets several requests share one row.
		func(_ context.Context, invitationId, bindingHash string) error {
			if h.bindErr != nil {
				return h.bindErr
			}
			switch existing := strings.TrimSpace(h.res.BindingHash); {
			case existing == "":
				h.res.BindingHash = bindingHash
				h.bound = append(h.bound, invitationId+":"+bindingHash)
				return nil
			case existing == bindingHash:
				return nil
			default:
				return identity.ErrInvitationBoundElsewhere
			}
		},
		func(context.Context, string, string) (InvitationAcceptResult, error) {
			if h.acceptErr != nil {
				return InvitationAcceptResult{}, h.acceptErr
			}
			h.accepted++
			return InvitationAcceptResult{EnrolmentCode: h.enrolmentCode, Email: h.res.InviteeEmail}, nil
		},
		h.audit,
	)
	mux := http.NewServeMux()
	srv.Mount(mux)
	h.mux = mux
	return h
}

const liveInvitation = "mql_inv_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// invitationPage is where a Continue sends the browser back to.
const invitationPage = "/invitation?code=" + liveInvitation

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

// continuedResolution is a live row that somebody already pressed Continue on,
// in the browser holding nonce.
func continuedResolution(nonce string) InvitationResolution {
	res := validResolution()
	res.BindingHash = hashBindingNonce(nonce)
	return res
}

// getInvitation issues a TLS-fronted GET, which is how every real request
// arrives (ingress terminates TLS and forwards plaintext with
// X-Forwarded-Proto).
func (h *invitationHarness) get(query string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/invitation"+query, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", "198.51.100.9")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	if req.Method == http.MethodGet {
		req.Header.Set("Accept", NativeMediaType)
		req.Header.Set("Origin", "https://os.example.test")
	}
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
	if req.Method == http.MethodGet {
		req.Header.Set("Accept", NativeMediaType)
		req.Header.Set("Origin", "https://os.example.test")
	}
	h.mux.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == CSRFCookieName {
			return c
		}
	}
	t.Fatal("the CSRF middleware minted no cookie, so no POST in this suite could ever succeed")
	return nil
}

// post submits the page's form the way the plain HTML page does, carrying the
// CSRF pair plus whatever binding cookie the caller wants to simulate. intent
// is the form's intent field; empty leaves it out, which is the Accept form.
func (h *invitationHarness) post(t *testing.T, code, intent string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	csrf := h.csrfCookie(t)

	form := url.Values{}
	form.Set("code", code)
	if intent != "" {
		form.Set("intent", intent)
	}
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

// accept posts the Accept form.
func (h *invitationHarness) accept(t *testing.T, code string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return h.post(t, code, "", cookies)
}

// continueWith posts the Continue form.
func (h *invitationHarness) continueWith(t *testing.T, code string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return h.post(t, code, webtempl.InvitationStepContinue, cookies)
}

// postWithoutCSRF is the negative control for the paragraph on csrfCookie.
func (h *invitationHarness) postWithoutCSRF(code, intent string) *httptest.ResponseRecorder {
	form := url.Values{}
	form.Set("code", code)
	if intent != "" {
		form.Set("intent", intent)
	}
	req := httptest.NewRequest(http.MethodPost, "/invitation/accept", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// bindingCookieFrom returns the binding cookie a response set, or nil.
func bindingCookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == invitationCookieName && strings.TrimSpace(c.Value) != "" {
			return c
		}
	}
	return nil
}

// eventNamed returns the last audit event with the given action.
func (a *capturingAudit) eventNamed(action string) (identity.AuditEvent, bool) {
	for i := len(a.events) - 1; i >= 0; i-- {
		if a.events[i].Action == action {
			return a.events[i], true
		}
	}
	return identity.AuditEvent{}, false
}

// ---------------------------------------------------------------------------
// One browser profile, as the server sees it
// ---------------------------------------------------------------------------

// browser is one browser profile's cookie jar: it sends back what the server
// set, the way a real profile does, so a test can make requests as one party
// and then as another.
type browser struct {
	h       *invitationHarness
	ip      string
	cookies map[string]*http.Cookie
}

func (h *invitationHarness) browser(ip string) *browser {
	return &browser{h: h, ip: ip, cookies: map[string]*http.Cookie{}}
}

// send serves one request as this browser: its cookies out, the server's back in.
func (b *browser) send(req *http.Request) *httptest.ResponseRecorder {
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", b.ip)
	for _, c := range b.cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	rec := httptest.NewRecorder()
	b.h.mux.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(b.cookies, c.Name)
			continue
		}
		b.cookies[c.Name] = c
	}
	return rec
}

// fetch is a mail scanner or a link preview: a plain GET of the emailed URL,
// with no MemQL OS behind it and nobody to press anything.
func (b *browser) fetch() *httptest.ResponseRecorder {
	return b.send(httptest.NewRequest(http.MethodGet, invitationPage, nil))
}

// open is a person looking at the page. Identity bounces the browser's own
// navigation to MemQL OS, which reads the page in the native representation;
// this is that read.
func (b *browser) open() *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, invitationPage, nil)
	req.Header.Set("Accept", NativeMediaType)
	req.Header.Set("Origin", "https://os.example.test")
	return b.send(req)
}

// press submits the page's form as MemQL OS does: the native representation,
// with the CSRF token from this browser's own jar. Empty intent is Accept.
func (b *browser) press(intent string) *httptest.ResponseRecorder {
	form := url.Values{"code": {liveInvitation}}
	if intent != "" {
		form.Set("intent", intent)
	}
	req := httptest.NewRequest(http.MethodPost, "/invitation/accept", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", NativeMediaType)
	req.Header.Set("Origin", "https://os.example.test")
	if c := b.cookies[CSRFCookieName]; c != nil {
		req.Header.Set(CSRFHeader, c.Value)
	}
	return b.send(req)
}

// binding is the binding nonce this browser holds, or "".
func (b *browser) binding() string {
	if c := b.cookies[invitationCookieName]; c != nil {
		return c.Value
	}
	return ""
}

// nativeInvitation is the part of the native representation these tests read:
// a page, or a redirect for the client to follow.
type nativeInvitation struct {
	Page     string `json:"page"`
	Redirect string `json:"redirect"`
	Data     struct {
		Rejection    string
		Heading      string
		Step         string
		Code         string
		InviteeEmail string
		StepUp       bool
	} `json:"data"`
}

func readNative(t *testing.T, rec *httptest.ResponseRecorder) nativeInvitation {
	t.Helper()
	var out nativeInvitation
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("the response is not the native representation (status %d): %v\n%s", rec.Code, err, rec.Body.String())
	}
	return out
}

// ---------------------------------------------------------------------------
// The GET renders, and writes nothing
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

// The GET does not bind either. Binding on the first fetch handed the
// invitation to whatever fetched the link first -- a scanner, a preview, an
// email app's own browser -- and the invitee, arriving second, could not
// accept their own invitation. So neither representation of the GET stamps the
// row or sets the cookie, and the page offers Continue instead.
func TestInvitationGetDoesNotBind(t *testing.T) {
	h := newInvitationHarness(t, validResolution())

	native := h.get("?code=" + liveInvitation)
	plain := h.browser("203.0.113.50").fetch()

	if len(h.bound) != 0 {
		t.Fatalf("a GET stamped %d binding(s) on the row: %v. Whatever fetches the link first then "+
			"owns the invitation, which is the production bug", len(h.bound), h.bound)
	}
	if h.res.BindingHash != "" {
		t.Fatalf("the row carries a binding after only GETs: %q", h.res.BindingHash)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{"native GET": native, "plain GET": plain} {
		if c := bindingCookieFrom(rec); c != nil {
			t.Errorf("the %s set the binding cookie %q; a GET must hand out nothing", name, c.Name)
		}
	}
	page := readNative(t, native)
	if page.Data.Rejection != "" {
		t.Fatalf("a live unbound invitation rendered the refusal %q", page.Data.Rejection)
	}
	if page.Data.Step != webtempl.InvitationStepContinue {
		t.Errorf("the page offers step %q on an unbound invitation, want %q: Accept would only be refused",
			page.Data.Step, webtempl.InvitationStepContinue)
	}
	if h.accepted != 0 {
		t.Error("a GET spent the invitation")
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
	if !strings.Contains(body, `"Code":"`+liveInvitation+`"`) {
		t.Error("the page's form carries no code field, so the POST could not re-resolve the invitation")
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
	if !strings.Contains(rec.Body.String(), `"Rejection":""`) {
		t.Error("a link using the old parameter did not render the live page")
	}
}

// The plain HTML page shows one step at a time, and only the browser the GET
// has recognised as the one that continued is ever shown Accept. An empty step
// renders Continue: offering Accept by default would show a button that can
// only be refused.
func TestTheInvitationTemplateOffersOneStepAtATime(t *testing.T) {
	h := newInvitationHarness(t, validResolution())
	render := func(step string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, invitationPage, nil)
		data := webtempl.InvitationData{
			Layout:       h.srv.LayoutData(req, "You have been invited", false, nil, nil),
			Code:         liveInvitation,
			InviteeEmail: "colleague@example.test",
			Step:         step,
		}
		var buf bytes.Buffer
		if err := webtempl.Invitation(data).Render(context.Background(), &buf); err != nil {
			t.Fatalf("render step %q: %v", step, err)
		}
		return buf.String()
	}
	continueField := `name="intent" value="` + webtempl.InvitationStepContinue + `"`

	for _, step := range []string{webtempl.InvitationStepContinue, ""} {
		body := render(step)
		if !strings.Contains(body, continueField) {
			t.Errorf("step %q renders no Continue form (want %s)", step, continueField)
		}
		if strings.Contains(body, "Accept the invitation") {
			t.Errorf("step %q offers Accept before anybody pressed Continue", step)
		}
	}
	accept := render(webtempl.InvitationStepAccept)
	if strings.Contains(accept, continueField) {
		t.Error("the Accept step still posts intent=continue")
	}
	if !strings.Contains(accept, "Accept the invitation") {
		t.Error("the Accept step renders no Accept button")
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
		if !strings.Contains(body, `"Rejection":"`+string(st)+`"`) {
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
	nonce := "this-browser-pressed-continue"
	h := newInvitationHarness(t, continuedResolution(nonce))
	rec := h.accept(t, liveInvitation, []*http.Cookie{{Name: invitationCookieName, Value: nonce}})

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
// Continue binds; Accept requires the browser that continued
// ---------------------------------------------------------------------------

// THE PRODUCTION BUG. Something fetches the link before the invitee does -- a
// scanner, a preview -- and then the invitee opens it, presses Continue, sees
// Accept, and presses it. Under first-touch binding the fetch took the
// invitation and the invitee's accept was refused with "Finish in the browser
// that opened the invitation".
func TestScannerGetThenHumanContinueAndAccept(t *testing.T) {
	h := newInvitationHarness(t, validResolution())

	// A: the scanner. A plain fetch, and a second one through the OS
	// representation for a scanner that renders pages. Neither presses anything.
	scanner := h.browser("203.0.113.50")
	scanner.fetch()
	scanner.open()
	if len(h.bound) != 0 || scanner.binding() != "" {
		t.Fatalf("the scanner's fetches bound the invitation (stamps %v, cookie %q); the invitee "+
			"can now never accept it", h.bound, scanner.binding())
	}

	// B: the invitee.
	invitee := h.browser("198.51.100.9")
	if page := readNative(t, invitee.open()); page.Data.Step != webtempl.InvitationStepContinue {
		t.Fatalf("the invitee was offered step %q (refusal %q), want Continue", page.Data.Step, page.Data.Rejection)
	}
	if next := readNative(t, invitee.press(webtempl.InvitationStepContinue)); next.Redirect != invitationPage {
		t.Fatalf("Continue answered %+v, want a redirect back to %s", next, invitationPage)
	}
	if invitee.binding() == "" {
		t.Fatal("Continue set no binding cookie in the invitee's browser")
	}
	page := readNative(t, invitee.open())
	if page.Data.Step != webtempl.InvitationStepAccept {
		t.Fatalf("after Continue the page offers step %q (refusal %q), want Accept", page.Data.Step, page.Data.Rejection)
	}
	done := readNative(t, invitee.press(""))
	if !strings.HasPrefix(done.Redirect, "/enroll?code=") {
		t.Fatalf("the invitee's accept answered %+v (refusal %q: %s), want the enrolment page",
			done, done.Data.Rejection, done.Data.Heading)
	}
	if h.accepted != 1 {
		t.Fatalf("the invitation was spent %d time(s), want 1", h.accepted)
	}
}

// The same failure with nobody else involved. The emailed link opens on
// identity's host (adminops.invitationURL); identity answers that GET by
// sending the browser to MemQL OS, which reads the page and posts both forms
// through its OWN origin (component/edge, identityUIPaths). Cookies are
// host-only, so to the server one browser is two jars: whatever the link's GET
// set never reaches the OS's posts. First-touch binding put its cookie in the
// first jar and then refused the Accept the OS posted from the second.
func TestTheLinksHostAndTheOSHostShareNoCookies(t *testing.T) {
	h := newInvitationHarness(t, validResolution())

	linkHost := h.browser("198.51.100.9") // identity.<domain>, the emailed link
	if rec := linkHost.fetch(); rec.Code != http.StatusSeeOther {
		t.Fatalf("the emailed link's GET = %d, want 303 on to MemQL OS", rec.Code)
	}
	if linkHost.binding() != "" || len(h.bound) != 0 {
		t.Fatalf("the link's own GET bound the invitation to identity's host (cookie %q, stamps %v), "+
			"where the OS's posts can never present it", linkHost.binding(), h.bound)
	}

	osHost := h.browser("198.51.100.9") // os.<domain>, the same person
	osHost.open()
	osHost.press(webtempl.InvitationStepContinue)
	osHost.open()
	if done := readNative(t, osHost.press("")); !strings.HasPrefix(done.Redirect, "/enroll?code=") {
		t.Fatalf("the OS could not finish an invitation its own browser opened: %+v", done)
	}
}

// Continue on an unbound row stamps the digest, hands this browser the nonce
// and sends it back to the page, where Accept is now offered.
func TestContinueBindsAndRedirects(t *testing.T) {
	h := newInvitationHarness(t, validResolution())
	rec := h.continueWith(t, liveInvitation, nil)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("continue status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != invitationPage {
		t.Errorf("continue redirected to %q, want the invitation page %q", loc, invitationPage)
	}
	if len(h.bound) != 1 {
		t.Fatalf("continue stamped %d binding(s), want 1", len(h.bound))
	}
	found := bindingCookieFrom(rec)
	if found == nil {
		t.Fatal("no binding cookie was set, so the accept could not tell this browser from any other")
	}
	if !found.HttpOnly {
		t.Error("the binding cookie is readable from script")
	}
	if found.Path != invitationCookiePath {
		t.Errorf("the binding cookie's path is %q, want %q", found.Path, invitationCookiePath)
	}
	if found.SameSite != http.SameSiteLaxMode {
		t.Errorf("the binding cookie's SameSite is %v, want Lax", found.SameSite)
	}
	if strings.Contains(strings.Join(h.bound, " "), found.Value) {
		t.Error("the plaintext nonce was stamped on the row; only its digest may be persisted")
	}
	if want := "inv-1:" + hashBindingNonce(found.Value); h.bound[0] != want {
		t.Errorf("the row was stamped %q, want the digest of the cookie handed out (%q)", h.bound[0], want)
	}
	if ev, ok := h.audit.eventNamed("invitation_continued"); !ok || ev.Outcome != identity.AuditOutcomeSuccess {
		t.Errorf("a successful continue was not audited as invitation_continued (got %+v, found %v)", ev, ok)
	}
	if h.accepted != 0 {
		t.Error("Continue spent the invitation; only Accept may")
	}
}

// A second press in the same browser -- a double tap, the back button --
// writes nothing and goes back to the page.
func TestContinueTwiceInTheSameBrowserWritesOnce(t *testing.T) {
	h := newInvitationHarness(t, validResolution())
	b := h.browser("198.51.100.9")
	b.open()
	b.press(webtempl.InvitationStepContinue)
	held := b.binding()

	next := readNative(t, b.press(webtempl.InvitationStepContinue))
	if next.Redirect != invitationPage {
		t.Fatalf("a second Continue answered %+v, want a redirect back to the page", next)
	}
	if len(h.bound) != 1 {
		t.Errorf("two presses stamped %d bindings, want 1", len(h.bound))
	}
	if b.binding() != held {
		t.Error("the second press replaced this browser's binding cookie")
	}
	if page := readNative(t, b.open()); page.Data.Step != webtempl.InvitationStepAccept {
		t.Errorf("after two presses the page offers %q, want Accept", page.Data.Step)
	}
}

// Once somebody has pressed Continue, nobody else can: not with a second
// press, and not by accepting.
func TestContinueFromASecondBrowserIsRefused(t *testing.T) {
	h := newInvitationHarness(t, validResolution())
	first := h.browser("198.51.100.9")
	first.open()
	first.press(webtempl.InvitationStepContinue)
	held := h.res.BindingHash
	if held == "" {
		t.Fatal("the first Continue bound nothing")
	}

	second := h.browser("203.0.113.7")
	second.open()
	rec := second.press(webtempl.InvitationStepContinue)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a second browser's Continue = %d, want 403", rec.Code)
	}
	page := readNative(t, rec)
	if page.Data.Rejection != string(InvitationBoundElsewhere) {
		t.Errorf("the refusal is %q, want %q", page.Data.Rejection, InvitationBoundElsewhere)
	}
	if !strings.Contains(page.Data.Heading, "continued with") {
		t.Errorf("the refusal heading %q does not say where to finish", page.Data.Heading)
	}
	if h.res.BindingHash != held || len(h.bound) != 1 {
		t.Fatalf("the second Continue moved the binding (row %q, stamps %v); the invitation now belongs "+
			"to whoever pressed last", h.res.BindingHash, h.bound)
	}
	if second.binding() != "" {
		t.Error("the second browser was handed a binding cookie it does not hold")
	}
	ev := h.audit.last()
	if ev.Action != "invitation_binding_taken" || ev.Outcome != identity.AuditOutcomeBlocked || ev.FailureReason != "bound_elsewhere" {
		t.Errorf("the refusal was audited as %s/%s/%s, want invitation_binding_taken/blocked/bound_elsewhere",
			ev.Action, ev.Outcome, ev.FailureReason)
	}

	if rec := second.press(""); rec.Code != http.StatusForbidden {
		t.Errorf("the second browser's Accept = %d, want 403", rec.Code)
	}
	if h.accepted != 0 {
		t.Fatal("the second browser spent the invitation")
	}
	if done := readNative(t, first.press("")); !strings.HasPrefix(done.Redirect, "/enroll?code=") {
		t.Errorf("the browser that continued could not accept: %+v", done)
	}
}

// A browser opening an invitation somebody continued elsewhere is told so, and
// is not shown what the invitation says. The browser that continued sees Accept.
func TestTheGetOfAnInvitationContinuedElsewhereIsRefused(t *testing.T) {
	nonce := "the-browser-that-continued"
	h := newInvitationHarness(t, continuedResolution(nonce))

	rec := h.get("?code=" + liveInvitation)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET from another browser = %d, want 403", rec.Code)
	}
	page := readNative(t, rec)
	if page.Data.Rejection != string(InvitationBoundElsewhere) {
		t.Errorf("the page is %q, want the %q refusal", page.Data.Rejection, InvitationBoundElsewhere)
	}
	if strings.Contains(rec.Body.String(), h.res.InviteeEmail) {
		t.Error("the refusal shows the invited address to a browser that cannot accept it")
	}
	if ev := h.audit.last(); ev.Outcome != identity.AuditOutcomeBlocked || ev.FailureReason != "bound_elsewhere" {
		t.Errorf("the refusal was audited as %s/%s/%s, want blocked/bound_elsewhere", ev.Action, ev.Outcome, ev.FailureReason)
	}

	mine := readNative(t, h.get("?code="+liveInvitation, &http.Cookie{Name: invitationCookieName, Value: nonce}))
	if mine.Data.Step != webtempl.InvitationStepAccept {
		t.Errorf("the browser that continued is offered %q (refusal %q), want Accept", mine.Data.Step, mine.Data.Rejection)
	}
	if len(h.bound) != 0 {
		t.Errorf("a GET wrote a binding: %v", h.bound)
	}
}

// The accept must come from the browser that pressed Continue. This is what
// stops a forwarded invitation from being redeemed by the forwardee.
func TestAcceptRequiresTheBindingCookie(t *testing.T) {
	nonce := "the-browser-that-continued"
	res := continuedResolution(nonce)

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
		t.Fatal("the browser that continued could not spend its own invitation")
	}
}

// Nobody has pressed Continue, so nobody can accept. The page never offers
// Accept on an unbound row; this is a post that skipped it, and it must not
// spend the invitation or bind it on the way past.
func TestAcceptWithoutContinueIsRefused(t *testing.T) {
	h := newInvitationHarness(t, validResolution()) // BindingHash empty
	rec := h.accept(t, liveInvitation, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("accept on an unbound invitation = %d, want 403", rec.Code)
	}
	if h.accepted != 0 {
		t.Fatal("an invitation nobody continued was spent")
	}
	if len(h.bound) != 0 {
		t.Errorf("the refused accept bound the invitation: %v", h.bound)
	}
	if !strings.Contains(rec.Body.String(), `data-invitation-state="`+string(InvitationNotContinued)+`"`) {
		t.Errorf("the refusal is not the %q page:\n%s", InvitationNotContinued, rec.Body.String())
	}
	if ev := h.audit.last(); ev.Action != "invitation_accept_denied" || ev.FailureReason != "not_continued" {
		t.Errorf("the refusal was audited as %s/%s, want invitation_accept_denied/not_continued", ev.Action, ev.FailureReason)
	}

	// The step-up branch sits behind the same check: no Continue, no email.
	res := validResolution()
	res.Role, res.StepUp = "admin", true
	stepped := newInvitationHarness(t, res)
	issued := false
	stepped.srv.IssueMagicLink = func(context.Context, IssueMagicLinkInput) (IssueMagicLinkResult, error) {
		issued = true
		return IssueMagicLinkResult{RequestId: "req-1", BindingNonce: "nonce-1", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	if rec := stepped.accept(t, liveInvitation, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a step-up accept nobody continued = %d, want 403", rec.Code)
	}
	if issued {
		t.Error("a step-up invitation nobody continued sent its sign-in link")
	}
}

// An intent the page never posts is a broken client, not an Accept.
func TestAnUnknownIntentSpendsAndBindsNothing(t *testing.T) {
	h := newInvitationHarness(t, validResolution())
	rec := h.post(t, liveInvitation, "cancel", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("intent=cancel = %d, want 400", rec.Code)
	}
	if h.accepted != 0 || len(h.bound) != 0 {
		t.Errorf("an unknown intent spent (%d) or bound (%v) the invitation", h.accepted, h.bound)
	}
}

// ---------------------------------------------------------------------------
// Step-up for privileged roles
// ---------------------------------------------------------------------------

// An admin/owner invitation buys a magic link, not an account. It must NOT be
// spent here: burning it now would leave somebody who never received the mail
// holding a dead invitation and no account.
func TestStepUpIssuesAMagicLinkAndDoesNotSpendTheInvitation(t *testing.T) {
	nonce := "the-browser-that-continued"
	res := continuedResolution(nonce)
	res.Role = "admin"
	res.StepUp = true
	h := newInvitationHarness(t, res)

	var issuedTo, issuedInvitation string
	h.srv.IssueMagicLink = func(_ context.Context, in IssueMagicLinkInput) (IssueMagicLinkResult, error) {
		issuedTo = in.Email
		issuedInvitation = in.InvitationId
		return IssueMagicLinkResult{RequestId: "req-1", BindingNonce: "nonce-1", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}

	rec := h.accept(t, liveInvitation, []*http.Cookie{{Name: invitationCookieName, Value: nonce}})
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

// The Accept step warns about the extra step before its button is pressed. An
// unannounced extra step reads as the flow failing.
func TestTheStepUpPageSaysTheExtraStepIsComing(t *testing.T) {
	nonce := "the-browser-that-continued"
	cookie := &http.Cookie{Name: invitationCookieName, Value: nonce}
	stepUp := continuedResolution(nonce)
	stepUp.StepUp = true

	plain := newInvitationHarness(t, continuedResolution(nonce)).get("?code="+liveInvitation, cookie)
	stepped := newInvitationHarness(t, stepUp).get("?code="+liveInvitation, cookie)
	if page := readNative(t, stepped); page.Data.Step != webtempl.InvitationStepAccept || !page.Data.StepUp {
		t.Fatalf("the step-up Accept step reads step %q with StepUp=%v, want Accept and true", page.Data.Step, page.Data.StepUp)
	}
	if plain.Body.String() == stepped.Body.String() {
		t.Error("the step-up page is identical to the ordinary one, so the holder is not told an email is coming")
	}
}

// ---------------------------------------------------------------------------
// Transport, CSRF and the rate limit -- Continue shares all three
// ---------------------------------------------------------------------------

// The code is a bearer in a URL. Over plaintext it lands in proxy logs.
func TestPlaintextIsRefusedOnBothRoutes(t *testing.T) {
	h := newInvitationHarness(t, validResolution())

	req := httptest.NewRequest(http.MethodGet, "/invitation?code="+liveInvitation, nil)
	rec := httptest.NewRecorder()
	if req.Method == http.MethodGet {
		req.Header.Set("Accept", NativeMediaType)
		req.Header.Set("Origin", "https://os.example.test")
	}
	h.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("plaintext GET = %d, want 403", rec.Code)
	}

	for _, intent := range []string{"", webtempl.InvitationStepContinue} {
		form := url.Values{}
		form.Set("code", liveInvitation)
		if intent != "" {
			form.Set("intent", intent)
		}
		req2 := httptest.NewRequest(http.MethodPost, "/invitation/accept", strings.NewReader(form.Encode()))
		req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec2 := httptest.NewRecorder()
		h.mux.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusForbidden {
			t.Errorf("plaintext POST (intent %q) = %d, want 403", intent, rec2.Code)
		}
	}
	if h.accepted != 0 || len(h.bound) != 0 {
		t.Errorf("a plaintext POST spent (%d) or bound (%v) the invitation", h.accepted, h.bound)
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
	if req.Method == http.MethodGet {
		req.Header.Set("Accept", NativeMediaType)
		req.Header.Set("Origin", "https://os.example.test")
	}
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unwired /invitation = %d, want 404", rec.Code)
	}
}

// The form is CSRF-protected, Continue included: it writes the binding. /login
// and /setup are carved out of the middleware because nothing renders them on
// our behalf; this form is ours, so it takes the protection rather than the
// exemption.
func TestAcceptRequiresTheCSRFToken(t *testing.T) {
	h := newInvitationHarness(t, validResolution())
	if rec := h.postWithoutCSRF(liveInvitation, webtempl.InvitationStepContinue); rec.Code != http.StatusForbidden {
		t.Errorf("continue with no CSRF token = %d, want 403", rec.Code)
	}
	if len(h.bound) != 0 {
		t.Error("an invitation was bound by a request carrying no CSRF token")
	}

	bound := newInvitationHarness(t, continuedResolution("the-browser-that-continued"))
	if rec := bound.postWithoutCSRF(liveInvitation, ""); rec.Code != http.StatusForbidden {
		t.Errorf("accept with no CSRF token = %d, want 403", rec.Code)
	}
	if bound.accepted != 0 {
		t.Error("an invitation was spent by a request carrying no CSRF token")
	}
}

// Continue spends a token from the same per-IP bucket as everything else on
// these routes; a write is no reason to sit outside the limit.
func TestContinueIsRateLimitedLikeTheAccept(t *testing.T) {
	t.Setenv(envInvitationPerHour, "1")
	h := newInvitationHarness(t, validResolution())
	b := h.browser("198.51.100.9")
	b.open() // the only token this hour

	rec := b.press(webtempl.InvitationStepContinue)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("continue past the limit = %d, want 429", rec.Code)
	}
	if len(h.bound) != 0 || b.binding() != "" {
		t.Errorf("a rate-limited continue bound the invitation (stamps %v, cookie %q)", h.bound, b.binding())
	}
}

// Losing the binding race is the control WORKING. It must not be recorded as
// the same event as a broken write: a run of the first on one invitation is a
// forwarded email being continued by several people, and a run of the second
// is infrastructure. An operator greps the trail for exactly that difference.
func TestLosingTheBindingRaceIsRecordedApartFromABrokenWrite(t *testing.T) {
	lost := newInvitationHarness(t, validResolution())
	lost.bindErr = identity.ErrInvitationBoundElsewhere
	lostRec := lost.continueWith(t, liveInvitation, nil)

	broke := newInvitationHarness(t, validResolution())
	broke.bindErr = errors.New("the database fell over")
	brokeRec := broke.continueWith(t, liveInvitation, nil)

	lostEv, brokeEv := lost.audit.last(), broke.audit.last()
	if lostEv.Action != "invitation_binding_taken" || brokeEv.Action != "invitation_binding_not_stamped" {
		t.Fatalf("the two outcomes were audited as %q and %q", lostEv.Action, brokeEv.Action)
	}
	if lostEv.FailureReason == brokeEv.FailureReason {
		t.Errorf("both outcomes recorded the same reason %q, so a forwarding incident is indistinguishable from a database wobble", lostEv.FailureReason)
	}
	if lostEv.Outcome == identity.AuditOutcomeFailure {
		t.Error("losing the race was recorded as a failure; the control worked")
	}

	// Neither may hand this browser a binding cookie -- it does not hold one --
	// and neither may send it on to an Accept it could not use.
	for name, rec := range map[string]*httptest.ResponseRecorder{"bound-elsewhere": lostRec, "write-failed": brokeRec} {
		if c := bindingCookieFrom(rec); c != nil {
			t.Errorf("%s handed out a binding cookie for a binding it does not hold", name)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("%s redirected to %q as if the browser had continued", name, loc)
		}
	}

	// The race is the refusal; the broken write asks for a retry.
	if lostRec.Code != http.StatusForbidden {
		t.Errorf("a lost race = %d, want 403", lostRec.Code)
	}
	if brokeRec.Code != http.StatusInternalServerError {
		t.Errorf("a failed write = %d, want 500", brokeRec.Code)
	}
}
