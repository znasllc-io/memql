package edge

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// requestlog_test.go -- the edge's per-request record (epic memql#4906).
//
// Three properties, and only the first is about counting:
//
//   - what the edge DID is what the record says (the path classes), including
//     the fallback-versus-document distinction that says whether a build
//     emitted its routes;
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
	}
}

// THE CLUSTER'S OWN SURFACES WRITE NOTHING, and the gate is the ROW FIELD.
// A third system-owned surface added tomorrow is excluded by the same line;
// a check against a hostname would have to learn its name.
func TestRequestLogSkipsSystemOwnedSites(t *testing.T) {
	portal := &Site{ID: "site-portal", Hostname: "shop.example.com", Status: "live", Kind: "spa", SystemOwned: true}
	rec, records := serveWithLog(t, portal, map[string]string{"index.html": "PORTAL"}, http.MethodGet, "/")
	if rec.Code != http.StatusOK || rec.Body.String() != "PORTAL" {
		t.Fatalf("the system-owned site must serve normally; got %d %q", rec.Code, rec.Body.String())
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
	if rec.Code != http.StatusOK || rec.Body.String() != "ROOT" {
		t.Errorf("got %d %q, want 200 ROOT", rec.Code, rec.Body.String())
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

	if rec.Body.String() != "ROOT" {
		t.Fatalf("body = %q, want ROOT", rec.Body.String())
	}
	select {
	case got := <-seen:
		if got.Bytes != int64(len("ROOT")) {
			t.Errorf("bytes = %d, want %d -- the record is taken once the body is written", got.Bytes, len("ROOT"))
		}
	case <-time.After(time.Second):
		t.Fatal("no record arrived")
	}
}

type recorderFunc func(RequestRecord)

func (f recorderFunc) Record(r RequestRecord) { f(r) }
