// component/edge/requestlog.go -- the edge's per-request record (epic
// memql#4906, the Run epic of the Deployables program).
package edge

import (
	"net/http"
	"time"
)

// RequestRecord is one served request, as the edge saw it. The edge declares
// its own shape rather than importing component/sitetraffic, so the dependency
// runs one way: the app bootstrap builds a recorder there and hands it here.
type RequestRecord struct {
	// SiteId is the resolved deployable's BARE id.
	SiteId string
	// ServedAt is when the response finished, UTC.
	ServedAt time.Time
	// Status is the HTTP status answered.
	Status int
	// PathClass is what the edge DID with the request -- one of the
	// pathClass* constants below.
	PathClass string
	// Bytes is the response body size counted through the wrapper.
	Bytes int64
	// DurationNs is how long the handler took.
	DurationNs int64
}

// RequestRecorder is the one method the handler needs.
//
// IT MUST NOT BLOCK. It is called once per served request, on the serving
// path, and an observability write that can wait on Postgres is a
// head-of-line latency hazard for every visitor of every site this replica
// serves. component/sitetraffic.Sink satisfies it with a non-blocking channel
// send that drops and counts when its buffer is full -- a low figure rather
// than a slow site.
type RequestRecorder interface {
	Record(RequestRecord)
}

// The path classes, mirroring component/sitetraffic's constants. Spelled here
// rather than imported for the reason RequestRecord is: this package does not
// depend on that one. sitetraffic's own test asserts the two lists agree.
const (
	pathClassAsset    = "asset"
	pathClassDocument = "document"
	pathClassFallback = "fallback"
	pathClassProxy    = "proxy"
	pathClassConfig   = "config"
	pathClassUnserved = "unserved"
)

// recordingWriter counts what was written and remembers the status.
//
// # Unwrap is what keeps the proxy working
//
// The API proxy hands this writer to httputil.ReverseProxy, which reaches
// Flush and Hijack through http.ResponseController -- and a ResponseController
// walks Unwrap() to find them. So a wrapper that implemented neither directly
// but exposes Unwrap keeps streaming responses streaming and WebSocket
// upgrades upgrading, with no method list to keep in step as net/http grows.
// A wrapper WITHOUT Unwrap would compile, serve a plain GET correctly, and
// silently break every WebSocket -- which is why TestRecordingWriter... proves
// it rather than trusting it.
//
// # Bytes after a hijack are not counted, deliberately
//
// Once a connection is hijacked the bytes leave through the raw connection
// and this writer never sees them. That undercount is named here and on
// sitetraffic.Record.Bytes rather than papered over: the alternative is
// wrapping the hijacked connection, which would put this package in the path
// of every WebSocket frame for a number nothing reads.
type recordingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *recordingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		// An implicit 200: net/http writes the header on the first Write.
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap exposes the underlying writer to http.ResponseController, which is
// how Flush, Hijack and SetWriteDeadline reach the real writer through this
// one. See the type's own note.
func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// statusOr returns the status answered, or fallback when the handler wrote a
// body through no explicit WriteHeader and no Write -- a 200 with an empty
// body, which is what a 304 or a HEAD can look like from here.
func (w *recordingWriter) statusOr(fallback int) int {
	if w.status == 0 {
		return fallback
	}
	return w.status
}

// classifyServed names what the edge did, from the name it resolved and the
// branch it took. "Did" rather than "was asked for" is the useful reading:
// PathClassFallback firing is the fact that says whether a build emitted its
// routes, and a request for /products/shoe looks identical from the ask side
// whether it was prerendered or fell back.
func classifyServed(name string, fellBack bool) string {
	if fellBack {
		return pathClassFallback
	}
	if isHTMLDocument(name) {
		return pathClassDocument
	}
	return pathClassAsset
}

// isHTMLDocument reports whether a resolved bundle name is an HTML document
// rather than an asset: index.html at the root, a directory index, or a
// prerendered <path>.html.
func isHTMLDocument(name string) bool {
	return len(name) >= 5 && name[len(name)-5:] == ".html"
}

// PathClassesForTest is the closed set this package writes, in order.
//
// Exported for ONE reason and used from exactly one place: component/sitetraffic
// declares the same set (its Record's PathClass is this one's), and two
// spellings of one closed list can disagree with nothing noticing -- rows
// carrying a class the reader never expects, and no test that would see it.
// The comparison lives in that package's test, which is why this is exported
// rather than the two being merged: merging them would make component/edge
// depend on the writer it is deliberately independent of.
func PathClassesForTest() []string {
	return []string{
		pathClassAsset,
		pathClassDocument,
		pathClassFallback,
		pathClassProxy,
		pathClassConfig,
		pathClassUnserved,
	}
}
