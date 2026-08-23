package web

// magiclink_flow_test.go -- the acceptance criteria of the device-bound,
// approve-on-click flow (memql#4302; design section 9 items 1-5).
//
// The property under test is one sentence: A SESSION CAN ONLY EVER LAND ON
// THE DEVICE THAT ASKED FOR IT. Everything below is a way of failing that
// sentence.
//
// Two RETIRED behaviours get their own negative tests, because both are the
// kind a well-meaning simplification puts straight back:
//
//   - a GET that consumes (TestGetAuthCompleteNeverWrites)
//   - a no-cookie click that signs somebody in (TestNoCookieClickSignsNobodyIn)

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

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/magiclink"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeMLVerifier models the two verifier entry points over one row.
//
// It records every mutation so a test can assert that a GET performed NONE,
// which is the whole point of design decision D3 and cannot be observed any
// other way.
type fakeMLVerifier struct {
	mu       sync.Mutex
	row      *identity.MagicLinkRow
	inspects int
	finishes int
	// finishResult is what Finish returns on success.
	finishResult *magiclink.VerifyResult
	// inspectErr, when set, is returned by Inspect instead of the row.
	inspectErr error
}

func (f *fakeMLVerifier) Inspect(_ context.Context, plainToken, _, _ string) (*identity.MagicLinkRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects++
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	if strings.TrimSpace(plainToken) != "plain-token" {
		return nil, magiclink.ErrInvalidToken
	}
	if !f.row.ConsumedAt.IsZero() {
		return nil, magiclink.ErrTokenAlreadyUsed
	}
	cp := *f.row
	return &cp, nil
}

func (f *fakeMLVerifier) Finish(_ context.Context, in magiclink.FinishInput) (*magiclink.VerifyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishes++
	if !f.row.ConsumedAt.IsZero() {
		return nil, magiclink.ErrTokenAlreadyUsed
	}
	f.row.ConsumedAt = time.Now().UTC()
	return f.finishResult, nil
}

func (f *fakeMLVerifier) snapshot() identity.MagicLinkRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.row
}

// flowEngine is the narrowest EngineExecutor that answers the two constructs
// this flow's store calls issue. It shares its row with the fake verifier, so
// a test asserting "the GET changed nothing" is looking at the same state the
// handler would have written.
type flowEngine struct {
	mu  sync.Mutex
	row *identity.MagicLinkRow
	// unknown records anything the store issued that this fake does not
	// model, so a construct that silently returns zero rows -- which reads
	// exactly like "no such request" -- fails loudly instead.
	unknown []string
}

func (e *flowEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "query magicLinkRequestById("):
		fields := map[string]*structpb.Value{
			"id":    structpb.NewStringValue(e.row.ID),
			"email": structpb.NewStringValue(e.row.Email),
		}
		if e.row.BindingHash != "" {
			fields["bindingHash"] = structpb.NewStringValue(e.row.BindingHash)
		}
		if !e.row.ApprovedAt.IsZero() {
			fields["approvedAt"] = structpb.NewStringValue(e.row.ApprovedAt.Format(time.RFC3339Nano))
			fields["approvedFromIP"] = structpb.NewStringValue(e.row.ApprovedFromIP)
			fields["approvedUserAgent"] = structpb.NewStringValue(e.row.ApprovedUserAgent)
		}
		if !e.row.ConsumedAt.IsZero() {
			fields["consumedAt"] = structpb.NewStringValue(e.row.ConsumedAt.Format(time.RFC3339Nano))
		}
		if !e.row.ExpiresAt.IsZero() {
			fields["expiresAt"] = structpb.NewStringValue(e.row.ExpiresAt.Format(time.RFC3339Nano))
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{{Id: e.row.ID, Payload: &structpb.Struct{Fields: fields}}},
		}}, nil

	case strings.HasPrefix(q, "query clusterSettingsCurrent("):
		// The finish path reads the cluster domain to build the post-login
		// landing. Zero rows is a legitimate answer (a cluster with no
		// settings row yet) and exercises the fallback.
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil

	case strings.HasPrefix(q, "mutation approveMagicLinkRequest("):
		e.row.ApprovedAt = time.Now().UTC()
		e.row.ApprovedFromIP = "198.51.100.7"
		e.row.ApprovedUserAgent = "ClickerBrowser/1.0"
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	e.unknown = append(e.unknown, q)
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

const (
	testNonce       = "the-binding-nonce-value"
	testRequestId   = "ml-req-1"
	testMagicToken  = "plain-token"
	testAccountMail = "team@example.test"
)

type flowFixture struct {
	server   *Server
	verifier *fakeMLVerifier
	engine   *flowEngine
	audit    *recordingAudit
	sessions int
}

func newFlowFixture(t *testing.T) *flowFixture {
	t.Helper()
	cfg := identity.Config{
		Enabled:      true,
		BaseURL:      "https://identity.test",
		JWTAudience:  "memql",
		MagicLinkTTL: 10 * time.Minute,
	}
	s, err := NewServer(cfg, slog.Default(), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	fx := &flowFixture{
		verifier: &fakeMLVerifier{
			row: &identity.MagicLinkRow{
				ID:          testRequestId,
				Email:       testAccountMail,
				BindingHash: magiclink.HashBindingNonce(testNonce),
				ExpiresAt:   time.Now().UTC().Add(10 * time.Minute),
			},
			finishResult: &magiclink.VerifyResult{
				UserId:       "user-1",
				Email:        testAccountMail,
				AdminSession: true,
			},
		},
		audit: &recordingAudit{},
	}
	fx.engine = &flowEngine{row: fx.verifier.row}
	s.Store = &identity.Store{Engine: fx.engine, Logger: slog.Default()}
	s.mlVerifier = fx.verifier
	s.mlAudit = fx.audit
	s.mlSession = func(w http.ResponseWriter, _ *http.Request, _ BrowserSessionInput) error {
		fx.sessions++
		http.SetCookie(w, &http.Cookie{Name: "memql_admin", Value: "a-session-jwt", Path: "/"})
		return nil
	}
	fx.server = s
	return fx
}

// withBinding attaches the binding cookie -- i.e. makes this the requesting
// browser.
func withBinding(req *http.Request) *http.Request {
	req.AddCookie(&http.Cookie{Name: magicLinkCookieName, Value: testNonce})
	return req
}

func getComplete(t *testing.T, fx *flowFixture, bind bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/complete?ml="+testMagicToken, nil)
	if bind {
		req = withBinding(req)
	}
	rec := httptest.NewRecorder()
	fx.server.handleAuthComplete(rec, req)
	return rec
}

func postLanding(t *testing.T, fx *flowFixture, bind bool) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"ml": {testMagicToken}}
	req := httptest.NewRequest(http.MethodPost, "/auth/landing", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// X-Forwarded-For, because that is what reaches the identity node behind
	// the front door and it is what clientIP prefers.
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	req.Header.Set("User-Agent", "ClickerBrowser/1.0")
	if bind {
		req = withBinding(req)
	}
	rec := httptest.NewRecorder()
	fx.server.handleAuthLanding(rec, req)
	return rec
}

func getStatus(t *testing.T, fx *flowFixture, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/magic-link/status?request="+testRequestId, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: magicLinkCookieName, Value: cookie})
	}
	rec := httptest.NewRecorder()
	fx.server.handleMagicLinkStatus(rec, req)
	return rec
}

func postFinish(t *testing.T, fx *flowFixture, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"request": {testRequestId}}
	req := httptest.NewRequest(http.MethodPost, "/auth/magic-link/finish", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: magicLinkCookieName, Value: cookie})
	}
	rec := httptest.NewRecorder()
	fx.server.handleMagicLinkFinish(rec, req)
	return rec
}

func setCookieNames(rec *httptest.ResponseRecorder) []string {
	var out []string
	for _, raw := range rec.Result().Cookies() {
		if raw.MaxAge < 0 || raw.Value == "" {
			continue // a deletion, not a grant
		}
		out = append(out, raw.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// item 1 -- a GET never changes state
// ---------------------------------------------------------------------------

func TestGetAuthCompleteNeverWrites(t *testing.T) {
	fx := newFlowFixture(t)
	before := fx.verifier.snapshot()

	first := getComplete(t, fx, false)
	if first.Code != http.StatusOK {
		t.Fatalf("first GET status = %d, want 200", first.Code)
	}
	if !strings.Contains(first.Body.String(), "Confirm sign-in") {
		t.Fatalf("first GET did not render the confirmation page; body=%s", first.Body.String())
	}

	// A SECOND GET IS STILL A PAGE. This is the mail-scanner case: Outlook
	// SafeLinks, Gmail's proxy and every security appliance fetch the URLs in
	// a message, and against a consuming GET each of those burned the link
	// before its owner ever saw it.
	second := getComplete(t, fx, false)
	if second.Code != http.StatusOK {
		t.Fatalf("second GET status = %d, want 200 -- a prefetch must not burn the link", second.Code)
	}
	if !strings.Contains(second.Body.String(), "Confirm sign-in") {
		t.Error("the second GET did not render the confirmation page; a prefetcher burned the link")
	}

	after := fx.verifier.snapshot()
	if after != before {
		t.Errorf("GET /auth/complete changed the row.\n before: %+v\n  after: %+v\n\n"+
			"A GET MUST NOT CHANGE STATE (design D3). This is the retired behaviour: consumption "+
			"used to happen on the click, before any human had interacted with the page, which is "+
			"how mail scanners burned links and how whoever read a shared mailbox first spent "+
			"somebody else's credential.", before, after)
	}
	if fx.verifier.finishes != 0 {
		t.Errorf("%d finish(es) ran during GET requests, want 0", fx.verifier.finishes)
	}
	if names := setCookieNames(first); len(names) != 0 {
		t.Errorf("GET /auth/complete set cookies %v, want none", names)
	}
}

// ---------------------------------------------------------------------------
// item 2 -- the no-cookie click approves and nothing else
// ---------------------------------------------------------------------------

func TestNoCookieClickSignsNobodyIn(t *testing.T) {
	fx := newFlowFixture(t)

	rec := postLanding(t, fx, false)

	if fx.sessions != 0 {
		t.Fatal("a click without the binding cookie started a session.\n" +
			"This is the hijack the whole design exists to remove: on a shared mailbox it means " +
			"whoever reads the message first becomes the account, on a machine the account holder " +
			"cannot see or revoke.")
	}
	if names := setCookieNames(rec); len(names) != 0 {
		t.Errorf("the no-cookie branch set cookies %v, want none -- it must hand the clicker nothing", names)
	}
	if fx.verifier.finishes != 0 {
		t.Errorf("the no-cookie branch ran finish %d time(s), want 0", fx.verifier.finishes)
	}
}

// ---------------------------------------------------------------------------
// item 5 -- the cookie-bearing click finishes directly
// ---------------------------------------------------------------------------

func TestCookieClickFinishesInPlace(t *testing.T) {
	fx := newFlowFixture(t)

	rec := postLanding(t, fx, true)

	if fx.verifier.finishes != 1 {
		t.Fatalf("a click FROM the requesting browser ran finish %d time(s), want 1 -- "+
			"the same-device case must complete in the tab the mail client opened", fx.verifier.finishes)
	}
	if fx.sessions != 1 {
		t.Fatalf("the same-device click started %d session(s), want 1", fx.sessions)
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("same-device finish status = %d, want 303 -- the redirect follows a POST, and 303 "+
			"is the code that says GET the next thing", rec.Code)
	}
	// The binding is spent and must be retired.
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == magicLinkCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the binding cookie was not cleared after a successful finish")
	}
}

// ---------------------------------------------------------------------------
// item 3 -- the poll is cookie-gated
// ---------------------------------------------------------------------------

func TestStatusRequiresTheBindingCookie(t *testing.T) {
	fx := newFlowFixture(t)

	// No cookie at all.
	if rec := getStatus(t, fx, ""); rec.Code == http.StatusOK {
		t.Error("the status endpoint answered a caller with no binding cookie")
	}
	// A cookie that does not match.
	if rec := getStatus(t, fx, "some-other-nonce"); rec.Code == http.StatusOK {
		t.Error("the status endpoint answered a caller whose cookie does not match the row")
	}
}

// ---------------------------------------------------------------------------
// item 4 -- finish requires the cookie AND the approval
// ---------------------------------------------------------------------------

func TestFinishRefusesTheWrongDevice(t *testing.T) {
	fx := newFlowFixture(t)

	rec := postFinish(t, fx, "not-the-nonce")
	if fx.verifier.finishes != 0 {
		t.Fatal("finish ran for a caller who does not hold the binding cookie.\n" +
			"The request id is rendered on a page, so it is not a secret -- the cookie is the " +
			"only thing that says this browser asked for the link.")
	}
	if fx.sessions != 0 {
		t.Fatal("finish started a session for a caller who does not hold the binding cookie")
	}
	_ = rec
}

// ---------------------------------------------------------------------------
// the four terminal states each say something different
// ---------------------------------------------------------------------------

func TestInspectErrorsRenderDistinctMessages(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"invalid", magiclink.ErrInvalidToken, "invalid"},
		{"expired", magiclink.ErrTokenExpired, "expired"},
		{"already used", magiclink.ErrTokenAlreadyUsed, "already been used"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFlowFixture(t)
			fx.verifier.inspectErr = tc.err
			rec := getComplete(t, fx, false)
			body := strings.ToLower(rec.Body.String())
			if !strings.Contains(body, strings.ToLower(tc.want)) {
				t.Errorf("the %s state did not say so; body=%s\n\n"+
					"Each of these asks the reader for a DIFFERENT next step, so collapsing them "+
					"into one message costs somebody a support ticket.", tc.name, rec.Body.String())
			}
			if fx.verifier.finishes != 0 {
				t.Error("a failed inspect still ran finish")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the landing page never offers a way to continue in the wrong browser
// ---------------------------------------------------------------------------

func TestLandingPageOffersNoEscapeHatch(t *testing.T) {
	fx := newFlowFixture(t)
	body := getComplete(t, fx, false).Body.String()

	// It must NAME the account, masked -- an unnamed "confirm sign-in" page
	// is a phishing shape -- and must not print the address in full, because
	// anybody holding the link can reach this page.
	if strings.Contains(body, testAccountMail) {
		t.Errorf("the landing page printed the full address %q; it must be masked", testAccountMail)
	}
	if !strings.Contains(body, "@example.test") {
		t.Error("the landing page did not name the account at all; a confirm-sign-in page for an " +
			"unnamed account is indistinguishable from a phishing page")
	}
	// The no-cookie copy must say the session lands elsewhere.
	if !strings.Contains(strings.ToLower(body), "different device") {
		t.Error("the cross-device copy does not say the link was opened on a different device")
	}
	if !strings.Contains(strings.ToLower(body), "will not sign you in here") {
		t.Error("the cross-device page does not say it will not sign the clicker in.\n" +
			"There is deliberately no 'continue here anyway' affordance -- that button IS the " +
			"hijack (design D4).")
	}
}

// ---------------------------------------------------------------------------
// The whole point, end to end
// ---------------------------------------------------------------------------

// TestBClicksAndASignsIn is the epic's headline sentence as a test.
//
// A asks for a link from computer A and holds the binding cookie. B opens the
// link first on computer B. B is handed nothing; the request becomes
// approved; A's tab notices and completes the sign-in on A's machine.
//
// It is also design section 9 item 12 in miniature. The flow keeps NO process
// memory: the approval is a row field and the cookie's digest is on the same
// row, so the handlers below could be served by two different identity
// replicas and this test would read identically. That is why there is no
// affinity requirement anywhere in the flow.
func TestBClicksAndASignsIn(t *testing.T) {
	fx := newFlowFixture(t)

	// --- B, on another machine, with no binding cookie ---
	bRec := postLanding(t, fx, false)

	if fx.sessions != 0 {
		t.Fatal("B's click signed B in. This is the entire hijack.")
	}
	if names := setCookieNames(bRec); len(names) != 0 {
		t.Fatalf("B's click set cookies %v -- B must be handed nothing at all", names)
	}
	if body := bRec.Body.String(); !strings.Contains(body, "Sign-in confirmed") {
		t.Fatalf("B did not get the go-back-to-your-device page; body=%s", body)
	}
	if got, ok := fx.audit.find("magic_link_approved"); !ok {
		t.Fatalf("no magic_link_approved audit row; actions=%v", auditActions(fx.audit))
	} else {
		// THE ROW THAT NAMES B. This is what an operator reads after the fact.
		if got.SourceIP != "198.51.100.7" {
			t.Errorf("magic_link_approved recorded sourceIP %q, want the approving device's; "+
				"naming who clicked is the trace the cross-device case exists to leave", got.SourceIP)
		}
	}

	// --- A's tab polls and sees the approval ---
	status := getStatus(t, fx, testNonce)
	if status.Code != http.StatusOK {
		t.Fatalf("A's poll status = %d, want 200", status.Code)
	}
	if !strings.Contains(status.Body.String(), `"approved"`) {
		t.Fatalf("A's poll returned %s, want approved", strings.TrimSpace(status.Body.String()))
	}

	// --- A finishes, on A's machine ---
	finish := postFinish(t, fx, testNonce)
	if fx.verifier.finishes != 1 {
		t.Fatalf("finish ran %d time(s) for A, want 1", fx.verifier.finishes)
	}
	if fx.sessions != 1 {
		t.Fatalf("A ended with %d session(s), want 1 -- the session must land on the device that asked", fx.sessions)
	}
	if finish.Code != http.StatusSeeOther {
		t.Errorf("finish status = %d, want 303", finish.Code)
	}

	// --- and the link is spent ---
	if again := postFinish(t, fx, testNonce); fx.verifier.finishes != 1 {
		t.Errorf("a second finish ran; the link must be spent exactly once (status=%d)", again.Code)
	}

	if len(fx.engine.unknown) > 0 {
		t.Fatalf("the handlers issued constructs the fake does not model: %s",
			strings.Join(fx.engine.unknown, "; "))
	}
}

// TestSecondApprovalIsIdempotent pins the conditional half of the approval
// write: a second click on the same link must not overwrite the first
// approver's device facts, and must audit the denial rather than silently
// doing nothing.
func TestSecondApprovalIsIdempotent(t *testing.T) {
	fx := newFlowFixture(t)

	postLanding(t, fx, false)
	first := fx.verifier.snapshot()

	second := postLanding(t, fx, false)
	after := fx.verifier.snapshot()

	if !after.ApprovedAt.Equal(first.ApprovedAt) {
		t.Errorf("a second click moved approvedAt from %v to %v -- the write is conditional on "+
			"approvedAt being empty, so the FIRST approver's facts are the ones that stand",
			first.ApprovedAt, after.ApprovedAt)
	}
	if _, ok := fx.audit.find("magic_link_approval_denied"); !ok {
		t.Errorf("a repeat approval emitted no magic_link_approval_denied row; actions=%v", auditActions(fx.audit))
	}
	if !strings.Contains(second.Body.String(), "Already confirmed") {
		t.Errorf("a repeat approval did not say so; body=%s", second.Body.String())
	}
	if fx.sessions != 0 {
		t.Error("a repeat approval started a session")
	}
}

// TestFinishBeforeApprovalIsRefused pins the asymmetry between the two
// finishing paths.
//
// The landing POST carries the emailed token, so a cookie-bearing click may
// finish outright. This one carries only a request id -- a value rendered on
// a page -- so it additionally needs the approval, which is what says a human
// holding the token said yes.
func TestFinishBeforeApprovalIsRefused(t *testing.T) {
	fx := newFlowFixture(t)

	rec := postFinish(t, fx, testNonce)

	if fx.verifier.finishes != 0 {
		t.Fatal("finish ran before anybody had opened the link.\n" +
			"The request id is not a credential -- it is printed on the check-email page -- so " +
			"the cookie alone must not complete a sign-in nobody confirmed.")
	}
	if fx.sessions != 0 {
		t.Fatal("a session was created before the link was opened")
	}
	if got, ok := fx.audit.find("magic_link_finish_blocked"); !ok {
		t.Errorf("no magic_link_finish_blocked row; actions=%v", auditActions(fx.audit))
	} else if got.FailureReason != "not_approved" {
		t.Errorf("finish blocked for reason %q, want not_approved", got.FailureReason)
	}
	_ = rec
}

// auditActions lists the recorded actions, for a failure message that names
// what DID happen rather than only what did not.
func auditActions(a *recordingAudit) []string {
	out := make([]string, 0, len(a.events))
	for _, ev := range a.events {
		out = append(out, ev.Action)
	}
	return out
}

// TestCheckEmailRendersAWorkingPoller asserts the page the requesting tab
// sits on actually carries everything the poller needs.
//
// It is the join between the two halves of the flow, and it fails silently
// if it is wrong: the page renders, the person waits, and nothing ever
// happens. Three ways that can happen, all checked here --
//
//   - no request id, so the poller has nothing to ask about;
//   - no script, so nothing polls;
//   - an EMPTY CSRF token in the finish form, so the POST the poller
//     eventually submits is rejected by the middleware. That one is the
//     subtle one: the token is minted by the middleware on this very GET and
//     reaches the template through the request context, so it is only
//     non-empty because that plumbing works.
func TestCheckEmailRendersAWorkingPoller(t *testing.T) {
	fx := newFlowFixture(t)
	mux := http.NewServeMux()
	fx.server.Mount(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/check-email?email=team%40acme.test&request="+testRequestId, nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /check-email status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `data-request="`+testRequestId+`"`) {
		t.Error("the page does not carry the request id; the poller has nothing to ask about")
	}
	if !strings.Contains(body, "check-email.js") {
		t.Error("the poller script is not loaded; nothing will ever notice the approval")
	}
	if !strings.Contains(body, `action="/auth/magic-link/finish"`) {
		t.Error("the finish form is missing; the poller has nothing to submit")
	}

	// The CSRF field must carry a REAL token, not an empty attribute.
	idx := strings.Index(body, CSRFFormField)
	if idx < 0 {
		t.Fatal("the finish form carries no CSRF field, so the middleware will reject the POST")
	}
	segment := body[idx:]
	if end := strings.Index(segment, ">"); end >= 0 {
		segment = segment[:end]
	}
	if strings.Contains(segment, `value=""`) {
		t.Errorf("the CSRF field is empty: %s\n\n"+
			"The token is minted by the middleware on THIS GET and reaches the template through "+
			"the request context. An empty value means that plumbing broke, and the failure is "+
			"silent: the page renders, the person waits, and the finish POST is rejected.", segment)
	}

	// And a page with no request id renders no poller at all, rather than one
	// that will ask about nothing forever.
	bare := httptest.NewRecorder()
	mux.ServeHTTP(bare, httptest.NewRequest(http.MethodGet, "/check-email?email=team%40acme.test", nil))
	if strings.Contains(bare.Body.String(), "check-email.js") {
		t.Error("a check-email page with no request id still loaded the poller")
	}
}

// TestRevokedSessionIsRejectedOnMePages is memql#4303's acceptance clause
// "after revoke the memql_admin bearer is rejected by the middleware".
//
// # Why it is not free
//
// The DSL comment on revokeAuthSession says the middleware's tokenHash lookup
// rejects a revoked row, and on the mesh nodes revocation is enforced by the
// epoch claim instead -- so on identity's OWN cookie surface, nothing was
// consulting the row. A JWT that verifies and has not expired would have kept
// every /me/* page working for the rest of its life after "sign out
// everywhere", which is exactly the reassurance somebody presses it for.
func TestRevokedSessionIsRejectedOnMePages(t *testing.T) {
	const bearer = "a-signed-jwt"
	revoked := false

	eng := &revocationEngine{tokenHash: identity.HashSessionToken(bearer), revoked: &revoked}
	s, err := NewServer(identity.Config{Enabled: true, BaseURL: "https://identity.test", JWTAudience: "memql"}, slog.Default(), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.Store = &identity.Store{Engine: eng, Logger: slog.Default()}

	req := httptest.NewRequest(http.MethodGet, "/me/devices", nil)
	req.AddCookie(&http.Cookie{Name: "memql_admin", Value: bearer})

	if s.sessionRevoked(req, bearer) {
		t.Fatal("a live session was reported revoked")
	}
	revoked = true
	if !s.sessionRevoked(req, bearer) {
		t.Fatal("a REVOKED session was not reported revoked.\n" +
			"Signature and expiry say the token was minted and has not aged out. Neither says the " +
			"person has since signed out everywhere -- that fact is on the row, which is the reason " +
			"the row exists.")
	}

	// FAILS OPEN on an unreadable store, so a database blip does not sign
	// everybody out of their own settings page. The token is still a validly
	// signed, unexpired credential in that case.
	broken := &identity.Store{Engine: &revocationEngine{err: true}, Logger: slog.Default()}
	s.Store = broken
	if s.sessionRevoked(req, bearer) {
		t.Error("an unreadable session store was treated as a revocation")
	}
	// And a bearer with no row at all is not revoked -- a PAT, or a row that
	// predates the change.
	s.Store = &identity.Store{Engine: &revocationEngine{tokenHash: "some-other-hash"}, Logger: slog.Default()}
	if s.sessionRevoked(req, bearer) {
		t.Error("a bearer with no session row was treated as revoked")
	}
}

// revocationEngine answers authSessionByTokenHash for one known hash.
type revocationEngine struct {
	tokenHash string
	revoked   *bool
	err       bool
}

func (e *revocationEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	if e.err {
		return nil, context.DeadlineExceeded
	}
	if !strings.HasPrefix(q, "query authSessionByTokenHash(") {
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	if !strings.Contains(q, e.tokenHash) || e.tokenHash == "" {
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	fields := map[string]*structpb.Value{
		"id":        structpb.NewStringValue("sess-1"),
		"tokenHash": structpb.NewStringValue(e.tokenHash),
	}
	if e.revoked != nil && *e.revoked {
		fields["revokedAt"] = structpb.NewStringValue(time.Now().UTC().Format(time.RFC3339Nano))
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{{Id: "sess-1", Payload: &structpb.Struct{Fields: fields}}},
	}}, nil
}
