package edge

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/edge/visit"
	"github.com/znasllc-io/memql/component/memql"
)

// view_test.go -- the page-change ping and the script that sends it (the
// website statistics design, section 6; an owner-approved HTTP exception).
//
// In the order a request meets them:
//
//   - the script is cacheable JavaScript the edge serves itself, and the path
//     it pings is the route's;
//   - a ping is a page view only when it is a POST from the site's own origin,
//     within its address's budget, naming a path -- and it is answered 204,
//     with nothing forwarded, whichever of those it failed;
//   - a ping from a page served under a preview grant is never a page view;
//   - the tag is in every HTML document of a live, non-system deployable and
//     in no other, and the document's ETag says which.

// pingSite is the live deployable the ping tests are served by. Its API proxy
// is ON, so a ping that fell through to the generic /_memql/* proxy would
// reach the harness's bff and be counted there.
func pingSite() *Site {
	site := visitSite()
	site.APIProxy = true
	return site
}

// pingHarness is a recording handler with its ping budget's clock frozen. The
// budget refills twice a second, so a count against the wall clock would be
// asserting against a refill the test does not control (accessClock's reason).
type pingHarness struct {
	handler *Handler
	log     *captureRecorder
	clock   *accessClock
	// bff counts the requests that reached the API target. It must stay at
	// zero: both routes are answered by the edge itself.
	bff *atomic.Int64
}

func newPingHarness(t *testing.T, resolver Resolver) *pingHarness {
	t.Helper()
	bff := &atomic.Int64{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		bff.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(upstream.Close)
	visitors, countries := visitFakes()
	log := &captureRecorder{}
	h := NewHandler(Options{
		Resolver:   resolver,
		Opener:     mapOpener{"index.html": "ROOT"},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		APITarget:  upstream.URL,
		RequestLog: log,
		Visitors:   visitors,
		Countries:  countries,
	})
	clock := &accessClock{now: time.Unix(1_800_000_000, 0)}
	h.viewNow = clock.Now
	return &pingHarness{handler: h, log: log, clock: clock, bff: bff}
}

func (p *pingHarness) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	p.handler.ServeHTTP(rec, req)
	return rec
}

// last is the record the most recent request left.
func (p *pingHarness) last(t *testing.T) RequestRecord {
	t.Helper()
	if len(p.log.records) == 0 {
		t.Fatal("nothing was recorded")
	}
	return p.log.records[len(p.log.records)-1]
}

// pageViews counts the page views recorded so far.
func (p *pingHarness) pageViews() int {
	n := 0
	for _, r := range p.log.records {
		if r.PageView {
			n++
		}
	}
	return n
}

// pingRequest is a page-change ping as a browser's sendBeacon sends one from a
// page on the test site: a text/plain POST naming the path, with the Origin
// and Fetch Metadata of a same-origin beacon, arriving through the front door.
func pingRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, viewPingPath, strings.NewReader(body))
	req.Host = "shop.example.com"
	req.Header.Set("User-Agent", chromeOnWindows)
	req.Header.Set("X-Forwarded-For", forgedAddress+", "+frontDoorAddress)
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Origin", "https://shop.example.com")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	return req
}

// assertPingAnswer fails unless rec is the one answer every ping gets,
// whatever became of it: 204 with no body, never stored, never sniffed.
func assertPingAnswer(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("answered %d with %d body bytes, want 204 and no body", rec.Code, rec.Body.Len())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// assertNoPageView fails unless got is a recorded ping that is no page view:
// counted under health, and carrying no field a statistic about people is cut
// by.
func assertNoPageView(t *testing.T, got RequestRecord) {
	t.Helper()
	if got.PathClass != pathClassView || got.Status != http.StatusNoContent || !got.Measured || got.PageView {
		t.Errorf("recorded class=%q status=%d measured=%v pageView=%v, want a measured ping that is no page view",
			got.PathClass, got.Status, got.Measured, got.PageView)
	}
	assertNoVisitDimensions(t, got)
}

// THE SCRIPT IS CACHEABLE JAVASCRIPT the edge serves itself, on a site whose
// API proxy is on as on any other, and the path it pings is the route's: a
// rename on one side only would leave every single-page app pinging a path
// nobody answers.
func TestViewScriptIsServedAsCacheableJavaScript(t *testing.T) {
	p := newPingHarness(t, staticResolver{site: pingSite()})
	request := func(method string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, viewScriptPath, nil)
		req.Host = "shop.example.com"
		return p.serve(req)
	}

	got := request(http.MethodGet)
	if got.Code != http.StatusOK {
		t.Fatalf("GET %s answered %d, want 200", viewScriptPath, got.Code)
	}
	for header, want := range map[string]string{
		"Content-Type":           "text/javascript; charset=utf-8",
		"Cache-Control":          "public, max-age=86400",
		"X-Content-Type-Options": "nosniff",
	} {
		if value := got.Header().Get(header); value != want {
			t.Errorf("%s = %q, want %q", header, value, want)
		}
	}
	if len(viewScript) == 0 || got.Body.String() != string(viewScript) {
		t.Errorf("served %q, want the embedded script", got.Body.String())
	}
	if !strings.Contains(string(viewScript), `navigator.sendBeacon("`+viewPingPath+`", p)`) {
		t.Errorf("the script does not ping %s:\n%s", viewPingPath, viewScript)
	}

	head := request(http.MethodHead)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Errorf("HEAD answered %d with %d body bytes and %q, want the GET's headers and no body",
			head.Code, head.Body.Len(), head.Header().Get("Content-Type"))
	}
	refused := request(http.MethodPost)
	if refused.Code != http.StatusMethodNotAllowed || refused.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST answered %d with Allow %q, want 405 naming GET, HEAD", refused.Code, refused.Header().Get("Allow"))
	}
	if n := p.bff.Load(); n != 0 {
		t.Errorf("%d requests reached the bff, want none: the script is the edge's own", n)
	}
}

// THE HEADLINE CASE: a single-page app moves to /products/a and its view
// script says so. The edge answers 204 itself (the bff hears nothing, though
// this site's proxy is on) and records a page view of the path the ping named,
// normalized as a navigation's is. Its visit fields come from the request as a
// navigation's do: the front door's address and the browser's families. Its
// source is empty, since a page change inside a site is not an arrival at it.
func TestAPingFromTheSiteRecordsAPageView(t *testing.T) {
	p := newPingHarness(t, staticResolver{site: pingSite()})

	assertPingAnswer(t, p.serve(pingRequest("/products/a?x=1")))
	if len(p.log.records) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(p.log.records))
	}
	got := p.last(t)
	if !got.Measured || !got.PageView || got.Bot {
		t.Errorf("measured=%v pageView=%v bot=%v, want a measured page view by a person", got.Measured, got.PageView, got.Bot)
	}
	if got.PathClass != pathClassView || got.Status != http.StatusNoContent || got.SiteId != "site-1" {
		t.Errorf("recorded %s %d for %q, want the view class, 204 and the resolved deployable", got.PathClass, got.Status, got.SiteId)
	}
	for field, pair := range map[string][2]string{
		"path":    {got.Path, "/products/a"},
		"source":  {got.Source, ""},
		"country": {got.Country, "NL"},
		"device":  {got.Device, "desktop"},
		"browser": {got.Browser, "Chrome"},
		"os":      {got.OS, "Windows"},
		"visitor": {got.Visitor, "visitor-from-the-front-door"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", field, pair[0], pair[1])
		}
	}
	if n := p.bff.Load(); n != 0 {
		t.Errorf("%d requests reached the bff, want none: a ping is never forwarded", n)
	}
}

// WITHOUT AN ORIGIN, THE BROWSER'S OWN WORD IS NEEDED. Browsers attach Origin
// to the POSTs they send, so one without it is accepted only when Fetch
// Metadata says the sending page is this very origin. same-site is not enough:
// every deployable is a label under the cluster's one domain, so same-site is
// any other deployable on the cluster. And with neither header there is
// nothing to go on at all.
func TestAPingWithoutOriginNeedsSameOriginFetchSite(t *testing.T) {
	for name, tc := range map[string]struct {
		fetchSite string
		pageView  bool
	}{
		"same-origin":                       {"same-origin", true},
		"same-site, a sibling deployable":   {"same-site", false},
		"cross-site":                        {"cross-site", false},
		"none, a request the user typed in": {"none", false},
		"neither header":                    {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPingHarness(t, staticResolver{site: pingSite()})
			req := pingRequest("/products/a")
			req.Header.Del("Origin")
			req.Header.Del("Sec-Fetch-Site")
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			assertPingAnswer(t, p.serve(req))
			got := p.last(t)
			if tc.pageView {
				if !got.PageView || got.Path != "/products/a" {
					t.Errorf("pageView=%v path=%q, want a page view of /products/a", got.PageView, got.Path)
				}
				return
			}
			assertNoPageView(t, got)
		})
	}
}

// A PING FROM ANOTHER ORIGIN IS NOT A PAGE VIEW, or any page on the web could
// make its own visitors' browsers report pages here. The cluster's other
// deployables are another origin too, and so is the same host on another port.
// An empty Origin is refused rather than read as absent, and an Origin naming
// another host outranks a same-origin Fetch Metadata claim (every request here
// makes one). "null" is the one Origin a same-origin page can send, and its
// rule is TestAPingWithNullOriginIsAcceptedOnlyWhenSameOrigin's.
//
// Every refusal is answered exactly as an accepted ping is, header for header,
// so the route tells a prober nothing about which check it failed.
func TestAPingFromAnotherOriginRecordsNothing(t *testing.T) {
	accepted := newPingHarness(t, staticResolver{site: pingSite()}).serve(pingRequest("/products/a"))
	for name, origin := range map[string]string{
		"another site":                  "https://elsewhere.example",
		"a sibling deployable":          "https://other.example.com",
		"the same host on another port": "https://shop.example.com:8443",
		"an empty origin":               "",
	} {
		t.Run(name, func(t *testing.T) {
			p := newPingHarness(t, staticResolver{site: pingSite()})
			req := pingRequest("/products/a")
			req.Header.Set("Origin", origin)
			rec := p.serve(req)
			assertPingAnswer(t, rec)
			assertNoPageView(t, p.last(t))
			if !reflect.DeepEqual(rec.Header(), accepted.Header()) {
				t.Errorf("a refused ping answered with headers %v, an accepted one with %v: the difference discloses the check", rec.Header(), accepted.Header())
			}
			if n := p.bff.Load(); n != 0 {
				t.Errorf("%d requests reached the bff, want none", n)
			}
		})
	}
}

// "null" FROM A SAME-ORIGIN PAGE IS STILL THE SITE. A beacon is a no-cors POST,
// and the Fetch standard sends its Origin as "null" when the sending page's
// referrer policy is no-referrer, which a site may set in its own HTML. Fetch
// Metadata is the browser's own word, a header no page can write, and a sender
// whose origin is opaque is cross-site to everything, so "null" together with
// same-origin is a same-origin page and is accepted. "null" with anything else
// is refused, and so is an Origin spelled otherwise: only the exact
// serialization stands for an origin the browser chose not to send. An empty
// or another host's Origin stays refused beside a same-origin claim, and a
// ping with neither header is TestAPingWithoutOriginNeedsSameOriginFetchSite's.
func TestAPingWithNullOriginIsAcceptedOnlyWhenSameOrigin(t *testing.T) {
	for name, tc := range map[string]struct {
		origin, fetchSite string
		pageView          bool
	}{
		"null, same-origin":                 {"null", "same-origin", true},
		"null, no Fetch Metadata":           {"null", "", false},
		"null, same-site":                   {"null", "same-site", false},
		"null, cross-site":                  {"null", "cross-site", false},
		"null, none":                        {"null", "none", false},
		"Null in another case, same-origin": {"Null", "same-origin", false},
		"an empty Origin, same-origin":      {"", "same-origin", false},
		"another host's Origin, same-origin": {
			"https://elsewhere.example", "same-origin", false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPingHarness(t, staticResolver{site: pingSite()})
			req := pingRequest("/products/a")
			req.Header.Set("Origin", tc.origin)
			req.Header.Del("Sec-Fetch-Site")
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			assertPingAnswer(t, p.serve(req))
			got := p.last(t)
			if tc.pageView {
				if !got.PageView || got.Path != "/products/a" {
					t.Errorf("pageView=%v path=%q, want a page view of /products/a", got.PageView, got.Path)
				}
				return
			}
			assertNoPageView(t, got)
		})
	}

	// Only as the one Origin. A browser sends one; a request carrying "null"
	// beside a second value is not the serialization this rule trusts.
	t.Run("null beside a second Origin, same-origin", func(t *testing.T) {
		p := newPingHarness(t, staticResolver{site: pingSite()})
		req := pingRequest("/products/a")
		req.Header.Set("Origin", "null")
		req.Header.Add("Origin", "https://shop.example.com")
		assertPingAnswer(t, p.serve(req))
		assertNoPageView(t, p.last(t))
	})
}

// ONLY A POST IS A PING. Any other method is answered as a refused ping is,
// and a GET answered 204 is no navigation the page-view rule could count, even
// one a person typed into the address bar.
func TestOnlyAPostIsAPing(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			p := newPingHarness(t, staticResolver{site: pingSite()})
			req := pingRequest("/products/a")
			req.Method = method
			if method == http.MethodGet {
				navigate(req)
			}
			assertPingAnswer(t, p.serve(req))
			assertNoPageView(t, p.last(t))
			if n := p.bff.Load(); n != 0 {
				t.Errorf("%d requests reached the bff, want none", n)
			}
		})
	}
}

// A BODY THAT IS NOT A PATH IS NOT A PAGE VIEW. The cap is 1024 bytes, read
// through http.MaxBytesReader so a body that declares no length is held to it
// as well, and what is read must still name a path once normalized.
func TestAnOversizedOrNonPathBodyRecordsNothing(t *testing.T) {
	oversized := "/" + strings.Repeat("a", viewPingMaxBytes)
	for name, tc := range map[string]struct {
		body           string
		declaresLength bool
	}{
		"1025 bytes":                     {oversized, true},
		"1025 bytes declaring no length": {oversized, false},
		"no leading slash":               {"products", true},
		"empty":                          {"", true},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPingHarness(t, staticResolver{site: pingSite()})
			req := pingRequest(tc.body)
			if !tc.declaresLength {
				req.ContentLength = -1
			}
			assertPingAnswer(t, p.serve(req))
			assertNoPageView(t, p.last(t))
		})
	}

	// The cap is 1024 bytes and not fewer: a path of exactly that length is a
	// page view, recorded as the path a navigation to it would record.
	t.Run("exactly 1024 bytes", func(t *testing.T) {
		body := "/" + strings.Repeat("a", viewPingMaxBytes-1)
		p := newPingHarness(t, staticResolver{site: pingSite()})
		assertPingAnswer(t, p.serve(pingRequest(body)))
		if got := p.last(t); !got.PageView || got.Path != visit.NormalizePath(body) {
			t.Errorf("pageView=%v path of %d bytes, want a page view of the normalized path", got.PageView, len(got.Path))
		}
	})
}

// THE BUDGET IS PER ADDRESS, PER SITE: 120 pings a minute on each replica,
// keyed on the address the front door wrote. A ping past it is still answered
// 204 and is no page view. The caller's own left-most forwarding entry buys
// nothing (it changes on every ping here), while another address, the same
// address on another site, and the same address a minute later each have a
// budget of their own.
func TestPingsAreRateLimitedPerAddress(t *testing.T) {
	shop := pingSite()
	other := &Site{ID: "site-2", Hostname: "other.example.com", Status: "live", Kind: "spa", APIProxy: true}
	p := newPingHarness(t, accessHostResolver{shop.Hostname: shop, other.Hostname: other})
	from := func(host, frontDoor string, n int) *http.Request {
		req := pingRequest("/products/a")
		req.Host = host
		req.Header.Set("Origin", "https://"+host)
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d, %s", n%250, frontDoor))
		return req
	}

	for n := 0; n < viewPingPerMinute; n++ {
		assertPingAnswer(t, p.serve(from(shop.Hostname, frontDoorAddress, n)))
	}
	if got := p.pageViews(); got != viewPingPerMinute {
		t.Fatalf("%d of the %d pings within the budget were page views, want all of them", got, viewPingPerMinute)
	}

	assertPingAnswer(t, p.serve(from(shop.Hostname, frontDoorAddress, viewPingPerMinute)))
	assertNoPageView(t, p.last(t))

	for name, req := range map[string]*http.Request{
		"another address":                  from(shop.Hostname, "198.51.100.8", 0),
		"the same address on another site": from(other.Hostname, frontDoorAddress, 0),
	} {
		p.serve(req)
		if !p.last(t).PageView {
			t.Errorf("%s: not a page view, want a budget of its own", name)
		}
	}

	p.clock.advance(time.Minute)
	p.serve(from(shop.Hostname, frontDoorAddress, 0))
	if !p.last(t).PageView {
		t.Error("a minute later: not a page view, want the budget refilled")
	}
}

// A PING NAMES A PAGE AS A NAVIGATION TO IT DOES. location.pathname is
// percent-encoded and a request line's path reaches the record decoded, so the
// body is decoded the same way: the page a visitor landed on and the same page
// reached inside the app are one row of a statistic, not two. A body that does
// not decode names no page a navigation could reach.
func TestAPingNamesAPageAsANavigationToItDoes(t *testing.T) {
	p := newPingHarness(t, staticResolver{site: pingSite()})
	const encoded = "/caf%C3%A9/a%20b"

	p.serve(navigate(visitRequest(http.MethodGet, encoded, chromeOnWindows)))
	navigation := p.last(t)
	assertPingAnswer(t, p.serve(pingRequest(encoded)))
	ping := p.last(t)
	if !navigation.PageView || !ping.PageView {
		t.Fatalf("navigation pageView=%v, ping pageView=%v, want both page views", navigation.PageView, ping.PageView)
	}
	if navigation.Path != "/café/a b" || ping.Path != navigation.Path {
		t.Errorf("the navigation recorded %q and the ping %q, want both to record /café/a b", navigation.Path, ping.Path)
	}

	assertPingAnswer(t, p.serve(pingRequest("/caf%zz")))
	assertNoPageView(t, p.last(t))
}

// WHAT A PING PUTS ON THE RECORD IS CLEAN TEXT, OR NOTHING. Its body is raw
// bytes the sender chose, decoded as a request line is, so an escaped NUL or
// an escaped byte that is not UTF-8 arrives as one, beside whatever raw
// control characters and invalid bytes the body carried. A NUL among them
// would fail the database's whole batch insert (requestlog_test.go's
// TestARecordCarriesOnlyCleanText); a body that is nothing but noise names no
// page and is no page view.
func TestAPingRecordsOnlyCleanText(t *testing.T) {
	for name, tc := range map[string]struct{ body, path string }{
		"an escaped NUL and an escaped invalid byte":       {"/products/%00a%ff/b", "/products/a/b"},
		"raw control characters and invalid UTF-8":         {"/products/\x01a\x7f/\xffb\n", "/products/a/b"},
		"nothing but noise, which is no path once cleaned": {"%00\x01\xff", ""},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPingHarness(t, staticResolver{site: pingSite()})
			assertPingAnswer(t, p.serve(pingRequest(tc.body)))
			got := p.last(t)
			assertRecordIsCleanText(t, got)
			if tc.path == "" {
				assertNoPageView(t, got)
				return
			}
			if !got.PageView || got.Path != tc.path {
				t.Errorf("pageView=%v path=%q, want a page view of %q", got.PageView, got.Path, tc.path)
			}
		})
	}
}

// A PING FROM A PREVIEWED PAGE IS TESTING, NOT VISITING. A preview of a live
// deployable is served the document the public is, view script included, so
// its in-app page changes do ping. The route is dispatched below the preview
// substitution, so such a ping is marked as served under the grant and, like
// the navigation that loaded the page, is no page view. The same ping without
// the grant is one, so the grant is the whole difference.
func TestAPingFromAPreviewedPageIsNotAPageView(t *testing.T) {
	site := previewSiteRow(siteStatusLiveValue)
	exec := newStubPreviewExec()
	token := exec.issue(t, PreviewGrant{ID: "g-ping", SiteID: site.ID, CandidateRef: candidateRef})
	log := &captureRecorder{}
	h := NewHandler(Options{
		Resolver:    staticResolver{site: site},
		Opener:      previewBundles(),
		PreviewExec: exec,
		RequestLog:  log,
	})

	page := getWithToken(h, site, "/", token)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "the candidate version") {
		t.Fatalf("the grant was not honoured (%d), so this test would prove nothing: %.200s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Body.String(), viewScriptTag) {
		t.Fatalf("the previewed page carries no view script, so it would send no ping: %.300s", page.Body.String())
	}

	ping := func(underGrant bool) RequestRecord {
		t.Helper()
		req := pingRequest("/products/a")
		req.Host = site.Hostname
		req.Header.Set("Origin", "https://"+site.Hostname)
		if underGrant {
			req.AddCookie(&http.Cookie{Name: memql.PreviewCookieName, Value: token})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assertPingAnswer(t, rec)
		return log.records[len(log.records)-1]
	}
	previewed, public := ping(true), ping(false)
	assertNoPageView(t, previewed)
	if !public.PageView || public.Path != "/products/a" {
		t.Errorf("public: pageView=%v path=%q, want the same ping without the grant to be a page view of /products/a", public.PageView, public.Path)
	}
}

// A PREVIEW STAYS PRIVATE. Everything served under a grant is no-store
// (previewHeaders), and the script's day-long caching must not widen that, as
// serveFile refuses to for a bundle file; without the grant it is cacheable.
func TestTheViewScriptKeepsAPreviewPrivate(t *testing.T) {
	site := previewSiteRow(siteStatusLiveValue)
	exec := newStubPreviewExec()
	token := exec.issue(t, PreviewGrant{ID: "g-script", SiteID: site.ID, CandidateRef: candidateRef})
	h := previewHandler(site, exec)

	previewed := getWithToken(h, site, viewScriptPath, token)
	if previewed.Code != http.StatusOK || previewed.Body.String() != string(viewScript) {
		t.Fatalf("under the grant the script answered %d %.80q, want 200 and the script", previewed.Code, previewed.Body.String())
	}
	if cc := previewed.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") || strings.Contains(cc, "public") {
		t.Errorf("under the grant Cache-Control = %q, want the grant's no-store kept", cc)
	}
	if cc := getWithToken(h, site, viewScriptPath, "").Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("without the grant Cache-Control = %q, want public, max-age=86400", cc)
	}
}

// liveDocument is an HTML document whose head has a child of its own, so a
// test can say where the inserted tag landed relative to the document's own
// content.
const liveDocument = `<!doctype html><html><head><title>Shop</title></head><body>home</body></html>`

// assertViewTagFollowsHead fails unless body carries the view script tag
// exactly once, after <head> and ahead of everything the document itself put
// there: inserted early, so it wraps history.pushState before the app's own
// deferred scripts run.
func assertViewTagFollowsHead(t *testing.T, path, body string) {
	t.Helper()
	if n := strings.Count(body, viewScriptTag); n != 1 {
		t.Fatalf("GET %s carries the view script %d times, want once: %s", path, n, body)
	}
	head, tag, title := strings.Index(body, "<head>"), strings.Index(body, viewScriptTag), strings.Index(body, "<title>")
	if head < 0 || tag < head || title < tag {
		t.Errorf("GET %s: <head> at %d, the tag at %d, <title> at %d, want the tag after <head> and before the document's own head: %s",
			path, head, tag, title, body)
	}
}

// THE TAG IS IN EVERY HTML DOCUMENT OF A LIVE DEPLOYABLE, of every kind and by
// every rung of the resolution order, and in no asset. It sits beside the
// refresh tag, and its condition is its own: a subtree that suppresses the
// refresh script still carries it.
//
// THE ETAG SAYS WHICH. The same document served with no tag (here, the same
// site marked system-owned) has another ETag, so a copy a browser or a CDN
// cached before the tag existed is answered with a full body carrying it
// rather than revalidated as current.
func TestLiveDocumentsCarryTheViewScript(t *testing.T) {
	files := mapOpener{"index.html": liveDocument, "about.html": liveDocument, "assets/app.js": "console.log('asset')"}
	for _, kind := range []string{"static", "spa", storefrontKind} {
		t.Run(kind, func(t *testing.T) {
			live := &Site{ID: "site", Hostname: "shop.example.test", Kind: kind, Status: "live", BundleRef: "blob://one/"}
			tagless := *live
			tagless.SystemOwned = true
			h := NewHandler(Options{Resolver: staticResolver{site: live}, Opener: files})
			without := NewHandler(Options{Resolver: staticResolver{site: &tagless}, Opener: files})

			paths := []string{"/", "/about"}
			if kind != "static" {
				paths = append(paths, "/client-route")
			}
			for _, path := range paths {
				got := get(t, h, path, nil)
				if got.Code != http.StatusOK {
					t.Fatalf("GET %s answered %d, want 200", path, got.Code)
				}
				assertViewTagFollowsHead(t, path, got.Body.String())
				if strings.Count(got.Body.String(), `src="`+siteRefreshPath+`"`) != 1 {
					t.Errorf("GET %s lost the refresh tag beside it: %s", path, got.Body.String())
				}

				plain := get(t, without, path, nil)
				if plain.Code != http.StatusOK || strings.Contains(plain.Body.String(), viewScriptTag) {
					t.Fatalf("GET %s on the system-owned copy answered %d %s, want the document with no view script", path, plain.Code, plain.Body.String())
				}
				etag, plainETag := got.Header().Get("ETag"), plain.Header().Get("ETag")
				if etag == "" || etag == plainETag {
					t.Errorf("GET %s: ETag %q with the tag and %q without it, want two different validators", path, etag, plainETag)
				}
				stale := get(t, h, path, map[string]string{"If-None-Match": plainETag})
				if stale.Code != http.StatusOK || !strings.Contains(stale.Body.String(), viewScriptTag) {
					t.Errorf("GET %s with the tagless copy's validator answered %d, want 200 and the document with the tag", path, stale.Code)
				}
				if current := get(t, h, path, map[string]string{"If-None-Match": etag}); current.Code != http.StatusNotModified {
					t.Errorf("GET %s with its own validator answered %d, want 304", path, current.Code)
				}
			}
			if asset := get(t, h, "/assets/app.js", nil); asset.Body.String() != "console.log('asset')" {
				t.Errorf("an asset was rewritten: %q", asset.Body.String())
			}
		})
	}

	t.Run("a subtree that suppresses the refresh script", func(t *testing.T) {
		site := extensionSite()
		h := NewHandler(Options{Resolver: staticResolver{site: site}, Opener: mapOpener{"editor/index.html": liveDocument}})
		got := get(t, h, "/editor/", nil)
		if got.Code != http.StatusOK || strings.Contains(got.Body.String(), siteRefreshPath) {
			t.Fatalf("GET /editor/ answered %d %s, want the editor with no refresh script", got.Code, got.Body.String())
		}
		assertViewTagFollowsHead(t, "/editor/", got.Body.String())
	})
}

// NO TAG WHERE NO PING COULD COUNT. A system-owned site's requests are never
// recorded, and the console somebody reads a figure in must not be counted in
// it; its document is still rewritten for the refresh script, so the absence
// is the condition's and not a document left alone. A draft deployable is
// served to nobody but a preview grant's holder, whose pings are never page
// views.
func TestSystemOwnedAndDraftDocumentsCarryNoViewScript(t *testing.T) {
	system := &Site{ID: "site-os", Hostname: "os.example.test", Kind: "spa", Status: "live", SystemOwned: true}
	rec := serve(t, system, map[string]string{"index.html": liveDocument}, "/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `src="`+siteRefreshPath+`"`) {
		t.Fatalf("the system-owned site answered %d %s, want its document with the refresh script", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), viewScriptTag) {
		t.Errorf("a system-owned document carries the view script: %s", rec.Body.String())
	}

	draft := previewSiteRow(siteStatusDraftValue)
	exec := newStubPreviewExec()
	token := exec.issue(t, PreviewGrant{ID: "g-draft", SiteID: draft.ID, CandidateRef: candidateRef})
	page := getWithToken(previewHandler(draft, exec), draft, "/", token)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "the candidate version") {
		t.Fatalf("the grant was not honoured (%d), so this test would prove nothing: %.200s", page.Code, page.Body.String())
	}
	if strings.Contains(page.Body.String(), viewScriptTag) {
		t.Errorf("a draft deployable's previewed document carries the view script: %s", page.Body.String())
	}
}

// A RENDERER ORIGIN IS NO DEPLOYABLE'S OWN. Its documents are an extension's
// isolated frames, served with no cookies, runtime config or API, so they get
// no tag, and neither route exists there.
func TestRendererOriginDocumentsCarryNoViewScript(t *testing.T) {
	site := extensionSite()
	site.ResourceParentHost, site.Hostname = site.Hostname, resourceHost
	h := NewHandler(Options{Resolver: staticResolver{site: site}, Opener: mapOpener{"editor/assets/fake.html": liveDocument}})

	frame := get(t, h, "/editor/assets/fake.html", nil)
	if frame.Code != http.StatusOK {
		t.Fatalf("the renderer origin answered %d for its frame, want 200", frame.Code)
	}
	if strings.Contains(frame.Body.String(), viewScriptTag) {
		t.Errorf("a renderer origin's document carries the view script: %s", frame.Body.String())
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, path := range []string{viewScriptPath, viewPingPath} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s on a renderer origin answered %d, want 404", method, path, rec.Code)
			}
		}
	}
}
