// component/edge/resolution_tail_test.go
package edge

import (
	"net/http"
	"testing"
)

// tailFiles is the one bundle every case here is served from: a root
// document, a prerendered route, and an asset. Shared per FILE, never per
// package -- resolution_tail is a different question from handler_test.go's
// resolution ORDER and must not drift with it.
func tailFiles() map[string]string {
	return map[string]string{
		"index.html":         "ROOT",
		"products/shoe.html": "SHOE-PRERENDERED",
		"assets/app.js":      "JS",
	}
}

func tailSite(kind, tail string) *Site {
	return &Site{
		ID:             "s1",
		Hostname:       "shop.example.com",
		Status:         "live",
		Kind:           kind,
		ResolutionTail: tail,
	}
}

// AN ABSENT TAIL IS EXACTLY TODAY, PER KIND. This is the assertion that
// makes the field additive: every site row in every cluster carries no
// value, and every one of them must resolve the way it did before the field
// existed.
func TestAnAbsentResolutionTailReproducesTheKindsOwnBehaviour(t *testing.T) {
	for _, tc := range []struct {
		kind       string
		wantStatus int
		wantBody   string
	}{
		{"spa", http.StatusOK, "ROOT"},
		{storefrontKind, http.StatusOK, "ROOT"},
		{"static", http.StatusNotFound, ""},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			rec := serve(t, tailSite(tc.kind, ""), tailFiles(), "/nothing/here")
			if rec.Code != tc.wantStatus {
				t.Fatalf("kind %q with no tail: status %d, want %d", tc.kind, rec.Code, tc.wantStatus)
			}
			if tc.wantBody != "" && sourceHTML(rec.Body.String()) != tc.wantBody {
				t.Errorf("kind %q with no tail: body %q, want %q", tc.kind, sourceHTML(rec.Body.String()), tc.wantBody)
			}
		})
	}
}

// THE CASE THE FIELD EXISTS FOR (memql#5535). A multi-page storefront bundle
// -- the shape the first product actually built -- must be able to 404 a
// mistyped path while still being a storefront: still bound to its store,
// still served the storefront block in its runtime document, still admitted
// to Shopify by the policy. Before this field, choosing 404 meant declaring
// kind: static and giving up all three.
func TestAStorefrontCanChooseToAnswerNotFound(t *testing.T) {
	rec := serve(t, tailSite(storefrontKind, "not_found"), tailFiles(), "/nothing/here")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a storefront declaring not_found answered %d, want 404", rec.Code)
	}
	if sourceHTML(rec.Body.String()) == "ROOT" {
		t.Error("a storefront declaring not_found still fell back to index.html")
	}
}

// The other direction, and it is not hypothetical: a static-kind bundle that
// grows client-side routing can take the fallback without changing kind.
func TestAStaticSiteCanChooseTheFallback(t *testing.T) {
	rec := serve(t, tailSite("static", "fallback"), tailFiles(), "/nothing/here")
	if rec.Code != http.StatusOK {
		t.Fatalf("a static site declaring fallback answered %d, want 200", rec.Code)
	}
	if sourceHTML(rec.Body.String()) != "ROOT" {
		t.Errorf("body %q, want ROOT", sourceHTML(rec.Body.String()))
	}
}

// AN UNRECOGNISED VALUE READS AS ABSENT, NOT AS 404.
//
// The site row's enum is validated at the write path, so a value that is
// neither is a raw write that bypassed it. Falling back to the KIND is the
// fail-safe reading: a typo must not take a live site's every client-side
// route dark, which is what treating "anything I do not recognise" as
// not_found would do (memory: a default branch decides what an unknown value
// means).
func TestAnUnrecognisedResolutionTailReadsAsAbsent(t *testing.T) {
	for _, kind := range []string{"spa", storefrontKind} {
		t.Run(kind, func(t *testing.T) {
			rec := serve(t, tailSite(kind, "fallback_maybe"), tailFiles(), "/nothing/here")
			if rec.Code != http.StatusOK || sourceHTML(rec.Body.String()) != "ROOT" {
				t.Errorf("an unrecognised tail on %q answered %d/%q, want 200/ROOT",
					kind, rec.Code, sourceHTML(rec.Body.String()))
			}
		})
	}
	rec := serve(t, tailSite("static", "fallback_maybe"), tailFiles(), "/nothing/here")
	if rec.Code != http.StatusNotFound {
		t.Errorf("an unrecognised tail on static answered %d, want 404", rec.Code)
	}
}

// THE TAIL GOVERNS THE MISS AND NOTHING ELSE. Every rung of the resolution
// order above it is untouched: a real asset, a prerendered route and the
// root document all still resolve under not_found, which is the whole point
// of a multi-page bundle choosing it.
func TestTheResolutionTailGovernsOnlyTheMiss(t *testing.T) {
	site := tailSite(storefrontKind, "not_found")
	for _, tc := range []struct{ path, want string }{
		{"/", "ROOT"},
		{"/assets/app.js", "JS"},
		{"/products/shoe", "SHOE-PRERENDERED"},
	} {
		rec := serve(t, site, tailFiles(), tc.path)
		if rec.Code != http.StatusOK || sourceHTML(rec.Body.String()) != tc.want {
			t.Errorf("%s under not_found answered %d/%q, want 200/%q",
				tc.path, rec.Code, sourceHTML(rec.Body.String()), tc.want)
		}
	}
}

// A BUNDLE WITH NO index.html CANNOT FALL BACK, whatever it declares. The
// pre-existing fs.Stat guard is what makes that true, and it must survive
// the tail becoming a site property.
func TestFallbackStillRequiresAnIndexDocument(t *testing.T) {
	files := map[string]string{"assets/app.js": "JS"}
	rec := serve(t, tailSite("static", "fallback"), files, "/nothing/here")
	if rec.Code != http.StatusNotFound {
		t.Errorf("a bundle with no index.html answered %d under fallback, want 404", rec.Code)
	}
}
