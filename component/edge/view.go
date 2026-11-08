// component/edge/view.go -- the page-change ping: how a single-page app's
// in-app pages reach a deployable's website statistics (design record
// docs/superpowers/specs/2026-11-08-site-and-email-statistics-design.md,
// section 6).
//
// # Why there is a ping at all
//
// A page view is a browser navigation (visit.IsPageView, spec D2), and a
// single-page app navigates once: every page after the first is a
// history.pushState the edge never sees. view.js reports those and nothing
// else. It never reports the first load: that arrived as the navigation the
// edge already counted, so reporting it too would count every landing twice.
// The edge inserts the script into every HTML document of a live deployable
// (carriesViewScript), so a client adds nothing to their site.
//
// # Why it is an HTTP exception
//
// GET /_memql/view.js and POST /_memql/view were approved by the owner on
// 2026-11-08 as an exception to the gRPC-first policy (root CLAUDE.md, Allowed
// HTTP Exceptions). A beacon is a plain POST a page sends on its way to the
// next view; it cannot open a stream, and a visitor has no MemQL identity to
// open one with. Both routes are answered by the edge itself on a deployable's
// own origin. Neither is forwarded, and neither reads a cookie.
//
// # Why every ping is answered 204
//
// sendBeacon never shows the page its answer, so a status would inform nobody
// but a prober, who would learn from it which check a forged ping failed and
// tune the next one. A refusal is no error a visitor or the site's developer
// should find in a console either. So every outcome is the same empty 204 with
// the same headers, and only the request log says what happened: a refused
// ping is recorded for health and is no page view. Nothing is logged either:
// how many pings arrive is the sender's to choose, so a line per refusal would
// let any page write the edge's logs.
//
// # Why same-origin only
//
// Any page on the web can make its visitors' browsers POST here, and would
// otherwise put page views into a client's figures. The cluster's other
// deployables are the sharpest case: each is a label under the cluster's one
// domain, so they are same-SITE with this one, and only same-ORIGIN shuts them
// out (viewPingFromSite). A client that is not a browser can write any header
// it likes, but it can forge a navigation just as easily, so the ping gives a
// forger nothing it lacked; the budget per address bounds how much a ping can
// add.

package edge

import (
	"bytes"
	_ "embed"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/znasllc-io/memql/component/edge/visit"
)

const (
	// The two routes share the /_memql/ marker the API proxy and the access
	// surface use, so a deployable's origin keeps ONE reserved namespace, and
	// serveResolved dispatches them above the proxy branch. EXACT paths, not
	// a prefix: /_memql/views and /_memql/view/x stay the generic proxy's, as
	// they were before these existed.
	viewScriptPath = "/_memql/view.js"
	viewPingPath   = "/_memql/view"

	// viewPingMaxBytes bounds a ping's body, which is one path; the record
	// keeps at most 256 bytes of it (visit.NormalizePath).
	viewPingMaxBytes = 1024

	// viewPingPerMinute bounds pings per (site, address) on each replica: two
	// a second sustained, which a person clicking quickly through an app does
	// not reach. It bounds what one address can add to a figure and cost to
	// serve; it does not tell a forged ping from a real one, which nothing
	// here can (the file comment). Per replica for shopperBucket's reason, so
	// the cluster-wide ceiling is this times the replica count. THE COST:
	// everyone behind one address (an office, a carrier's NAT, an IPv6 /64)
	// shares one budget, so many people moving quickly through one app from
	// behind it read low.
	viewPingPerMinute = 120
)

// viewScriptTag is what serveFile inserts into a document beside the refresh
// tag. External and same-origin, so the served policy's script-src 'self'
// admits it with no hash and the document's inline scripts keep the exact
// bytes their hashes name. Deferred scripts run in document order once the
// document is parsed, and this one is inserted right after <head>, so it wraps
// history.pushState before the app's own deferred and module scripts run.
const viewScriptTag = `<script src="` + viewScriptPath + `" defer></script>`

// viewScript is the page-change script, the same bytes for every site.
//
//go:embed view.js
var viewScript []byte

// carriesViewScript says whether a site's HTML documents get viewScriptTag: a
// live deployable's, on its own origin.
//
// NOT A SYSTEM-OWNED SITE'S. The request log never records one (handler.go),
// so its pings would count nothing, and the console somebody reads a figure in
// must not be counted in it. NOT A RENDERER ORIGIN'S: that is an extension's
// isolated asset origin, no deployable's own, and it serves no ping. NOT A
// DRAFT'S: only a preview grant's holder is served a draft, and no ping sent
// under a grant is a page view. A LIVE site served under a grant does carry
// it, since a preview is served what the public is; its pings are told apart
// by the request log instead (addVisitFields).
//
// ITS OWN CONDITION, NOT THE REFRESH SCRIPT'S. A subtree that suppresses the
// refresh script (an extension runtime, whose unsaved work a reload would
// lose) still carries this one, which reloads nothing.
func carriesViewScript(site *Site) bool {
	return site != nil && site.Status == siteStatusLiveValue && !site.SystemOwned && site.ResourceParentHost == ""
}

// serveViewScript answers GET and HEAD for the script.
//
// CACHED FOR A DAY: the bytes are the same for every site and change only with
// an engine release. The price is that a browser may run the previous
// release's script for up to a day after an upgrade, so a change to what the
// script sends must stay one this route still accepts from the old script.
// Not cached when something upstream has already made the response private:
// under a preview grant it keeps the grant's no-store, as serveFile keeps it
// for a bundle file.
func serveViewScript(w http.ResponseWriter, r *http.Request) string {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return pathClassUnserved
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !alreadyPrivate(w) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	http.ServeContent(w, r, "view.js", time.Time{}, bytes.NewReader(viewScript))
	return pathClassConfig
}

// serveViewPing answers a page-change ping, 204 whatever became of it (see the
// file comment). An accepted ping leaves the path it named on the request's
// recording writer, which is how the record ServeHTTP files becomes a page view
// (addVisitFields). There is nothing else for it to do: it forwards nothing and
// writes nowhere.
func (h *Handler) serveViewPing(w http.ResponseWriter, r *http.Request, site *Site) string {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if path, ok := h.acceptViewPing(w, r, site); ok {
		// A recording writer exists only while the request is being recorded,
		// which is never for a system-owned site or on a replica with the
		// request log off. Without one an accepted ping counts nothing.
		if rec, recording := w.(*recordingWriter); recording {
			rec.viewPath = path
		}
	}
	w.WriteHeader(http.StatusNoContent)
	return pathClassView
}

// acceptViewPing runs the checks a ping must pass to be a page view, and
// returns the path it named, decoded as a request line's path is.
//
// THE ORDER IS COST. The method and the origin cost nothing and come first, so
// a page on another site, which can make browsers send pings that fail them,
// spends no visitor's budget. The budget comes before the body, so once an
// address is over it, its pings are refused without their bodies being read.
func (h *Handler) acceptViewPing(w http.ResponseWriter, r *http.Request, site *Site) (string, bool) {
	if r.Method != http.MethodPost || !viewPingFromSite(r) {
		return "", false
	}
	// THE FRONT DOOR'S ADDRESS: the right-most forwarding entry, which the
	// caller cannot choose. Keyed on an entry it can, a sender would rotate
	// the value and never meet the limit. It is the address the visitor is
	// derived from as well, so the budget and the statistic agree about who
	// is asking. With no address there is no budget to draw on, and no ping.
	addr, ok := visit.ClientAddress(r)
	if !ok || !h.viewRate.allow(site.ID+"|"+visit.AddressKey(addr), h.viewNow()) {
		return "", false
	}
	// THROUGH MaxBytesReader, so a body that declares no length is held to the
	// cap too: a check of Content-Length alone is one a sender opts out of by
	// not sending it.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, viewPingMaxBytes))
	if err != nil {
		return "", false
	}
	// DECODED, AS A REQUEST LINE IS. The script sends location.pathname, which
	// is percent-encoded, and a navigation's path reaches the record decoded
	// (r.URL.Path), so this must decode or one page would be two rows of every
	// statistic: /café from a link, /caf%C3%A9 from inside the app. A body that
	// does not decode names no page a navigation could reach, since the server
	// refuses such a request line outright. What decodes is the client's text
	// and is cleaned by visit.NormalizePath, here to decide and again where the
	// record is filled.
	path, err := url.PathUnescape(string(body))
	if err != nil || visit.NormalizePath(path) == "" {
		return "", false
	}
	return path, true
}

// viewPingFromSite reports whether a ping was sent by a page on the origin it
// was sent to: its Origin names the request's host, or its Fetch Metadata says
// same-origin and its Origin is absent or exactly "null".
//
// THE ORIGIN RULE IS THE ACCESS SURFACE'S (accessSameOrigin): the request's
// host rather than the site's, since a custom domain reaches the same site
// under its own name; host and port compared and never the scheme, since the
// edge sits behind a TLS terminator; and an Origin that is present but cannot
// be read (empty, or any other unparseable value) refused rather than taken
// for absent.
//
// ABSENT IS WHERE THE TWO DIFFER. The access surface lets a POST with no Origin
// through, because a client that is not a browser has no victim there. Here
// the figure is the victim, so a missing Origin proves nothing: the browser
// must still say same-origin, and a ping with neither header is refused. Not
// same-site, which every deployable on the cluster is (the file comment).
//
// "null" WITH same-origin IS A SAME-ORIGIN PAGE. A beacon is a no-cors POST,
// and for one of those the Fetch standard serializes Origin as "null" when the
// sending page's referrer policy is no-referrer. The edge itself sends strict-origin-when-cross-origin, which
// keeps the real origin, but a site may set no-referrer in its own HTML, and
// refusing it would leave that site's in-app pages uncounted with nothing to
// say why. Sec-Fetch-Site is set by the browser, never by a page, and a sender
// whose own origin is opaque (a sandboxed frame, say) is cross-site to
// everything, so "null" beside same-origin can only be a same-origin page.
// Only the exact serialization, and only as the one Origin: "null" with
// same-site, cross-site, none or no Fetch Metadata is refused, as is any
// other spelling.
func viewPingFromSite(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 || (len(origins) == 1 && origins[0] == "null") {
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	return accessSameOrigin(r)
}
