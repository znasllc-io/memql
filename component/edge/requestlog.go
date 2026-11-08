// component/edge/requestlog.go -- the edge's per-request record (epic
// memql#4906, the Run epic of the Deployables program), and the visit fields a
// deployable's website statistics are counted from
// (docs/superpowers/specs/2026-11-08-site-and-email-statistics-design.md,
// section 4).
//
// # What a record carries
//
// Every request the edge answers for a deployable is recorded with its
// outcome: status, what the edge did (the path class), bytes and duration.
// That is the health half, and it is unchanged. The cluster's own sites and
// the synthetic probes are still not recorded at all (handler.go).
//
// Each record now also says whether a bot asked and whether it was a page
// view, and a page view alone carries the fields a statistic is cut by: the
// page's path, where the visitor arrived from, their country, device, browser
// and operating system, and a visitor id. They exist because a client is
// promised visitors, pages and sources, and a count of requests answers none
// of those. Neither a bot's request nor one served under a preview grant (an
// operator testing a candidate version) is ever a page view.
//
// # What it never carries
//
// The IP address and the user agent are read here, on the serving goroutine,
// only to derive those fields, and are never placed on the record. They are
// not fields of RequestRecord, so nothing downstream of the handler -- the
// recorder, the sink, the table, a log line -- can store them. The visitor id
// is a keyed hash under a secret that exists for one UTC day (spec D1): it
// counts a day's visitors without a cookie, and once the day's secret is
// deleted nothing stored can link it back to an address. Neither the query
// string, the fragment nor a referring URL is kept: a path is normalized, and
// a source is a utm_source or a referring host.
package edge

import (
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/edge/visit"
)

// RequestRecord is one served request, as the edge saw it. The edge declares
// its own shape rather than importing component/sitetraffic, so the dependency
// runs one way: the app bootstrap builds a recorder there and hands it here.
type RequestRecord struct {
	// SiteId is the resolved deployable's BARE id.
	SiteId string
	// ServedAt is when the response finished, UTC. The visitor id is
	// derived at this same instant, so a row filed under one UTC day is
	// never hashed with another day's secret.
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

	// Measured is true on every record this edge files. It exists for the
	// rows that predate it: they read false from the column default, which
	// is what lets a statistic say "not measured" for those days instead of
	// "nobody visited".
	Measured bool
	// PageView is a person opening a page: a browser navigation
	// (visit.IsPageView, spec D2) or an accepted page-change ping. Never true
	// for a bot, nor for a request served under a preview grant.
	PageView bool
	// Bot is true when the user agent is empty or names automation. Set on
	// every record, so bot requests count under health and stay out of
	// visitors.
	Bot bool

	// The dimensions, filled for a page view ONLY: a statistic about people
	// is cut by them, and filling them on an asset or a bot's request would
	// only give a later query something to filter back out.

	// Path is the page's path with no query or fragment
	// (visit.NormalizePath): a navigation's, or the one an accepted ping
	// named, normalized the same way.
	Path string
	// Source is where a navigation arrived from: a utm_source, a referring
	// host, or "direct". Empty for a navigation inside the site and for
	// every ping, so a source counts arrivals rather than clicks.
	Source string
	// Country is an ISO 3166-1 alpha-2 code, empty when no country database
	// is loaded or the address is not in it.
	Country string
	// Device, Browser and OS are members of visit.Classify's closed sets.
	Device  string
	Browser string
	OS      string
	// Visitor is the day's visitor id, empty whenever the edge holds no
	// secret for the request's UTC day: visitors then read low rather than
	// being linked across days.
	Visitor string
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
	// pathClassView is the page-change ping a single-page app's view script
	// sends, answered by the edge itself. A class of its own because the
	// ping is neither a document nor an asset, and "page views that arrived
	// as pings" is how anyone checks the script is working.
	pathClassView = "view"
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
	// viewPath is the path an ACCEPTED page-change ping named, decoded as a
	// request line's path is, set by the ping handler (view.go) and empty for
	// every other request. It rides on the writer because the writer is the
	// one thing both the ping handler and ServeHTTP hold: serve returns only a
	// path class, and the record is built after it returns. A value that
	// normalizes to a path makes the record a page view of that path
	// (addVisitFields).
	viewPath string
	// servedUnderPreview is true when serve answered the request under a
	// preview grant (markServedUnderPreview): an operator exercising a
	// candidate version, or a storefront's private testing origin. Carried
	// here for viewPath's reason, and it keeps such a request from ever being
	// a page view -- it is testing, not visiting.
	servedUnderPreview bool
}

// markServedUnderPreview notes on the writer that serve is answering this
// request under a preview grant. The writer is a recordingWriter only while
// the request is being recorded; otherwise there is nothing to mark.
func markServedUnderPreview(w http.ResponseWriter) {
	if rec, ok := w.(*recordingWriter); ok {
		rec.servedUnderPreview = true
	}
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
	name = strings.ToLower(name)
	return strings.HasSuffix(name, ".html") || strings.HasSuffix(name, ".htm")
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
		pathClassView,
	}
}

// addVisitFields fills the visit half of a record whose outcome (Status,
// PathClass, ServedAt) is already set. served is the request's recording
// writer, which carries what serve learned beyond the path class: the path an
// accepted page-change ping named, and whether a preview grant was in force.
//
// It runs on the serving goroutine after the response is written, so it is
// bounded work and nothing else: string matching, one HMAC under a secret held
// in memory, and one in-memory country lookup. Nothing here takes a lock or
// waits on the network or the database (VisitorHasher, CountryLookup).
//
// The user agent and the client address are locals of this function and
// leave it only as what is derived from them.
func (h *Handler) addVisitFields(record *RequestRecord, r *http.Request, site *Site, served *recordingWriter) {
	userAgent := r.UserAgent()
	agent := visit.Classify(userAgent)
	// NORMALIZED HERE, WHOEVER SET IT. The ping handler only accepts a body
	// that normalizes to a path, and this does not take its word for it: the
	// promise that the record carries clean text is kept where the record is
	// filled, so it holds however viewPath came to be set. Once, as a
	// navigation's path is normalized once, so one page is one row whichever
	// way it was reached; a value that normalizes to nothing is no page view.
	viewPath := visit.NormalizePath(served.viewPath)
	record.Measured = true
	record.Bot = agent.Bot
	// Never a page view: a bot, not even through a ping (a headless browser
	// runs the view script exactly as a person's browser does); nor a request
	// served under a preview grant, which is an operator testing a candidate
	// and would put their checks into a client's visitor figures. Both are
	// still recorded, so health counts every request the edge answered.
	record.PageView = !agent.Bot && !served.servedUnderPreview &&
		(viewPath != "" || visit.IsPageView(r, record.PathClass, record.Status))
	if !record.PageView {
		return
	}

	if viewPath != "" {
		// A ping names the page it moved to; its own URL is the ping route.
		// No source: the visitor arrived once, on the navigation that loaded
		// the app, and a ping's headers are its sender's to write.
		record.Path = viewPath
	} else {
		record.Path = visit.NormalizePath(r.URL.Path)
		record.Source = visit.Source(r, site.Hostname)
	}
	record.Device, record.Browser, record.OS = agent.Device, agent.Browser, agent.OS

	// THE FRONT DOOR'S ADDRESS, never the client's claim: visit.ClientAddress
	// takes the right-most forwarding entry, so a forged left-most entry can
	// mint neither a visitor nor a country.
	addr, ok := visit.ClientAddress(r)
	if !ok {
		// No address, no visitor and no country. Hashing an absent address
		// would fold every such request into one visitor.
		return
	}
	if h.countries != nil {
		// Unmapped, so an IPv4 visitor reaching a dual-stack listener
		// (::ffff:a.b.c.d) is looked up as the IPv4 address it is.
		record.Country = h.countries.Country(addr.Unmap())
	}
	if h.visitors != nil {
		// At the record's own instant, so the row and its visitor id belong
		// to the same UTC day.
		record.Visitor = h.visitors.Visitor(site.ID, visit.AddressKey(addr), userAgent, record.ServedAt)
	}
}
