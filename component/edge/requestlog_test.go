package edge

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/memql"
)

// requestlog_test.go -- the edge's per-request record (epic memql#4906).
//
// Four properties, and only the first two are about counting:
//
//   - what the edge DID is what the record says (the path classes), including
//     the fallback-versus-document distinction that says whether a build
//     emitted its routes;
//   - a page view carries its visit fields, derived from the address the
//     front door wrote and never from one the client claimed, and nothing
//     else does; the address and the user agent never reach the record;
//   - the cluster's own surfaces write nothing, gated on the ROW FIELD;
//   - the wrapper does not break the proxy, which is the failure that would
//     compile, serve a plain GET correctly and silently kill every WebSocket.

// captureRecorder collects records instead of writing them.
type captureRecorder struct{ records []RequestRecord }

func (c *captureRecorder) Record(r RequestRecord) { c.records = append(c.records, r) }

var _ RequestRecorder = (*captureRecorder)(nil)

// serveWithLog serves one request through a handler that records, and returns
// the response and whatever was recorded.
func serveWithLog(t *testing.T, site *Site, files map[string]string, method, path string) (*httptest.ResponseRecorder, []RequestRecord) {
	t.Helper()
	log := &captureRecorder{}
	h := NewHandler(Options{
		Resolver:   staticResolver{site: site},
		Opener:     mapOpener(files),
		RequestLog: log,
	})
	req := httptest.NewRequest(method, path, nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, log.records
}

// WHAT THE EDGE DID, one case per branch of the resolution order. The
// fallback and the document are the pair worth reading: both serve
// index.html, and a record that called them the same thing could not tell
// anybody whether their prerendering was working.
func TestRequestLogClassifiesWhatWasServed(t *testing.T) {
	files := map[string]string{
		"index.html":         "ROOT",
		"about/index.html":   "ABOUT",
		"products/shoe.html": "SHOE",
		"assets/app.js":      "JS",
	}
	live := &Site{ID: "site-1", Hostname: "shop.example.com", Status: "live", Kind: "spa"}

	for _, tc := range []struct {
		path, want string
		status     int
	}{
		{"/", pathClassDocument, http.StatusOK},
		{"/about", pathClassDocument, http.StatusOK},
		{"/products/shoe", pathClassDocument, http.StatusOK},
		{"/assets/app.js", pathClassAsset, http.StatusOK},
		{"/cart/anything", pathClassFallback, http.StatusOK},
		{runtimeConfigPath, pathClassConfig, http.StatusOK},
	} {
		rec, records := serveWithLog(t, live, files, http.MethodGet, tc.path)
		if len(records) != 1 {
			t.Fatalf("GET %s recorded %d requests, want 1", tc.path, len(records))
		}
		got := records[0]
		if got.PathClass != tc.want {
			t.Errorf("GET %s recorded pathClass %q, want %q", tc.path, got.PathClass, tc.want)
		}
		if got.Status != tc.status || rec.Code != tc.status {
			t.Errorf("GET %s recorded status %d (answered %d), want %d", tc.path, got.Status, rec.Code, tc.status)
		}
		if got.SiteId != "site-1" {
			t.Errorf("GET %s recorded siteId %q, want the resolved deployable's", tc.path, got.SiteId)
		}
		if got.Bytes <= 0 {
			t.Errorf("GET %s recorded %d bytes, want the body it served", tc.path, got.Bytes)
		}
		if got.ServedAt.IsZero() || got.DurationNs <= 0 {
			t.Errorf("GET %s recorded servedAt=%v duration=%d, want both filled", tc.path, got.ServedAt, got.DurationNs)
		}
		// Every row this edge writes is MEASURED, whatever its class: the
		// flag says the visit fields were computed, which is what tells a
		// day of these rows from a day recorded before they existed.
		if !got.Measured {
			t.Errorf("GET %s recorded measured=false, want every recorded row measured", tc.path)
		}
	}
}

// A static site's 404 and a request nothing matched are `unserved`: "this
// deployable answered nothing" is the state a person reading the figure is
// trying to tell apart from "nobody asked".
func TestRequestLogClassifiesWhatWasNotServed(t *testing.T) {
	for name, tc := range map[string]struct {
		site   *Site
		path   string
		status int
	}{
		"a static site's 404": {&Site{ID: "s", Hostname: "shop.example.com", Status: "live", Kind: "static"}, "/nope", http.StatusNotFound},
		"a draft deployable":  {&Site{ID: "s", Hostname: "shop.example.com", Status: "draft", Kind: "spa"}, "/", http.StatusNotFound},
		"a paused deployable": {&Site{ID: "s", Hostname: "shop.example.com", Status: "disabled", Kind: "spa"}, "/", http.StatusServiceUnavailable},
		"an archived one":     {&Site{ID: "s", Hostname: "shop.example.com", Status: "archived", Kind: "spa"}, "/", http.StatusNotFound},
	} {
		_, records := serveWithLog(t, tc.site, map[string]string{"index.html": "ROOT"}, http.MethodGet, tc.path)
		if len(records) != 1 {
			t.Fatalf("%s: recorded %d requests, want 1 -- a request to a deployable that answered nothing is still a request to it", name, len(records))
		}
		if records[0].PathClass != pathClassUnserved {
			t.Errorf("%s: pathClass = %q, want %q", name, records[0].PathClass, pathClassUnserved)
		}
		if records[0].Status != tc.status {
			t.Errorf("%s: status = %d, want %d", name, records[0].Status, tc.status)
		}
		if !records[0].Measured || records[0].PageView {
			t.Errorf("%s: measured=%v pageView=%v, want a measured request that is no page view -- nothing was served", name, records[0].Measured, records[0].PageView)
		}
	}
}

// THE CLUSTER'S OWN SURFACES WRITE NOTHING, and the gate is the ROW FIELD.
// A third system-owned surface added tomorrow is excluded by the same line;
// a check against a hostname would have to learn its name.
func TestRequestLogSkipsSystemOwnedSites(t *testing.T) {
	portal := &Site{ID: "site-portal", Hostname: "shop.example.com", Status: "live", Kind: "spa", SystemOwned: true}
	rec, records := serveWithLog(t, portal, map[string]string{"index.html": "PORTAL"}, http.MethodGet, "/")
	if rec.Code != http.StatusOK || sourceHTML(rec.Body.String()) != "PORTAL" {
		t.Fatalf("the system-owned site must serve normally; got %d %q", rec.Code, sourceHTML(rec.Body.String()))
	}
	if len(records) != 0 {
		t.Errorf("recorded %d requests for a system-owned site, want none", len(records))
	}
}

// An unknown host has no deployable to attribute the request to.
func TestRequestLogSkipsAnUnknownHost(t *testing.T) {
	_, records := serveWithLog(t, nil, nil, http.MethodGet, "/")
	if len(records) != 0 {
		t.Errorf("recorded %d requests for an unresolved host, want none", len(records))
	}
}

// A handler built with no recorder serves exactly as it did before this
// existed -- the ordinary state for every test in this package and for a node
// with the request log switched off.
func TestNoRecorderChangesNothing(t *testing.T) {
	rec := serve(t,
		&Site{ID: "s", Hostname: "shop.example.com", Status: "live", Kind: "spa"},
		map[string]string{"index.html": "ROOT"}, "/")
	if rec.Code != http.StatusOK || sourceHTML(rec.Body.String()) != "ROOT" {
		t.Errorf("got %d %q, want 200 ROOT", rec.Code, sourceHTML(rec.Body.String()))
	}
}

// ---------------------------------------------------------------------------
// The wrapper
// ---------------------------------------------------------------------------

// THE FAILURE THIS EXISTS TO CATCH compiles and serves a plain GET correctly.
// httputil.ReverseProxy reaches Flush and Hijack through
// http.ResponseController, which walks Unwrap() -- so a wrapper without one
// would leave every streaming response unflushed and every WebSocket upgrade
// dead, with the ordinary tests green.
func TestRecordingWriterExposesFlushAndHijackThroughResponseController(t *testing.T) {
	inner := &hijackableWriter{ResponseRecorder: httptest.NewRecorder()}
	w := &recordingWriter{ResponseWriter: inner}
	rc := http.NewResponseController(w)

	if err := rc.Flush(); err != nil {
		t.Errorf("Flush through the wrapper: %v", err)
	}
	if !inner.flushed {
		t.Error("Flush did not reach the underlying writer")
	}
	if _, _, err := rc.Hijack(); err != nil {
		t.Errorf("Hijack through the wrapper: %v", err)
	}
	if !inner.hijacked {
		t.Error("Hijack did not reach the underlying writer")
	}
}

// The counted bytes are the bytes written, and the status is the one
// answered -- including the implicit 200 net/http writes on a first Write
// with no WriteHeader.
func TestRecordingWriterCountsBytesAndStatus(t *testing.T) {
	w := &recordingWriter{ResponseWriter: httptest.NewRecorder()}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(" world")); err != nil {
		t.Fatal(err)
	}
	if w.bytes != 11 {
		t.Errorf("bytes = %d, want 11", w.bytes)
	}
	if w.statusOr(0) != http.StatusOK {
		t.Errorf("status = %d, want the implicit 200 a first Write writes", w.statusOr(0))
	}

	explicit := &recordingWriter{ResponseWriter: httptest.NewRecorder()}
	explicit.WriteHeader(http.StatusNotFound)
	explicit.WriteHeader(http.StatusOK) // a second call must not overwrite
	if explicit.statusOr(0) != http.StatusNotFound {
		t.Errorf("status = %d, want the FIRST status written", explicit.statusOr(0))
	}

	// A handler that wrote nothing at all -- a 304, or a HEAD -- has no
	// status of its own, and the fallback is what the caller passes.
	empty := &recordingWriter{ResponseWriter: httptest.NewRecorder()}
	if empty.statusOr(http.StatusOK) != http.StatusOK {
		t.Errorf("statusOr = %d, want the fallback", empty.statusOr(http.StatusOK))
	}
}

// hijackableWriter is an httptest recorder that also answers Flush and
// Hijack, so a ResponseController has something real to find.
type hijackableWriter struct {
	*httptest.ResponseRecorder
	flushed  bool
	hijacked bool
}

func (h *hijackableWriter) Flush() { h.flushed = true }

func (h *hijackableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

// ---------------------------------------------------------------------------
// The cost
// ---------------------------------------------------------------------------

// BenchmarkServeWithAndWithoutTheRequestLog is the measurement the epic asks
// for: the write must be off the request path. Run both and compare:
//
//	go test ./component/edge -run xxx -bench 'ServeSite' -benchtime 200000x
//
// `discardRecorder` is what the real sink's Record costs at its cheapest --
// a channel send that never blocks -- so the delta is the recording path's
// own overhead rather than the database's.
func BenchmarkServeSiteWithoutTheRequestLog(b *testing.B) {
	benchmarkServe(b, nil)
}

func BenchmarkServeSiteWithTheRequestLog(b *testing.B) {
	benchmarkServe(b, newDiscardRecorder())
}

func benchmarkServe(b *testing.B, log RequestRecorder) {
	b.Helper()
	h := NewHandler(Options{
		Resolver:   staticResolver{site: &Site{ID: "site-1", Hostname: "shop.example.com", Status: "live", Kind: "spa"}},
		Opener:     mapOpener{"index.html": "ROOT", "assets/app.js": "JS"},
		RequestLog: log,
	})
	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	req.Host = "shop.example.com"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// discardRecorder is the real sink's cheapest shape: a buffered channel a
// drainer empties. It is not a no-op function, because a no-op would measure
// a cost the real path does not have.
type discardRecorder struct{ ch chan RequestRecord }

func newDiscardRecorder() *discardRecorder {
	d := &discardRecorder{ch: make(chan RequestRecord, 4096)}
	go func() {
		for range d.ch {
		}
	}()
	return d
}

func (d *discardRecorder) Record(r RequestRecord) {
	select {
	case d.ch <- r:
	default:
	}
}

// The recorder is called AFTER the response, so a recorder that took its time
// would not delay a byte reaching the visitor. Proven by pinning the response
// body before the record lands.
func TestTheRecordHappensAfterTheResponse(t *testing.T) {
	seen := make(chan RequestRecord, 1)
	h := NewHandler(Options{
		Resolver:   staticResolver{site: &Site{ID: "s", Hostname: "shop.example.com", Status: "live", Kind: "spa"}},
		Opener:     mapOpener{"index.html": "ROOT"},
		RequestLog: recorderFunc(func(r RequestRecord) { seen <- r }),
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if sourceHTML(rec.Body.String()) != "ROOT" {
		t.Fatalf("body = %q, want ROOT", sourceHTML(rec.Body.String()))
	}
	select {
	case got := <-seen:
		if got.Bytes != int64(rec.Body.Len()) {
			t.Errorf("bytes = %d, want %d -- the record is taken once the body is written", got.Bytes, rec.Body.Len())
		}
	case <-time.After(time.Second):
		t.Fatal("no record arrived")
	}
}

type recorderFunc func(RequestRecord)

func (f recorderFunc) Record(r RequestRecord) { f(r) }

func TestHealthProbesDoNotInflateVisitorTraffic(t *testing.T) {
	log := &captureRecorder{}
	h := NewHandler(Options{Resolver: staticResolver{site: &Site{ID: "shop", Status: "live", Kind: "spa", BundleRef: "file:///site"}}, Opener: mapOpener(map[string]string{"index.html": "hello"}), RequestLog: log})
	for _, ua := range []string{"MemQL-Site-Health/1.0", "ordinary visitor"} {
		req := httptest.NewRequest(http.MethodGet, "https://shop.example.com/", nil)
		req.Header.Set("User-Agent", ua)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		if response.Code != 200 {
			t.Fatalf("probe and visitor must see the same response, got %d", response.Code)
		}
	}
	if len(log.records) != 1 {
		t.Fatalf("want only the visitor, got %d traffic records", len(log.records))
	}
}

// ---------------------------------------------------------------------------
// The visit fields
// ---------------------------------------------------------------------------

// chromeOnWindows is a real desktop browser's user agent: not a bot, and
// every family visit.Classify reports is filled.
const chromeOnWindows = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// The two entries of a forwarded request. The client wrote the left-most one
// and the front door appended the right-most, so only the right-most may ever
// reach a visitor or a country.
const (
	forgedAddress    = "203.0.113.9"
	frontDoorAddress = "198.51.100.7"
)

// fakeVisitors stands in for the day's visitor secret. It answers a different
// id per address key, so the id on a record says which address it was derived
// from without the address itself appearing anywhere on the record.
type fakeVisitors struct {
	byAddressKey map[string]string
	calls        []visitorCall
}

type visitorCall struct {
	siteID, addressKey, userAgent string
	at                            time.Time
}

func (f *fakeVisitors) Visitor(siteID, addressKey, userAgent string, at time.Time) string {
	f.calls = append(f.calls, visitorCall{siteID: siteID, addressKey: addressKey, userAgent: userAgent, at: at})
	return f.byAddressKey[addressKey]
}

// fakeCountries stands in for the country database, for the same reason.
type fakeCountries struct {
	byAddress map[netip.Addr]string
	asked     []netip.Addr
}

func (f *fakeCountries) Country(addr netip.Addr) string {
	f.asked = append(f.asked, addr)
	return f.byAddress[addr]
}

var (
	_ VisitorHasher = (*fakeVisitors)(nil)
	_ CountryLookup = (*fakeCountries)(nil)
)

// visitFakes answers a distinct visitor id and country for each of the two
// addresses, so a field derived from the forged entry is told apart from one
// derived from the front door's.
func visitFakes() (*fakeVisitors, *fakeCountries) {
	visitors := &fakeVisitors{byAddressKey: map[string]string{
		frontDoorAddress: "visitor-from-the-front-door",
		forgedAddress:    "visitor-from-the-forged-entry",
	}}
	countries := &fakeCountries{byAddress: map[netip.Addr]string{
		netip.MustParseAddr(frontDoorAddress): "NL",
		netip.MustParseAddr(forgedAddress):    "US",
	}}
	return visitors, countries
}

// visitSite is the live deployable every visit-field test is served by.
func visitSite() *Site {
	return &Site{ID: "site-1", Hostname: "shop.example.com", Status: "live", Kind: "spa"}
}

// serveVisit serves one request through a handler that records visit fields.
func serveVisit(t *testing.T, req *http.Request, files map[string]string, visitors VisitorHasher, countries CountryLookup) (*httptest.ResponseRecorder, []RequestRecord) {
	t.Helper()
	log := &captureRecorder{}
	h := NewHandler(Options{
		Resolver:   staticResolver{site: visitSite()},
		Opener:     mapOpener(files),
		RequestLog: log,
		Visitors:   visitors,
		Countries:  countries,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, log.records
}

// visitRequest is a request to the test site as it arrives through the front
// door: the forwarding header carries an entry the client wrote, followed by
// the address the front door appended.
func visitRequest(method, target, userAgent string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Host = "shop.example.com"
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Forwarded-For", forgedAddress+", "+frontDoorAddress)
	return req
}

// navigate marks a request as a top-level navigation to a document, which is
// how a browser says a person is opening a page.
func navigate(req *http.Request) *http.Request {
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	return req
}

// assertNoVisitDimensions fails when any field a statistic groups on is set:
// they belong to page views alone.
func assertNoVisitDimensions(t *testing.T, got RequestRecord) {
	t.Helper()
	for field, value := range map[string]string{
		"path":    got.Path,
		"source":  got.Source,
		"country": got.Country,
		"device":  got.Device,
		"browser": got.Browser,
		"os":      got.OS,
		"visitor": got.Visitor,
	} {
		if value != "" {
			t.Errorf("%s = %q on a request that is not a page view, want empty", field, value)
		}
	}
}

// THE HEADLINE CASE, and the plan's first review focus: a person opening a
// page from a search result is a page view with every field filled, and the
// visitor and the country come from the address the FRONT DOOR wrote. The
// left-most forwarding entry is whatever the client chose to send; were it
// read, anybody could mint visitors or choose their country with a header.
func TestANavigationIsRecordedAsAPageViewWithItsFields(t *testing.T) {
	visitors, countries := visitFakes()
	// A trailing slash and a query string, so the path on the record is
	// visibly the normalized one rather than whatever the URL said.
	req := navigate(visitRequest(http.MethodGet, "/products/a/?ref=spring", chromeOnWindows))
	req.Header.Set("Referer", "https://www.google.com/search?q=shoes")

	rec, records := serveVisit(t, req, map[string]string{"index.html": "ROOT", "products/a.html": "A"}, visitors, countries)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /products/a/ answered %d, want 200", rec.Code)
	}
	if len(records) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(records))
	}
	got := records[0]

	if !got.Measured || !got.PageView || got.Bot {
		t.Errorf("measured=%v pageView=%v bot=%v, want a measured page view by a person", got.Measured, got.PageView, got.Bot)
	}
	for field, pair := range map[string][2]string{
		"path":    {got.Path, "/products/a"},
		"source":  {got.Source, "google"},
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

	// The visitor was derived from this site, the front door's address, the
	// browser's own user agent and the instant the record carries. The
	// instant matters: it decides which UTC day's secret hashes the visitor,
	// so it must be the same instant the row is filed under.
	if len(visitors.calls) != 1 {
		t.Fatalf("the visitor hasher was asked %d times, want once", len(visitors.calls))
	}
	call := visitors.calls[0]
	if call.siteID != "site-1" || call.addressKey != frontDoorAddress || call.userAgent != chromeOnWindows {
		t.Errorf("visitor derived from (%q, %q, %q), want (site-1, the front door's address, the user agent)", call.siteID, call.addressKey, call.userAgent)
	}
	if !call.at.Equal(got.ServedAt) {
		t.Errorf("visitor derived at %v, want the record's own servedAt %v", call.at, got.ServedAt)
	}
	if len(countries.asked) != 1 || countries.asked[0] != netip.MustParseAddr(frontDoorAddress) {
		t.Errorf("the country was looked up for %v, want only the front door's address", countries.asked)
	}

	// Derived, and nothing more: neither address nor the user agent is
	// anywhere on the record, so nothing downstream of it can store them.
	dump := fmt.Sprintf("%+v", got)
	for _, raw := range []string{frontDoorAddress, forgedAddress, chromeOnWindows, "Mozilla"} {
		if strings.Contains(dump, raw) {
			t.Errorf("the record carries %q: %s", raw, dump)
		}
	}
}

// AN ASSET IS NOT A PAGE. A browser fetching a page's script says so in its
// Fetch Metadata, and the request counts under health and nowhere else: no
// visit field is filled, and neither the visitor secret nor the country
// database is consulted -- which also keeps assets, most of what the edge
// serves, at the cost they had before.
func TestAnAssetIsMeasuredButNotAPageView(t *testing.T) {
	visitors, countries := visitFakes()
	req := visitRequest(http.MethodGet, "/assets/app.js", chromeOnWindows)
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	req.Header.Set("Sec-Fetch-Dest", "script")
	req.Header.Set("Referer", "https://shop.example.com/products/a")

	_, records := serveVisit(t, req, map[string]string{"index.html": "ROOT", "assets/app.js": "JS"}, visitors, countries)
	if len(records) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(records))
	}
	got := records[0]
	if got.PathClass != pathClassAsset || got.Status != http.StatusOK {
		t.Fatalf("recorded %s %d, want a served asset", got.PathClass, got.Status)
	}
	if !got.Measured || got.PageView || got.Bot {
		t.Errorf("measured=%v pageView=%v bot=%v, want a measured request by a person that is not a page view", got.Measured, got.PageView, got.Bot)
	}
	assertNoVisitDimensions(t, got)
	if len(visitors.calls) != 0 || len(countries.asked) != 0 {
		t.Errorf("an asset consulted the visitor secret %d times and the country database %d times, want neither", len(visitors.calls), len(countries.asked))
	}
}

// A BOT IS NEVER A PAGE VIEW (spec D9), even one that navigates exactly as a
// browser does. It is still recorded and flagged, because bot requests count
// under health, but it carries no visitor and none of the fields a visitor
// figure is cut by. An empty user agent is a bot too: no browser sends none.
func TestABotNavigationIsNotAPageView(t *testing.T) {
	for name, userAgent := range map[string]string{
		"a crawler":            "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"no user agent at all": "",
	} {
		t.Run(name, func(t *testing.T) {
			visitors, countries := visitFakes()
			req := navigate(visitRequest(http.MethodGet, "/", userAgent))

			_, records := serveVisit(t, req, map[string]string{"index.html": "ROOT"}, visitors, countries)
			if len(records) != 1 {
				t.Fatalf("recorded %d requests, want 1 -- a bot's request still counts under health", len(records))
			}
			got := records[0]
			if !got.Measured || !got.Bot || got.PageView {
				t.Errorf("measured=%v bot=%v pageView=%v, want a measured bot request that is not a page view", got.Measured, got.Bot, got.PageView)
			}
			if got.Visitor != "" {
				t.Errorf("visitor = %q, want none for a bot", got.Visitor)
			}
			assertNoVisitDimensions(t, got)
			if len(visitors.calls) != 0 || len(countries.asked) != 0 {
				t.Errorf("a bot consulted the visitor secret %d times and the country database %d times, want neither", len(visitors.calls), len(countries.asked))
			}
		})
	}
}

// AN ACCEPTED PING IS A PAGE VIEW (spec D2): a single-page app's in-app page
// change, which the page-change ping reports and, once it passes its checks,
// leaves on the writer as viewPath. It is a POST answered 204, so the
// navigation rule alone would refuse it.
//
// Its path is the one the ping named, and its source is EMPTY whatever its
// Referer says: a ping's headers are its sender's to choose, and a page change
// inside a site is not an arrival at it. A bot's ping is still not a page
// view.
func TestAnAcceptedPingIsAPageViewWithNoSource(t *testing.T) {
	visitors, countries := visitFakes()
	h := NewHandler(Options{Visitors: visitors, Countries: countries})
	ping := func(userAgent string) (*http.Request, RequestRecord) {
		req := visitRequest(http.MethodPost, "/_memql/view", userAgent)
		req.Header.Set("Origin", "https://shop.example.com")
		req.Header.Set("Referer", "https://www.google.com/")
		return req, RequestRecord{
			SiteId:    "site-1",
			ServedAt:  time.Date(2026, 11, 8, 14, 30, 0, 0, time.UTC),
			Status:    http.StatusNoContent,
			PathClass: pathClassView,
		}
	}

	acceptedPing := &recordingWriter{ResponseWriter: httptest.NewRecorder(), viewPath: "/products/b"}
	req, got := ping(chromeOnWindows)
	h.addVisitFields(&got, req, visitSite(), acceptedPing)
	if !got.Measured || !got.PageView || got.Bot {
		t.Errorf("measured=%v pageView=%v bot=%v, want an accepted ping to be a page view", got.Measured, got.PageView, got.Bot)
	}
	if got.Path != "/products/b" || got.Source != "" {
		t.Errorf("path=%q source=%q, want the ping's own path and no source", got.Path, got.Source)
	}
	if got.Visitor != "visitor-from-the-front-door" || got.Country != "NL" {
		t.Errorf("visitor=%q country=%q, want both from the front door's address", got.Visitor, got.Country)
	}
	if got.Device != "desktop" || got.Browser != "Chrome" || got.OS != "Windows" {
		t.Errorf("device=%q browser=%q os=%q, want the user agent's families", got.Device, got.Browser, got.OS)
	}

	req, bot := ping("")
	h.addVisitFields(&bot, req, visitSite(), acceptedPing)
	if !bot.Measured || !bot.Bot || bot.PageView {
		t.Errorf("measured=%v bot=%v pageView=%v, want a bot's ping to be no page view", bot.Measured, bot.Bot, bot.PageView)
	}
	assertNoVisitDimensions(t, bot)
}

// WITH NO SECRET AND NO COUNTRY DATABASE A PAGE VIEW STILL COUNTS. Both are
// optional to the handler (nil in a test; in production the secret is absent
// until a keeper has read the day's row, and the country database until one
// has been downloaded): the page, source and agent fields are filled, and the
// visitor and the country are left empty rather than guessed. A keeper's own
// "no secret for this day" answer reaches the record the same way.
func TestAPageViewWithNoSecretOrCountryDatabaseLeavesThoseEmpty(t *testing.T) {
	req := navigate(visitRequest(http.MethodGet, "/", chromeOnWindows))
	_, records := serveVisit(t, req, map[string]string{"index.html": "ROOT"}, nil, nil)
	if len(records) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(records))
	}
	got := records[0]
	if !got.PageView || got.Path != "/" || got.Source != "direct" || got.Browser != "Chrome" {
		t.Errorf("pageView=%v path=%q source=%q browser=%q, want a page view of / arriving direct in Chrome", got.PageView, got.Path, got.Source, got.Browser)
	}
	if got.Visitor != "" || got.Country != "" {
		t.Errorf("visitor=%q country=%q, want both empty with nothing to derive them from", got.Visitor, got.Country)
	}

	unheld := &fakeVisitors{byAddressKey: map[string]string{}}
	_, records = serveVisit(t, navigate(visitRequest(http.MethodGet, "/", chromeOnWindows)), map[string]string{"index.html": "ROOT"}, unheld, nil)
	if len(records) != 1 || !records[0].PageView || records[0].Visitor != "" {
		t.Errorf("a keeper holding no secret for the day gave %+v, want a page view with no visitor", records)
	}
}

// A PREVIEW IS TESTING, NOT VISITING. An operator exercising a candidate
// version through a preview grant is answered on the site's own origin, by the
// same serving path, with the same Fetch Metadata a visitor's browser sends,
// so nothing about the request itself says it is not a visit. Counting it
// would put an operator's checks into a client's visitor figures. It is still
// recorded, because health counts every request the edge answered, but like a
// bot's: no page view, no dimension and no visitor.
func TestAPreviewServedRequestIsNotAPageView(t *testing.T) {
	site := previewSiteRow(siteStatusLiveValue)
	exec := newStubPreviewExec()
	token := exec.issue(t, PreviewGrant{ID: "g-statistics", SiteID: site.ID, CandidateRef: candidateRef})
	visitors, countries := visitFakes()
	log := &captureRecorder{}
	h := NewHandler(Options{
		Resolver:    staticResolver{site: site},
		Opener:      previewBundles(),
		PreviewExec: exec,
		RequestLog:  log,
		Visitors:    visitors,
		Countries:   countries,
	})
	navigation := func(underGrant bool) *http.Request {
		req := navigate(visitRequest(http.MethodGet, "/", chromeOnWindows))
		req.Host = site.Hostname
		if underGrant {
			req.AddCookie(&http.Cookie{Name: memql.PreviewCookieName, Value: token})
		}
		return req
	}

	previewed := httptest.NewRecorder()
	h.ServeHTTP(previewed, navigation(true))
	if previewed.Code != http.StatusOK || !strings.Contains(previewed.Body.String(), "the candidate version") {
		t.Fatalf("the grant was not honoured (%d), so this test would prove nothing: %.120s", previewed.Code, previewed.Body.String())
	}
	// The same navigation without the grant IS a page view, so the difference
	// between the two records is the grant's and nothing else's.
	h.ServeHTTP(httptest.NewRecorder(), navigation(false))

	if len(log.records) != 2 {
		t.Fatalf("recorded %d requests, want 2 -- a previewed request still counts for health", len(log.records))
	}
	preview, public := log.records[0], log.records[1]
	if !preview.Measured || preview.PageView || preview.Bot {
		t.Errorf("previewed: measured=%v pageView=%v bot=%v, want a measured request by a person that is no page view", preview.Measured, preview.PageView, preview.Bot)
	}
	assertNoVisitDimensions(t, preview)
	if !public.PageView || public.Visitor == "" {
		t.Errorf("public: pageView=%v visitor=%q, want the same navigation without a grant to be a page view with a visitor", public.PageView, public.Visitor)
	}
	if len(visitors.calls) != 1 || len(countries.asked) != 1 {
		t.Errorf("the visitor secret was consulted %d times and the country database %d, want once each, for the public request only", len(visitors.calls), len(countries.asked))
	}
}

// WHAT REACHES THE RECORD IS CLEAN TEXT. The path and the utm_source arrive
// percent-decoded, so a navigation can carry control characters and bytes
// that are not UTF-8 in both; a NUL among them would make the database refuse
// the batch the row rides in, losing every other row in it. The edge answers
// "/%01..." (only a NUL or an invalid byte in the PATH is refused before
// serving), so this page view is real, and it is recorded with them removed.
//
// Every string on the record is checked, not only the two built from the
// request, so a field added later that copies client text unchanged fails
// here as well.
func TestARecordCarriesOnlyCleanText(t *testing.T) {
	visitors, countries := visitFakes()
	req := navigate(visitRequest(http.MethodGet, "/products/%01a/?utm_source=News%00letter%ff", chromeOnWindows))

	rec, records := serveVisit(t, req, map[string]string{"index.html": "ROOT"}, visitors, countries)
	if rec.Code != http.StatusOK || len(records) != 1 {
		t.Fatalf("answered %d and recorded %d requests, want a served page and one record", rec.Code, len(records))
	}
	got := records[0]
	if !got.PageView || got.Path != "/products/a" || got.Source != "newsletter" {
		t.Errorf("pageView=%v path=%q source=%q, want a page view of /products/a from newsletter", got.PageView, got.Path, got.Source)
	}
	assertRecordIsCleanText(t, got)
}

// assertRecordIsCleanText fails when any string on the record carries a control
// character or a byte that is not UTF-8. Every string field is checked, not
// only the ones a test expects to be dirty, so a field added later that copies
// client text unchanged fails here too. The page-change ping's coverage
// (view_test.go) shares it.
func assertRecordIsCleanText(t *testing.T, got RequestRecord) {
	t.Helper()
	fields := reflect.ValueOf(got)
	for i := 0; i < fields.NumField(); i++ {
		if fields.Field(i).Kind() != reflect.String {
			continue
		}
		value := fields.Field(i).String()
		if !utf8.ValidString(value) || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			t.Errorf("%s = %q carries a control character or a byte that is not UTF-8", fields.Type().Field(i).Name, value)
		}
	}
}

// THE CLEAN-TEXT GUARANTEE HOLDS HOWEVER viewPath WAS SET. The ping handler
// leaves only a path that normalizes, but addVisitFields does not take its
// word for it: it normalizes viewPath as it normalizes a navigation's path, so
// a value that is not a path is no page view and one carrying control
// characters, a query or a fragment is recorded as the page alone.
func TestAViewPathIsNormalizedHoweverItWasSet(t *testing.T) {
	h := NewHandler(Options{})
	for name, tc := range map[string]struct {
		viewPath, path string
	}{
		"no leading slash":          {"products", ""},
		"nothing but noise":         {"\x00\x01\x7f", ""},
		"a NUL, a query, a slash":   {"/products/\x00a/?x=1#top", "/products/a"},
		"invalid UTF-8 and a tab":   {"/products/\xffa\t/b", "/products/a/b"},
		"already a normalized path": {"/products/b", "/products/b"},
	} {
		t.Run(name, func(t *testing.T) {
			got := RequestRecord{
				SiteId:    "site-1",
				ServedAt:  time.Date(2026, 11, 8, 14, 30, 0, 0, time.UTC),
				Status:    http.StatusNoContent,
				PathClass: pathClassView,
			}
			served := &recordingWriter{ResponseWriter: httptest.NewRecorder(), viewPath: tc.viewPath}
			h.addVisitFields(&got, visitRequest(http.MethodPost, "/_memql/view", chromeOnWindows), visitSite(), served)
			assertRecordIsCleanText(t, got)
			if tc.path == "" {
				if got.PageView {
					t.Errorf("viewPath %q made a page view of %q, want none: it names no page", tc.viewPath, got.Path)
				}
				assertNoVisitDimensions(t, got)
				return
			}
			if !got.PageView || got.Path != tc.path {
				t.Errorf("viewPath %q: pageView=%v path=%q, want a page view of %q", tc.viewPath, got.PageView, got.Path, tc.path)
			}
		})
	}
}
