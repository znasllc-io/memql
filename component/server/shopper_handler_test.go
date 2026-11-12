package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// shopperRecordingExecutor records every call, so "nothing was executed" is an
// assertion rather than an inference.
type shopperRecordingExecutor struct {
	calls []string
	actor []string
	// account is the account each call's context stated it acts in
	// (auth.CallAccountFromContext), or shopperNoAccount when it stated none.
	account []string
	err     error
	result  any
	// errForCall fails ONE call rather than all of them, which is what an
	// extension's partial-write test needs: the pack's construct succeeds
	// and the client's does not (design record 2026-09-21, D5).
	errForCall func(n int, query string) error
}

// shopperNoAccount is what the recording executor notes for a call whose
// context stated no account, so "none" is a value a test can assert rather
// than an empty string that also means "not recorded".
const shopperNoAccount = "(none)"

func (e *shopperRecordingExecutor) Execute(ctx context.Context, query string) (any, error) {
	e.calls = append(e.calls, query)
	userID := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		userID = ac.UserId
	}
	e.actor = append(e.actor, userID)
	account, stated := auth.CallAccountFromContext(ctx)
	if !stated {
		account = shopperNoAccount
	}
	e.account = append(e.account, account)
	if e.errForCall != nil {
		if err := e.errForCall(len(e.calls)-1, query); err != nil {
			return nil, err
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	if e.result != nil {
		return e.result, nil
	}
	return map[string]any{"ok": true}, nil
}

type shopperFakeSites struct {
	site  *ShopperSite
	owner string
	err   error
	asked int
}

func (f *shopperFakeSites) ShopperSite(_ context.Context, siteID, ownerUserID string) (*ShopperSite, error) {
	f.asked++
	if f.err != nil {
		return nil, f.err
	}
	// THE OWNER IS SELF-CHECKING: a user who does not own the site reads
	// zero rows. The fake models exactly that, because it is the property
	// the handler leans on.
	if f.site == nil || f.site.ID != siteID || ownerUserID != f.owner {
		return nil, nil
	}
	return f.site, nil
}

func shopperFixture(t *testing.T) (*ShopperHandler, *shopperRecordingExecutor, *shopperFakeSites) {
	t.Helper()
	t.Cleanup(memql.ResetShopperSurfaceForTest)
	memql.ResetShopperSurfaceForTest()
	memql.RegisterShopperForm(memql.ShopperForm{
		Pack: "reviews", Name: "review", Construct: "submitReview",
		Fields: []memql.ShopperField{
			{Name: "productHandle", Required: true, MaxLength: 200},
			{Name: "body", Required: true, MaxLength: 4000},
			{Name: "rating", Numeric: true, MaxLength: 2},
			{Name: "authorName", MaxLength: 120},
		},
		RedirectOK: "/reviews/thanks", RedirectError: "/reviews/problem",
	})
	memql.RegisterShopperRead(memql.ShopperRead{
		Pack: "reviews", Name: "published", Construct: "reviewsPublishedForProduct",
		Kind:   memql.ShopperReadKindBuiltin,
		Fields: []memql.ShopperField{{Name: "productHandle", Required: true, MaxLength: 200}},
	})
	exec := &shopperRecordingExecutor{}
	sites := &shopperFakeSites{
		owner: "user-merchant",
		site:  &ShopperSite{ID: "site1", ShopperForms: true, StoreID: "store-live", PreviewStoreID: "store-dev", AccountID: "acct-merchant"},
	}
	return NewShopperHandler(exec, sites, nil), exec, sites
}

func shopperPost(h *ShopperHandler, path, body string, stamp map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range stamp {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func goodStamp() map[string]string {
	return map[string]string{
		memql.ShopperSiteHeader:  "site1",
		memql.ShopperStoreHeader: "store-live",
		memql.ShopperOwnerHeader: "user-merchant",
	}
}

// FAILS CLOSED WITH NO CREDENTIALS, which is what membership in
// HandlerAuthorizedPaths() certifies. A request with no edge stamp -- the
// shape a direct caller sends -- reaches no read and no write.
func TestAShopperFormWithNoStampExecutesNothing(t *testing.T) {
	h, exec, sites := shopperFixture(t)

	rec := shopperPost(h, "/forms/reviews/review", "productHandle=boot&body=nice", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executed %v, want nothing", exec.calls)
	}
	if sites.asked != 0 {
		t.Fatal("an unstamped request must not even reach the site read")
	}
}

// A FORGED OWNER REFUSES ITSELF. The site is read under the named user, so
// a user who does not own it reads zero rows -- with no comparison written
// down to get wrong.
func TestAForgedOwnerReadsZeroRowsAndExecutesNothing(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	stamp := goodStamp()
	stamp[memql.ShopperOwnerHeader] = "user-someone-else"

	rec := shopperPost(h, "/forms/reviews/review", "productHandle=boot&body=nice", stamp)

	if len(exec.calls) != 0 {
		t.Fatalf("executed %v, want nothing", exec.calls)
	}
	assertRedirect(t, rec, "/reviews/problem", shopperReasonUnavailable)
}

func TestASiteWithTheSurfaceOffExecutesNothing(t *testing.T) {
	h, exec, sites := shopperFixture(t)
	sites.site.ShopperForms = false

	rec := shopperPost(h, "/forms/reviews/review", "productHandle=boot&body=nice", goodStamp())

	if len(exec.calls) != 0 {
		t.Fatalf("executed %v, want nothing", exec.calls)
	}
	assertRedirect(t, rec, "/reviews/problem", shopperReasonUnavailable)
}

// A STAMP CANNOT AIM A ROW AT A STORE THE SITE IS NOT BOUND TO.
func TestAStoreTheSiteIsNotBoundToExecutesNothing(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	stamp := goodStamp()
	stamp[memql.ShopperStoreHeader] = "store-somebody-elses"

	rec := shopperPost(h, "/forms/reviews/review", "productHandle=boot&body=nice", stamp)

	if len(exec.calls) != 0 {
		t.Fatalf("executed %v, want nothing", exec.calls)
	}
	assertRedirect(t, rec, "/reviews/problem", shopperReasonUnavailable)
}

// The preview store is accepted, because that is how a submit made while
// previewing lands under the development store.
func TestThePreviewStoreIsAccepted(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	stamp := goodStamp()
	stamp[memql.ShopperStoreHeader] = "store-dev"

	shopperPost(h, "/forms/reviews/review", "productHandle=boot&body=nice", stamp)

	if len(exec.calls) != 1 {
		t.Fatalf("executed %v, want one call", exec.calls)
	}
	if !strings.Contains(exec.calls[0], `storeId: "store-dev"`) {
		t.Fatalf("call %q does not carry the preview store", exec.calls[0])
	}
}

// AN UNDECLARED ROUTE IS A 404 AND EXECUTES NOTHING -- the shape a disabled
// pack takes, since its constructs are never loaded.
func TestAnUndeclaredRouteExecutesNothing(t *testing.T) {
	h, exec, _ := shopperFixture(t)

	rec := shopperPost(h, "/forms/reviews/nosuchform", "body=nice", goodStamp())

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executed %v, want nothing", exec.calls)
	}
}

// A GOOD POST: the declared fields reach the construct, the stamped ones
// are added, and the actor is the merchant.
func TestAGoodPostWritesUnderTheSiteOwner(t *testing.T) {
	h, exec, _ := shopperFixture(t)

	rec := shopperPost(h, "/forms/reviews/review",
		"productHandle=boot&body=Great+boots&rating=5&authorName=Sam", goodStamp())

	if len(exec.calls) != 1 {
		t.Fatalf("executed %v, want one call", exec.calls)
	}
	call := exec.calls[0]
	for _, want := range []string{
		"submitReview(",
		`authorName: "Sam"`,
		`body: "Great boots"`,
		`productHandle: "boot"`,
		"rating: 5",
		`siteId: "site1"`,
		`storeId: "store-live"`,
	} {
		if !strings.Contains(call, want) {
			t.Errorf("call %q does not contain %q", call, want)
		}
	}
	if exec.actor[0] != "user-merchant" {
		t.Fatalf("actor = %q, want the site owner", exec.actor[0])
	}
	// AND IN THE SITE'S ACCOUNT. The owner is borrowed, and the account they
	// belong to is not necessarily the one this storefront does: resolved from
	// the actor, a review a shopper left on one client's site would be recorded
	// in whichever account its owner joined first.
	if exec.account[0] != "acct-merchant" {
		t.Fatalf("the write acts in account %q, want the site's", exec.account[0])
	}
	assertRedirect(t, rec, "/reviews/thanks", "")
}

// THE SITE'S ACCOUNT, in whichever spelling the row hands it back, and none when
// the site has none. A site with no account states nothing, and the engine then
// resolves the account from the borrowed owner as it did before this existed.
func TestAShopperCallActsInTheSitesAccount(t *testing.T) {
	for name, tc := range map[string]struct{ stored, want string }{
		"bare":                   {"acct-merchant", "acct-merchant"},
		"canonical":              {"v1:accounts:account:acct-merchant", "acct-merchant"},
		"the primary account":    {"self", "self"},
		"a site with no account": {"", shopperNoAccount},
	} {
		t.Run(name, func(t *testing.T) {
			h, exec, sites := shopperFixture(t)
			sites.site.AccountID = tc.stored

			shopperPost(h, "/forms/reviews/review", "productHandle=boot&body=nice", goodStamp())
			req := httptest.NewRequest(http.MethodGet, "/reads/reviews/published?productHandle=boot", nil)
			for k, v := range goodStamp() {
				req.Header.Set(k, v)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if len(exec.calls) != 2 {
				t.Fatalf("executed %v, want the form's write and the read", exec.calls)
			}
			for i, kind := range []string{"the form's write", "the read"} {
				if exec.account[i] != tc.want {
					t.Errorf("%s acts in account %q, want %q", kind, exec.account[i], tc.want)
				}
			}
		})
	}
}

// NOTHING THE REQUEST SENT CAN NAME THE ACCOUNT. It is read off the site row the
// handler resolved under the owner, like the store, and a form field or a query
// parameter of the same name is an undeclared input: dropped.
func TestAShopperCannotNameTheAccount(t *testing.T) {
	h, exec, _ := shopperFixture(t)

	shopperPost(h, "/forms/reviews/review",
		"productHandle=boot&body=nice&accountId=acct-somebody-elses", goodStamp())

	if len(exec.calls) != 1 {
		t.Fatalf("executed %v, want one call", exec.calls)
	}
	if exec.account[0] != "acct-merchant" {
		t.Fatalf("a form-supplied accountId placed the write in %q", exec.account[0])
	}
	if strings.Contains(exec.calls[0], "accountId") {
		t.Fatalf("an undeclared accountId reached the construct: %q", exec.calls[0])
	}
}

// AN UNDECLARED INPUT IS DROPPED, NOT REFUSED: a browser posts what the
// page contains, and a hidden token or a stray input must not turn every
// submission into an error.
func TestAnUndeclaredInputIsDropped(t *testing.T) {
	h, exec, _ := shopperFixture(t)

	shopperPost(h, "/forms/reviews/review",
		"productHandle=boot&body=nice&csrf=abc&storeId=store-somebody-elses", goodStamp())

	if len(exec.calls) != 1 {
		t.Fatalf("executed %v, want one call", exec.calls)
	}
	if strings.Contains(exec.calls[0], "csrf") {
		t.Fatal("an undeclared input reached the construct")
	}
	// AND THE STAMPED VALUE WINS. The registry refuses a declared field
	// named storeId; this is the second half of that, at the only place it
	// could still go wrong.
	if !strings.Contains(exec.calls[0], `storeId: "store-live"`) {
		t.Fatalf("a form-supplied storeId overrode the stamp: %q", exec.calls[0])
	}
}

// A DECLARED field that fails its own rule IS refused -- that is the
// merchant's contract being broken, and the shopper can fix it.
func TestADeclaredFieldThatFailsItsRuleIsRefused(t *testing.T) {
	cases := map[string]string{
		"missing required": "body=nice",
		"over max length":  "productHandle=boot&body=" + strings.Repeat("x", 4001),
		"not a number":     "productHandle=boot&body=nice&rating=five",
		"blank required":   "productHandle=&body=nice",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h, exec, _ := shopperFixture(t)
			rec := shopperPost(h, "/forms/reviews/review", body, goodStamp())
			if len(exec.calls) != 0 {
				t.Fatalf("executed %v, want nothing", exec.calls)
			}
			assertRedirect(t, rec, "/reviews/problem", shopperReasonInvalid)
		})
	}
}

// A GET must never write: prefetchers and link scanners follow links.
func TestAFormRouteRefusesGet(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/forms/reviews/review", nil)
	for k, v := range goodStamp() {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executed %v, want nothing", exec.calls)
	}
}

// A downstream refusal is a redirect, not a 500: the shopper sees the
// merchant's own page saying it did not go through.
func TestARefusedWriteRedirectsWithAReason(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	exec.err = context.DeadlineExceeded

	rec := shopperPost(h, "/forms/reviews/review", "productHandle=boot&body=nice", goodStamp())

	assertRedirect(t, rec, "/reviews/problem", shopperReasonFailed)
}

func TestAReadAnswersJSONUnderTheSiteOwner(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/reads/reviews/published?productHandle=boot", nil)
	for k, v := range goodStamp() {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	if len(exec.calls) != 1 || !strings.Contains(exec.calls[0], "builtin reviewsPublishedForProduct(") {
		t.Fatalf("executed %v", exec.calls)
	}
	if !strings.Contains(exec.calls[0], `storeId: "store-live"`) {
		t.Fatalf("the read does not carry the effective store: %q", exec.calls[0])
	}
	if exec.actor[0] != "user-merchant" {
		t.Fatalf("actor = %q, want the site owner", exec.actor[0])
	}
	// A declared read can be a builtin, and a builtin can write: it acts in the
	// site's account for the same reason the form does.
	if exec.account[0] != "acct-merchant" {
		t.Fatalf("the read acts in account %q, want the site's", exec.account[0])
	}
}

func TestAReadWithNoStampExecutesNothing(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/reads/reviews/published?productHandle=boot", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executed %v, want nothing", exec.calls)
	}
}

// The declared route set is what the surface IS, so a route declared on a
// pack this cluster did not load is unreachable -- which is the mechanism
// that makes a disabled pack inert on this path too.
func TestTheShopperSurfaceIsExactlyWhatIsDeclared(t *testing.T) {
	h, exec, _ := shopperFixture(t)
	memql.ResetShopperSurfaceForTest()

	rec := shopperPost(h, "/forms/reviews/review", "productHandle=boot&body=nice", goodStamp())

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 once the declaration is gone", rec.Code)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executed %v, want nothing", exec.calls)
	}
}

func assertRedirect(t *testing.T, rec *httptest.ResponseRecorder, path, reason string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (a form post's reply must be fetched with GET)", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, path) {
		t.Fatalf("Location = %q, want it to start with %q", loc, path)
	}
	if reason == "" {
		if strings.Contains(loc, shopperReasonParam+"=") {
			t.Fatalf("Location = %q, want no reason on success", loc)
		}
		return
	}
	if !strings.Contains(loc, shopperReasonParam+"="+reason) {
		t.Fatalf("Location = %q, want reason=%s", loc, reason)
	}
}

// accountForm registers a form whose construct asks for the site's account,
// beside the fixture's review form, which does not.
func accountForm(t *testing.T) {
	t.Helper()
	memql.RegisterShopperForm(memql.ShopperForm{
		Pack: "trade", Name: "apply", Construct: "submitTradeApplication",
		Fields:         []memql.ShopperField{{Name: "company", Required: true, MaxLength: 200}},
		AcceptsAccount: true,
		RedirectOK:     "/trade/thanks", RedirectError: "/trade/problem",
	})
}

// THE SITE'S ORGANIZATION IS STAMPED INTO THE ARGUMENTS OF A FORM THAT ASKS
// FOR IT, the way its store and its id are, so the rows its construct writes
// carry the organization whose people may read them.
func TestTheHandlerStampsTheSitesAccountIntoAFormThatAcceptsIt(t *testing.T) {
	h, exec, sites := shopperFixture(t)
	accountForm(t)
	sites.site.AccountID = "acct-north"

	rec := shopperPost(h, "/forms/trade/apply", "company=North+Ltd", goodStamp())

	if len(exec.calls) != 1 {
		t.Fatalf("executed %v, want one call", exec.calls)
	}
	if !strings.Contains(exec.calls[0], `accountId: "acct-north"`) {
		t.Fatalf("the site's organization was not stamped into the construct of a form that asks for it: %q", exec.calls[0])
	}
	assertRedirect(t, rec, "/trade/thanks", "")
}

// A FORM THAT DOES NOT ASK IS HANDED NONE: its construct's arguments carry no
// accountId, whatever the site's account is. The account it acts in is on the
// context (TestAShopperCallActsInTheSitesAccount holds that).
func TestAFormThatDoesNotAskForTheAccountIsHandedNone(t *testing.T) {
	h, exec, sites := shopperFixture(t)
	sites.site.AccountID = "acct-north"

	shopperPost(h, "/forms/reviews/review", "productHandle=boot&body=nice", goodStamp())

	if len(exec.calls) != 1 {
		t.Fatalf("executed %v, want one call", exec.calls)
	}
	if strings.Contains(exec.calls[0], "accountId") {
		t.Fatalf("a form that does not ask was handed an accountId: %q", exec.calls[0])
	}
	if exec.account[0] != "acct-north" {
		t.Fatalf("the construct acts in account %q, want the site's", exec.account[0])
	}
}

// A POSTED accountId LOSES TO THE SITE'S. It is not a declared field, so it is
// dropped; and the stamp is written after the declared fields, so it could not
// win even if it got that far.
func TestAFormCannotSupplyItsOwnAccountId(t *testing.T) {
	h, exec, sites := shopperFixture(t)
	accountForm(t)
	sites.site.AccountID = "acct-north"

	shopperPost(h, "/forms/trade/apply", "company=North+Ltd&accountId=acct-other", goodStamp())

	if len(exec.calls) != 1 {
		t.Fatalf("executed %v, want one call", exec.calls)
	}
	if strings.Contains(exec.calls[0], "acct-other") {
		t.Fatalf("a form-supplied accountId reached the construct: %q", exec.calls[0])
	}
	if !strings.Contains(exec.calls[0], `accountId: "acct-north"`) {
		t.Fatalf("the site's organization was not stamped: %q", exec.calls[0])
	}
}

// evidenceRecordingExecutor records the evidence each call's context carried.
type evidenceRecordingExecutor struct {
	shopperRecordingExecutor
	evidence []memql.ShopperEvidence
	had      []bool
}

func (e *evidenceRecordingExecutor) Execute(ctx context.Context, query string) (any, error) {
	ev, ok := memql.ShopperEvidenceFrom(ctx)
	e.evidence = append(e.evidence, ev)
	e.had = append(e.had, ok)
	return e.shopperRecordingExecutor.Execute(ctx, query)
}

// evidenceFixture declares one form that asks for evidence and one that does not.
func evidenceFixture(t *testing.T) (*ShopperHandler, *evidenceRecordingExecutor) {
	t.Helper()
	t.Cleanup(memql.ResetShopperSurfaceForTest)
	memql.ResetShopperSurfaceForTest()
	memql.RegisterShopperForm(memql.ShopperForm{
		Pack: "wholesale", Name: "acceptance", Construct: "wholesaleAcceptTerms", Kind: memql.ShopperReadKindBuiltin,
		Fields: []memql.ShopperField{
			{Name: "signedName", Required: true, MaxLength: 120},
			{Name: "acceptTerms", Required: true, MaxLength: 8, Enum: []string{"on"}},
			{Name: "termsSha256", Required: true, MaxLength: 64, Format: memql.ShopperFormatSHA256},
		},
		Evidence:   true,
		RedirectOK: "/wholesale/accepted", RedirectError: "/wholesale/link-problem",
	})
	memql.RegisterShopperForm(memql.ShopperForm{
		Pack: "reviews", Name: "review", Construct: "submitReview",
		Fields:     []memql.ShopperField{{Name: "body", Required: true, MaxLength: 4000}},
		RedirectOK: "/reviews/thanks", RedirectError: "/reviews/problem",
	})
	exec := &evidenceRecordingExecutor{}
	sites := &shopperFakeSites{
		owner: "user-merchant",
		site:  &ShopperSite{ID: "site1", ShopperForms: true, StoreID: "store-live"},
	}
	return NewShopperHandler(exec, sites, nil), exec
}

// AN EVIDENCE FORM CARRIES WHAT THE EDGE SAW AND WHAT THE BROWSER SENT, on the
// context the construct runs under and nowhere a form could reach: the address
// from the edge's own header, the User-Agent cut to 512 bytes.
func TestAnEvidenceFormCarriesTheEdgesAddressAndTheBrowserOnItsContext(t *testing.T) {
	h, exec := evidenceFixture(t)
	stamp := goodStamp()
	stamp[memql.ShopperClientHeader] = "198.51.100.7"
	stamp["User-Agent"] = strings.Repeat("a", 600)
	rec := shopperPost(h, "/forms/wholesale/acceptance", "signedName=Ada+Example&acceptTerms=on&termsSha256="+strings.Repeat("a", 64)+"&clientAddress=203.0.113.9&userAgent=forged", stamp)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/wholesale/accepted" {
		t.Fatalf("the post answered %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(exec.evidence) != 1 || !exec.had[0] {
		t.Fatalf("the construct ran with no evidence: %+v", exec.had)
	}
	got := exec.evidence[0]
	if got.ClientAddress != "198.51.100.7" || got.UserAgent != strings.Repeat("a", 512) {
		t.Fatalf("the evidence is %q / %d bytes, want the edge's address and 512 bytes", got.ClientAddress, len(got.UserAgent))
	}
	if strings.Contains(exec.calls[0], "clientAddress") || strings.Contains(exec.calls[0], "forged") {
		t.Fatalf("a posted copy of the evidence reached the construct: %s", exec.calls[0])
	}
}

// A User-Agent cut mid-character is cut at the character before it.
func TestAnEvidenceUserAgentIsCutOnACharacterBoundary(t *testing.T) {
	h, exec := evidenceFixture(t)
	stamp := goodStamp()
	stamp["User-Agent"] = strings.Repeat("a", 511) + "é"
	shopperPost(h, "/forms/wholesale/acceptance", "signedName=Ada&acceptTerms=on&termsSha256="+strings.Repeat("a", 64), stamp)
	if got := exec.evidence[0].UserAgent; got != strings.Repeat("a", 511) {
		t.Fatalf("the User-Agent was cut to %d bytes, want the 511 before the cut character", len(got))
	}
}

// A FORM THAT DID NOT ASK FOR EVIDENCE IS GIVEN NONE, whatever the edge sent.
func TestANonEvidenceFormIsGivenNoEvidence(t *testing.T) {
	h, exec := evidenceFixture(t)
	stamp := goodStamp()
	stamp[memql.ShopperClientHeader] = "198.51.100.7"
	stamp["User-Agent"] = "Example Browser/1.0"
	rec := shopperPost(h, "/forms/reviews/review", "body=nice", stamp)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the post answered %d", rec.Code)
	}
	if len(exec.had) != 1 || exec.had[0] {
		t.Fatalf("a form that asked for none carried evidence: %+v", exec.evidence)
	}
}

// A CONSTRUCT'S invalid IS NOT FORWARDED (ruling 53): every undeclared code is
// still failed, on a form and on a read, exactly as before.
func TestAConstructsInvalidIsStillFailed(t *testing.T) {
	h, exec := evidenceFixture(t)
	exec.err = fmt.Errorf("wholesale: %w", memql.NewShopperRefusal("invalid", "a box is not on"))
	rec := shopperPost(h, "/forms/wholesale/acceptance", "signedName=Ada&acceptTerms=on&termsSha256="+strings.Repeat("a", 64), goodStamp())
	if loc := rec.Header().Get("Location"); loc != "/wholesale/link-problem?reason=failed" {
		t.Fatalf("the refusal redirected to %q", loc)
	}
	memql.RegisterShopperRead(memql.ShopperRead{Pack: "wholesale", Name: "terms", Construct: "wholesaleTermsForLink",
		Kind: memql.ShopperReadKindBuiltin, Fields: []memql.ShopperField{{Name: "requestToken", Required: true}}, Private: true})
	req := httptest.NewRequest(http.MethodGet, "/reads/wholesale/terms?requestToken=t", nil)
	for k, v := range goodStamp() {
		req.Header.Set(k, v)
	}
	read := httptest.NewRecorder()
	h.ServeHTTP(read, req)
	if read.Code != http.StatusBadGateway || !strings.Contains(read.Body.String(), `"failed"`) {
		t.Fatalf("the read answered %d %s", read.Code, read.Body.String())
	}
}

// THE HANDLER REFUSES A BOX THAT IS NOT on AND A FINGERPRINT THAT IS NOT 64 HEX
// CHARACTERS BEFORE THE CONSTRUCT RUNS (ruling 53): the field rules are the
// declaration's, and the refusal is the surface's own invalid.
func TestTheHandlerRefusesABoxAndAFingerprintBeforeTheConstructRuns(t *testing.T) {
	good := "signedName=Ada&acceptTerms=on&termsSha256=" + strings.Repeat("a", 64)
	for name, body := range map[string]string{
		"a box that is not on":   "signedName=Ada&acceptTerms=yes&termsSha256=" + strings.Repeat("a", 64),
		"63 hex characters":      "signedName=Ada&acceptTerms=on&termsSha256=" + strings.Repeat("a", 63),
		"64 characters, not hex": "signedName=Ada&acceptTerms=on&termsSha256=" + strings.Repeat("g", 64),
	} {
		h, exec := evidenceFixture(t)
		rec := shopperPost(h, "/forms/wholesale/acceptance", body, goodStamp())
		if loc := rec.Header().Get("Location"); loc != "/wholesale/link-problem?reason=invalid" || len(exec.calls) != 0 {
			t.Fatalf("%s: redirected to %q after %d calls", name, loc, len(exec.calls))
		}
	}
	h, exec := evidenceFixture(t)
	if loc := shopperPost(h, "/forms/wholesale/acceptance", good, goodStamp()).Header().Get("Location"); loc != "/wholesale/accepted" || len(exec.calls) != 1 {
		t.Fatalf("a good post redirected to %q after %d calls", loc, len(exec.calls))
	}
}

// AN EDGE HEADER THAT IS NOT AN ADDRESS IS HANDED ON AS NOTHING.
func TestAnEvidenceAddressThatIsNotAnAddressIsEmpty(t *testing.T) {
	h, exec := evidenceFixture(t)
	stamp := goodStamp()
	stamp[memql.ShopperClientHeader] = "any text the caller likes"
	shopperPost(h, "/forms/wholesale/acceptance", "signedName=Ada&acceptTerms=on&termsSha256="+strings.Repeat("a", 64), stamp)
	if len(exec.evidence) != 1 || exec.evidence[0].ClientAddress != "" {
		t.Fatalf("the evidence carries %+v", exec.evidence)
	}
}

// THE HANDLER REFUSES AN ADDRESS THAT IS NOT ONE BEFORE THE CONSTRUCT RUNS
// (ruling 54): a field declaring the email format is judged by the declaration,
// and the refusal is the surface's own invalid.
func TestTheHandlerRefusesAMalformedAddressBeforeTheConstructRuns(t *testing.T) {
	fixture := func() (*ShopperHandler, *shopperRecordingExecutor) {
		t.Cleanup(memql.ResetShopperSurfaceForTest)
		memql.ResetShopperSurfaceForTest()
		memql.RegisterShopperForm(memql.ShopperForm{
			Pack: "wholesale", Name: "application", Construct: "wholesaleSubmitApplication", Kind: memql.ShopperReadKindBuiltin,
			Fields: []memql.ShopperField{
				{Name: "companyName", Required: true, MaxLength: 200},
				{Name: "applicantEmail", Required: true, MaxLength: 320, Format: memql.ShopperFormatEmail},
			},
			RedirectOK: "/wholesale/thank-you", RedirectError: "/wholesale/problem",
		})
		exec := &shopperRecordingExecutor{}
		sites := &shopperFakeSites{owner: "user-merchant", site: &ShopperSite{ID: "site1", ShopperForms: true, StoreID: "store-live"}}
		return NewShopperHandler(exec, sites, nil), exec
	}
	for _, email := range []string{"not+an+address", "a%40b%40example.com", "a+b%40example.com", "%40example.com", "a%40"} {
		h, exec := fixture()
		rec := shopperPost(h, "/forms/wholesale/application", "companyName=Kestrel+Cycles&applicantEmail="+email, goodStamp())
		if loc := rec.Header().Get("Location"); loc != "/wholesale/problem?reason=invalid" || len(exec.calls) != 0 {
			t.Errorf("%s: redirected to %q after %d calls", email, loc, len(exec.calls))
		}
	}
	h, exec := fixture()
	rec := shopperPost(h, "/forms/wholesale/application", "companyName=Kestrel+Cycles&applicantEmail=+ada%40kestrel.example.com+", goodStamp())
	if loc := rec.Header().Get("Location"); loc != "/wholesale/thank-you" || len(exec.calls) != 1 {
		t.Fatalf("a good address redirected to %q after %d calls", loc, len(exec.calls))
	}
}
