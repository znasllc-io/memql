package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/core/logger"
)

// access_handler.go -- THE BFF HALF OF THE ACCESS SURFACE (design record
// docs/superpowers/specs/2026-10-29-deployable-access-codes-design.md, section
// 6). It is what a visitor's browser actually reaches, and it decides on every
// request whether somebody is still allowed in.
//
// # The header is a POINTER, never an assertion
//
// The edge stamps which site a request arrived on and whose authority to borrow,
// having stripped any copy the client sent. That the route is classified
// servedButNotExternallyRouted is defence in depth, for the reason
// shopper_handler.go gives. So this handler re-derives everything from the graph,
// and there are two differences from the shopper surface, and they are the point of
// resolveSite and findSession below.
//
// A ROW COMING BACK PROVES NOTHING HERE. The site is read as the cluster, so it comes
// back whoever is named. So the stamp is compared with the row's own ownerUserId, and that
// comparison is THE check on the owner, as the row's account is the check on a code's
// copy of it. From then on the owner this handler uses is the row's and never the
// header's.
//
// NO TIER SCOPES THE CODES AND SESSIONS. The store reads and updates those rows as the
// cluster acting, because a row's current owner must not decide whether a code opens: in
// review, a whole-user ownership transfer moved a code to an heir while the site kept its
// owner, and a read under the site's owner stopped finding it, with nothing anywhere
// saying why. So every row read here has to name the stamped site, the site's row as well
// as each code and session, and a row that does not is one this site never had.
//
// # A stranger costs nothing
//
// Anybody on the internet can send these four requests, and the budget in front of them
// is 600 a minute per (site, address) on each edge replica, so what a request with no
// credential costs is the floor of what the cluster can be made to spend. Measured
// before this order: a cookie-less status check ran 52 statements, because the site was
// resolved first and under the owner's authority, which reads who the owner is and what
// they belong to, and a pool of four connections flattened at about 50 requests a second.
//
// SO NOTHING IS READ UNTIL A CREDENTIAL OF THE RIGHT SHAPE IS PRESENTED, the first read is
// its digest, and a digest that finds nothing is the end of it: no site read and no other
// write. Only a digest that found a row pays for the site, one point read as the cluster.
//
// THE OWNER'S AUTHORITY IS READ ONCE, RIGHT BEFORE IT IS LENT. A call reads the site and
// its package under the owner just ahead of running the construct, which already pays for
// that authority, and answers 404 if the owner cannot read them. That is what closes a
// tied site whose owner left its organization. The other three requests never lend it, so
// such a site still unlocks and still says who is in, and nothing gated runs.
//
// # What a session is and is not
//
// It authenticates nobody to MemQL: no JWT, no actor, no stream, no reach to any
// other route. It is honored here, for one site, and everything it opens runs as
// the site's owner, BORROWED, and never with internal origin, so a @serverOnly
// construct stays out of reach of anybody holding a code (D6). The reads that
// decide a request are the store's, which makes them as the cluster with its own
// internal origin, because this package may not stamp one and the rows are
// unreadable without it (D14). The one read that is not is the owner's own, ahead of a
// construct.
//
// # Every answer is decided fresh
//
// Validity is read from the database on every request and compared with the clock,
// with no grace and no leeway (D11, section 8). AccessSessionEnd is worked out from
// the code's CURRENT expiry, so an operator moving it moves every open session
// without a write to any of them, and nothing here writes when a session lapses: an
// expiry is a comparison. The one write outside an unlock is a visitor leaving. A call
// decides twice, ahead of the construct and again before its rows leave, because an end
// can land while it runs.
//
// # One answer each, identical to the byte
//
// A reply that differs is an oracle. Unknown, another site's, removed and superseded
// codes are one `invalid`. A session that ended, for any reason, is one `ended`. A
// construct this session's gate does not reach is one 404 whatever the reason, so a
// holder of one gate's code learns nothing about another's, and it is the same 404 a
// request with no stamp gets. The bodies are constants below, so "identical" is not
// something a branch can get wrong.
//
// # What is never logged
//
// A code, a token, a digest or a call argument. An engine error can repeat the
// statement it was given, which holds every argument and the stamped ids, so a
// refusal logs the site and the construct and NOT the error. The store's errors are
// the exception, by contract: see AccessStore.
//
// # Why this is HTTP at all
//
// Owner-approved exception, 2026-10-29. A visitor's browser has no MemQL identity to
// open a stream with, and only an HTTP response can set an HttpOnly cookie, so the
// other party dictates the wire.

// AccessCookieName is the cookie a visitor's session token travels in. It is memql's,
// because the edge has to keep it to the four routes and neither can import the other.
const AccessCookieName = memql.AccessCookieName

// AccessStampSiteHeader and AccessStampOwnerHeader are the edge's stamp: the site a
// request arrived on and the owner to borrow. They are memql's, because component/edge
// and this package cannot import each other and that is the one package both can. A
// second spelling here would only have to stay equal, and a name that drifted would make
// every visit an unstamped request, which is a 404 that says nothing about why.
const AccessStampSiteHeader, AccessStampOwnerHeader = memql.AccessStampSiteHeader, memql.AccessStampOwnerHeader

// The op and result labels of memql_site_access_requests_total. Fixed sets, so a
// label never holds anything a visitor sent.
const (
	accessOpUnlock  = "unlock"
	accessOpSession = "session"
	accessOpLeave   = "leave"
	accessOpCall    = "call"

	accessResultOK          = "ok"
	accessResultInvalid     = "invalid"
	accessResultExpired     = "expired"
	accessResultNone        = "none"
	accessResultEnded       = "ended"
	accessResultNotFound    = "not_found"
	accessResultInvalidArgs = "invalid_args"
	accessResultRefused     = "refused"
)

// The refusals, as the bytes they are on the wire.
var (
	accessBodyInvalid     = []byte(`{"state":"invalid"}`)
	accessBodyExpired     = []byte(`{"state":"expired"}`)
	accessBodyNone        = []byte(`{"state":"none"}`)
	accessBodyEnded       = []byte(`{"state":"ended"}`)
	accessBodyNotFound    = []byte(`{"error":"not_found"}`)
	accessBodyInvalidArgs = []byte(`{"error":"invalid_args"}`)
	accessBodyRefused     = []byte(`{"error":"refused"}`)
)

// accessSiteConcept is what a line about a site says it is about, so the Logs app can
// narrow on it and an app's own Logs section can select it (logger.Subject).
const accessSiteConcept = "v1:platform:site"

// Why a site is turned away, as an operator reads it. Fixed text, because this is a reason
// and not a report: nothing a request carried belongs in it.
const (
	accessReasonUnreadable = "the site's owner cannot read this site or its package, which is what an owner removed from the site's organization looks like"
	accessReasonNoRow      = "the stamped site has no row"
	accessReasonNotOwner   = "the stamped owner is not this site's owner"
)

// accessRoute is one of the four method and path pairs. One table, because the
// routes this handler answers and the routes the verifier steps aside for
// (AccessSelfAuthenticatedPaths) have to be the same four, and two lists that agree
// by coincidence are the pair that drifts. The paths are memql's, for the same reason
// about the edge.
type accessRoute struct {
	method  string
	path    string
	op      string
	maxBody int64
}

// THE CAPS ARE ENFORCED WHILE READING, before anything is decoded or hashed: the
// normalizer and the digests run over caller input and neither bounds its own, and the
// edge's cap is the edge's.
var accessRoutes = []accessRoute{
	{http.MethodPost, memql.AccessUnlockPath, accessOpUnlock, memql.AccessSmallBodyBytes},
	{http.MethodGet, memql.AccessSessionPath, accessOpSession, memql.AccessSmallBodyBytes},
	{http.MethodPost, memql.AccessLeavePath, accessOpLeave, memql.AccessSmallBodyBytes},
	{http.MethodPost, memql.AccessCallPath, accessOpCall, memql.AccessCallBodyBytes},
}

func accessRouteFor(method, path string) (accessRoute, bool) {
	for _, route := range accessRoutes {
		if route.method == method && slices.Contains(pathsWithBase(route.path), path) {
			return route, true
		}
	}
	return accessRoute{}, false
}

// AccessSite is the projection of a site this handler decides on.
//
// The omissions are the point, as they are on ShopperSite: a handler that cannot see
// the bundle or the settings cannot leak them into a reply.
type AccessSite struct {
	ID, OwnerUserID, AccountID, PackageID, DeployableName string
	// PackageDomains are the DSL domains the site's package ships
	// (package.dslDomains), as the read under the owner found them. The read as the
	// cluster leaves it empty, and PackageDomainsAsCluster is that read's answer. Empty
	// reaches no gate, so a package whose list is unknown fails closed (D5).
	PackageDomains []string
}

// AccessCode is one v1:platform:siteAccessCode, as the decision needs it.
type AccessCode struct {
	ID, SiteID, AccountID, Gate, Subject string
	Generation                           int
	ExpiresAt                            time.Time
	Revoked                              bool
}

// AccessSession is one v1:platform:siteAccessSession, as the decision needs it.
type AccessSession struct {
	ID, SiteID, CodeID string
	Generation         int
	IssuedAt           time.Time
	Ended              bool
}

// AccessStore is the rows this handler reads and writes, in its own terms: this
// package may not stamp internal origin, and it may not import integrations/.
//
// ONLY ONE READ IS MADE AS ANYBODY IN PARTICULAR. A site, a package, a code and a session
// are read as the cluster acting, so these answer whoever's row it is now, and nothing
// about who asked narrows them: resolveSite and findSession are what hold a row to the
// stamped site. SiteAsOwner is the one read under a person, and a session is the one thing
// still given an owner, because its row is stamped with the site's owner when it is born.
//
// A MISS IS (nil, nil), and that covers every way of not being allowed to see a row.
// A read that FAILED is an error and never looks like a miss: it opens nothing, ends
// nothing and clears nothing, so a database blip is not a visitor taken out. That holds
// for the second read of a call as well, which delivers nothing and costs one retry.
//
// AN ERROR MUST BE SAFE TO LOG, and that is part of this contract rather than
// something the handler can check: the handler logs it, and the digests these
// methods are handed must not be in it. integrations/siteaccess scrubs its errors for
// exactly this.
type AccessStore interface {
	// SiteAsCluster reads the site as the cluster acting, one point read. It says nothing
	// about who may do what: what makes the row mean something is the handler comparing
	// the stamp with it. A site with no package deployable is not a miss: it comes back
	// with a blank DeployableName, and the handler refuses it, because it was read and has
	// simply never had a code to open.
	SiteAsCluster(ctx context.Context, siteID string) (*AccessSite, error)
	// PackageDomainsAsCluster reads the DSL domains a package ships, as the cluster acting,
	// one point read. No package and no list are both no domains, which reaches no gate.
	PackageDomainsAsCluster(ctx context.Context, packageID string) ([]string, error)
	// SiteAsOwner reads the site and its package UNDER owner's own actor, or a miss when
	// they cannot read the site. It is what a call asks just before it lends that person's
	// authority to a construct, and it costs what resolving that person costs, so it is
	// made there and nowhere else.
	SiteAsOwner(ctx context.Context, siteID, owner string) (*AccessSite, error)
	// CodeByHash finds a code by the digest of what a visitor typed. A removed or
	// expired code still answers; which of those means which reply is decided here.
	CodeByHash(ctx context.Context, siteID, codeHash string) (*AccessCode, error)
	// CodeByID resolves the code a session was opened with.
	CodeByID(ctx context.Context, codeID string) (*AccessCode, error)
	// SessionByTokenHash finds a session by the digest of the cookie presented. An
	// ended session still answers.
	SessionByTokenHash(ctx context.Context, tokenHash string) (*AccessSession, error)
	// CreateSession writes a session under the site's owner, minting an id when in has
	// none.
	CreateSession(ctx context.Context, owner string, in AccessSession, tokenHash string) error
	// EndSession marks a session as left.
	EndSession(ctx context.Context, sessionID string, at time.Time) error
}

// AccessHandler serves a deployable's access surface.
type AccessHandler struct {
	engine MemQLExecutor
	store  AccessStore
	logger *slog.Logger
	now    func() time.Time
}

// NewAccessHandler wires the handler. `now` is the clock every decision is compared
// with, passed in so a test can stand one millisecond either side of an instant.
func NewAccessHandler(engine MemQLExecutor, store AccessStore, logger *slog.Logger, now func() time.Time) *AccessHandler {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &AccessHandler{engine: engine, store: store, logger: logger, now: now}
}

var _ http.Handler = (*AccessHandler)(nil)

func (h *AccessHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// EVERY REPLY, refusals included, so these sit ahead of every branch that could
	// forget them. No cache between here and a browser may hold an answer to "are
	// you still in" (D11).
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	route, known := accessRouteFor(r.Method, r.URL.Path)
	if !known {
		writeAccessJSON(w, http.StatusNotFound, accessBodyNotFound)
		return
	}
	// THE REAL CAP, enforced while reading. It goes on before anything can read the
	// body, which is every branch below.
	r.Body = http.MaxBytesReader(w, r.Body, route.maxBody)
	metrics.SiteAccessRequest(route.op, h.serve(route.op, w, r))
}

// serve answers one request and says how it went, for the counter.
func (h *AccessHandler) serve(op string, w http.ResponseWriter, r *http.Request) string {
	stamp, stamped := readAccessStamp(r)
	if !stamped {
		writeAccessJSON(w, http.StatusNotFound, accessBodyNotFound)
		return accessResultNotFound
	}
	switch op {
	case accessOpUnlock:
		return h.serveUnlock(w, r, stamp)
	case accessOpSession:
		return h.serveSession(w, r, stamp)
	case accessOpLeave:
		return h.serveLeave(w, r, stamp)
	default:
		return h.serveCall(w, r, stamp)
	}
}

// accessStamp is what the edge said a request arrived on. Nothing here has been believed
// yet: see resolveSite. The site is bare because the rows are compared with it and handed
// it, and the owner stays as it was spelled because it is only ever compared.
type accessStamp struct {
	site, owner string
}

// readAccessStamp is what the edge said, before any of it is believed.
//
// FAILS CLOSED WITH NO CREDENTIALS, which is exactly what membership in
// HandlerAuthorizedPaths() certifies: a request with no stamp, the shape a direct
// caller sends, reaches no read and no write.
func readAccessStamp(r *http.Request) (accessStamp, bool) {
	site := strings.TrimSpace(r.Header.Get(AccessStampSiteHeader))
	owner := strings.TrimSpace(r.Header.Get(AccessStampOwnerHeader))
	return accessStamp{site: memql.BareShortId(site), owner: owner}, site != "" && owner != ""
}

// resolveSite re-derives the site from the graph, and answers nil for every way the
// stamp can fail to check out. All of them are the one 404: a stamp naming a site whose
// row says otherwise says nothing about which of those it was.
//
// IT RUNS ONLY FOR A REQUEST WHOSE DIGEST FOUND A ROW. See the file note. Past a miss
// nothing here is ever reached, which is what keeps a stranger from costing a read.
//
// WHAT COMES OUT OF HERE IS THE ONE THING EVERY LATER ROW IS HELD TO. The codes and
// sessions are read as the cluster, so nothing but a comparison with this site's id
// stops a row of another site being honored on this one, and that makes these two
// comparisons the checks on the stamp, rather than a second opinion about it.
func (h *AccessHandler) resolveSite(ctx context.Context, stamp accessStamp) (*AccessSite, error) {
	site, err := h.store.SiteAsCluster(ctx, stamp.site)
	if err != nil {
		return nil, err
	}
	// The row has to be the site the stamp named, and not whatever the read turned up:
	// site.ID is what every code and session below is compared with.
	if site == nil || !sameBareID(site.ID, stamp.site) {
		h.warnSiteClosed(stamp.site, accessReasonNoRow)
		return nil, nil
	}
	// THE CHECK ON THE OWNER, and until a call reads the site under that owner there is no
	// other. The row comes back whoever the stamp named, and the row's owner is the only
	// thing that tells a forged one from the real one. Past here nothing is read as the
	// stamped owner at all. A row with no owner matches no stamp, since a stamp is never
	// blank by here: a site with no owner cannot have codes (D6).
	if !sameBareID(site.OwnerUserID, stamp.owner) {
		h.warnSiteClosed(site.ID, accessReasonNotOwner)
		return nil, nil
	}
	if strings.TrimSpace(site.DeployableName) == "" {
		return nil, nil
	}
	return site, nil
}

// answerSite resolves the site and, when that did not come to a site, answers the request
// and says how it went. "" means the site is good and nothing has been written.
func (h *AccessHandler) answerSite(w http.ResponseWriter, r *http.Request, stamp accessStamp) (*AccessSite, string) {
	site, err := h.resolveSite(r.Context(), stamp)
	if err != nil {
		h.logger.Warn("access surface: resolving the site failed",
			"component", "server", "site", stamp.site, "err", err)
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return nil, accessResultRefused
	}
	if site == nil {
		writeAccessJSON(w, http.StatusNotFound, accessBodyNotFound)
		return nil, accessResultNotFound
	}
	return site, ""
}

// warnSiteClosed says why a site's visitors are answered 404, which the reply never will.
// A tied site whose owner was removed from its organization reads as zero rows under
// that owner: every gated call it has stops working, correctly, and without this line
// nothing anywhere tells the operator looking at them why.
//
// NOTHING A VISITOR SENT GOES IN THIS LINE, and neither does the owner the stamp named:
// the reason is a constant, and the site is the edge's stamp or the row's own id. It is
// said once per request, because there is no state here to say it once with. Only a
// credential that found a row can reach it, so a stranger cannot drive it at all.
func (h *AccessHandler) warnSiteClosed(siteID, reason string) {
	h.logger.Warn("access surface: a credential holder was answered 404, see reason",
		"component", "server", "reason", reason,
		logger.Subject(accessSiteConcept, memql.BareShortId(siteID)))
}

// sameBareID compares two ids whatever their spelling. A view hands relationship
// fields back canonical and an id can arrive bare, and one person written two ways
// must not read as two people, or as a site that moved.
func sameBareID(a, b string) bool {
	return memql.BareShortId(a) == memql.BareShortId(b)
}

// codeOpensSite is what makes a code this site's and still alive, apart from time.
//
// THE CODE'S OWN SITE, whichever read found it. The store reads as the cluster, so this
// is what stops one site's code opening another, and it is compared even where the read
// was already given the site, because the row is what is being believed.
//
// THE COPIED ACCOUNT IS COMPARED WITH THE SITE'S CURRENT ONE, because a move racing a
// mint can leave a code carrying the organization the site just left (D15). Nothing
// is locked for that: a code whose copy differs is treated as gone, here and for every
// session opened with it, so the race leaves a code that opens nothing. Two blanks are
// not a difference: a site with no organization has codes with no copy.
//
// A GENERATION STARTS AT 1, so anything below it is a generation that could not be read,
// and it fails here as a zero expiry fails below. judgeSession only tests equality, and 0
// on a code and 0 on its session compare equal: with both read as absent, a regenerate
// would stop ending sessions and nothing would look wrong.
func codeOpensSite(code *AccessCode, site *AccessSite) bool {
	return !code.Revoked && code.Generation >= 1 &&
		sameBareID(code.SiteID, site.ID) && sameBareID(code.AccountID, site.AccountID)
}

// ---------------------------------------------------------------------------
// unlock
// ---------------------------------------------------------------------------

func (h *AccessHandler) serveUnlock(w http.ResponseWriter, r *http.Request, stamp accessStamp) string {
	// A BODY THAT CANNOT HOLD A CODE IS AN INVALID CODE. The wire has one word for a
	// failed attempt, and a request too long or malformed to be one is not a different
	// thing to have failed at. Nor is one that could not be a code, and neither of them
	// is worth a read: every minted code is 25 symbols once normalized.
	var body struct {
		Code string `json:"code"`
	}
	if !decodeAccessBody(r.Body, &body) || !memql.AccessCodeWellFormed(body.Code) {
		writeAccessJSON(w, http.StatusUnauthorized, accessBodyInvalid)
		return accessResultInvalid
	}

	// THE DIGEST FIRST, AND A MISS ENDS IT. No site read and no other write, so a guess
	// costs one lookup. The site is resolved only for a request holding a code it found.
	code, err := h.store.CodeByHash(r.Context(), stamp.site, memql.AccessCodeDigest(body.Code))
	if err != nil {
		h.logger.Warn("access surface: looking up a code failed",
			"component", "server", "site", stamp.site, "err", err)
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	}
	if code == nil {
		writeAccessJSON(w, http.StatusUnauthorized, accessBodyInvalid)
		return accessResultInvalid
	}
	site, answered := h.answerSite(w, r, stamp)
	if answered != "" {
		return answered
	}
	if !codeOpensSite(code, site) {
		writeAccessJSON(w, http.StatusUnauthorized, accessBodyInvalid)
		return accessResultInvalid
	}
	// LAST, and only somebody holding the exact code can learn this. A zero expiry,
	// which is what an unreadable one comes back as, is before now: a code with no
	// readable expiry is expired rather than eternal.
	now := h.now()
	if !now.Before(code.ExpiresAt) {
		writeAccessJSON(w, http.StatusForbidden, accessBodyExpired)
		return accessResultExpired
	}

	// WHOLE-SECOND UTC, because the latest-session sort compares this field as text.
	// The store enforces that form too; rounding here as well means the end this reply
	// states is the end the next request will work out from the row, and rounding down
	// can only shorten the 24 hours.
	issuedAt := now.UTC().Truncate(time.Second)
	token, digest, err := memql.MintAccessSessionToken()
	if err != nil {
		h.logger.Error("access surface: minting a session token failed",
			"component", "server", "site", site.ID, "err", err)
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	}
	session := AccessSession{SiteID: site.ID, CodeID: code.ID, Generation: code.Generation, IssuedAt: issuedAt}
	if err := h.store.CreateSession(r.Context(), site.OwnerUserID, session, digest); err != nil {
		h.logger.Warn("access surface: writing a session failed",
			"component", "server", "site", site.ID, "err", err)
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	}

	setAccessCookie(w, token)
	writeAccessSession(w, code, memql.AccessSessionEnd(issuedAt, code.ExpiresAt), now)
	return accessResultOK
}

// ---------------------------------------------------------------------------
// the session a request presents
// ---------------------------------------------------------------------------

// accessOpen is a session that has just been found alive: the code it was opened
// with, the instant it stops being honored, and the instant it was found alive at, so
// a reply states the remainder as of the decision rather than of a later reading of
// the clock.
type accessOpen struct {
	code *AccessCode
	end  time.Time
	at   time.Time
}

// findSession reads what the cookie names and says whether it is a live row of the stamped
// site. The string is accessResultNone with no cookie, accessResultEnded for a cookie
// that names nothing here, accessResultRefused for a read that failed, and "" for a
// session the caller should go on to judge.
//
// IT COMPARES ONLY WITH THE STAMP, and that is enough to say no: the session's own site
// has to be the stamped one, and whether the stamp itself is good is resolveSite's to say
// once something has been found. So every answer here but the last costs one read at
// most, and a cookie that could not be a token costs none.
func (h *AccessHandler) findSession(r *http.Request, stamp accessStamp) (*AccessSession, string) {
	cookie, err := r.Cookie(AccessCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return nil, accessResultNone
	}
	// THE COOKIE IS THE CALLER'S TO WRITE, so one MintAccessSessionToken could not have
	// made is a miss nobody needs the database to establish, and it reads as one.
	if !memql.AccessSessionTokenWellFormed(cookie.Value) {
		return nil, accessResultEnded
	}
	session, err := h.store.SessionByTokenHash(r.Context(), memql.AccessSessionTokenDigest(cookie.Value))
	if err != nil {
		h.logger.Warn("access surface: reading a session failed",
			"component", "server", "site", stamp.site, "err", err)
		return nil, accessResultRefused
	}
	// A cookie with no row is `ended` as well, so a fixated or invented token and one
	// that was really left cannot be told apart. The site is the comparison a read as
	// the cluster cannot make for itself: a session is honored on the site it was opened
	// on and no other.
	if session == nil || session.Ended || !sameBareID(session.SiteID, stamp.site) {
		return nil, accessResultEnded
	}
	return session, ""
}

// judgeSession decides whether a session found on this site still opens anything. The
// string is accessResultEnded for every way it can have stopped being good,
// accessResultRefused for a read that failed, and "" for a live session.
func (h *AccessHandler) judgeSession(r *http.Request, session *AccessSession, site *AccessSite) (*accessOpen, string) {
	code, err := h.store.CodeByID(r.Context(), session.CodeID)
	if err != nil {
		h.logger.Warn("access surface: reading a session's code failed",
			"component", "server", "site", site.ID, "err", err)
		return nil, accessResultRefused
	}
	// THE GENERATION IS WHAT A REGENERATE ENDS. The code's id survives one (D7), so a
	// session finds the same row afterwards, and this is the one comparison that says
	// it was opened with the secret that row no longer holds.
	if code == nil || !codeOpensSite(code, site) || code.Generation != session.Generation {
		return nil, accessResultEnded
	}
	end, now := memql.AccessSessionEnd(session.IssuedAt, code.ExpiresAt), h.now()
	if !now.Before(end) {
		return nil, accessResultEnded
	}
	return &accessOpen{code: code, end: end, at: now}, ""
}

func (h *AccessHandler) serveSession(w http.ResponseWriter, r *http.Request, stamp accessStamp) string {
	session, state := h.findSession(r, stamp)
	var open *accessOpen
	if state == "" {
		site, answered := h.answerSite(w, r, stamp)
		if answered != "" {
			return answered
		}
		open, state = h.judgeSession(r, session, site)
	}
	switch state {
	case accessResultNone:
		// NO CLEAR, BECAUSE THERE IS NOTHING TO SAY IT ABOUT. No cookie on this request does
		// not mean the browser holds none: under SameSite=Strict a cross-site navigation
		// to this path arrives without it, and answering that with a clear would delete
		// the real session of whoever followed the link.
		writeAccessJSON(w, http.StatusUnauthorized, accessBodyNone)
		return accessResultNone
	case accessResultEnded:
		clearAccessCookie(w)
		writeAccessJSON(w, http.StatusUnauthorized, accessBodyEnded)
		return accessResultEnded
	case accessResultRefused:
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	}
	writeAccessSession(w, open.code, open.end, open.at)
	return accessResultOK
}

// ---------------------------------------------------------------------------
// leave
// ---------------------------------------------------------------------------

// serveLeave ends the session the cookie names, and no other: two browsers on one
// code are two sessions, and leaving in one is not leaving in both.
//
// IT LOOKS AT THE SESSION ONLY, not at whether its code is still good: a visitor
// whose code was removed under them can still say they left. A stranger's leave, a cookie
// that names nothing, one that has already ended and one of another site all come back
// from findSession without a site read, and each is told the same 204.
//
// IDEMPOTENT BY DECIDING FIRST. The store would write the time again for a session that
// has already ended, and the time somebody left is not something a repeat should move.
func (h *AccessHandler) serveLeave(w http.ResponseWriter, r *http.Request, stamp accessStamp) string {
	session, state := h.findSession(r, stamp)
	switch state {
	case accessResultRefused:
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	case "":
		site, answered := h.answerSite(w, r, stamp)
		if answered != "" {
			return answered
		}
		if err := h.store.EndSession(r.Context(), session.ID, h.now()); err != nil {
			// THE COOKIE STAYS. Clearing it would tell the browser this is over while
			// the row still honours the token, so a copy of the cookie would go on
			// working with nobody able to retry the end.
			h.logger.Warn("access surface: ending a session failed",
				"component", "server", "site", site.ID, "err", err)
			writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
			return accessResultRefused
		}
	}
	clearAccessCookie(w)
	w.WriteHeader(http.StatusNoContent)
	return accessResultOK
}

// ---------------------------------------------------------------------------
// the call
// ---------------------------------------------------------------------------

func (h *AccessHandler) serveCall(w http.ResponseWriter, r *http.Request, stamp accessStamp) string {
	session, state := h.findSession(r, stamp)
	var open *accessOpen
	var site *AccessSite
	if state == "" {
		var answered string
		site, answered = h.answerSite(w, r, stamp)
		if answered != "" {
			return answered
		}
		open, state = h.judgeSession(r, session, site)
	}
	switch state {
	case accessResultNone, accessResultEnded:
		// A CALL HAS ONE WORD FOR THIS. `none` is an answer to a stranger asking whether
		// they are in; somebody calling a construct with no cookie has a session they
		// no longer hold.
		clearAccessCookie(w)
		writeAccessJSON(w, http.StatusUnauthorized, accessBodyEnded)
		return accessResultEnded
	case accessResultRefused:
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	}

	// UseNumber in the decoder, so a long integer reaches the engine as the digits it
	// was sent as rather than as the nearest float64.
	var body struct {
		Call string         `json:"call"`
		Args map[string]any `json:"args"`
	}
	if !decodeAccessBody(r.Body, &body) {
		writeAccessJSON(w, http.StatusBadRequest, accessBodyInvalidArgs)
		return accessResultInvalidArgs
	}

	// THE PACKAGE IS READ HERE AND NOWHERE ELSE: a status check has no use for it, and it
	// is polled. A read that failed is the same refusal as any other, and opens nothing.
	domains, err := h.store.PackageDomainsAsCluster(r.Context(), site.PackageID)
	if err != nil {
		h.logger.Warn("access surface: reading the site's package failed",
			"component", "server", "site", site.ID, "err", err)
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	}

	// THE REGISTRY FIRST, so that nothing about the arguments can tell a construct
	// that is in reach from one that is not. The request's name is only ever compared
	// with what is declared here: what gets rendered is the entry's own Call().
	entry := memql.AccessGateFor(site.DeployableName, open.code.Gate, body.Call, domains)
	if entry == nil {
		writeAccessJSON(w, http.StatusNotFound, accessBodyNotFound)
		return accessResultNotFound
	}
	args, ok := accessCallArgs(entry, body.Args, site, open.code)
	if !ok {
		writeAccessJSON(w, http.StatusBadRequest, accessBodyInvalidArgs)
		return accessResultInvalidArgs
	}
	call, err := dslCall(entry.Call(), args)
	if err != nil {
		// Not the error: it names an argument and may quote its value.
		h.logger.Error("access surface: could not render the construct call",
			"component", "server", "site", site.ID, "construct", entry.Construct)
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	}

	// THE OWNER HAS TO BE ABLE TO SEE WHAT THEY ARE ABOUT TO LEND, asked last, because it
	// costs what resolving that person costs and everything above can say no for less. It
	// is what closes a tied site whose owner left its organization: the site reads as zero
	// rows under them, and a construct would run under somebody with no standing here. The
	// row is held to the one the cluster read, and the construct's domain to the list they
	// can see, so this cannot lend authority over anything but what was just decided on.
	owned, err := h.store.SiteAsOwner(r.Context(), site.ID, site.OwnerUserID)
	if err != nil {
		h.logger.Warn("access surface: reading the site under its owner failed",
			"component", "server", "site", site.ID, "err", err)
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	}
	if owned == nil || !sameBareID(owned.ID, site.ID) || !sameBareID(owned.OwnerUserID, site.OwnerUserID) ||
		!slices.Contains(owned.PackageDomains, entry.Domain) {
		h.warnSiteClosed(site.ID, accessReasonUnreadable)
		writeAccessJSON(w, http.StatusNotFound, accessBodyNotFound)
		return accessResultNotFound
	}

	// BORROWED AUTHORITY, and the row's owner rather than the stamp's spelling of one.
	// The owner owns what is written through their site, and the visitor is data on it.
	// Never internal origin: that would put every @serverOnly construct in reach of
	// anybody holding a code.
	result, err := h.engine.Execute(auth.ContextWithUserActor(r.Context(), site.OwnerUserID), call)
	if err != nil {
		// NOT THE ERROR. It can repeat the statement, and the statement is every argument
		// the visitor sent and every id this handler stamped. The site and the construct
		// are what an operator needs to go and find out.
		h.logger.Warn("access surface: the construct refused the call",
			"component", "server", "site", site.ID, "construct", entry.Construct)
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	}

	// NOTHING LEAVES UNTIL THE SESSION HAS BEEN READ AGAIN (section 8). The read ahead of
	// the call says nothing about what happened while it ran, and a regenerate, a removal,
	// an expiry moved into the past or the instant of expiry itself can each land in that
	// window. Measured before this was here: each of the three acts answered 200 with
	// rows. So the whole decision is made again, which costs two point reads on every call
	// and is what makes "taken out now" true of a query already running. A write that
	// landed stays landed; what it says to the visitor is that they are out.
	//
	// A READ THAT FAILED FOUND NOTHING ENDED, so it is not answered as though it had. The
	// rows are in hand and nothing has said they may leave, so they do not, but the session
	// may be as good as it was: this is the same refusal as any other failed read, and it
	// clears nothing, so a database blip costs one retry and takes nobody out. `ended` is
	// for a read that succeeded and said so.
	session, state = h.findSession(r, stamp)
	if state == "" {
		_, state = h.judgeSession(r, session, site)
	}
	switch state {
	case "":
	case accessResultRefused:
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	default:
		clearAccessCookie(w)
		writeAccessJSON(w, http.StatusUnauthorized, accessBodyEnded)
		return accessResultEnded
	}

	isQuery := entry.Kind == memql.AccessKindQuery
	shaped := isQuery && accessResultIsShaped(result)
	if isQuery && !shaped {
		// Said at WARN because the fields a page expects are missing, and this is the only
		// place its author can find out why.
		h.logger.Warn("access surface: a gated query has no shape clause, so only its ids are answered; give it one",
			"component", "server", "site", site.ID, "construct", entry.Construct)
	}
	payload, err := accessReply(result, shaped)
	if err != nil {
		// Not the error, which could quote a row.
		h.logger.Error("access surface: could not encode the construct's rows",
			"component", "server", "site", site.ID, "construct", entry.Construct)
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	}
	writeAccessJSON(w, http.StatusOK, payload)
	return accessResultOK
}

// accessCallArgs builds a construct's arguments, or says the request may not make
// this call.
//
// EVERY NAME THAT REACHES dslCall IS ONE THE CONSTRUCT DECLARED, because dslCall
// writes names as raw text and encodes only the values: `x: 1) mutation evil(` as a
// key would otherwise be rendered as source. The engine is no second check. Measured,
// it ignores an argument nobody declared. So an unknown key is refused here, before
// anything is rendered, and a reserved one is refused even on a construct that does
// not declare it, so a forged argument cannot tell anybody what a construct declares.
//
// THE RESERVED NAMES ARE STAMPED LAST, so nothing a caller sent can stand in for them
// whatever the entry says. accessSubject is stamped only for a code that has one, and
// not as an empty string. Measured on the engine, `row.subject == args.accessSubject`
// with "" matches every row whose subject was never set, so a subject-less code would
// read rows that belong to no client, while the absent argument is refused. A
// construct that scopes by subject must not open to a code with none.
func accessCallArgs(entry *memql.AccessGateEntry, sent map[string]any, site *AccessSite, code *AccessCode) (map[string]any, bool) {
	args := make(map[string]any, len(sent)+len(entry.Stamped))
	for name, value := range sent {
		if slices.Contains(memql.ReservedAccessArgs, name) || !slices.Contains(entry.Args, name) {
			return nil, false
		}
		args[name] = value
	}
	for _, name := range entry.Stamped {
		switch name {
		case memql.AccessArgSiteID:
			args[name] = memql.BareShortId(site.ID)
		case memql.AccessArgCodeID:
			args[name] = memql.BareShortId(code.ID)
		case memql.AccessArgGate:
			args[name] = code.Gate
		case memql.AccessArgSubject:
			if code.Subject != "" {
				args[name] = code.Subject
			}
		}
	}
	return args, true
}

// accessResultIsShaped reports whether a result is a shape's rows rather than the
// nodes themselves. The engine sets a query's output when a shape ran, and leaves
// only the bundle when none did. Anything that is not the engine's own result type is
// not known to be shaped, so it gets the narrow answer.
func accessResultIsShaped(result any) bool {
	executed, ok := result.(*memql.ExecuteResult)
	if !ok {
		return false
	}
	_, shaped := executed.FlatOutput()
	return shaped
}

// accessReply is what a construct's result may say to a visitor, as the bytes of the
// reply.
//
// A SHAPE'S ROWS, because a shape is the construct's author saying what leaves. A row
// that came from no shape is the node, with the payload's every field, its creator and
// the concept's own description in its schema, and on a repeat of a known id a
// mutation's node holds what another code's visitor wrote. So anything else, and a
// mutation always, answers with only its bare id.
//
// THE ROOTS AND NOT THE NODES, because an unshaped query's bundle also carries what its
// rows are related to. Measured: a brief with a relationship to its site brings the
// site's node along, so reading ids off the nodes would answer with the site's id as
// though it were a brief. The roots are the rows the construct actually selected, which
// is also what a shape projects.
func accessReply(result any, shaped bool) ([]byte, error) {
	rows := []map[string]any{}
	if shaped {
		bare, err := bareAccessRows(memql.MaterializeRows(result))
		if err != nil {
			return nil, err
		}
		rows = bare
	} else if executed, ok := result.(*memql.ExecuteResult); ok && executed != nil && executed.Bundle != nil {
		for _, root := range executed.Bundle.GetRootIds() {
			rows = append(rows, map[string]any{"id": memql.BareShortId(root)})
		}
	}
	return json.Marshal(map[string]any{"data": rows})
}

// bareAccessRows returns copies of a shape's rows with every id-shaped value bare, as
// every other wire has them (#2441): clients never see a v1: id.
//
// THE WALK IS THE STREAM WIRE'S OWN, BareifyEventPayload being bareifyMap with a name
// for its first caller, so what counts as an id here cannot drift from what counts as
// one there, and a concept's own name stays as it is.
//
// THE JSON ROUND TRIP IS NOT DECORATION, on either count. That walk descends only the
// decoded-JSON types, so an id inside a []string or a []map[string]any would pass
// through it canonical, and the stream wire is safe from that only because it has
// already been through structpb by then. And a shaped result can be a cached one, so
// these rows are not ours to rewrite: the round trip is also the copy.
func bareAccessRows(rows []map[string]any) ([]map[string]any, error) {
	if len(rows) == 0 {
		return []map[string]any{}, nil
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out []map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	for _, row := range out {
		memql.BareifyEventPayload(row)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// the wire
// ---------------------------------------------------------------------------

// decodeAccessBody reads one JSON value and nothing after it. False for anything
// else, a body over the cap included: MaxBytesReader surfaces as a read error here.
// Numbers stay numbers (UseNumber) for the reason serveCall gives.
func decodeAccessBody(body io.Reader, into any) bool {
	decoder := json.NewDecoder(body)
	decoder.UseNumber()
	if err := decoder.Decode(into); err != nil {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

// writeAccessSession is the 200 an unlock and a status check share.
//
// `expiresAt` AND `remainingMs` ARE ONE INSTANT, the session's end, stated two ways.
// remainingMs is the one a client schedules from, so a wrong client clock does not
// move the moment. A long-lived code's own expiry would sit here beside a remainder of
// at most a day and leave a client holding two answers to when it is taken out.
func writeAccessSession(w http.ResponseWriter, code *AccessCode, end, now time.Time) {
	// Strings and an integer, which do not fail to encode.
	payload, _ := json.Marshal(struct {
		Gate        string `json:"gate"`
		Subject     string `json:"subject"`
		ExpiresAt   string `json:"expiresAt"`
		RemainingMs int64  `json:"remainingMs"`
	}{
		Gate:        code.Gate,
		Subject:     code.Subject,
		ExpiresAt:   end.UTC().Format(time.RFC3339Nano),
		RemainingMs: end.Sub(now).Milliseconds(),
	})
	writeAccessJSON(w, http.StatusOK, payload)
}

func writeAccessJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// setAccessCookie hands the browser its session.
//
// A __Host- COOKIE, AND A BROWSER HOLDS IT TO THAT: Secure, Path=/ and no Domain, or it is
// refused without a word. That is what stops a sibling site on the same parent domain
// planting one (memql.AccessCookieName says how that went without it).
//
// NO Max-Age, on purpose. The server is the authority on expiry, and an operator
// extending a code must not be cut short by a browser-side clock.
func setAccessCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: AccessCookieName, Value: token, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// clearAccessCookie takes it back, with the attributes it was given: a browser will
// not let a weaker cookie overwrite a Secure one, and it refuses a __Host- cookie that
// does not carry them.
func clearAccessCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: AccessCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}
