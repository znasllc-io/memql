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
// resolveSite and openSession below.
//
// A ROW COMING BACK PROVES NOTHING HERE. v1:platform:site is composite tier, and its
// account arm admits any member of the site's organization, so a forged owner who is
// one reads the row. So the stamp is compared with the row's own ownerUserId, and that
// comparison is THE check on the owner. From then on the owner this handler uses is the
// row's and never the header's.
//
// NO TIER SCOPES THE CODES AND SESSIONS. The store reads and updates those rows as the
// cluster acting, because a row's current owner must not decide whether a code opens: in
// review, a whole-user ownership transfer moved a code to an heir while the site kept its
// owner, and a read under the site's owner stopped finding it, with nothing anywhere
// saying why. So every row read here has to name the stamped site, the site's row as well
// as each code and session, and a row that does not is one this site never had.
//
// # What a session is and is not
//
// It authenticates nobody to MemQL: no JWT, no actor, no stream, no reach to any
// other route. It is honored here, for one site, and everything it opens runs as
// the site's owner, BORROWED, and never with internal origin, so a @serverOnly
// construct stays out of reach of anybody holding a code (D6). The reads that
// decide a request are the store's, which makes them as the cluster with its own
// internal origin, because this package may not stamp one and the rows are
// unreadable without it (D14).
//
// # Every answer is decided fresh
//
// Validity is read from the database on every request and compared with the clock,
// with no grace and no leeway (D11, section 8). AccessSessionEnd is worked out from
// the code's CURRENT expiry, so an operator moving it moves every open session
// without a write to any of them, and nothing here writes when a session lapses: an
// expiry is a comparison. The one write outside an unlock is a visitor leaving.
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

// AccessCookieName is the cookie a visitor's session token travels in.
const AccessCookieName = "memql_access"

// AccessStampSiteHeader and AccessStampOwnerHeader are the edge's stamp: the site a
// request arrived on and the owner to borrow. They are memql's, because component/edge
// and this package cannot import each other and that is the one package both can. A
// second spelling here would only have to stay equal, and a name that drifted would make
// every visit an unstamped request, which is a 404 that says nothing about why.
const AccessStampSiteHeader, AccessStampOwnerHeader = memql.AccessStampSiteHeader, memql.AccessStampOwnerHeader

// accessCookiePath is where the browser sends the cookie: the site's own origin, on
// the four routes and nowhere else. It is the EDGE's spelling of the route family,
// not this handler's, because the browser only ever talks to the edge.
const accessCookiePath = "/_memql/access"

// The body caps. Enforced while reading, before anything is decoded or hashed: the
// normalizer and the digests run over caller input and neither bounds its own, and
// the edge's cap is the edge's. A code is 29 characters and a leave says nothing, so
// 1 KiB is generous for both.
const (
	accessSmallBodyBytes = 1 << 10
	accessCallBodyBytes  = 64 << 10
)

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

// accessRoute is one of the four method and path pairs. One table, because the
// routes this handler answers and the routes the verifier steps aside for
// (AccessSelfAuthenticatedPaths) have to be the same four, and two lists that agree
// by coincidence are the pair that drifts.
type accessRoute struct {
	method  string
	path    string
	op      string
	maxBody int64
}

var accessRoutes = []accessRoute{
	{http.MethodPost, "/access/unlock", accessOpUnlock, accessSmallBodyBytes},
	{http.MethodGet, "/access/session", accessOpSession, accessSmallBodyBytes},
	{http.MethodPost, "/access/leave", accessOpLeave, accessSmallBodyBytes},
	{http.MethodPost, "/access/call", accessOpCall, accessCallBodyBytes},
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
	ID, OwnerUserID, AccountID, DeployableName string
	// PackageDomains are the DSL domains the site's package ships
	// (package.dslDomains). Empty reaches no gate, so a package whose list is unknown
	// fails closed (D5).
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
// ONLY THE SITE IS READ AS ANYBODY IN PARTICULAR. A code and a session are read and
// updated as the cluster acting, so these answer whoever's row it is now, and nothing
// about who asked narrows them: resolveSite and openSession are what hold a row to the
// stamped site. A session is the one thing still given an owner, because its row is
// stamped with the site's owner when it is born.
//
// A MISS IS (nil, nil), and that covers every way of not being allowed to see a row.
// A read that FAILED is an error and never looks like a miss: it opens nothing, ends
// nothing and clears nothing, so a database blip is not a visitor taken out.
//
// AN ERROR MUST BE SAFE TO LOG, and that is part of this contract rather than
// something the handler can check: the handler logs it, and the digests these
// methods are handed must not be in it. integrations/siteaccess scrubs its errors for
// exactly this.
type AccessStore interface {
	// AccessSite resolves the site UNDER owner's own actor, or a miss when they
	// cannot read it or it has no package deployable.
	AccessSite(ctx context.Context, siteID, owner string) (*AccessSite, error)
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
	siteID, owner, stamped := readAccessStamp(r)
	if !stamped {
		writeAccessJSON(w, http.StatusNotFound, accessBodyNotFound)
		return accessResultNotFound
	}
	site, err := h.resolveSite(r.Context(), siteID, owner)
	if err != nil {
		h.logger.Warn("access surface: resolving the site failed",
			"component", "server", "site", siteID, "err", err)
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	}
	if site == nil {
		writeAccessJSON(w, http.StatusNotFound, accessBodyNotFound)
		return accessResultNotFound
	}
	switch op {
	case accessOpUnlock:
		return h.serveUnlock(w, r, site)
	case accessOpSession:
		return h.serveSession(w, r, site)
	case accessOpLeave:
		return h.serveLeave(w, r, site)
	default:
		return h.serveCall(w, r, site)
	}
}

// readAccessStamp is what the edge said, before any of it is believed.
//
// FAILS CLOSED WITH NO CREDENTIALS, which is exactly what membership in
// HandlerAuthorizedPaths() certifies: a request with no stamp, the shape a direct
// caller sends, reaches no read and no write.
func readAccessStamp(r *http.Request) (siteID, owner string, stamped bool) {
	siteID = strings.TrimSpace(r.Header.Get(AccessStampSiteHeader))
	owner = strings.TrimSpace(r.Header.Get(AccessStampOwnerHeader))
	return siteID, owner, siteID != "" && owner != ""
}

// resolveSite re-derives the site from the graph, and answers nil for every way the
// stamp can fail to check out. All of them are the one 404: a stamp naming a site its
// owner cannot read says nothing about which of those it was.
//
// WHAT COMES OUT OF HERE IS THE ONE THING EVERY LATER ROW IS HELD TO. The codes and
// sessions are read as the cluster, so nothing but a comparison with this site's id
// stops a row of another site being honored on this one, and that makes these two
// comparisons the checks on the stamp, rather than a second opinion about it.
func (h *AccessHandler) resolveSite(ctx context.Context, siteID, owner string) (*AccessSite, error) {
	site, err := h.store.AccessSite(ctx, siteID, owner)
	if err != nil || site == nil {
		return nil, err
	}
	// THE CHECK ON THE OWNER, and there is no other. The tier's account arm admits a
	// member of the organization, so a forged owner who is one has just read this row,
	// and the row's owner is the only thing that tells them apart from the real one.
	// Past here nothing is read as the stamped owner at all. A row with no owner matches
	// no stamp, since a stamp is never blank by here: a site with no owner cannot have
	// codes (D6).
	if !sameBareID(site.OwnerUserID, owner) {
		h.logger.Warn("access surface: refusing a stamp that does not name this site's owner",
			"component", "server", "site", siteID)
		return nil, nil
	}
	// The row has to be the site the stamp named, and not whatever the read turned up:
	// site.ID is what every code and session below is compared with.
	if !sameBareID(site.ID, siteID) || strings.TrimSpace(site.DeployableName) == "" {
		return nil, nil
	}
	return site, nil
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
func codeOpensSite(code *AccessCode, site *AccessSite) bool {
	return !code.Revoked && sameBareID(code.SiteID, site.ID) && sameBareID(code.AccountID, site.AccountID)
}

// ---------------------------------------------------------------------------
// unlock
// ---------------------------------------------------------------------------

func (h *AccessHandler) serveUnlock(w http.ResponseWriter, r *http.Request, site *AccessSite) string {
	// A BODY THAT CANNOT HOLD A CODE IS AN INVALID CODE. The wire has one word for a
	// failed attempt, and a request too long or malformed to be one is not a different
	// thing to have failed at.
	var body struct {
		Code string `json:"code"`
	}
	if !decodeAccessBody(r.Body, &body) {
		writeAccessJSON(w, http.StatusUnauthorized, accessBodyInvalid)
		return accessResultInvalid
	}

	code, err := h.store.CodeByHash(r.Context(), site.ID, memql.AccessCodeDigest(body.Code))
	if err != nil {
		h.logger.Warn("access surface: looking up a code failed",
			"component", "server", "site", site.ID, "err", err)
		writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
		return accessResultRefused
	}
	if code == nil || !codeOpensSite(code, site) {
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

// openSession hashes the cookie and decides whether it still opens anything. The
// string is accessResultNone with no cookie, accessResultEnded for every way a cookie
// can have stopped being good, accessResultRefused for a read that failed, and "" for
// a live session.
func (h *AccessHandler) openSession(r *http.Request, site *AccessSite) (*accessOpen, string) {
	cookie, err := r.Cookie(AccessCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return nil, accessResultNone
	}
	session, err := h.store.SessionByTokenHash(r.Context(), memql.AccessSessionTokenDigest(cookie.Value))
	if err != nil {
		h.logger.Warn("access surface: reading a session failed",
			"component", "server", "site", site.ID, "err", err)
		return nil, accessResultRefused
	}
	// A cookie with no row is `ended` as well, so a fixated or invented token and one
	// that was really left cannot be told apart. The site is the comparison a read as
	// the cluster cannot make for itself: a session is honored on the site it was opened
	// on and no other.
	if session == nil || session.Ended || !sameBareID(session.SiteID, site.ID) {
		return nil, accessResultEnded
	}
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

func (h *AccessHandler) serveSession(w http.ResponseWriter, r *http.Request, site *AccessSite) string {
	open, state := h.openSession(r, site)
	switch state {
	case accessResultNone:
		clearAccessCookie(w)
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
// whose code was removed under them can still say they left.
func (h *AccessHandler) serveLeave(w http.ResponseWriter, r *http.Request, site *AccessSite) string {
	if cookie, err := r.Cookie(AccessCookieName); err == nil && strings.TrimSpace(cookie.Value) != "" {
		session, err := h.store.SessionByTokenHash(r.Context(), memql.AccessSessionTokenDigest(cookie.Value))
		if err != nil {
			h.logger.Warn("access surface: reading a session to end it failed",
				"component", "server", "site", site.ID, "err", err)
			writeAccessJSON(w, http.StatusBadGateway, accessBodyRefused)
			return accessResultRefused
		}
		// IDEMPOTENT BY DECIDING FIRST. The store would write the time again for a
		// session that has already ended, and the time somebody left is not something
		// a repeat should move. Another site's session is not this site's to end.
		if session != nil && !session.Ended && sameBareID(session.SiteID, site.ID) {
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
	}
	clearAccessCookie(w)
	w.WriteHeader(http.StatusNoContent)
	return accessResultOK
}

// ---------------------------------------------------------------------------
// the call
// ---------------------------------------------------------------------------

func (h *AccessHandler) serveCall(w http.ResponseWriter, r *http.Request, site *AccessSite) string {
	open, state := h.openSession(r, site)
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

	// THE REGISTRY FIRST, so that nothing about the arguments can tell a construct
	// that is in reach from one that is not. The request's name is only ever compared
	// with what is declared here: what gets rendered is the entry's own Call().
	entry := memql.AccessGateFor(site.DeployableName, open.code.Gate, body.Call, site.PackageDomains)
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

	// AFTER THE INSTANT OF EXPIRY NO GATED DATA LEAVES (section 8), and the check ahead
	// of the call has no opinion about how long the call took. A write that landed
	// stays landed; what it says to the visitor is that they are out.
	if !h.now().Before(open.end) {
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
// HOST-ONLY AND WITH NO Max-Age, on purpose. The server is the authority on expiry,
// and an operator extending a code must not be cut short by a browser-side clock. The
// path means the cookie travels with these four requests and nothing else.
func setAccessCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: AccessCookieName, Value: token, Path: accessCookiePath,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// clearAccessCookie takes it back, with the attributes it was given: a browser will
// not let a weaker cookie overwrite a Secure one.
func clearAccessCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: AccessCookieName, Value: "", Path: accessCookiePath, MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}
