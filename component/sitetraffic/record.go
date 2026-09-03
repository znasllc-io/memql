// Package sitetraffic is the edge's request log and the traffic figure folded
// from it (epic memql#4906, the Run epic of the Deployables program).
//
// The edge emitted no per-site metrics, so "is anybody using this deployable,
// and is it healthy" had no truthful answer. This package is both halves of
// the answer: a bounded writer that records one row per served request off the
// request path, and the read that answers a window from the continuous
// aggregate TimescaleDB folds those rows into.
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

// Record is one served request, as the edge saw it.
//
// Deliberately small and free of anything that identifies a VISITOR. There is
// no IP address, no user agent, no path and no referrer here, and that is a
// decision rather than an omission: the question this table exists to answer
// is "is anybody using this deployable, and is it healthy", which needs counts
// and outcomes and nothing about who. A table with per-visitor detail would be
// one an operator has to reason about under data-protection law to run a
// dashboard.
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
)

// PathClasses is the closed set, for a caller that wants to validate one.
var PathClasses = []string{
	PathClassAsset,
	PathClassDocument,
	PathClassFallback,
	PathClassProxy,
	PathClassConfig,
	PathClassUnserved,
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
