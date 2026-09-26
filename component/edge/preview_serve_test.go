// component/edge/preview_serve_test.go
package edge

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// Candidate-preview coverage for non-storefront deployables. Storefronts use
// independent stable destinations, covered in storefront_destinations_test.go.
// These tests preserve the original same-origin candidate behavior: public
// visitors get the serving bundle and valid grants get the named candidate.

// --- the harness ------------------------------------------------------------

// refOpener is a BundleOpener that serves a DIFFERENT tree per bundle ref.
//
// mapOpener beside it ignores the ref, which is right for every other test in
// this package and is exactly wrong here: the whole claim under test is that
// one request opens `bundleRef` and another opens `candidateRef`, and an opener
// that answered the same bytes to both would make every assertion below vacuous
// while reporting a pass.
type refOpener map[string]map[string]string

func (o refOpener) Open(ref string) (fs.FS, error) {
	files := fstest.MapFS{}
	for name, content := range o[ref] {
		files[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return files, nil
}

var _ BundleOpener = refOpener(nil)

const (
	servingRef   = "blob://sites/s-preview/v/serving/"
	candidateRef = "blob://sites/s-preview/v/candidate/"

	// The two documents differ in their BODY, not in a header, because the
	// question is which bundle was opened and a header could be written by
	// anything.
	servingBody   = "<!doctype html><title>Acme</title><p>the serving version</p>"
	candidateBody = "<!doctype html><title>Acme</title><p>the candidate version</p>"

	liveStoreDomain = "acme.myshopify.com"
	devStoreDomain  = "acme-dev.myshopify.com"
)

// previewBundles is the pair every test in this file serves from.
func previewBundles() refOpener {
	return refOpener{
		servingRef:   {"index.html": servingBody},
		candidateRef: {"index.html": candidateBody},
	}
}

// stubPreviewExec stands in for the engine's grant read. It is keyed by token
// DIGEST rather than by token, exactly as the real read is, so a test that
// presented the wrong cookie would miss here for the same reason a browser
// would.
type stubPreviewExec struct {
	mu       sync.Mutex
	byDigest map[string]*PreviewGrant
	touched  []string
}

func newStubPreviewExec() *stubPreviewExec {
	return &stubPreviewExec{byDigest: map[string]*PreviewGrant{}}
}

// issue records a grant for a freshly minted token and returns the plain token,
// which is the only place a test ever sees one.
func (s *stubPreviewExec) issue(t *testing.T, grant PreviewGrant) string {
	t.Helper()
	token, digest, err := memql.MintPreviewToken()
	if err != nil {
		t.Fatalf("mint preview token: %v", err)
	}
	if grant.ExpiresAt.IsZero() {
		grant.ExpiresAt = time.Now().UTC().Add(15 * time.Minute)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g := grant
	s.byDigest[digest] = &g
	return token
}

func (s *stubPreviewExec) PreviewGrantByToken(_ context.Context, digest string) (*PreviewGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byDigest[digest], nil
}

func (s *stubPreviewExec) TouchPreviewGrant(_ context.Context, grantId string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched = append(s.touched, grantId)
	return nil
}

var _ PreviewExecutor = (*stubPreviewExec)(nil)

// previewSiteRow is the deployable every test here serves: a storefront with a
// serving version, a candidate beside it, and BOTH stores resolved -- which is
// what the real resolver caches, so a preview request and a public request are
// answered from the same value.
func previewSiteRow(status string) *Site {
	return &Site{
		ID:           "s-preview",
		Hostname:     "acme.example.com",
		Status:       status,
		Kind:         "spa",
		BundleRef:    servingRef,
		CandidateRef: candidateRef,
		Binding:      map[string]any{"storeId": "acme"},
		Store: &BoundStore{
			ID:                 "acme",
			Domain:             liveStoreDomain,
			StorefrontTokenRef: "acme_storefront_token",
		},
		PreviewBinding: map[string]any{"storeId": "acme-dev"},
		PreviewStore: &BoundStore{
			ID:                 "acme-dev",
			Domain:             devStoreDomain,
			StorefrontTokenRef: "acme_dev_storefront_token",
		},
	}
}

// previewHandler builds the handler under test. The SecretResolver answers a
// per-ref token so a document's token can be traced back to the store the
// request was served under.
func previewHandler(site *Site, exec PreviewExecutor) *Handler {
	return NewHandler(Options{
		Resolver:    staticResolver{site: site},
		Opener:      previewBundles(),
		PreviewExec: exec,
		SecretResolver: func(_ context.Context, ref string) (string, error) {
			return "token-for-" + ref, nil
		},
	})
}

// getWithToken issues one GET at the site's own hostname, carrying the preview
// cookie when token is non-empty.
func getWithToken(h *Handler, site *Site, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = site.Hostname
	if token != "" {
		req.AddCookie(&http.Cookie{Name: memql.PreviewCookieName, Value: token})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- what the public gets, which must not have changed ----------------------

// A DRAFT DEPLOYABLE IS STILL 404 FOR EVERYBODY. The whole feature is a way to
// see a draft deployable's candidate, so the first thing to assert is that it
// did not become a way for anybody else to see anything.
func TestADraftDeployableWithACandidateIsStill404WithoutAGrant(t *testing.T) {
	site := previewSiteRow(siteStatusDraftValue)
	h := previewHandler(site, newStubPreviewExec())

	for _, path := range []string{"/", "/index.html", runtimeConfigPath} {
		rec := getWithToken(h, site, path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s on a draft deployable answered %d, want 404 -- a draft deployable "+
				"does not exist as far as the internet is concerned, candidate or no candidate:\n%.200s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// A LIVE DEPLOYABLE CARRYING A CANDIDATE STILL SERVES THE SERVING VERSION. The
// bug this rules out is the one that would be worst: the resolver's Site is a
// CACHED value shared by every request to this hostname, so a preview that
// mutated it rather than copying it would serve the candidate to the public
// until the cache expired.
func TestALiveDeployableWithACandidateStillServesTheServingVersion(t *testing.T) {
	site := previewSiteRow("live")
	exec := newStubPreviewExec()
	h := previewHandler(site, exec)

	rec := getWithToken(h, site, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / answered %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "the serving version") {
		t.Fatalf("an unauthenticated request was not served the serving version: %.200s", body)
	}

	// AND IT STAYS THAT WAY AFTER A PREVIEW HAS BEEN SERVED from the same
	// handler and the same cached Site -- which is the actual mutation bug,
	// and is invisible to a test that only ever issues one request.
	token := exec.issue(t, PreviewGrant{ID: "g1", SiteID: site.ID, CandidateRef: candidateRef})
	if rec := getWithToken(h, site, "/", token); !strings.Contains(rec.Body.String(), "the candidate version") {
		t.Fatalf("the grant was not served the candidate: %.200s", rec.Body.String())
	}
	after := getWithToken(h, site, "/", "")
	if body := after.Body.String(); !strings.Contains(body, "the serving version") {
		t.Fatalf("after one preview, an unauthenticated request was served %.200s -- the preview "+
			"wrote through the resolver's CACHED Site instead of copying it", body)
	}
}

// --- what a grant gets ------------------------------------------------------

// A VALID GRANT IS SERVED THE CANDIDATE, ON THE SITE'S OWN ORIGIN, FOR A DRAFT
// DEPLOYABLE AND FOR A LIVE ONE.
//
// The live row is the requirement that shapes the whole design (design record
// section 5, step 4): after publishing, the owner keeps working and exercises
// version N+1 while N goes on serving the public. A design that previewed only
// a draft deployable would deliver the sandbox once and never again.
func TestAGrantIsServedTheCandidateOnADraftAndOnALiveDeployable(t *testing.T) {
	for _, status := range []string{siteStatusDraftValue, siteStatusLiveValue} {
		t.Run(status, func(t *testing.T) {
			site := previewSiteRow(status)
			exec := newStubPreviewExec()
			h := previewHandler(site, exec)
			token := exec.issue(t, PreviewGrant{ID: "g-" + status, SiteID: site.ID, CandidateRef: candidateRef})

			rec := getWithToken(h, site, "/", token)
			if rec.Code != http.StatusOK {
				t.Fatalf("a valid grant against a %s deployable answered %d, want 200: %.200s",
					status, rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, "the candidate version") {
				t.Fatalf("a valid grant was not served the candidate: %.200s", body)
			}
			if strings.Contains(body, "the serving version") {
				t.Fatalf("a valid grant was served the serving version: %.200s", body)
			}
			// THE ORIGIN IS THE DEPLOYABLE'S OWN. Nothing here redirects to a
			// preview host, because there is no preview host: a storefront's
			// cookies, its cart and Shopify's checkout return all assume this
			// origin.
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("a preview redirected to %q -- it must be served on the site's own origin", loc)
			}
		})
	}
}

// A PREVIEWED RESPONSE IS PRIVATE AND UNINDEXABLE. Both halves are load-bearing
// and neither is obvious: without `no-store` a CDN or a corporate proxy could
// serve an unpublished storefront to the next person through, and without
// `noindex` a crawler that somehow reached a preview could put an unpublished
// version -- and its development-store prices -- into a search index.
//
// THE Cache-Control ARM IS THE ONE THAT ALMOST DID NOT HOLD, and it is worth
// knowing why it is asserted at the RESPONSE rather than at previewHeaders.
// handler.go writes the preview freshness policy and then serves the resolved
// response, whose own freshness policy `Set`s Cache-Control -- so a `no-store`
// written first and never defended is overwritten, to `public, no-cache,
// must-revalidate` for a document and to `public, max-age=31536000, immutable`
// for a content-addressed asset. Reading the header off the finished response
// is what makes that visible; reading it off the function that wrote it would
// have reported a pass on a candidate version any shared cache could store.
func TestAPreviewedResponseIsNoStoreAndNoindex(t *testing.T) {
	site := previewSiteRow(siteStatusLiveValue)
	exec := newStubPreviewExec()
	h := previewHandler(site, exec)
	token := exec.issue(t, PreviewGrant{ID: "g-headers", SiteID: site.ID, CandidateRef: candidateRef})

	rec := getWithToken(h, site, "/", token)
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("a previewed document carries Cache-Control %q, want no-store. Either the "+
			"preview headers were not written, or the resolved response's own freshness policy "+
			"was Set over them -- and either way an unpublished version of somebody's storefront "+
			"is now storable by every shared cache between this cluster and the operator's "+
			"browser.", cc)
	}
	if rt := rec.Header().Get("X-Robots-Tag"); !strings.Contains(rt, "noindex") {
		t.Errorf("a previewed response carries X-Robots-Tag %q, want noindex", rt)
	}
	if v := rec.Header().Values("Vary"); !containsFold(v, "Cookie") {
		t.Errorf("a previewed response does not Vary on Cookie: %v", v)
	}

	// The reachable positive: the PUBLIC answer for the same deployable is not
	// marked no-store by this feature, so the three assertions above are about
	// the preview rather than about every response this handler writes.
	pub := getWithToken(h, site, "/", "")
	if rt := pub.Header().Get("X-Robots-Tag"); strings.Contains(rt, "noindex") {
		t.Errorf("the PUBLIC answer carries X-Robots-Tag %q -- the preview headers leaked onto it", rt)
	}
}

func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.Contains(strings.ToLower(v), strings.ToLower(want)) {
			return true
		}
	}
	return false
}

func TestEachPreviewCheckFallsThroughToThePublicAnswer(t *testing.T) {
	site := previewSiteRow(siteStatusLiveValue)

	cases := map[string]PreviewGrant{
		// Door 3: a cookie carried to a sibling hostname.
		"a grant for another deployable": {ID: "g", SiteID: "s-somebody-else", CandidateRef: candidateRef},
		// Door 4: withdrawing or republishing a candidate ends every open
		// preview of it, which is the mechanism clearSiteCandidate relies on.
		"a grant whose candidate moved": {ID: "g", SiteID: site.ID, CandidateRef: "blob://sites/s-preview/v/older/"},
		"a grant naming no candidate":   {ID: "g", SiteID: site.ID, CandidateRef: ""},
		// Door 5.
		"an expired grant": {ID: "g", SiteID: site.ID, CandidateRef: candidateRef,
			ExpiresAt: time.Now().UTC().Add(-time.Minute)},
		"a revoked grant": {ID: "g", SiteID: site.ID, CandidateRef: candidateRef,
			RevokedAt: "2026-09-20T10:00:00Z"},
	}

	for name, grant := range cases {
		t.Run(name, func(t *testing.T) {
			exec := newStubPreviewExec()
			h := previewHandler(site, exec)
			token := exec.issue(t, grant)

			rec := getWithToken(h, site, "/", token)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s answered %d, want the ordinary public answer", name, rec.Code)
			}
			if body := rec.Body.String(); !strings.Contains(body, "the serving version") {
				t.Errorf("%s was served %.200s, want the serving version", name, body)
			}
		})
	}
}

// A ZERO EXPIRY IS EXPIRED, never eternal. The single field bounding a bearer
// credential must fail toward the refusal when it is absent or unreadable --
// and an absent stamp is exactly what a row written by an older build, or read
// back unparseable, produces.
func TestAGrantWithNoExpiryIsExpired(t *testing.T) {
	site := previewSiteRow(siteStatusLiveValue)
	exec := newStubPreviewExec()
	h := previewHandler(site, exec)

	// Bypassing issue()'s default, which fills an expiry in: the point here is
	// the zero value.
	token, digest, err := memql.MintPreviewToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	exec.byDigest[digest] = &PreviewGrant{ID: "g", SiteID: site.ID, CandidateRef: candidateRef}

	if body := getWithToken(h, site, "/", token).Body.String(); !strings.Contains(body, "the serving version") {
		t.Errorf("a grant with a zero expiry was honoured: %.200s", body)
	}
}

// A MALFORMED COOKIE COSTS A STRING COMPARISON, NOT A READ. The cookie arrives
// on every request to this origin, including every asset, so a cookie somebody
// pasted in from another site must not drive a query per request.
func TestAMalformedPreviewCookieNeverReachesTheEngine(t *testing.T) {
	site := previewSiteRow(siteStatusLiveValue)
	exec := &countingPreviewExec{stubPreviewExec: newStubPreviewExec()}
	h := previewHandler(site, exec)

	for _, junk := range []string{"hello", "mql_prv_", "mql_prv_not-base64-at-all!!", "mql_pat_abc"} {
		getWithToken(h, site, "/", junk)
	}
	if exec.reads != 0 {
		t.Errorf("a malformed cookie drove %d engine read(s); it must be rejected on shape alone", exec.reads)
	}

	// The reachable positive: a WELL-FORMED token that names no grant does
	// reach the read, so the counter above is measuring the shape check rather
	// than a handler that never calls the executor at all.
	token, _, err := memql.MintPreviewToken()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	getWithToken(h, site, "/", token)
	if exec.reads == 0 {
		t.Error("a well-formed token reached no engine read -- the counter above proves nothing")
	}
}

type countingPreviewExec struct {
	*stubPreviewExec
	reads int
}

func (c *countingPreviewExec) PreviewGrantByToken(ctx context.Context, digest string) (*PreviewGrant, error) {
	c.reads++
	return c.stubPreviewExec.PreviewGrantByToken(ctx, digest)
}

// A PREVIEW IS REFUSED FOR A DEPLOYABLE THAT IS PAUSED OR DECOMMISSIONED. A
// preview that served through either would be a way to bring a deployable back
// that does not touch its status, and an operator reading the row would have no
// idea anything was being served.
func TestAPreviewIsRefusedForADisabledOrArchivedDeployable(t *testing.T) {
	for _, status := range []string{"disabled", "archived"} {
		t.Run(status, func(t *testing.T) {
			site := previewSiteRow(status)
			exec := newStubPreviewExec()
			h := previewHandler(site, exec)
			token := exec.issue(t, PreviewGrant{ID: "g", SiteID: site.ID, CandidateRef: candidateRef})

			rec := getWithToken(h, site, "/", token)
			if strings.Contains(rec.Body.String(), "the candidate version") {
				t.Errorf("a %s deployable served its candidate under a grant", status)
			}
			if rec.Code == http.StatusOK {
				t.Errorf("a %s deployable answered 200 under a grant", status)
			}
		})
	}
}

// --- entering and leaving ---------------------------------------------------

// THE ENTRY POINT TURNS A LINK INTO A COOKIE AND ANSWERS IDENTICALLY EITHER
// WAY. The redirect is what keeps the token out of every Referer header the
// page's own assets send; the identical answer is what stops the path becoming
// an oracle for which tokens exist.
func TestThePreviewEntryPointSetsACookieAndRedirectsIdentically(t *testing.T) {
	site := previewSiteRow(siteStatusDraftValue)
	exec := newStubPreviewExec()
	h := previewHandler(site, exec)
	token := exec.issue(t, PreviewGrant{ID: "g", SiteID: site.ID, CandidateRef: candidateRef})

	good := getWithToken(h, site, memql.PreviewEnterPath+"?"+memql.PreviewGrantParam+"="+token, "")
	if good.Code != http.StatusSeeOther {
		t.Fatalf("the entry point answered %d, want 303", good.Code)
	}
	if loc := good.Header().Get("Location"); loc != "/" {
		t.Errorf("the entry point redirected to %q, want /", loc)
	}
	cookie := previewCookieFrom(t, good)
	if cookie == nil {
		t.Fatal("the entry point set no preview cookie for a well-formed token")
	}
	if cookie.Value != token {
		t.Errorf("the cookie carries %q, want the token", cookie.Value)
	}
	for label, got := range map[string]bool{"HttpOnly": cookie.HttpOnly, "Secure": cookie.Secure} {
		if !got {
			t.Errorf("the preview cookie is not %s -- it is a bearer credential for an "+
				"unpublished version", label)
		}
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("the preview cookie is SameSite=%v, want Lax -- Strict would drop it on "+
			"Shopify's top-level redirect back from the hosted checkout", cookie.SameSite)
	}
	if cookie.Domain != "" {
		t.Errorf("the preview cookie names Domain=%q; it must be host-only so it belongs to no "+
			"sibling under the cluster domain", cookie.Domain)
	}

	// Redeemed, it serves the candidate -- otherwise the assertions above are
	// about a cookie nothing honours.
	if body := getWithToken(h, site, "/", cookie.Value).Body.String(); !strings.Contains(body, "the candidate version") {
		t.Fatalf("the cookie the entry point set was not honoured: %.200s", body)
	}

	// A BAD TOKEN GETS THE SAME 303 and no cookie.
	bad := getWithToken(h, site, memql.PreviewEnterPath+"?"+memql.PreviewGrantParam+"=nonsense", "")
	if bad.Code != good.Code || bad.Header().Get("Location") != good.Header().Get("Location") {
		t.Errorf("a bad token answered %d/%q and a good one %d/%q -- the entry point is an oracle",
			bad.Code, bad.Header().Get("Location"), good.Code, good.Header().Get("Location"))
	}
	if previewCookieFrom(t, bad) != nil {
		t.Error("a malformed token set a preview cookie")
	}
}

// LEAVING CLEARS THE COOKIE IN THIS BROWSER AND IS NOT REVOCATION. The grant
// row is untouched, which is why a link somebody else holds goes on working --
// ending a preview everywhere is revokeSitePreviewGrant.
func TestLeavingAPreviewClearsTheCookieAndNotTheGrant(t *testing.T) {
	site := previewSiteRow(siteStatusLiveValue)
	exec := newStubPreviewExec()
	h := previewHandler(site, exec)
	token := exec.issue(t, PreviewGrant{ID: "g", SiteID: site.ID, CandidateRef: candidateRef})

	rec := getWithToken(h, site, memql.PreviewLeavePath, token)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the leave path answered %d, want 303", rec.Code)
	}
	cookie := previewCookieFrom(t, rec)
	if cookie == nil || cookie.MaxAge >= 0 {
		t.Fatalf("the leave path did not expire the preview cookie: %+v", cookie)
	}
	// The grant still resolves -- leaving is a browser act, not a revoke.
	if body := getWithToken(h, site, "/", token).Body.String(); !strings.Contains(body, "the candidate version") {
		t.Error("leaving a preview in one browser stopped the grant working, which makes it a " +
			"revoke -- revocation is revokeSitePreviewGrant and leaves a record")
	}
}

func previewCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == memql.PreviewCookieName {
			return c
		}
	}
	return nil
}

// --- the record that a preview was used -------------------------------------

// lastSeenAt IS EVIDENCE A URL WAS USED, NOT A REQUEST COUNTER. A preview loads
// a document and every asset under it, and a write per asset would put the
// graph in the serving path of a page.
func TestHonouringAGrantRecordsItAtMostOncePerThrottleWindow(t *testing.T) {
	site := previewSiteRow(siteStatusLiveValue)
	exec := newStubPreviewExec()
	h := previewHandler(site, exec)
	token := exec.issue(t, PreviewGrant{ID: "g-seen", SiteID: site.ID, CandidateRef: candidateRef})

	for i := 0; i < 5; i++ {
		getWithToken(h, site, "/", token)
	}
	// The write is detached from the request, so wait for it rather than
	// reading immediately: a flake here would be this test's, not the feature's.
	deadline := time.Now().Add(2 * time.Second)
	for {
		exec.mu.Lock()
		n := len(exec.touched)
		exec.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			if n != 1 {
				t.Fatalf("five previewed requests wrote lastSeenAt %d time(s), want exactly 1", n)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
