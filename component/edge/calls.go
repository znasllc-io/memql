package edge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	callhistory "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// calls.go -- THE EDGE HALF OF THE CALLS SURFACE (the owner-approved HTTP route family of
// 2026-11-17; the bff half is component/server/calls_handler.go).
//
// # What arrives here
//
// A person's browser on a deployable's OWN origin: somebody with a key to trade for a
// session, a call's link, or the session cookie one of them opened. They are nobody to
// MemQL: no identity to open a stream with, and only an HTTP response can set an HttpOnly
// cookie, so the other party dictates the wire. A fixed set of method and path pairs, and nothing else:
//
//	POST /_memql/calls/unlock      ->  bff POST /calls/unlock      {"key": "..."}
//	GET  /_memql/calls/session     ->  bff GET  /calls/session
//	POST /_memql/calls/start       ->  bff POST /calls/start       {"name": "...", "confirm": {...}}
//	POST /_memql/calls/schedule    ->  bff POST /calls/schedule    {"start": "...", "minutes": n, "title": "..."}
//	POST /_memql/calls/reschedule  ->  bff POST /calls/reschedule  {"id": "...", "start": ..., ...}
//	POST /_memql/calls/cancel      ->  bff POST /calls/cancel      {"id": "..."}
//	POST /_memql/calls/join        ->  bff POST /calls/join        {"token" or "hostToken", "name", "confirm"}
//	GET  /_memql/calls/queue       ->  bff GET  /calls/queue
//	POST /_memql/calls/host        ->  bff POST /calls/host        {"action": "...", "seat": "..."}
//	POST /_memql/calls/leave       ->  bff POST /calls/leave       {}
//	GET/POST /_memql/calls/history ->  bff GET/POST /calls/history
//	GET/POST /_memql/calls/account ->  bff GET/POST /calls/account
//
// # What this file owns
//
// What only the EDGE can know or should do, every refusal BEFORE the proxy hop, so a request
// that is wrong, too big or too frequent never reaches the bff. The order is
// serveAccessSurface's, with the switch added after the route:
//
//  1. THE ROUTE. Exactly the fixed pairs, by method and path, never a prefix
//     (callsRouteFor). Anything else under /_memql/calls is the generic proxy's, as it was
//     before this surface existed.
//  2. THE SWITCH. site.Calls, the deployable's own, off by default and independent of
//     apiProxy. Off, each route answers the one 404 and nothing is forwarded, to
//     the surface's routes or to the generic proxy: "calls are off here" costs the bff
//     nothing. The bff checks the switch again on the row it reads (calls_handler.go), so an
//     edge whose cached copy of the site is older than a switch turned off forwards a request
//     the bff then refuses; the edge is the cheap refusal, not the only one.
//  3. THE SHAPE OF A POST: it must declare JSON and must not name another origin, and every
//     body is capped by what is READ (the fixed per-route caps below).
//  4. THE BUDGETS (callsLimits).
//  5. THE STAMP, and the two reply headers. The site and owner this edge resolved are written
//     after any copy the client sent is deleted, and every reply carries no-store and nosniff
//     whatever the bff said.
//
// It does NOT decide anything about a call, and it NEVER READS THE COOKIE'S VALUE, for the
// access surface's reason (access.go): the edge is not an auth boundary, and a verdict formed
// here would be a second decision that could disagree with the bff's. The cookie goes through
// as the browser sent it, and so does Set-Cookie coming back.
//
// IT DOES READ THE COOKIE'S NAME, on every way out of here. memql.CallsCookieName is a
// __Host- cookie, so it has Path=/ and a browser sends it with every request to the
// deployable: on the fixed pairs the bff gets that cookie and no other credential a browser
// holds (callsStamp), and every other way out takes it, and any stamp a client wrote, from
// what it forwards (callsStripRequest, called by both reverse proxies).
//
// The stamp is a POINTER, not an assertion: the bff reads the site it names again under the
// stamped owner, requires the row's own owner to equal the stamp and its switch to be on.
// Stripping first is what lets it treat the header as the edge's alone.
//
// # What it shares with the access surface, and what it must not
//
// The two surfaces stand on the same rules, so the access surface's helpers are used here
// rather than copied: the JSON and same-origin checks, the body cap, the client address and
// the cookie-line editing (access.go). What is NOT shared is every name a request or a budget
// carries: the stamp headers, the cookie and the buckets are the calls surface's own, so the
// edge's stripping of one surface's stamp and cookie can never carry them onto the other's
// routes (memql.CallsStampSiteHeader's file note), and a crowd on one surface spends nothing
// of the other's budgets.

// The headers the edge stamps and the bff reads, declared ONCE in component/memql for the
// reason AccessStampSiteHeader is, and aliased here so a reader sees they are not this
// package's to choose.
const (
	CallsStampSiteHeader  = memql.CallsStampSiteHeader
	CallsStampOwnerHeader = memql.CallsStampOwnerHeader
)

// callsStampHeaders is every header this surface controls. Both reverse proxies delete them
// from everything they forward, not only from the fixed pairs, so a client cannot supply its
// own on any path.
var callsStampHeaders = []string{CallsStampSiteHeader, CallsStampOwnerHeader, memql.CallsAgentCredentialHeader}

// The operations. A fixed set, because the budget a request draws on is chosen by it.
const (
	callsOpWorkflows   = "workflows"
	callsOpAgent       = "agent"
	callsOpWebhook     = "provider-webhook"
	callsOpUTCP        = "utcp"
	callsOpFamily      = "family"
	callsOpAccount     = "account"
	callsOpAccountRead = "account-read"
	callsOpLogout      = "logout"
	callsOpHistory     = "history"
	callsOpUnlock      = "unlock"
	callsOpSession     = "session"
	callsOpStart       = "start"
	callsOpSchedule    = "schedule"
	callsOpReschedule  = "reschedule"
	callsOpCancel      = "cancel"
	callsOpJoin        = "join"
	callsOpQueue       = "queue"
	callsOpHost        = "host"
	callsOpLeave       = "leave"
)

// callsRoute is one of the fixed method and path pairs and everything the edge needs to know
// about it: one table, so the routing decision and the forward cannot name different pairs.
// A type of its own rather than accessRoute's, so neither surface's stamp can be handed the
// other's route.
type callsRoute struct {
	method   string
	inbound  string // on the deployable's origin
	upstream string // on the bff
	op       string
	maxBody  int64
}

// callsInbound is where a route the bff serves under memql.CallsSurfacePathPrefix is reached
// on a deployable's origin: memql.CallsInboundPath, a slash and the route's name, which is the
// rule calls_surface.go states. Derived rather than spelled twice, so the inbound and upstream
// names cannot drift apart; the tests spell every pair out.
func callsInbound(upstream string) string {
	return memql.CallsInboundPath + "/" + strings.TrimPrefix(upstream, memql.CallsSurfacePathPrefix)
}

var callsRoutes = [...]callsRoute{
	{http.MethodPost, callsInbound(memql.CallsFamilyPath), memql.CallsFamilyPath, callsOpFamily, memql.CallsSmallBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsWorkflowsPath), memql.CallsWorkflowsPath, callsOpWorkflows, memql.CallsWorkflowBodyBytes},
	{http.MethodGet, callsInbound(memql.CallsDelegationPath), memql.CallsDelegationPath, callsOpAccountRead, memql.CallsSmallBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsDelegationPath), memql.CallsDelegationPath, callsOpAccount, memql.CallsDelegationBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsAgentPath), memql.CallsAgentPath, callsOpAgent, memql.CallsWorkflowBodyBytes},
	{http.MethodGet, callsInbound(memql.CallsUTCPPath), memql.CallsUTCPPath, callsOpUTCP, memql.CallsSmallBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsStripeWebhookPath), memql.CallsStripeWebhookPath, callsOpWebhook, memql.CallsProviderBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsDiditWebhookPath), memql.CallsDiditWebhookPath, callsOpWebhook, memql.CallsProviderBodyBytes},
	{http.MethodGet, callsInbound(memql.CallsHistoryPath), memql.CallsHistoryPath, callsOpHistory, memql.CallsSmallBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsHistoryPath), memql.CallsHistoryPath, callsOpHistory, memql.CallsScheduleBodyBytes},
	{http.MethodGet, callsInbound(memql.CallsAccountPath), memql.CallsAccountPath, callsOpAccountRead, memql.CallsSmallBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsAccountPath), memql.CallsAccountPath, callsOpAccount, memql.CallsAccountBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsUnlockPath), memql.CallsUnlockPath, callsOpUnlock, memql.CallsSmallBodyBytes},
	{http.MethodGet, callsInbound(memql.CallsSessionPath), memql.CallsSessionPath, callsOpSession, memql.CallsSmallBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsStartPath), memql.CallsStartPath, callsOpStart, memql.CallsScheduleBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsSchedulePath), memql.CallsSchedulePath, callsOpSchedule, memql.CallsScheduleBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsReschedulePath), memql.CallsReschedulePath, callsOpReschedule, memql.CallsScheduleBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsCancelPath), memql.CallsCancelPath, callsOpCancel, memql.CallsSmallBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsJoinPath), memql.CallsJoinPath, callsOpJoin, memql.CallsSmallBodyBytes},
	{http.MethodGet, callsInbound(memql.CallsQueuePath), memql.CallsQueuePath, callsOpQueue, memql.CallsSmallBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsHostPath), memql.CallsHostPath, callsOpHost, memql.CallsScheduleBodyBytes},
	{http.MethodPost, callsInbound(memql.CallsLeavePath), memql.CallsLeavePath, callsOpLeave, memql.CallsSmallBodyBytes},
}

// callsRouteFor reports which of the fixed pairs a request is, if it is one.
//
// AN EXACT MATCH ON BOTH, never a prefix. Claiming /_memql/calls/* would make
// /_memql/calls/x and /_memql/callsign this file's business, and they are the generic
// proxy's: with apiProxy on a site has always been able to forward them, and with it off they
// are the 404 they have always been. It does not look at the site, so the switch is decided
// in serveCallsSurface, where an off switch is the surface's own answer rather than a fall
// through to the generic proxy.
func callsRouteFor(method, p string) (callsRoute, bool) {
	for _, route := range callsRoutes {
		if route.method == method && route.inbound == p {
			return route, true
		}
	}
	return callsRoute{}, false
}

const (
	// The budgets, per replica (see callsLimits). Unlock has the access surface's numbers:
	// trading a credential for a session is the act worth slowing. Join is allowed three
	// times as many per address, because one address is routinely a household or an office
	// whose people come into a call at once, and its per-site ceiling is sized for a
	// deployable whose whole job is calls rather than for one gate on a site. A host's
	// controls, hang-up and explicit logout have a budget of their own, sized for the acts of a call's
	// people (a host admitting a line of knockers, a room hanging up at once) rather than
	// for polling: no amount of polling or joining from an address can keep a person there
	// from leaving or a host from using the controls. Everything else is ordinary use -- a
	// page polls its session/account every few seconds -- and shares one larger budget.
	// Account authentication mutations share unlock's limits; reads never spend them. There is no
	// environment override, as there is none for the access surface's, because nothing asks
	// for one yet.
	callsUnlockPerMinutePerAddress  = 10
	callsUnlockPerMinutePerSite     = 60
	callsJoinPerMinutePerAddress    = 30
	callsJoinPerMinutePerSite       = 600
	callsControlPerMinutePerAddress = 120
	callsOtherPerMinutePerAddress   = 600
)

// callsLimits are the budgets in front of the surface: six token buckets and the clock they
// are read against.
//
// PER REPLICA, BY DESIGN, for accessLimits' reason: a shared counter would put a database
// round trip on the path of every public request, and these exist to stop a flood, not to
// meter exactly. Two replicas each allow these rates, so the cluster-wide ceiling is the rate
// times the replica count. A limit that has to hold across replicas -- a room's join window,
// a key's calls -- is the calls service's, in Postgres.
//
// THEY BOUND LOAD, NOT GUESSING. What stops guessing is the 128 bits of a key and the 160 of
// a link or a session token, not a counter. The address is accessClientAddress's: the
// right-most X-Forwarded-For entry, which the ingress records and a caller cannot choose, an
// IPv6 address by its /64.
//
// BEHIND A FRONTING PROXY THE ADDRESS IS THE FRONT'S. A deployable on its own domain behind
// a front such as Azure Front Door reaches the ingress from the front's egress addresses, so
// the right-most entry is the front's, and every visitor arriving through one front address
// shares each per-address budget below: one of them flooding spends everybody's. KNOWN
// LIMITATION, carried to launch hardening, before public traffic: the
// edge keys on the visitor's own address only once the front is proven by a value it adds.
// What part 1 ships is the minimum: the hang-up and the host's controls have a bucket of
// their own, so the polls of everybody behind one front address cannot keep a person from
// hanging up or a host from using the controls.
//
// UNLOCK AND JOIN EACH DRAW ON TWO, ADDRESS FIRST, and the order is the point, as it is for
// the access surface's unlock: one address flooding is stopped by its own bucket before it
// reaches the site's, so it can take its own share of the site's budget and no more (an
// address standing for a whole front is one address here too). Each operation's buckets are
// its own, so a crowd joining cannot spend what a key holder's unlock needs, a flood of
// unlocks cannot keep a guest out, a host's polling spends neither, and neither polling nor
// joining spends what hanging up or the host's controls need.
type callsLimits struct {
	unlockAddress  *shopperBucket // (site, address): 10 a minute
	unlockSite     *shopperBucket // site: 60 a minute
	joinAddress    *shopperBucket // (site, address): 30 a minute
	joinSite       *shopperBucket // site: 600 a minute
	controlAddress *shopperBucket // (site, address): 120 a minute, for host controls, leave and logout
	other          *shopperBucket // (site, address): 600 a minute, for reads and other operations
	now            func() time.Time
}

func newCallsLimits() *callsLimits {
	return &callsLimits{
		unlockAddress:  newShopperBucket(callsUnlockPerMinutePerAddress),
		unlockSite:     newShopperBucket(callsUnlockPerMinutePerSite),
		joinAddress:    newShopperBucket(callsJoinPerMinutePerAddress),
		joinSite:       newShopperBucket(callsJoinPerMinutePerSite),
		controlAddress: newShopperBucket(callsControlPerMinutePerAddress),
		other:          newShopperBucket(callsOtherPerMinutePerAddress),
		now:            time.Now,
	}
}

// allow spends what an operation draws on. On a refusal it also says how long to wait: the
// time the bucket that refused takes to give one token back, an upper bound, since it has
// been refilling since it last looked.
func (l *callsLimits) allow(op, siteID, address string) (bool, time.Duration) {
	now := l.now()
	key := siteID + "|" + address
	switch op {
	case callsOpUnlock, callsOpAccount:
		if !l.unlockAddress.allow(key, now) {
			return false, callsTokenWait(callsUnlockPerMinutePerAddress)
		}
		if !l.unlockSite.allow(siteID, now) {
			return false, callsTokenWait(callsUnlockPerMinutePerSite)
		}
		return true, 0
	case callsOpJoin:
		if !l.joinAddress.allow(key, now) {
			return false, callsTokenWait(callsJoinPerMinutePerAddress)
		}
		if !l.joinSite.allow(siteID, now) {
			return false, callsTokenWait(callsJoinPerMinutePerSite)
		}
		return true, 0
	case callsOpHost, callsOpLeave, callsOpLogout:
		if !l.controlAddress.allow(key, now) {
			return false, callsTokenWait(callsControlPerMinutePerAddress)
		}
		return true, 0
	}
	if !l.other.allow(key, now) {
		return false, callsTokenWait(callsOtherPerMinutePerAddress)
	}
	return true, 0
}

// callsTokenWait is how long one token takes to come back to a bucket that refills perMinute
// of them a minute.
func callsTokenWait(perMinute int) time.Duration {
	return time.Minute / time.Duration(perMinute)
}

// callsRefusal is one answer this file gives without asking the bff.
//
// IN THE CALLS PROTOCOL'S WORDS, the ones the bff answers with (calls_handler.go
// callsAnswerFor): {"error": <word>, "reason"?: <code>, "retryAfterMs"?: <n>}. A page reads
// one vocabulary whichever of the two refused it, so a body too large is too_large and a
// request too soon is limited from either, and the 404 is the bff's own bytes. Every body is
// a constant or built from numbers, so there is nothing from the request to escape.
//
// NOT EVERY REPLY THE EDGE CAN SEND IS IN THIS TABLE, as for the access surface: when the bff
// cannot be reached the shared proxy builder answers its own 502 as text/plain. A client must
// not assume a JSON body on a 5xx.
type callsRefusal struct {
	status int
	body   string
}

var (
	// ONE 404 FOR EVERY CAUSE, identical to the byte to the bff's for an unknown route, an
	// unstamped request and a switch that is off: calls off, a deployable with no owner and
	// a deployment with nowhere to forward to are different facts, and telling them apart
	// would tell a prober which to work on.
	callsRefusalNotFound = callsRefusal{http.StatusNotFound, `{"error":"not_found"}`}

	callsRefusalInvalid  = callsRefusal{http.StatusBadRequest, `{"error":"invalid"}`}
	callsRefusalNotJSON  = callsRefusal{http.StatusUnsupportedMediaType, `{"error":"not_json"}`}
	callsRefusalTooLarge = callsRefusal{http.StatusRequestEntityTooLarge, `{"error":"too_large"}`}

	// The one refusal the bff never makes, because it never sees an Origin worth checking:
	// the forbidden class, with a reason of its own so the cause is not confused with a
	// seat that was removed.
	callsRefusalForeignOrigin = callsRefusal{http.StatusForbidden, `{"error":"forbidden","reason":"foreign_origin"}`}
)

// callsRefusalLimited is the edge's 429, in the bff's words for a request made too soon
// (limited, rate), with the wait in milliseconds, rounded up.
func callsRefusalLimited(wait time.Duration) callsRefusal {
	ms := (wait + time.Millisecond - 1) / time.Millisecond
	return callsRefusal{http.StatusTooManyRequests,
		`{"error":"limited","reason":"rate","retryAfterMs":` + strconv.FormatInt(int64(ms), 10) + `}`}
}

func writeCallsRefusal(w http.ResponseWriter, refusal callsRefusal) {
	setAccessReplyHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(refusal.status)
	_, _ = w.Write([]byte(refusal.body))
}

// serveCallsSurface gates one of the ten calls requests and forwards it.
//
// THE REFUSALS THAT SPEND NOTHING COME BEFORE THE ONE THAT SPENDS A TOKEN, as in
// serveAccessSurface, whose order this keeps with the switch added after the route: a page on
// another site can make a browser send a request that fails every one of them, and if they
// cost a token that page could lock a real person out.
func (h *Handler) serveCallsSurface(w http.ResponseWriter, r *http.Request, site *Site) {
	setAccessReplyHeaders(w)

	// The caller has already matched the pair; this is the backstop that keeps the function
	// from forwarding a path it was never meant to stamp.
	route, ok := callsRouteFor(r.Method, r.URL.Path)
	if !ok {
		writeCallsRefusal(w, callsRefusalNotFound)
		return
	}

	// THE SWITCH, and it is NOT LOGGED: an off switch is the ordinary state of almost every
	// deployable on the cluster, and any passer-by can ask, so a line here would be one per
	// request at a rate a stranger chooses. A preview grant changes nothing: previewSite's
	// copy carries the site's own switch.
	if !site.Calls {
		writeCallsRefusal(w, callsRefusalNotFound)
		return
	}

	// A DEPLOYABLE WITH NO OWNER HAS NO AUTHORITY FOR A CALL TO RUN UNDER: every calls row of
	// a site is the owner's, and an empty owner is the state the cluster's own surfaces carry.
	// The operator's switch refuses to turn on for such a site, so reaching here is a raw
	// write; the answer is the one 404, unlogged for the switch's reason.
	owner := strings.TrimSpace(site.OwnerUserID)
	if owner == "" {
		writeCallsRefusal(w, callsRefusalNotFound)
		return
	}

	// NO API TARGET IS THE SAME 404, AND IT IS NOT LOGGED: the edge warned once at boot
	// (app/transport_edge.go), and a line per request would be a log a visitor drives.
	if strings.TrimSpace(h.apiTarget) == "" {
		writeCallsRefusal(w, callsRefusalNotFound)
		return
	}

	if r.Method == http.MethodPost {
		if !accessJSONContentType(r) {
			writeCallsRefusal(w, callsRefusalNotJSON)
			return
		}
		if !accessSameOrigin(r) {
			writeCallsRefusal(w, callsRefusalForeignOrigin)
			return
		}
	}
	// An honestly declared oversize body is refused on the header alone; the body that
	// declares nothing is caught below, once it is known to be worth reading.
	if r.ContentLength > route.maxBody {
		writeCallsRefusal(w, callsRefusalTooLarge)
		return
	}

	if route.op == callsOpHistory {
		if r.Method == http.MethodGet {
			if _, err := callhistory.ParseHistoryQuery(r.URL.RawQuery); err != nil {
				writeCallsRefusal(w, callsRefusalInvalid)
				return
			}
		} else if r.URL.RawQuery != "" {
			writeCallsRefusal(w, callsRefusalInvalid)
			return
		}
	}
	if route.op == callsOpSession {
		if _, err := callhistory.PortraitQuery(r.URL.RawQuery); err != nil {
			writeCallsRefusal(w, callsRefusalInvalid)
			return
		}
	}
	budget := route.op
	// Only account POST needs its bounded body before choosing a budget. A small,
	// unambiguous logout uses the exit/control budget, so reading the account or
	// exhausting login attempts cannot prevent sign-out. Every other account
	// payload stays authentication-limited; the BFF still validates and authorizes it.
	if route.op == callsOpFamily {
		if !boundCallsBody(w, r, route.maxBody) {
			return
		}
		budget = callsFamilyBudget(r)
	}
	if route.op == callsOpAccount {
		if !boundCallsBody(w, r, route.maxBody) {
			return
		}
		if callsAccountIsLogout(r) {
			budget = callsOpLogout
		} else if callsAccountIsReportRead(r) {
			budget = callsOpAccountRead
		}
	}
	if allowed, wait := h.callsRate.allow(budget, site.ID, accessClientAddress(r)); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		writeCallsRefusal(w, callsRefusalLimited(wait))
		return
	}

	if route.op != callsOpAccount && route.op != callsOpFamily && !boundCallsBody(w, r, route.maxBody) {
		return
	}

	proxy := h.newBFFProxy(site, callsStamp(route, site.ID, owner))
	if proxy == nil {
		writeCallsRefusal(w, callsRefusalNotFound)
		return
	}
	proxy.ModifyResponse = accessOwnReplyHeaders
	proxy.ServeHTTP(w, r)
}

func boundCallsBody(w http.ResponseWriter, r *http.Request, limit int64) bool {
	if err := boundAccessBody(w, r, limit); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeCallsRefusal(w, callsRefusalTooLarge)
		} else {
			writeCallsRefusal(w, callsRefusalInvalid)
		}
		return false
	}
	return true
}

// callsAccountIsLogout recognizes only {"action":"logout"}, with JSON whitespace
// and escaping allowed. Duplicate/unknown fields, extra JSON values and large
// bodies never earn the less restrictive bucket. The caller has already bounded
// and buffered this body; restore its exact bytes for the BFF's own decoder.
func callsAccountIsLogout(r *http.Request) bool {
	if r.Body == nil || r.ContentLength < 0 || r.ContentLength > memql.CallsSmallBodyBytes {
		return false
	}
	body, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(body))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	if token, err := d.Token(); err != nil || token != "action" {
		return false
	}
	var action string
	if err := d.Decode(&action); err != nil || action != "logout" {
		return false
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return false
	}
	_, err = d.Token()
	return err == io.EOF
}

// Family polling must not consume the household's mutation/exit allowance.
// Only an exact, unambiguous read or revoke body earns a different bucket; the
// service still validates all account, room, invitation and consent authority.
func callsFamilyBudget(r *http.Request) string {
	if r.Body == nil || r.ContentLength < 0 || r.ContentLength > memql.CallsSmallBodyBytes {
		return callsOpJoin
	}
	body, e := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if e != nil {
		return callsOpJoin
	}
	d := json.NewDecoder(bytes.NewReader(body))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return callsOpJoin
	}
	fields := map[string]string{}
	for d.More() {
		k, e := d.Token()
		if e != nil {
			return callsOpJoin
		}
		key, ok := k.(string)
		if !ok {
			return callsOpJoin
		}
		if _, seen := fields[key]; seen {
			return callsOpJoin
		}
		switch key {
		case "action", "roomId", "identity", "invitationId", "invitationToken", "idempotencyKey":
		default:
			return callsOpJoin
		}
		var value string
		if d.Decode(&value) != nil {
			return callsOpJoin
		}
		fields[key] = value
	}
	token, e = d.Token()
	if e != nil || token != json.Delim('}') {
		return callsOpJoin
	}
	if _, e = d.Token(); e != io.EOF {
		return callsOpJoin
	}
	switch fields["action"] {
	case "status":
		if fields["roomId"] == "" && fields["identity"] == "" && fields["idempotencyKey"] == "" {
			return callsOpAccountRead
		}
	case "list":
		if fields["invitationId"] == "" && fields["invitationToken"] == "" && fields["idempotencyKey"] == "" {
			return callsOpAccountRead
		}
	case "revoke":
		if fields["invitationToken"] == "" {
			return callsOpLeave
		}
	}
	return callsOpJoin
}

// callsStamp is the rewrite that makes the forwarded request the bff's.
//
// IT SETS AND DOES NOT STRIP. newBFFProxy has already deleted both headers from everything it
// forwards, on every path, and this runs after that delete, which is the only reason the
// values written here reach the bff.
//
// THE PATH IS SET HERE, not by upstreamPath: the shared rewrite swaps the marker for the
// bff's /memql root, and the bff serves these routes at its own root, under /calls/.
//
// HISTORY GET HAS THE ONLY QUERY ALLOWLIST: bounded filters and an opaque scoped
// cursor, validated before the proxy and again by the BFF. Credentials remain in
// the body/cookie; unknown names and duplicates are refused. Every other calls
// route still drops query strings, so a pasted key or token goes no further.
// ForceQuery is always cleared, since a bare "?" is carried by that flag.
//
// THE CALLS COOKIE, AND NO OTHER CREDENTIAL. The bff reads this one cookie and nothing else a
// browser sends, so nothing else travels: not memql_auth, not the access surface's session,
// not a bearer in either header the bff's middleware accepts one from (the session-revocation
// middleware looks up any bearer it finds, which would make a visitor's own browser state a
// read on this path, and a revoked sign-in a lockout nobody could explain), not a cookie the
// deployable's own page set. The shared rewrite has already taken the calls cookie out of what
// it copied, so this reads the request as it arrived, and the value goes through as sent.
func callsStamp(route callsRoute, siteID, owner string) func(*httputil.ProxyRequest) {
	return func(pr *httputil.ProxyRequest) {
		pr.Out.Header.Set(CallsStampSiteHeader, siteID)
		pr.Out.Header.Set(CallsStampOwnerHeader, owner)
		pr.Out.Header.Del("Authorization")
		pr.Out.Header.Del(memql.CallsAgentCredentialHeader)
		if route.op == callsOpAgent {
			// Only this exact route transports machine authority. It is deliberately
			// not a MemQL bearer, so identity middleware never interprets it as one.
			header := pr.In.Header.Get("Authorization")
			if strings.HasPrefix(header, "Bearer ") && len(header) < 512 {
				pr.Out.Header.Set(memql.CallsAgentCredentialHeader, strings.TrimPrefix(header, "Bearer "))
			}
		}
		pr.Out.Header.Del("Sec-WebSocket-Protocol")
		accessSetCookies(pr.Out.Header, accessCookiePairs(pr.In.Header.Values("Cookie"), func(name string) bool {
			return name == memql.CallsCookieName && route.op != callsOpAgent && route.op != callsOpWebhook && route.op != callsOpUTCP
		}))
		pr.Out.URL.Path = route.upstream
		pr.Out.URL.RawPath = ""
		pr.Out.URL.RawQuery = ""
		if route.op == callsOpSession {
			if portrait, err := callhistory.PortraitQuery(pr.In.URL.RawQuery); err == nil && portrait != "" {
				pr.Out.URL.RawQuery = "portrait=" + portrait
			}
		}
		if route.op == callsOpHistory && route.method == http.MethodGet {
			if _, err := callhistory.ParseHistoryQuery(pr.In.URL.RawQuery); err == nil {
				pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			}
		}
		pr.Out.URL.ForceQuery = false
	}
}

// callsStripRequest takes the calls stamp and the calls cookie out of a request leaving the
// edge for anywhere but the fixed calls routes, and leaves the rest of the header as it was.
//
// EVERY WAY OUT CALLS THIS: the bff proxy's shared rewrite (newBFFProxy, ahead of any stamp)
// and the identity proxy. A session token has no business in a request to the identity
// service, in a shopper's form, on an access route or on the API bridge, where anything that
// records a header would be recording a working seat; and a stamp a client wrote must reach
// no upstream, the bff above all, which with a base path also answers the calls routes at
// /memql/calls/..., where the generic proxy's marker swap sends an ordinary
// /_memql/calls/<anything> from an apiProxy site. TestCallsStripsThenStamps and
// TestTheCallsCookieGoesNowhereElse walk every route through both proxies.
func callsStripRequest(out http.Header) {
	for _, name := range callsStampHeaders {
		out.Del(name)
	}
	lines := out.Values("Cookie")
	if len(lines) == 0 {
		return
	}
	accessSetCookies(out, accessCookiePairs(lines, func(name string) bool {
		return name != memql.CallsCookieName
	}))
}
