package edge

import (
	"bytes"
	"errors"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// access.go -- THE EDGE HALF OF THE ACCESS SURFACE (deployable access codes,
// docs/superpowers/specs/2026-10-29-deployable-access-codes-design.md,
// section 6).
//
// # What arrives here
//
// A visitor's browser, on a deployable's OWN origin, holding an access code to
// trade or the session cookie one earned. It is nobody to MemQL: it has no
// identity to open a stream with, and only an HTTP response can set an HttpOnly
// cookie, so the other party dictates the wire (owner-approved HTTP exception,
// 2026-10-29). Four method and path pairs, and nothing else:
//
//	POST /_memql/access        ->  bff POST /access/unlock    {"code": "..."}
//	GET  /_memql/access        ->  bff GET  /access/session
//	POST /_memql/access/leave  ->  bff POST /access/leave
//	POST /_memql/access/call   ->  bff POST /access/call      {"call": "...", "args": {...}}
//
// # What this file owns, and what it deliberately does not
//
// It owns what only the EDGE can know or should do. Every refusal comes BEFORE
// the proxy hop, so a request that is wrong, too big or too frequent never
// reaches the bff:
//
//  1. THE ROUTE. Exactly the four pairs. Anything else under the same prefix
//     -- DELETE on /_memql/access, /_memql/access/x, /_memql/accessories -- is
//     not this surface's and falls through to the generic /_memql/* proxy, so a
//     site's apiProxy switch still means what it did before this surface
//     existed. The surface itself IGNORES that switch, as the shopper's forms do:
//     it is for a bundle holding a MemQL cookie, and a visitor with a code holds
//     none.
//  2. THE SHAPE OF A POST. It must declare JSON and must not name another
//     origin, and every body is capped by what is READ, not by what the request
//     claims.
//  3. THE BUDGETS. See accessLimits.
//  4. THE STAMP, and the two reply headers. The site and owner this edge
//     resolved are written after any copy the client sent is deleted; and every
//     reply carries no-store and nosniff whatever the bff said.
//
// It does NOT decide access, and it NEVER READS THE COOKIE'S VALUE. The edge is
// not an auth boundary (app/auth_mode_edge.go): it serves public bytes to
// everyone, and here it forwards a credential it has no way to judge. The bff
// reads the session from the database on every request, so a verdict formed
// here would be a second decision that could disagree with the first and could
// not be better informed -- and a cookie the edge had an opinion about is one
// it might cache. The cookie's value goes through as the browser sent it, and so
// does Set-Cookie coming back, which is the only thing that can open or end a
// session.
//
// IT DOES READ THE COOKIE'S NAME, on every way out of here, and that is the other
// half of what this file owns. The cookie is a __Host- cookie, so it has Path=/ and a
// browser sends it with every request to this host (memql.AccessCookieName says why it
// has to be). So on these four routes the bff gets that cookie and nothing else a
// browser holds, and on every other route it is taken out of what is forwarded
// (accessStripCookie). Nothing the edge serves itself ever reads it.
//
// The stamp is a POINTER, not an assertion. The bff reads the site it names and
// requires the row's own owner to equal the stamped one, so the most a
// forged value can do is name a site that really has codes. Stripping first is
// what lets it treat the header as the edge's alone.

// The three inbound paths, on a deployable's own origin, under the one the bff
// and this file share. Exact, and the routes below take no query string.
const (
	accessPath      = memql.AccessInboundPath
	accessLeavePath = memql.AccessInboundPath + "/leave"
	accessCallPath  = memql.AccessInboundPath + "/call"
)

// The four operations. A fixed set, because the budget a request draws on is
// chosen by it.
const (
	accessOpUnlock  = "unlock"
	accessOpSession = "session"
	accessOpLeave   = "leave"
	accessOpCall    = "call"
)

const (
	// The budgets, per replica (see accessLimits): the design record's numbers,
	// section 6. There is no environment override as the shopper's has, because
	// nothing asks for one yet and a knob is cheap to add the day something does.
	accessUnlockPerMinutePerAddress = 10
	accessUnlockPerMinutePerSite    = 60
	accessOtherPerMinutePerAddress  = 600

	// accessIPv6KeyBits is how much of an IPv6 address names one budget (see
	// accessClientAddress).
	accessIPv6KeyBits = 64
)

// The headers the edge stamps and the bff reads are declared ONCE, in
// component/memql, because that is the one package both modules can import --
// component/server cannot reach component/edge, and this package does not
// import component/server. Aliased here so this file reads as it does for the
// shopper's, and so a reader sees immediately that the names are not this
// package's to choose.
const (
	AccessStampSiteHeader  = memql.AccessStampSiteHeader
	AccessStampOwnerHeader = memql.AccessStampOwnerHeader
)

// accessStampHeaders is every header this surface controls. newBFFProxy deletes
// them from everything it forwards, not only from the four pairs, so a client
// cannot supply its own on any path.
var accessStampHeaders = []string{AccessStampSiteHeader, AccessStampOwnerHeader}

// accessRoute is one of the four method and path pairs and everything the edge
// needs to know about it. One table, so the routing decision and the forward
// cannot name different pairs.
type accessRoute struct {
	method   string
	inbound  string // on the deployable's origin
	upstream string // on the bff
	op       string
	maxBody  int64
}

var accessRoutes = [...]accessRoute{
	{http.MethodPost, accessPath, memql.AccessUnlockPath, accessOpUnlock, memql.AccessSmallBodyBytes},
	{http.MethodGet, accessPath, memql.AccessSessionPath, accessOpSession, memql.AccessSmallBodyBytes},
	{http.MethodPost, accessLeavePath, memql.AccessLeavePath, accessOpLeave, memql.AccessSmallBodyBytes},
	{http.MethodPost, accessCallPath, memql.AccessCallPath, accessOpCall, memql.AccessCallBodyBytes},
}

// accessRouteFor reports which of the four pairs a request is, if it is one.
//
// AN EXACT MATCH ON BOTH, never a prefix. Claiming /_memql/access/* would make
// /_memql/access/x and /_memql/accessories this file's business, and they are
// the generic proxy's: with apiProxy on a site has always been able to forward
// them, and with it off they are the 404 they have always been.
func accessRouteFor(method, p string) (accessRoute, bool) {
	for _, route := range accessRoutes {
		if route.method == method && route.inbound == p {
			return route, true
		}
	}
	return accessRoute{}, false
}

// accessLimits are the budgets in front of the surface: three token buckets and
// the clock they are read against.
//
// PER REPLICA, which is honest rather than ideal and for the shopper bucket's
// reason: a shared counter would put a database round trip on the path of every
// public request, and these exist to stop a flood, not to meter exactly. Two
// replicas each allow the configured rate, so the cluster-wide ceiling is the
// rate times the replica count.
//
// THEY BOUND LOAD, NOT GUESSING. The address is the right-most X-Forwarded-For
// entry (accessClientAddress): the hop the ingress itself recorded, which a
// caller cannot choose, so rotating a header buys no fresh per-address budget.
// Callers who really are at different addresses each still get one, and the
// per-site unlock bucket is the cap on what they spend together. What stops
// guessing is 125 bits of code, not a counter.
//
// UNLOCK DRAWS ON TWO, ADDRESS FIRST, and the order is the point. If the site's
// bucket were spent first, one machine sending a hundred requests would drain
// the whole site's sixty before its own ten ran out, and nobody else could get
// in. Address first, a flooding caller is stopped by its own bucket and never
// reaches the site's, so it can take ten of the sixty and no more.
type accessLimits struct {
	unlockAddress *shopperBucket // (site, address): 10 a minute
	unlockSite    *shopperBucket // site: 60 a minute
	other         *shopperBucket // (site, address): 600 a minute, for session, leave and call
	now           func() time.Time
}

func newAccessLimits() *accessLimits {
	return &accessLimits{
		unlockAddress: newShopperBucket(accessUnlockPerMinutePerAddress),
		unlockSite:    newShopperBucket(accessUnlockPerMinutePerSite),
		other:         newShopperBucket(accessOtherPerMinutePerAddress),
		now:           time.Now,
	}
}

// allow spends what an operation draws on. On a refusal it also says how many
// seconds to wait, which is how long the bucket that refused takes to give one
// token back: a hint a client can show, and an upper bound, since the bucket
// has been refilling since it last looked.
func (l *accessLimits) allow(op, siteID, address string) (bool, int) {
	now := l.now()
	key := siteID + "|" + address
	if op == accessOpUnlock {
		if !l.unlockAddress.allow(key, now) {
			return false, accessRetryAfter(accessUnlockPerMinutePerAddress)
		}
		if !l.unlockSite.allow(siteID, now) {
			return false, accessRetryAfter(accessUnlockPerMinutePerSite)
		}
		return true, 0
	}
	if !l.other.allow(key, now) {
		return false, accessRetryAfter(accessOtherPerMinutePerAddress)
	}
	return true, 0
}

// accessClientAddress is the address the access surface's budgets key on: the
// RIGHT-MOST non-empty X-Forwarded-For entry across every header line, when that
// entry is an IP address, in its canonical spelling; and the peer in every other
// case.
//
// IT IS NOT THE SHOPPER'S RULE, and the difference is the point.
// shopperClientAddress takes the leftmost entry and says plainly that a caller
// can write it. The edge sits behind an ingress that APPENDS the address it saw
// to whatever the caller already sent, so everything left of the last entry is
// the caller's to choose and the last entry is the ingress's own record of who
// connected. Keyed on the leftmost, a caller who rotates a made-up value gets a
// fresh per-address budget on every request, never meets the limit of ten, and
// can spend the site's sixty a minute until no real visitor can unlock. The
// right-most entry is the one a caller cannot choose.
//
// ONLY THE RIGHT-MOST ENTRY IS CONSIDERED, and only if it is an address. A
// caller who reaches the pod without the ingress writes that entry too, so it is
// parsed (netip.ParseAddr) rather than trusted as text: a string of the caller's
// choosing, as long as a header, would otherwise be a key held in the bucket map,
// and an ip:port entry would be a new key on every connection. Anything that is
// not an address -- a name, a port, brackets, an IPv6 zone, whose length is the
// caller's -- is the peer. It is NOT passed over for the entry before it, since
// walking left hands the choice back to the caller. The key is the canonical
// spelling (lower case, compressed, an IPv4-mapped address as plain IPv4), so one
// machine is one key and it is the same key the peer fallback gives it.
//
// AN IPv6 ADDRESS KEYS ON ITS /64, and an IPv4 address on every bit. One connection is
// routinely handed a whole /64 and may pick any address in it, so a key on the address
// would be a fresh budget for the asking, which is the rotation the right-most entry
// exists to stop. THE COST: a shared /64 shares a bucket, so everybody behind one
// office's or one carrier's prefix shares ten unlocks a minute.
//
// EVERY HEADER LINE, last first, because a header may arrive as several lines
// and an ingress may add its own rather than extend the caller's; reading only
// the first would key on what the caller wrote. EMPTY ENTRIES ARE SKIPPED, so a
// trailing comma or a blank line neither hides the entry before it nor becomes
// an address of its own.
//
// THE COST, stated rather than hidden: the right-most entry is the visitor only
// when the ingress is the first thing to see them. A proxy added in front of the
// ingress is the address the ingress records, and every visitor behind it then
// shares one bucket -- ten unlocks a minute between them, and a share of the
// site's sixty. An ingress that writes ip:port gets the peer, which is the same
// shared bucket. With no usable entry the peer is the address, as the shopper's
// is, which behind the ingress is the ingress itself.
func accessClientAddress(r *http.Request) string {
	if entry := accessRightMostForwarded(r); entry != "" {
		if addr, err := netip.ParseAddr(entry); err == nil && addr.Zone() == "" {
			return accessAddressKey(addr)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	// The peer's own zone is nobody's to choose, and it says nothing about who this is.
	if addr, err := netip.ParseAddr(host); err == nil {
		return accessAddressKey(addr.WithZone(""))
	}
	return host
}

// accessAddressKey is what one address's budget is kept under: an IPv4 address, an
// IPv4-mapped one as the address it maps, and an IPv6 address as its /64.
func accessAddressKey(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is6() {
		return netip.PrefixFrom(addr, accessIPv6KeyBits).Masked().String()
	}
	return addr.String()
}

// accessRightMostForwarded is the right-most non-empty entry of X-Forwarded-For
// across every header line, or "" when there is none.
//
// It scans from the right without splitting, since the header is the caller's and
// so is its length.
func accessRightMostForwarded(r *http.Request) string {
	lines := r.Header.Values("X-Forwarded-For")
	for i := len(lines) - 1; i >= 0; i-- {
		rest := lines[i]
		for {
			comma := strings.LastIndexByte(rest, ',')
			if entry := strings.TrimSpace(rest[comma+1:]); entry != "" {
				return entry
			}
			if comma < 0 {
				break
			}
			rest = rest[:comma]
		}
	}
	return ""
}

// accessRetryAfter is the whole seconds one token takes to come back to a bucket
// that refills perMinute of them a minute. Retry-After is whole seconds, so a
// bucket that refills faster than one a second still says 1.
func accessRetryAfter(perMinute int) int {
	return int(math.Ceil(60 / float64(perMinute)))
}

// accessRefusal is one answer this file gives without asking the bff. Every one
// in this table is JSON, because the caller is a script that branches on the
// body, and every body is a constant, so there is nothing from the request to
// escape.
//
// NOT EVERY REPLY THE EDGE CAN SEND IS IN THIS TABLE. When the bff cannot be
// reached the shared proxy builder answers its own 502, as text/plain, and this
// file does not change that handler. A client must not assume a JSON body on a
// 5xx.
type accessRefusal struct {
	status int
	body   string
}

var (
	// ONE 404 FOR SEVERAL CAUSES, and the same bytes the bff gives an unknown
	// route or an unstamped request. A site with no owner, a deployment with no
	// API target and a path nobody serves are three different facts, and telling
	// them apart would tell a prober which to work on.
	accessRefusalNotFound = accessRefusal{http.StatusNotFound, `{"error":"not_found"}`}

	accessRefusalBadRequest    = accessRefusal{http.StatusBadRequest, `{"error":"bad_request"}`}
	accessRefusalForeignOrigin = accessRefusal{http.StatusForbidden, `{"error":"forbidden_origin"}`}
	accessRefusalNotJSON       = accessRefusal{http.StatusUnsupportedMediaType, `{"error":"json_required"}`}
	accessRefusalTooLarge      = accessRefusal{http.StatusRequestEntityTooLarge, `{"error":"too_large"}`}
	accessRefusalRateLimited   = accessRefusal{http.StatusTooManyRequests, `{"error":"rate_limited"}`}
)

// setAccessReplyHeaders puts the two headers every reply carries on the writer.
//
// ON THE WRITER, AHEAD OF EVERYTHING, so that a refusal, the proxy's own 502 and
// a bff reply all have them without a branch that could forget. A proxied reply
// would otherwise get a SECOND copy from the bff, since the proxy adds upstream
// headers to the ones already set; accessOwnReplyHeaders is what removes that.
func setAccessReplyHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
}

// accessOwnReplyHeaders makes those two headers the edge's alone, by removing the
// bff's copies from the reply before the proxy merges its headers into ours.
//
// The bff sets the same two, and the edge does not rely on that: a reply the bff
// forgot to mark, or marked cacheable, must not be one a shared cache may keep,
// because no cache between here and a browser may hold an answer to "are you
// still in". Overriding rather than merging is also what keeps the header to one
// value, which is what a reader and a cache expect of it.
func accessOwnReplyHeaders(res *http.Response) error {
	res.Header.Del("Cache-Control")
	res.Header.Del("X-Content-Type-Options")
	return nil
}

func writeAccessRefusal(w http.ResponseWriter, refusal accessRefusal) {
	setAccessReplyHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(refusal.status)
	_, _ = io.WriteString(w, refusal.body)
}

// accessJSONContentType reports whether a request declares JSON.
//
// This is the CSRF rule that needs no state: a plain HTML form can send only
// urlencoded, multipart or text/plain, and a script on another origin that sends
// application/json must first pass a CORS preflight, which this origin does not
// grant it. The media type is parsed rather than prefix-matched, so
// "application/jsonx" is not JSON and a charset parameter does not make it
// something else.
func accessJSONContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

// accessSameOrigin reports whether a request's Origin, if it has one, names the
// host the request was made to.
//
// THE REQUEST'S HOST, not the site's, because a custom domain reaches the same
// site under its own name and a page there is same-origin with itself. Only the
// host (and port) is compared, never the scheme: the edge sits behind a TLS
// terminator, so the scheme it sees is not the one the browser used.
//
// ABSENT IS ALLOWED. A browser sends Origin on every POST it makes, so a POST
// without one was not made cross-site by a browser, and a non-browser client has
// no victim. An origin that is PRESENT and cannot be read -- "null", which a
// sandboxed frame or a cross-origin redirect produces, or an empty value -- is
// refused: failing open on an opaque origin would hand the one request the
// check is for a way around it.
func accessSameOrigin(r *http.Request) bool {
	for _, origin := range r.Header.Values("Origin") {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Host, r.Host) {
			return false
		}
	}
	return true
}

// boundAccessBody reads a request's body under its cap and puts the bytes back
// as the body the bff will receive.
//
// WHY READ IT HERE rather than trust the declared length, which is all
// shopper.go can see: a body sent chunked declares none, so a check on
// Content-Length alone is one the caller opts out of by not sending it. Reading
// at most the cap plus a byte makes the 413 certain and makes it come BEFORE
// the forward, and the bff gets a body of known length it can trust. The cost
// is at most 64 KiB held for the length of one request, and a slow sender now
// ties up this connection and not the bff's.
func boundAccessBody(w http.ResponseWriter, r *http.Request, limit int64) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	// A chunked request that is now fully read is no longer chunked; leaving the
	// encoding on would send the bff a body it was told to expect in pieces.
	r.TransferEncoding = nil
	return nil
}

// serveAccessSurface gates one of the four access requests and forwards it.
//
// EVERY REFUSAL THIS FUNCTION MAKES IS A SMALL JSON BODY, because the caller is a
// script and the protocol is JSON. The one reply that is not is not made here:
// when the bff cannot be reached the shared proxy builder answers 502 as
// text/plain. The order of the refusals is deliberate: the ones that cost nothing
// to make come before the one that spends a token.
func (h *Handler) serveAccessSurface(w http.ResponseWriter, r *http.Request, site *Site) {
	setAccessReplyHeaders(w)

	// The caller has already matched the pair; this is the backstop that keeps
	// the function from forwarding a path it was never meant to stamp.
	route, ok := accessRouteFor(r.Method, r.URL.Path)
	if !ok {
		writeAccessRefusal(w, accessRefusalNotFound)
		return
	}

	// A CLUSTER-OWNED SITE HAS NO AUTHORITY TO BORROW. A call runs under the
	// site owner's authority, and an empty owner -- the state the platform's own
	// sites carry -- has none to lend, so the surface is refused rather than run
	// under some synthetic actor. It is the same 404 as anywhere else, and it is
	// not logged: unlike the shopper's switch there is nothing to turn on here,
	// so every owner-less site on the cluster gets these from any passer-by.
	owner := strings.TrimSpace(site.OwnerUserID)
	if owner == "" {
		writeAccessRefusal(w, accessRefusalNotFound)
		return
	}

	// NO API TARGET IS THE SAME 404, AND IT IS NOT LOGGED. The edge warned once at
	// boot (edgeAPITargetFromEnv), and serveAPI is silent in the same condition. A
	// line here would be one per request, and how many requests there are is the
	// visitor's to choose, so a visitor could drive error-level logs at will.
	if strings.TrimSpace(h.apiTarget) == "" {
		writeAccessRefusal(w, accessRefusalNotFound)
		return
	}

	// THE REFUSALS THAT SPEND NOTHING come first. A page on another site can
	// make a browser send a request that fails every one of these, and if they
	// cost a token that page could lock a real visitor out of unlocking.
	if r.Method == http.MethodPost {
		if !accessJSONContentType(r) {
			writeAccessRefusal(w, accessRefusalNotJSON)
			return
		}
		if !accessSameOrigin(r) {
			writeAccessRefusal(w, accessRefusalForeignOrigin)
			return
		}
	}
	// An honestly-declared oversize body is refused on the header alone. The
	// body that declares nothing is caught below, once it is known to be worth
	// reading.
	if r.ContentLength > route.maxBody {
		writeAccessRefusal(w, accessRefusalTooLarge)
		return
	}

	// accessClientAddress and not the shopper's helper: the right-most
	// X-Forwarded-For entry, which the caller cannot write, where the shopper's
	// leftmost one is the caller's to rotate. See there for the cost.
	if allowed, wait := h.accessRate.allow(route.op, site.ID, accessClientAddress(r)); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		writeAccessRefusal(w, accessRefusalRateLimited)
		return
	}

	if err := boundAccessBody(w, r, route.maxBody); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAccessRefusal(w, accessRefusalTooLarge)
			return
		}
		writeAccessRefusal(w, accessRefusalBadRequest)
		return
	}

	proxy := h.newBFFProxy(site, accessStamp(route, site.ID, owner))
	if proxy == nil {
		writeAccessRefusal(w, accessRefusalNotFound)
		return
	}
	proxy.ModifyResponse = accessOwnReplyHeaders
	proxy.ServeHTTP(w, r)
}

// accessStamp is the rewrite that makes the forwarded request the bff's.
//
// IT SETS AND DOES NOT STRIP. newBFFProxy has already deleted both headers from
// everything it forwards, on every path, so there is no client-written copy left
// here to replace. The order is the other half of that: this runs after the
// delete, which is the only reason the values written here reach the bff.
//
// THE PATH IS SET HERE, not by upstreamPath. The shared rewrite swaps the marker
// for the bff's /memql root, which would ask for /memql/access. The bff's four
// routes live at its own root under names of their own (/access/unlock, not
// /access), and unlock and session share one inbound path, so no rule over the
// inbound path alone could produce them.
//
// NO QUERY STRING TRAVELS. The protocol carries a code in the body of a POST and
// has no query parameters at all, so whatever a caller put after the question mark
// is dropped, and a code pasted into a URL goes no further than it has to: not
// into the bff's request line, and so not into anything that records the URL of
// what it served. The route is chosen by the path alone (accessRouteFor), so
// nothing here depends on the query. ForceQuery is cleared as well as RawQuery,
// since a bare "?" is carried by that flag and the request line would otherwise
// end in one; the bff then sees the canonical path and nothing after it. (A query
// configured on MEMQL_EDGE_API_TARGET goes too, which these four routes do not
// need. The generic proxy is untouched and keeps the queries its routes use.)
//
// THE ACCESS COOKIE, AND NO OTHER CREDENTIAL. The bff reads this one cookie and nothing
// else a browser sends, so nothing else travels: not memql_auth, not a bearer in either
// header the bff's middleware accepts one from (Sec-WebSocket-Protocol is the other, and
// the shared rewrite copies it for the bridge), not a cookie the deployable's own page
// set. Left on, they were a read a visitor could drive, from any page with a WebSocket
// (the session-revocation middleware looks up any bearer it finds) and a lockout nobody
// could explain (a revoked sign-in answered a plain-text 401 on all four routes). The
// shared rewrite has already taken the access cookie out of what it copied, so this reads
// the request as it arrived, and the value goes through as it was sent.
func accessStamp(route accessRoute, siteID, owner string) func(*httputil.ProxyRequest) {
	return func(pr *httputil.ProxyRequest) {
		pr.Out.Header.Set(AccessStampSiteHeader, siteID)
		pr.Out.Header.Set(AccessStampOwnerHeader, owner)
		pr.Out.Header.Del("Authorization")
		pr.Out.Header.Del("Sec-WebSocket-Protocol")
		accessSetCookies(pr.Out.Header, accessCookiePairs(pr.In.Header.Values("Cookie"), func(name string) bool {
			return name == memql.AccessCookieName
		}))
		pr.Out.URL.Path = route.upstream
		pr.Out.URL.RawPath = ""
		pr.Out.URL.RawQuery = ""
		pr.Out.URL.ForceQuery = false
	}
}

// accessStripCookie takes the access cookie out of a request leaving the edge for
// anywhere but the four routes, and leaves the rest of the header as it was.
//
// EVERY WAY OUT CALLS THIS. The cookie has Path=/, so a browser sends it with everything
// (see the file note), and a session token has no business in a request to the identity
// service, in a shopper's form or on the API bridge, where anything that records a header
// would be recording a working session. There are two ways out of this package, the two
// reverse proxies, and TestTheAccessCookieGoesNowhereElse walks every route through both.
func accessStripCookie(out http.Header) {
	lines := out.Values("Cookie")
	if len(lines) == 0 {
		return
	}
	accessSetCookies(out, accessCookiePairs(lines, func(name string) bool {
		return name != memql.AccessCookieName
	}))
}

// accessCookiePairs is the header lines with only the pairs keep accepts by name.
//
// THE SPELLING OF WHAT STAYS IS LEFT ALONE. A line with nothing to take out comes back as
// it arrived, and from one with something to take out only that pair goes, so what is
// somebody else's credential reaches its service in the bytes it was sent in. A line left
// holding nothing is dropped. Names are matched as net/http reads them, trimmed and exact:
// a differently cased or longer name is another cookie, to a browser as well. The value is
// never looked at, and it can say what it likes.
func accessCookiePairs(lines []string, keep func(name string) bool) []string {
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		pairs := strings.Split(line, ";")
		retained := make([]string, 0, len(pairs))
		for _, pair := range pairs {
			name, _, _ := strings.Cut(pair, "=")
			if keep(textproto.TrimString(name)) {
				retained = append(retained, pair)
			}
		}
		if len(retained) == len(pairs) {
			kept = append(kept, line)
			continue
		}
		if rest := textproto.TrimString(strings.Join(retained, ";")); rest != "" {
			kept = append(kept, rest)
		}
	}
	return kept
}

// accessSetCookies replaces a request's Cookie lines.
func accessSetCookies(out http.Header, lines []string) {
	if len(lines) == 0 {
		out.Del("Cookie")
		return
	}
	out["Cookie"] = lines
}
