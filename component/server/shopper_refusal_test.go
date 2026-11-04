package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// shopper_refusal_test.go -- A DECLARED REFUSAL REACHES THE SHOPPER BY NAME.
//
// Before, every refusal from a construct became "failed", so a shopper whose
// review link had expired was told something went wrong on the merchant's
// side and to press Back and try again. A form or read that declares its
// refusal codes now has them forwarded; anything undeclared is still
// "failed", so a pack cannot leak an arbitrary error string onto the page.

func refusalFixture(t *testing.T) (*ShopperHandler, *shopperRecordingExecutor) {
	t.Helper()
	t.Cleanup(memql.ResetShopperSurfaceForTest)
	memql.ResetShopperSurfaceForTest()
	codes := []string{"link_expired", "link_used", "link_invalid"}
	memql.RegisterShopperForm(memql.ShopperForm{
		Pack: "reviews", Name: "review", Construct: "reviewsSubmitFromLink", Kind: memql.ShopperReadKindBuiltin,
		Fields: []memql.ShopperField{
			{Name: "requestToken", Required: true, MaxLength: 200},
			{Name: "productHandle", Required: true, MaxLength: 200},
			{Name: "body", Required: true, MaxLength: 4000},
		},
		RedirectOK: "/reviews/thank-you", RedirectError: "/reviews/problem",
		Refusals: codes,
	})
	memql.RegisterShopperRead(memql.ShopperRead{
		Pack: "reviews", Name: "draft", Construct: "reviewsDraftForLink", Kind: memql.ShopperReadKindBuiltin,
		Fields:   []memql.ShopperField{{Name: "requestToken", Required: true, MaxLength: 200}},
		Refusals: codes,
	})
	exec := &shopperRecordingExecutor{}
	sites := &shopperFakeSites{
		owner: "user-merchant",
		site:  &ShopperSite{ID: "site1", ShopperForms: true, StoreID: "store-live", PreviewStoreID: "store-dev"},
	}
	return NewShopperHandler(exec, sites, nil), exec
}

const linkPost = "requestToken=t0k3n&productHandle=boot&body=nice"

func TestADeclaredRefusalRedirectsWithItsOwnCode(t *testing.T) {
	h, exec := refusalFixture(t)
	exec.err = fmt.Errorf("reviews: %w", memql.NewShopperRefusal("link_expired", "expired 2026-11-01"))
	rec := shopperPost(h, "/forms/reviews/review", linkPost, goodStamp())
	assertRedirect(t, rec, "/reviews/problem", "link_expired")
}

func TestAnUndeclaredRefusalIsStillFailed(t *testing.T) {
	h, exec := refusalFixture(t)
	exec.err = memql.NewShopperRefusal("store_on_fire", "x")
	rec := shopperPost(h, "/forms/reviews/review", linkPost, goodStamp())
	assertRedirect(t, rec, "/reviews/problem", shopperReasonFailed)
}

func TestTheBuiltinFormIsCalledAsABuiltin(t *testing.T) {
	h, exec := refusalFixture(t)
	rec := shopperPost(h, "/forms/reviews/review", linkPost, goodStamp())
	assertRedirect(t, rec, "/reviews/thank-you", "")
	if len(exec.calls) != 1 || len(exec.calls[0]) < 8 || exec.calls[0][:8] != "builtin " {
		t.Fatalf("executed %v, want one builtin call", exec.calls)
	}
}

func TestAReadAnswersADeclaredRefusalAsJSON(t *testing.T) {
	h, exec := refusalFixture(t)
	exec.err = memql.NewShopperRefusal("link_used", "published")
	req := httptest.NewRequest(http.MethodGet, "/reads/reviews/draft?requestToken=t0k3n", nil)
	for k, v := range goodStamp() {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for a refusal the caller can be told: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != "link_used" {
		t.Fatalf("body = %s, want {\"error\":\"link_used\"}", rec.Body.String())
	}
}

// A READ THAT ANSWERS PER SECRET IS NEVER CACHED. The draft read's answer is
// one buyer's own review, keyed by the token in their link: a shared cache
// must not keep it, a stale copy would show old words right after an edit,
// and the link must not leak through a Referer. The published read is the
// same for everyone and stays cacheable.
func TestAPrivateReadIsNeverCachedAndLeaksNoReferrer(t *testing.T) {
	t.Cleanup(memql.ResetShopperSurfaceForTest)
	memql.ResetShopperSurfaceForTest()
	memql.RegisterShopperRead(memql.ShopperRead{
		Pack: "reviews", Name: "draft", Construct: "reviewsDraftForLink", Kind: memql.ShopperReadKindBuiltin,
		Fields:  []memql.ShopperField{{Name: "requestToken", Required: true, MaxLength: 200}},
		Private: true,
	})
	memql.RegisterShopperRead(memql.ShopperRead{
		Pack: "reviews", Name: "published", Construct: "reviewsPublishedForProduct", Kind: memql.ShopperReadKindBuiltin,
		Fields: []memql.ShopperField{{Name: "productHandle", Required: true, MaxLength: 200}},
	})
	sites := &shopperFakeSites{owner: "user-merchant",
		site: &ShopperSite{ID: "site1", ShopperForms: true, StoreID: "store-live"}}
	h := NewShopperHandler(&shopperRecordingExecutor{}, sites, nil)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for k, v := range goodStamp() {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	draft := get("/reads/reviews/draft?requestToken=t0k3n")
	if cc := draft.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("draft Cache-Control = %q, want no-store", cc)
	}
	if rp := draft.Header().Get("Referrer-Policy"); rp != "no-referrer" {
		t.Fatalf("draft Referrer-Policy = %q, want no-referrer", rp)
	}
	if cc := get("/reads/reviews/published?productHandle=boot").Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Fatalf("published Cache-Control = %q, want it still cacheable", cc)
	}
	// A refusal from a private read is not cached either.
	ex := &shopperRecordingExecutor{err: memql.NewShopperRefusal("link_used", "x")}
	memql.ResetShopperSurfaceForTest()
	memql.RegisterShopperRead(memql.ShopperRead{
		Pack: "reviews", Name: "draft", Construct: "reviewsDraftForLink", Kind: memql.ShopperReadKindBuiltin,
		Fields: []memql.ShopperField{{Name: "requestToken", Required: true, MaxLength: 200}},
		Private: true, Refusals: []string{"link_used"},
	})
	h = NewShopperHandler(ex, sites, nil)
	if cc := get("/reads/reviews/draft?requestToken=t0k3n").Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("a private read's refusal Cache-Control = %q, want no-store", cc)
	}
}
