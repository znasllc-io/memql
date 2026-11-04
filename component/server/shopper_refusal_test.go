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
