package server

import (
	"context"
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
	err   error
	// errForCall fails ONE call rather than all of them, which is what an
	// extension's partial-write test needs: the pack's construct succeeds
	// and the client's does not (design record 2026-09-21, D5).
	errForCall func(n int, query string) error
}

func (e *shopperRecordingExecutor) Execute(ctx context.Context, query string) (any, error) {
	e.calls = append(e.calls, query)
	userID := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		userID = ac.UserId
	}
	e.actor = append(e.actor, userID)
	if e.errForCall != nil {
		if err := e.errForCall(len(e.calls)-1, query); err != nil {
			return nil, err
		}
	}
	if e.err != nil {
		return nil, e.err
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
		site:  &ShopperSite{ID: "site1", ShopperForms: true, StoreID: "store-live", PreviewStoreID: "store-dev"},
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
	assertRedirect(t, rec, "/reviews/thanks", "")
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
