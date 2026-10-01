package server

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity/verifier"
	"github.com/znasllc-io/memql/component/memql"
)

func shopperAuthenticatedHandler(h http.Handler) http.Handler {
	return verifier.HTTPMiddleware(&verifier.Verifier{}, verifier.MiddlewareOptions{
		PublicPaths: PublicPaths(), SelfAuthenticatedPaths: SelfAuthenticatedPaths(),
	})(h)
}

func TestDeclaredShopperRoutesReachTheirAuthorizationWithoutBearer(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
			h, exec, _ := shopperFixture(t)
			path, body, want := "/reads/reviews/published?productHandle=boot", "", http.StatusOK
			if method == http.MethodPost {
				path, body, want = "/forms/reviews/review", "productHandle=boot&body=nice", http.StatusSeeOther
			}
			req := httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for key, value := range goodStamp() {
				req.Header.Set(key, value)
			}
			rec := httptest.NewRecorder()
			shopperAuthenticatedHandler(h).ServeHTTP(rec, req)
			if rec.Code != want || len(exec.calls) != 1 || exec.actor[0] != "user-merchant" {
				t.Fatalf("status=%d calls=%v actors=%v body=%s", rec.Code, exec.calls, exec.actor, rec.Body.String())
			}
			if !strings.Contains(exec.calls[0], `storeId: "store-live"`) {
				t.Fatalf("unscoped call: %s", exec.calls[0])
			}
		})
	}
}

func TestShopperMiddlewareDoesNotSkipStampAndBindingChecks(t *testing.T) {
	for _, name := range []string{"missing stamp", "wrong owner", "wrong site", "surface off", "wrong store", "live binding missing store", "preview binding missing store", "unbound with store"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
			h, exec, sites := shopperFixture(t)
			stamp := goodStamp()
			switch name {
			case "missing stamp":
				stamp = nil
			case "wrong owner":
				stamp[memql.ShopperOwnerHeader] = "another-user"
			case "wrong site":
				stamp[memql.ShopperSiteHeader] = "another-site"
			case "surface off":
				sites.site.ShopperForms = false
			case "wrong store":
				stamp[memql.ShopperStoreHeader] = "another-store"
			case "live binding missing store":
				sites.site.PreviewStoreID = ""
				delete(stamp, memql.ShopperStoreHeader)
			case "preview binding missing store":
				sites.site.StoreID = ""
				delete(stamp, memql.ShopperStoreHeader)
			case "unbound with store":
				sites.site.StoreID = ""
				sites.site.PreviewStoreID = ""
			}
			req := httptest.NewRequest(http.MethodGet, "/reads/reviews/published?productHandle=boot", nil)
			for key, value := range stamp {
				req.Header.Set(key, value)
			}
			rec := httptest.NewRecorder()
			shopperAuthenticatedHandler(h).ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound || len(exec.calls) != 0 {
				t.Fatalf("status=%d executed=%v", rec.Code, exec.calls)
			}
		})
	}
}

func TestShopperSelfAuthenticationDoesNotExemptOtherPaths(t *testing.T) {
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	shopperFixture(t)
	for _, path := range []string{
		"/reads/reviews", "/reads/reviews/", "/reads/reviews/unknown", "/reads/reviews/published/deeper",
		"/reads/reviews/published/", "/reads/reviews%2Fpublished", "/%72eads/reviews/published",
		"/reads/reviews/published%20", "/forms/reviews/unknown", "/forms/reviews/review/deeper",
		"/memql/query", "/automations/resume", "/artifacts",
	} {
		t.Run(path, func(t *testing.T) {
			called := false
			h := shopperAuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) }))
			req := httptest.NewRequest(http.MethodGet, path, nil)
			for key, value := range goodStamp() {
				req.Header.Set(key, value)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if called || rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d handlerCalled=%v", rec.Code, called)
			}
		})
	}
}

func TestShopperSelfAuthenticationUsesExactDeclaredPathsWithBase(t *testing.T) {
	shopperFixture(t)
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "/tenant")
	want := []string{"/forms/reviews/review", "/tenant/forms/reviews/review", "/reads/reviews/published", "/tenant/reads/reviews/published"}
	if got := ShopperSelfAuthenticatedPaths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths=%v want=%v", got, want)
	}
	if err := AssertSelfAuthenticatedRoutesFailClosed(); err != nil {
		t.Fatal(err)
	}
}
