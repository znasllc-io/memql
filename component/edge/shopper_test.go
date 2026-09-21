package edge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// shopperUpstream records what the bff would have received.
type shopperUpstream struct {
	server *httptest.Server
	path   string
	site   string
	store  string
	owner  string
	hits   int
}

func newShopperUpstream(t *testing.T) *shopperUpstream {
	t.Helper()
	up := &shopperUpstream{}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.hits++
		up.path = r.URL.Path
		up.site = r.Header.Get(ShopperSiteHeader)
		up.store = r.Header.Get(ShopperStoreHeader)
		up.owner = r.Header.Get(ShopperOwnerHeader)
		w.WriteHeader(http.StatusSeeOther)
	}))
	t.Cleanup(up.server.Close)
	return up
}

func shopperSite(mutate func(*Site)) *Site {
	s := &Site{
		ID: "site1", Hostname: "shop.example.com", Status: "live",
		Kind: "shopify_storefront", OwnerUserID: "user-merchant",
		ShopperForms: true,
		Store:        &BoundStore{ID: "store-live", Domain: "live.myshopify.com"},
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func shopperHandler(t *testing.T, site *Site, target string) *Handler {
	t.Helper()
	return NewHandler(Options{
		Resolver:  staticResolver{site: site},
		Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
		APITarget: target,
	})
}

func postShopperForm(t *testing.T, h *Handler, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = "shop.example.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "198.51.100.7:40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// PROPERTY 1: OFF BY DEFAULT, and off is 404 rather than 403. A site that
// did not ask for a public form endpoint must look like it has no such path
// rather than like it has one it is guarding.
func TestTheShopperSurfaceIsOffUnlessTheSiteTurnsItOn(t *testing.T) {
	up := newShopperUpstream(t)
	h := shopperHandler(t, shopperSite(func(s *Site) { s.ShopperForms = false }), up.server.URL)

	rec := postShopperForm(t, h, "/_memql/forms/reviews/review", "body=nice")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 -- an un-enabled surface must be absent, not forbidden", rec.Code)
	}
	if up.hits != 0 {
		t.Fatal("a refused request must not reach the bff at all")
	}
}

// PROPERTY 2: THE EFFECTIVE STORE IS WHATEVER previewSite ALREADY PUT
// THERE. This is design D9 falling out of D7 rather than being implemented
// a second time, so it is asserted directly: substituting the site's Store
// is the whole mechanism.
func TestTheStampCarriesTheEffectiveStore(t *testing.T) {
	up := newShopperUpstream(t)
	h := shopperHandler(t, shopperSite(nil), up.server.URL)

	postShopperForm(t, h, "/_memql/forms/reviews/review", "body=nice")

	if up.hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", up.hits)
	}
	if up.store != "store-live" {
		t.Fatalf("stamped store = %q, want store-live", up.store)
	}
	if up.site != "site1" || up.owner != "user-merchant" {
		t.Fatalf("stamped site/owner = %q/%q, want site1/user-merchant", up.site, up.owner)
	}
	if up.path != "/forms/reviews/review" {
		t.Fatalf("upstream path = %q, want /forms/reviews/review -- the marker is stripped, not swapped", up.path)
	}
}

// The preview substitution is what makes the above true under a preview, so
// the substituted Site is exercised directly: previewSite replaces Store
// with PreviewStore, and the stamp must follow it.
func TestAPreviewSubmitCarriesTheDevelopmentStore(t *testing.T) {
	up := newShopperUpstream(t)
	site := shopperSite(func(s *Site) {
		s.CandidateRef = "blob://sites/site1/v2/"
		s.PreviewStore = &BoundStore{ID: "store-dev", Domain: "dev.myshopify.com"}
	})
	previewed := previewSite(site, &PreviewGrant{ID: "g1", SiteID: "site1", CandidateRef: "blob://sites/site1/v2/"})
	h := shopperHandler(t, previewed, up.server.URL)

	postShopperForm(t, h, "/_memql/forms/reviews/review", "body=nice")

	if up.store != "store-dev" {
		t.Fatalf("stamped store under preview = %q, want store-dev -- a row written while "+
			"previewing must be invisible to every live-scoped read", up.store)
	}
}

// PROPERTY 3: THE STAMP IS STRIPPED THEN SET. A client supplying its own
// must not survive the hop -- this is the whole reason the bff may treat
// the header as a pointer.
func TestAClientSuppliedStampIsStripped(t *testing.T) {
	up := newShopperUpstream(t)
	h := shopperHandler(t, shopperSite(nil), up.server.URL)

	req := httptest.NewRequest(http.MethodPost, "/_memql/forms/reviews/review", strings.NewReader("body=nice"))
	req.Host = "shop.example.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(ShopperSiteHeader, "site-someone-elses")
	req.Header.Set(ShopperStoreHeader, "store-someone-elses")
	req.Header.Set(ShopperOwnerHeader, "user-someone-else")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if up.site != "site1" || up.store != "store-live" || up.owner != "user-merchant" {
		t.Fatalf("a client-supplied stamp survived the hop: site=%q store=%q owner=%q",
			up.site, up.store, up.owner)
	}
}

// AND THE STRIP IS WHAT CLOSES THE CASE `Set` DOES NOT.
//
// This test exists because the one above passes without the strip: the stamp
// is written with Set, which replaces, so a client-supplied site or owner is
// overwritten whether or not it was deleted first. The STORE header is
// different -- it is set only when there IS a store, so on an UNBOUND
// storefront nothing overwrites it and the client's own value would ride
// through to the bff untouched.
//
// That is the hole the strip closes, and without this case the strip could
// be deleted with every test still green.
func TestAForgedStoreCannotRideThroughAnUnboundStorefront(t *testing.T) {
	up := newShopperUpstream(t)
	h := shopperHandler(t, shopperSite(func(s *Site) { s.Store = nil }), up.server.URL)

	req := httptest.NewRequest(http.MethodPost, "/_memql/forms/reviews/review", strings.NewReader("body=nice"))
	req.Host = "shop.example.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(ShopperStoreHeader, "store-somebody-elses")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if up.hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", up.hits)
	}
	if up.store != "" {
		t.Fatalf("a client-supplied store survived to the bff on an unbound storefront: %q. "+
			"Nothing overwrites that header when there is no store, so the strip is the only "+
			"thing standing between a forged value and the write path", up.store)
	}
}

// PROPERTY 4a: SIZE CAPPED, before the hop.
func TestAnOversizeBodyIsRefusedBeforeTheHop(t *testing.T) {
	up := newShopperUpstream(t)
	h := shopperHandler(t, shopperSite(nil), up.server.URL)

	body := strings.Repeat("x", int(defaultShopperMaxBytes)+1)
	rec := postShopperForm(t, h, "/_memql/forms/reviews/review", "body="+body)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if up.hits != 0 {
		t.Fatal("an oversize body must be refused before it reaches the bff")
	}
	if got := rec.Header().Get(shopperRefusalHeader); got != "too_large" {
		t.Fatalf("refusal header = %q, want too_large", got)
	}
}

// PROPERTY 4b: RATE LIMITED, per site and per address, before the hop.
func TestSubmissionsAreRateLimitedPerSiteAndAddress(t *testing.T) {
	up := newShopperUpstream(t)
	h := shopperHandler(t, shopperSite(nil), up.server.URL)

	allowed := 0
	var last *httptest.ResponseRecorder
	for i := 0; i < defaultShopperRatePerMinute+3; i++ {
		last = postShopperForm(t, h, "/_memql/forms/reviews/review", "body=nice")
		if last.Code == http.StatusSeeOther {
			allowed++
		}
	}
	if allowed != defaultShopperRatePerMinute {
		t.Fatalf("allowed %d submissions, want exactly the burst of %d",
			allowed, defaultShopperRatePerMinute)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("status past the burst = %d, want 429", last.Code)
	}
	if up.hits != defaultShopperRatePerMinute {
		t.Fatalf("upstream hits = %d, want %d -- a refused request must not reach the bff",
			up.hits, defaultShopperRatePerMinute)
	}
}

// A DIFFERENT SITE HAS ITS OWN BUCKET: one merchant's storefront being
// hammered must not stop shoppers writing to another's.
func TestTheRateLimitIsPerSite(t *testing.T) {
	b := newShopperBucket(2)
	now := time.Now()
	if !b.allow("siteA|1.2.3.4", now) || !b.allow("siteA|1.2.3.4", now) {
		t.Fatal("the burst must be spendable")
	}
	if b.allow("siteA|1.2.3.4", now) {
		t.Fatal("the burst must be exhausted")
	}
	if !b.allow("siteB|1.2.3.4", now) {
		t.Fatal("a second site must have its own bucket")
	}
	if !b.allow("siteA|5.6.7.8", now) {
		t.Fatal("a second address must have its own bucket")
	}
	// It refills.
	if !b.allow("siteA|1.2.3.4", now.Add(time.Minute)) {
		t.Fatal("the bucket must refill over a minute")
	}
}

// A CLUSTER-OWNED SITE HAS NO AUTHORITY TO BORROW, so the surface is
// refused rather than run under a synthetic actor.
func TestASiteWithNoOwnerIsRefused(t *testing.T) {
	up := newShopperUpstream(t)
	h := shopperHandler(t, shopperSite(func(s *Site) { s.OwnerUserID = "" }), up.server.URL)

	rec := postShopperForm(t, h, "/_memql/forms/reviews/review", "body=nice")

	if up.hits != 0 {
		t.Fatal("a site with no owner must not reach the bff")
	}
	if got := rec.Header().Get(shopperRefusalHeader); got != "not_available" {
		t.Fatalf("refusal header = %q, want not_available", got)
	}
}

// A GET must never perform a write: link scanners and prefetchers follow
// links, which is the same rule /unsubscribe keeps.
func TestAFormPathRefusesGet(t *testing.T) {
	up := newShopperUpstream(t)
	h := shopperHandler(t, shopperSite(nil), up.server.URL)

	req := httptest.NewRequest(http.MethodGet, "/_memql/forms/reviews/review", nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if up.hits != 0 {
		t.Fatal("a GET to a form path must not reach the bff")
	}
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a GET to a form path must not succeed")
	}
}

func TestAReadPathRefusesPost(t *testing.T) {
	up := newShopperUpstream(t)
	h := shopperHandler(t, shopperSite(nil), up.server.URL)

	rec := postShopperForm(t, h, "/_memql/reads/reviews/published", "q=1")

	if up.hits != 0 {
		t.Fatal("a POST to a read path must not reach the bff")
	}
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a POST to a read path must not succeed")
	}
}

func TestAReadIsForwardedOnGet(t *testing.T) {
	up := newShopperUpstream(t)
	h := shopperHandler(t, shopperSite(nil), up.server.URL)

	req := httptest.NewRequest(http.MethodGet, "/_memql/reads/reviews/published?productHandle=boot", nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	_ = rec

	if up.path != "/reads/reviews/published" {
		t.Fatalf("upstream path = %q, want /reads/reviews/published", up.path)
	}
	if up.store != "store-live" {
		t.Fatalf("a read must carry the effective store too, got %q", up.store)
	}
}

// EXACTLY TWO SEGMENTS. A third is a route the registry can never name, and
// blessing it would mean the bff parses a shape the edge did not understand.
func TestTheShopperPathTakesExactlyTwoSegments(t *testing.T) {
	cases := map[string]bool{
		"/_memql/forms/reviews/review":       true,
		"/_memql/reads/reviews/published":    true,
		"/_memql/forms/reviews":              false,
		"/_memql/forms/reviews/review/extra": false,
		"/_memql/forms/":                     false,
		"/_memql/forms/reviews/":             false,
		"/_memql/ws":                         false,
	}
	for path, want := range cases {
		if _, ok := parseShopperPath(path); ok != want {
			t.Errorf("parseShopperPath(%q) ok = %v, want %v", path, ok, want)
		}
	}
}

// The refusal page carries NOTHING from the request, which is what makes
// escaping impossible to get wrong on a page served on somebody else's
// origin.
func TestTheRefusalPageCarriesNothingFromTheRequest(t *testing.T) {
	up := newShopperUpstream(t)
	h := shopperHandler(t, shopperSite(func(s *Site) { s.ShopperForms = false }), up.server.URL)

	req := httptest.NewRequest(http.MethodPost,
		"/_memql/forms/reviews/review?x=%3Cscript%3Ealert(1)%3C/script%3E",
		strings.NewReader("body=<script>alert(1)</script>"))
	req.Host = "shop.example.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "<script") || strings.Contains(body, "alert(1)") {
		t.Fatal("the refusal page reflected request content")
	}
}

func TestEveryRefusalPageIsUnindexableAndScriptless(t *testing.T) {
	for _, refusal := range []shopperRefusal{
		shopperRefusalTooLarge, shopperRefusalRateLimited, shopperRefusalNotAvailable,
	} {
		rec := httptest.NewRecorder()
		writeShopperRefusal(rec, refusal)

		if rec.Code != refusal.status {
			t.Errorf("%s: status = %d, want %d", refusal.code, rec.Code, refusal.status)
		}
		if got := rec.Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
			t.Errorf("%s: X-Robots-Tag = %q, want noindex -- this page is on a merchant's domain",
				refusal.code, got)
		}
		if got := rec.Header().Get(shopperRefusalHeader); got != refusal.code {
			t.Errorf("%s: refusal header = %q", refusal.code, got)
		}
		body := rec.Body.String()
		if strings.Contains(body, "<script") {
			t.Errorf("%s: the page carries a script", refusal.code)
		}
		// No brand, no product name: the shopper is owed an answer, not an
		// introduction.
		for _, forbidden := range []string{"MemQL", "memql", "Shopify"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s: the page names %q on a merchant's own domain", refusal.code, forbidden)
			}
		}
		if !strings.Contains(body, refusal.remedy) {
			t.Errorf("%s: the page does not say what to do about it", refusal.code)
		}
	}
}
