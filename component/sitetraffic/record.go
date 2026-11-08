// Package sitetraffic is the edge's request log and the traffic figure folded
// from it (epic memql#4906, the Run epic of the Deployables program), and the
// visit fields a deployable's website statistics are counted from
// (docs/superpowers/specs/2026-11-08-site-and-email-statistics-design.md).
//
// The edge emitted no per-site metrics, so "is anybody using this deployable,
// and is it healthy" had no truthful answer. This package is both halves of
// the answer: a bounded writer that records one row per served request off the
// request path, and the read that answers a window from the continuous
// aggregate TimescaleDB folds those rows into.
//
// # What a row records about a visitor, and what it never does
//
// A count of requests cannot answer what a client is promised about their
// website -- how many people, which pages, from where -- so every row also
// records whether it was a page view and whether a bot asked, and a page view
// alone records its path, its source, a country, a device, a browser, an
// operating system and a visitor id.
//
// The visitor id is HMAC-SHA256 over (site, client address, user agent) under
// a random secret that exists for one UTC day (VisitorSecrets) and is deleted
// when the day ends (spec D1). It counts a day's distinct visitors without a
// cookie, the alternative the design rejected because it would need a consent
// banner on every client's site; keyed by site, it cannot join one visitor
// across two sites; and once the day's secret is gone, nothing stored can link
// an id back to an address. The price: a person visiting on three days counts
// three times in a weekly total.
//
// What is still never stored: the IP address and the user agent. The edge
// derives the fields above from them and drops both before a Record exists, so
// they never reach this package, its table or its logs. Neither does a query
// string, a fragment or a referring URL: a path is normalized, and a source is
// a utm_source or a referring host. A stored row answers "how many people, on
// which pages, from where, on what", and never "who".
//
// # It lives in component/, not integrations/
//
// For component/packages' reason: an integration is an outbound call to
// somebody else's system, and this calls nobody. It writes to the cluster's
// own database and reads the cluster's own aggregate, which is the
// relationship `database` has to Postgres rather than the one `shopify` has to
// Shopify. module_taxonomy_test.go records the verdict.
//
// # One write, folded by the database
//
// The figure is a continuous aggregate over the rows, never a counter
// something increments, so the raw log and the figure cannot disagree. That is
// what makes a spike in errors followable: the number on the page and the
// lines behind it are the same evidence at two resolutions.
//
// # An absent figure and a zero are different answers
//
// A window nothing measured returns NO ROW, and a window with requests and no
// errors returns a row whose errorCount is 0. The reader never invents a zero
// (the campaignStats rule), because "nobody visited" and "we were not
// recording" send a person to two different places.
package sitetraffic

import "time"

// Record is one served request, as the edge saw it: its outcome, which every
// row carries, and the visit fields derived from it, which a page view alone
// fills (see the package comment for why they exist).
//
// It still carries no IP address and no user agent, and that remains a
// decision rather than an omission. The edge reads both on the serving path,
// derives every field below from them and drops them before building a
// Record, so nothing this package writes can hold them: a table with raw
// addresses in it would be one an operator has to reason about under
// data-protection law just to run a dashboard. What it carries instead is
// derived: members of closed sets, a host or utm_source, a normalized path and
// a day's keyed hash.
type Record struct {
	// SiteId is the BARE id of the v1:platform:site the request resolved to
	// (component/edge bare-ifies on projection, and the client contract is
	// bare ids at every wire seam). The reader compares the caller's bare ids
	// against this column directly, so the two spellings must not diverge.
	SiteId string
	// Node is MEMQL_NODE_ID -- which edge replica served it. Kept because a
	// figure that differs per replica is the first thing anyone asks about
	// when one replica is misconfigured; nothing reads it yet.
	Node string
	// ServedAt is when the response finished, in UTC.
	ServedAt time.Time
	// Status is the HTTP status the edge answered with.
	Status int
	// PathClass is what the edge DID with the request -- one of the constants
	// below.
	PathClass string
	// Bytes is the response body size in bytes, as counted through the
	// wrapped writer. Zero for a hijacked connection (a WebSocket upgrade
	// through the API proxy), where the bytes leave through the raw
	// connection and the wrapper never sees them; that undercount is
	// deliberate and named rather than guessed at.
	Bytes int64
	// DurationNs is how long the handler took, in nanoseconds.
	DurationNs int64

	// Measured is true on every row the edge writes since the visit fields
	// exist. Rows recorded before them read false from the column default,
	// and that difference is the whole point: it lets a statistic say "not
	// measured" for those days rather than "nobody visited", while the rows
	// keep counting for health.
	Measured bool
	// PageView is a person opening a page: a browser navigation (spec D2) or
	// an accepted page-change ping from a single-page app. Never true for a
	// bot, nor for a request the edge served under a preview grant (an
	// operator testing a candidate version, not a visitor).
	PageView bool
	// Bot is true when the user agent was empty or named automation. Set on
	// every row, page view or not, so bot requests count under health and
	// stay out of visitors and page views.
	Bot bool

	// The dimensions below are filled for a page view ONLY, and are empty on
	// every other row.

	// Path is the page's path with no query string or fragment, a trailing
	// slash removed except at the root, at most 256 bytes.
	Path string
	// Source is where a navigation arrived from: its utm_source, else the
	// referring host, else "direct". Empty for a navigation inside the site
	// and for every ping, so the Sources list counts arrivals.
	Source string
	// Country is an ISO 3166-1 alpha-2 code from the country database, empty
	// when none is loaded or the address is not in it.
	Country string
	// Device, Browser and OS are members of closed sets (desktop / mobile /
	// tablet; Chrome, Safari, ...; Windows, macOS, ...), so a statistic groups
	// on them without cleaning.
	Device  string
	Browser string
	OS      string
	// Visitor is the day's visitor id, 32 lowercase hex characters. Empty
	// whenever the edge held no secret for the request's UTC day: visitors
	// then read low rather than being linked across days.
	Visitor string
}

// The path classes: what the edge did, not what was asked for.
//
// "Did" rather than "asked" is the useful reading, and Fallback is why. The
// SPA fallback firing is the fact that tells somebody whether prerendering is
// working -- a site whose every page request lands on Fallback is one where
// the build emitted no routes, which looks identical from the request side.
const (
	// PathClassAsset is a file served from the bundle that is not an HTML
	// document -- the immutable, content-addressed majority.
	PathClassAsset = "asset"
	// PathClassDocument is an HTML document that existed in the bundle: the
	// root index.html, a directory index, or a prerendered <path>.html.
	PathClassDocument = "document"
	// PathClassFallback is index.html served because nothing matched -- the
	// spa tail of the resolution order.
	PathClassFallback = "fallback"
	// PathClassProxy is a request forwarded to the bff (/_memql/*) or to the
	// identity service (the four same-origin JSON paths).
	PathClassProxy = "proxy"
	// PathClassConfig is GET /runtime-config.json, the document every live
	// site answers with its own identity discovery and runtime settings.
	PathClassConfig = "config"
	// PathClassUnserved is a request the site did not serve: a draft,
	// disabled or archived deployable, or a path that matched nothing on a
	// static site. It is a class of its own rather than folded into
	// `document`, because "this deployable answered nothing" is the state
	// somebody reading the figure is trying to distinguish.
	PathClassUnserved = "unserved"
	// PathClassView is a page-change ping from a single-page app's view
	// script, which the edge answers itself. An accepted ping is a page
	// view; a refused one is still a request the edge answered, so it is
	// recorded either way and counts for health like every other class.
	PathClassView = "view"
)

// PathClasses is the closed set, for a caller that wants to validate one.
// Appended to, never reordered: the edge's own list is compared to this one
// index by index.
var PathClasses = []string{
	PathClassAsset,
	PathClassDocument,
	PathClassFallback,
	PathClassProxy,
	PathClassConfig,
	PathClassUnserved,
	PathClassView,
}

// Recorder is the one method the edge needs. The edge declares its own
// identical interface rather than importing this package, so the dependency
// runs one way: the app bootstrap builds a sink here and hands it over.
type Recorder interface {
	// Record files one served request. MUST NOT BLOCK: it is called from the
	// serving path, and an observability write that can wait on Postgres is a
	// head-of-line latency hazard for every visitor of every site.
	Record(r Record)
}
