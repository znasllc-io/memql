package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/logger"
)

// calls_handler.go -- THE BFF HALF OF THE CALLS SURFACE: the fixed calls routes a person's browser
// reaches on a deployable whose calls switch is on, to trade a key for a session, start or
// book a call, join one by its link, wait in line, make a host control and hang up. The
// rules are the calls service's (integrations/calls, over component/calls); this file
// decides what is believed before the service is asked, which site it is handed, and how
// each of its answers reads on the wire.
//
// # The stamp is a POINTER, never an assertion
//
// The edge reaches these routes in-cluster, having checked the site's calls switch, capped
// the body, spent its rate limits and stamped the site and its owner after stripping any
// copy the client sent. That the routes are classified servedButNotExternallyRouted is
// defence in depth, for the reason shopper_handler.go gives, so nothing here believes the
// stamp. The site is READ AGAIN UNDER THE STAMPED OWNER (CallsService.SiteAsOwner): a forged
// owner reads no row, the row's own owner has to equal the stamp, because a member of the
// site's organization reads the row through the tier's account arm, and the row's own calls
// switch has to be on, because the edge's copy of the site can be older than the row and the
// service trusts the site it is handed. Every way of failing that is the one 404. From then
// on the site the service is handed is the row's (SiteRef from the row, and Session.Site
// from the same), never the stamp's spelling and never anything a request sent.
//
// # A stranger costs nothing
//
// Anybody can send these requests, so what one with no credential costs is the floor of
// what the cluster can be made to spend. NOTHING IS READ UNTIL A CREDENTIAL OF THE RIGHT
// SHAPE IS PRESENTED: a session cookie or a link that calls.TokenWellFormed accepts, or a
// key calls.ParseKey reads. Each shape is the one the service itself refuses unread, so a
// request turned away here gets the answer the service gives that credential, without the
// site read and without the service's digest read. A credential of the right shape pays
// for one read of the site, then the service does the rest. A session is not resolved
// first: the service reads the row its token names on every call it is handed.
//
// # What a session is
//
// A calls session authenticates nobody to MemQL: no JWT, no actor, no stream, no reach to
// any other route. It is a browser's hold on a key's account (after unlock) or on one seat
// (after a first join by link), honoured on the deployable it was opened on and no other,
// and it rides an HttpOnly cookie so a page's script never holds it. A guest's seat is
// their own, so their place in line, their leaving and a host's removal are theirs alone.
// Leaving a call does not end the session: a key holder starts the next call on it, and a
// guest who hung up comes back on the same seat.
//
// EVERY SEAT'S PAGE POLLS GET /session while it is in a call, and the answer says what only
// the rows record (integrations/calls host.go SessionCall): `host`, true while the seat
// hosts its open call, which is how a key holder the call was handed to learns it is theirs,
// since a hand-over hands out no new pass; and `countdownEndsAt`, while the host-left
// countdown runs, the instant the call ends unless a host is back first (design section 8,
// "End": a countdown on screen), on the service's clock, which a page counts down from
// against this answer's own expiresAt and expiresInMs. Each is absent otherwise. A host's
// page polls about every three seconds to see who is knocking, and to a host seat in an
// open call the session answers the call's roster as well: whether the call is locked, so
// the page offers Lock or Unlock and not both; the seats in the call, each with its media
// identity so a page can match the media server's participants to seats; and the seats
// waiting to come in, in line order. Its seat ids are the ones POST /host takes; nothing
// else tells a page whom a waiter is, since a waiter is not in the media room. Every other
// session's answer carries no roster.
//
// # Every answer in one place
//
// The service answers with an error class (component/calls ErrNoSession ... ErrConflict)
// and, for a refusal, a stable reason. callsAnswerFor is the one place those become a
// status, and the routes' own refusals -- a body too large, not JSON, a credential of the
// wrong shape -- go through it as the error the service would have answered, so there is
// no second table. Every miss is one 404, identical to the byte.
//
// # What is never logged
//
// A key, a token, a link, a cookie, a pass or anything a person typed. A failure the
// service did not classify is logged by its route and its site, with the service's error,
// which the CallsService contract keeps free of secrets. The stamped owner is never logged.
//
// # Why this is HTTP at all
//
// Owner-approved exception, 2026-11-17. A person joining a call has no MemQL identity to
// open a stream with, and only an HTTP response can set an HttpOnly cookie, so the other
// party dictates the wire.

// CallsCookieName is the cookie a calls session's token travels in. It is memql's, because
// the edge keeps it to the surface's routes and neither package can import the other.
const CallsCookieName = memql.CallsCookieName

// CallsStampSiteHeader and CallsStampOwnerHeader are the edge's stamp: the site a request
// arrived on and its owner. memql's, for AccessStampSiteHeader's reason.
const CallsStampSiteHeader, CallsStampOwnerHeader = memql.CallsStampSiteHeader, memql.CallsStampOwnerHeader

// callsSiteConcept is what a line about a site says it is about (logger.Subject).
const callsSiteConcept = "v1:platform:site"

// CallsSite is the projection of a site the calls surface decides on. The omissions are the
// point, as on ShopperSite: a handler that cannot see the bundle or the settings cannot leak
// them into a reply.
type CallsSite struct {
	ID, OwnerUserID string
	// DeployableName is the site's package deployable, which a call's room records. A site
	// with none is not refused: calls need no gate of a deployable's.
	DeployableName string
	// Calls is the site's calls switch. Absent reads as off.
	Calls bool
}

// CallsService is the calls capability in this handler's terms: the service's own acts over
// component/calls' types, and the one read the handler makes itself. This package may not
// import integrations/, so app/ adapts integrations/calls to it.
//
// A MISS IS (nil, nil) FOR THE SITE, and that covers every way of not being allowed to see
// it. A read that FAILED is an error and never looks like a miss: it opens nothing and
// clears nothing, so a database blip costs a retry and takes nobody out of a call.
//
// AN ERROR MUST CARRY NO SECRET, and that is part of this contract rather than something the
// handler can check: the handler logs an error it cannot classify. integrations/calls hands
// out a secret once and never puts one in an error; it scrubs digests from the engine's
// errors and says nothing of a sealed secret (store.go scrubbed, quietly).
type CallsService interface {
	// SiteAsOwner reads the site UNDER owner's own actor, fresh, or a miss when they cannot
	// read it. It is the check on the stamp, so it runs with no internal origin.
	SiteAsOwner(ctx context.Context, siteID, owner string) (*CallsSite, error)
	Unlock(ctx context.Context, site calls.SiteRef, key string) (calls.Session, error)
	Session(ctx context.Context, site calls.SiteRef, token string) (calls.Session, error)
	// SessionCall is what a session seated in an open call is told about it: whether its
	// seat hosts the call now, when the host-left countdown ends while one runs, and, to a
	// host, the roster the host controls name. The zero SeatCall for every other session. A
	// read that writes nothing.
	SessionCall(ctx context.Context, sess calls.Session) (calls.SeatCall, error)
	Start(ctx context.Context, sess calls.Session, body calls.StartBody) (calls.StartResult, error)
	Schedule(ctx context.Context, sess calls.Session, req calls.ScheduleRequest) (calls.ScheduledCall, error)
	Reschedule(ctx context.Context, sess calls.Session, callID string, req calls.ScheduleRequest) (calls.ScheduledCall, error)
	Cancel(ctx context.Context, sess calls.Session, callID string) error
	Join(ctx context.Context, site calls.SiteRef, sess *calls.Session, body calls.JoinBody) (calls.JoinResult, calls.Session, error)
	Queue(ctx context.Context, sess calls.Session) (calls.JoinResult, error)
	Host(ctx context.Context, sess calls.Session, req calls.HostActionRequest) error
	Invite(ctx context.Context, sess calls.Session, req calls.InviteRequest) (calls.InviteResult, error)
	InvitationLink(ctx context.Context, sess calls.Session, req calls.InvitationLinkRequest) (calls.InvitationLinkResult, error)
	Leave(ctx context.Context, sess calls.Session) error
}

// The route names, which a log line carries. A fixed set, so a line never holds anything a
// request sent.
const (
	callsOpDelegation    = "delegation"
	callsOpWorkflows     = "workflows"
	callsOpAgent         = "agent"
	callsOpUTCP          = "utcp"
	callsOpStripeWebhook = "stripe-webhook"
	callsOpDiditWebhook  = "didit-webhook"
	callsOpFamily        = "family"
	callsOpAccount       = "account"
	callsOpHistory       = "history"
	callsOpUnlock        = "unlock"
	callsOpSession       = "session"
	callsOpStart         = "start"
	callsOpSchedule      = "schedule"
	callsOpReschedule    = "reschedule"
	callsOpCancel        = "cancel"
	callsOpJoin          = "join"
	callsOpQueue         = "queue"
	callsOpHost          = "host"
	callsOpLeave         = "leave"
)

// callsRoute is one of the fixed method and path pairs. One table, because the routes this
// handler answers and the routes the verifier steps aside for (CallsSelfAuthenticatedPaths)
// have to be the same set, and two lists that agree by coincidence are the pair that drifts.
type callsRoute struct {
	method  string
	path    string
	op      string
	maxBody int64
}

// THE CAPS ARE ENFORCED WHILE READING, before anything is decoded: the edge's cap is the
// edge's.
var callsRoutes = []callsRoute{
	{http.MethodPost, memql.CallsFamilyPath, callsOpFamily, memql.CallsSmallBodyBytes},
	{http.MethodPost, memql.CallsWorkflowsPath, callsOpWorkflows, memql.CallsWorkflowBodyBytes},
	{http.MethodGet, memql.CallsDelegationPath, callsOpDelegation, memql.CallsSmallBodyBytes},
	{http.MethodPost, memql.CallsDelegationPath, callsOpDelegation, memql.CallsDelegationBodyBytes},
	{http.MethodPost, memql.CallsAgentPath, callsOpAgent, memql.CallsWorkflowBodyBytes},
	{http.MethodGet, memql.CallsUTCPPath, callsOpUTCP, memql.CallsSmallBodyBytes},
	{http.MethodPost, memql.CallsStripeWebhookPath, callsOpStripeWebhook, memql.CallsProviderBodyBytes},
	{http.MethodPost, memql.CallsDiditWebhookPath, callsOpDiditWebhook, memql.CallsProviderBodyBytes},
	{http.MethodGet, memql.CallsHistoryPath, callsOpHistory, memql.CallsSmallBodyBytes},
	{http.MethodPost, memql.CallsHistoryPath, callsOpHistory, memql.CallsScheduleBodyBytes},
	{http.MethodGet, memql.CallsAccountPath, callsOpAccount, memql.CallsSmallBodyBytes},
	{http.MethodPost, memql.CallsAccountPath, callsOpAccount, memql.CallsAccountBodyBytes},
	{http.MethodPost, memql.CallsUnlockPath, callsOpUnlock, memql.CallsSmallBodyBytes},
	{http.MethodGet, memql.CallsSessionPath, callsOpSession, memql.CallsSmallBodyBytes},
	{http.MethodPost, memql.CallsStartPath, callsOpStart, memql.CallsScheduleBodyBytes},
	{http.MethodPost, memql.CallsSchedulePath, callsOpSchedule, memql.CallsScheduleBodyBytes},
	{http.MethodPost, memql.CallsReschedulePath, callsOpReschedule, memql.CallsScheduleBodyBytes},
	{http.MethodPost, memql.CallsCancelPath, callsOpCancel, memql.CallsSmallBodyBytes},
	{http.MethodPost, memql.CallsJoinPath, callsOpJoin, memql.CallsSmallBodyBytes},
	{http.MethodGet, memql.CallsQueuePath, callsOpQueue, memql.CallsSmallBodyBytes},
	{http.MethodPost, memql.CallsHostPath, callsOpHost, memql.CallsScheduleBodyBytes},
	{http.MethodPost, memql.CallsLeavePath, callsOpLeave, memql.CallsSmallBodyBytes},
}

func callsRouteFor(method, path string) (callsRoute, bool) {
	for _, route := range callsRoutes {
		if route.method == method && slices.Contains(pathsWithBase(route.path), path) {
			return route, true
		}
	}
	return callsRoute{}, false
}

// The refusals that are the route's own rather than the service's. They go through
// callsAnswerFor like every other, so one function decides every status.
var (
	// errCallsNotHere is a request nothing here answers: an unknown route, no stamp, a
	// stamp whose site does not check out, a node with no calls service. It is the one 404
	// on every route, an unlock's included, where the service's own miss means a key.
	errCallsNotHere  = errors.New("calls surface: nothing here answers this request")
	errCallsTooLarge = errors.New("calls surface: the body is larger than the route's cap")
	errCallsNotJSON  = errors.New("calls surface: the request is not JSON")
)

// Why a site is turned away, as an operator reads it. Fixed text: nothing a request carried
// belongs in it, and neither does the owner the stamp named.
const (
	callsReasonUnreadable = "the stamped owner reads no row of the stamped site"
	callsReasonOtherRow   = "the read answered a site other than the stamped one"
	callsReasonNotOwner   = "the stamped owner is not this site's owner"
	callsReasonSwitchOff  = "the site's calls switch is off"
)

type callsHandler struct {
	svc    CallsService
	logger *slog.Logger
	now    func() time.Time
}

// NewCallsHandler wires the handler. `now` is the clock a session's remaining time is worked
// out against; the service keeps its own for every decision.
//
// A NIL SERVICE IS A NODE WITH NO CALLS CAPABILITY, and every route answers the one 404, the
// answer to "there is nothing here". The routes still exist, so a request does not fall
// through to a protocol error.
func NewCallsHandler(svc CallsService, logger *slog.Logger, now func() time.Time) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &callsHandler{svc: svc, logger: logger, now: now}
}

func (h *callsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// EVERY REPLY, refusals included, so these sit ahead of every branch that could forget
	// them. A pass, a link and a session's state are nothing a cache may keep.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	route, known := callsRouteFor(r.Method, r.URL.Path)
	if !known || h.svc == nil {
		h.refuse(w, callsCall{}, errCallsNotHere, calls.JoinResult{})
		return
	}
	// THE REAL CAP, on before anything can read the body, which is every branch below.
	r.Body = http.MaxBytesReader(w, r.Body, route.maxBody)

	// FAILS CLOSED WITH NO CREDENTIALS, which is what membership in HandlerAuthorizedPaths()
	// certifies: a request with no stamp, the shape a direct caller sends, reaches no read.
	stamp, stamped := readCallsStamp(r)
	if !stamped {
		h.refuse(w, callsCall{route: route}, errCallsNotHere, calls.JoinResult{})
		return
	}
	c := callsCall{route: route, stamp: stamp, cookie: readCallsCookie(r)}
	if route.method == http.MethodPost && !callsBodyIsJSON(r) {
		h.refuse(w, c, errCallsNotJSON, calls.JoinResult{})
		return
	}
	switch route.op {
	case callsOpFamily:
		h.serveFamily(w, r, c)
	case callsOpWorkflows:
		h.serveWorkflow(w, r, c)
	case callsOpDelegation, callsOpAgent, callsOpUTCP:
		h.serveDelegated(w, r, c)
	case callsOpStripeWebhook, callsOpDiditWebhook:
		h.serveCommerceWebhook(w, r, c)
	case callsOpHistory:
		h.serveHistory(w, r, c)
	case callsOpAccount:
		h.serveAccount(w, r, c)
	case callsOpUnlock:
		h.serveUnlock(w, r, c)
	case callsOpSession:
		h.serveSession(w, r, c)
	case callsOpStart:
		h.serveStart(w, r, c)
	case callsOpSchedule, callsOpReschedule:
		h.serveSchedule(w, r, c)
	case callsOpCancel:
		h.serveCancel(w, r, c)
	case callsOpJoin:
		h.serveJoin(w, r, c)
	case callsOpQueue:
		h.serveQueue(w, r, c)
	case callsOpHost:
		h.serveHost(w, r, c)
	default:
		h.serveLeave(w, r, c)
	}
}

// callsStamp is what the edge said a request arrived on, bare, before any of it is believed.
type callsStamp struct {
	site, owner string
}

func readCallsStamp(r *http.Request) (callsStamp, bool) {
	site := strings.TrimSpace(r.Header.Get(CallsStampSiteHeader))
	owner := strings.TrimSpace(r.Header.Get(CallsStampOwnerHeader))
	return callsStamp{site: memql.BareShortId(site), owner: memql.BareShortId(owner)}, site != "" && owner != ""
}

// callsCookie is the session cookie a request presented, as far as its shape says.
type callsCookie struct {
	token string
	// presented is a cookie of the name sent with any value: what may be taken back when the
	// service says it opens nothing. No cookie on a request does not mean the browser holds
	// none, since under SameSite=Strict a cross-site navigation arrives without it.
	presented bool
	// wellFormed is a token the service could have issued: what is worth a read.
	wellFormed bool
}

func readCallsCookie(r *http.Request) callsCookie {
	cookie, err := r.Cookie(CallsCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return callsCookie{}
	}
	token := strings.TrimSpace(cookie.Value)
	return callsCookie{token: token, presented: true, wellFormed: calls.TokenWellFormed(token)}
}

// callsCall is one request as far as it has been believed: its route, the edge's stamp, the
// cookie, and once the stamp checks out, the site the service is handed.
type callsCall struct {
	route  callsRoute
	stamp  callsStamp
	cookie callsCookie
	site   calls.SiteRef
}

// session is the session the cookie names on the resolved site: a token and its deployable,
// which is all the service believes of a Session (its file note).
func (c callsCall) session() calls.Session {
	return calls.Session{Token: c.cookie.token, Site: c.site}
}

// siteID is the site a line about this request is about: the row's once it is resolved, the
// stamp's before.
func (c callsCall) siteID() string {
	if c.site.SiteID != "" {
		return c.site.SiteID
	}
	return c.stamp.site
}

// callsBodyIsJSON is whether a POST says it carries JSON. A cross-site form can send only
// form encodings and text/plain without a preflight, so holding every POST to JSON is a
// second line behind SameSite=Strict and the edge's Origin check.
func callsBodyIsJSON(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

// decode reads one JSON object into into, and nothing after it, or answers the request and
// says false. A field the route does not take is refused: the routes take typed inputs, and a
// misspelt hostToken read as no host token would seat a host as a guest without a word.
//
// AN OBJECT, NOT ANY VALUE. A JSON null decodes into a struct without a word, so a leave sent
// `null` would hang a person up; the value is read whole first and held to being an object.
func (h *callsHandler) decode(w http.ResponseWriter, r *http.Request, c callsCall, into any) bool {
	var raw json.RawMessage
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&raw)
	if err == nil {
		// One value and nothing after it, whitespace aside. Past the cap, this read is where
		// a body too large fails.
		if _, next := decoder.Token(); !errors.Is(next, io.EOF) {
			err = next
			if err == nil {
				err = errors.New("calls surface: more than one JSON value")
			}
		}
	}
	if err == nil && (len(raw) == 0 || raw[0] != '{') {
		err = errors.New("calls surface: the body is not a JSON object")
	}
	if err == nil {
		strict := json.NewDecoder(bytes.NewReader(raw))
		strict.DisallowUnknownFields()
		err = strict.Decode(into)
	}
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		h.refuse(w, c, errCallsTooLarge, calls.JoinResult{})
	} else {
		h.refuse(w, c, calls.ErrInvalid, calls.JoinResult{})
	}
	return false
}

// requireSession refuses a request whose cookie could not be a session, without a read, as
// the service would: no session. A cookie of the wrong shape is taken back.
func (h *callsHandler) requireSession(w http.ResponseWriter, c callsCall) bool {
	if c.cookie.wellFormed {
		return true
	}
	h.refuse(w, c, calls.ErrNoSession, calls.JoinResult{})
	return false
}

// resolveSite reads the site again under the stamped owner and holds the row to the stamp,
// or answers the request and says false. See the file note.
func (h *callsHandler) resolveSite(w http.ResponseWriter, r *http.Request, c *callsCall) bool {
	site, err := h.svc.SiteAsOwner(r.Context(), c.stamp.site, c.stamp.owner)
	if err != nil {
		h.refuse(w, *c, fmt.Errorf("calls surface: reading the site under its owner: %w", err), calls.JoinResult{})
		return false
	}
	reason := ""
	switch {
	case site == nil:
		reason = callsReasonUnreadable
	case !sameBareID(site.ID, c.stamp.site):
		reason = callsReasonOtherRow
	// THE CHECK ON THE OWNER: a row with no owner matches no stamp, since a stamp is never
	// blank by here, and a site with no owner holds no calls rows.
	case strings.TrimSpace(site.OwnerUserID) == "" || !sameBareID(site.OwnerUserID, c.stamp.owner):
		reason = callsReasonNotOwner
	case !site.Calls:
		reason = callsReasonSwitchOff
	}
	if reason != "" {
		// SAID BECAUSE THE REPLY NEVER WILL. An edge that forwarded this saw a site whose
		// switch was on and whose owner it stamped, so a line here is the two disagreeing,
		// which an operator looking at a deployable's calls has to be able to find.
		h.logger.Warn("calls surface: a request was answered 404, see reason",
			"component", "server", "op", c.route.op, "reason", reason, logger.Subject(callsSiteConcept, c.stamp.site))
		h.refuse(w, *c, errCallsNotHere, calls.JoinResult{})
		return false
	}
	c.site = calls.SiteRef{
		SiteID:      memql.BareShortId(site.ID),
		OwnerUserID: memql.BareShortId(site.OwnerUserID),
		Deployable:  strings.TrimSpace(site.DeployableName),
	}
	return true
}

// ---------------------------------------------------------------------------
// the routes
// ---------------------------------------------------------------------------

func (h *callsHandler) serveUnlock(w http.ResponseWriter, r *http.Request, c callsCall) {
	var body struct {
		Key string `json:"key"`
	}
	if !h.decode(w, r, c, &body) {
		return
	}
	// A KEY THAT DOES NOT PARSE IS REFUSED UNREAD, and as the service refuses it. Saying so
	// tells nobody which keys exist, since the checksum is public, and it tells a person their
	// key is mistyped rather than wrong.
	if _, err := calls.ParseKey(body.Key); err != nil {
		h.refuse(w, c, calls.ErrInvalid, calls.JoinResult{})
		return
	}
	if !h.resolveSite(w, r, &c) {
		return
	}
	// As typed: tidying a key is ParseKey's, and the service parses it again.
	var sess calls.Session
	var err error
	if replacement, ok := h.svc.(interface {
		UnlockReplacing(context.Context, calls.SiteRef, string, string) (calls.Session, error)
	}); ok {
		sess, err = replacement.UnlockReplacing(r.Context(), c.site, body.Key, c.cookie.token)
	} else {
		sess, err = h.svc.Unlock(r.Context(), c.site, body.Key)
	}
	if err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	setCallsCookie(w, sess.Token)
	h.write(w, http.StatusOK, h.sessionReply(sess, calls.SeatCall{}))
}

// serveSession says what the cookie holds, and to a session seated in an open call what it
// is told about that call: whether it hosts it, when the host-left countdown ends while one
// runs, and to a host, who is in the call, who is knocking and whether it is locked (the
// roster). EVERY SEAT'S PAGE POLLS THIS while it is in the call (the file note), a host's
// about every three seconds, the queue's own pace, to see who waits to be let in: nothing
// else tells it, since a waiter is not in the media room.
//
// THE SERVICE DECIDES WHO IS A HOST, from the seat, so the call is asked of every seated
// session: a session's kind does not say, because a guest's session can hold a host seat by
// the host link, and a key holder's guest seat becomes a host's when the call is handed to
// it. A session seated nowhere has no call to tell it about, and costs no read of one.
func (h *callsHandler) serveSession(w http.ResponseWriter, r *http.Request, c callsCall) {
	if !h.requireSession(w, c) || !h.resolveSite(w, r, &c) {
		return
	}
	portrait, err := calls.PortraitQuery(r.URL.RawQuery)
	if err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	if portrait != "" {
		h.servePortrait(w, r, c, portrait)
		return
	}
	sess, err := h.svc.Session(r.Context(), c.site, c.cookie.token)
	if err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	var call calls.SeatCall
	if strings.TrimSpace(sess.SeatID) != "" {
		if call, err = h.svc.SessionCall(r.Context(), c.session()); err != nil {
			h.refuse(w, c, err, calls.JoinResult{})
			return
		}
	}
	reply := h.sessionReply(sess, call)
	if svc, ok := h.svc.(CallsFamilyService); ok {
		reply.Family, err = svc.FamilySession(r.Context(), c.site, c.cookie.token)
		if err != nil {
			h.refuse(w, c, err, calls.JoinResult{})
			return
		}
	}
	h.write(w, http.StatusOK, reply)
}

// callsConfirmBody is the three confirmations every seat records.
type callsConfirmBody struct {
	Participation     string `json:"participation,omitempty"`
	AuthorityAccepted bool   `json:"authorityAccepted,omitempty"`
	Adult             bool   `json:"adult"`
	NoMinors          bool   `json:"noMinors"`
	Terms             string `json:"terms"`
}

func (b callsConfirmBody) confirmation() calls.Confirmation {
	return calls.Confirmation{Adult: b.Adult, NoMinors: b.NoMinors, Terms: b.Terms, Participation: b.Participation, AuthorityAccepted: b.AuthorityAccepted}
}

func (h *callsHandler) serveStart(w http.ResponseWriter, r *http.Request, c callsCall) {
	var body struct {
		QuoteID        string           `json:"quoteId"`
		IdempotencyKey string           `json:"idempotencyKey"`
		Title          string           `json:"title"`
		Name           string           `json:"name"`
		Confirm        callsConfirmBody `json:"confirm"`
	}
	if !h.decode(w, r, c, &body) || !h.requireSession(w, c) || !h.resolveSite(w, r, &c) {
		return
	}
	// THE HOST'S SEAT IS THE SESSION THEY BROUGHT, so a start hands the browser nothing new.
	result, err := h.svc.Start(r.Context(), c.session(), calls.StartBody{QuoteID: body.QuoteID, IdempotencyKey: body.IdempotencyKey, Title: body.Title, Name: body.Name, Confirm: body.Confirm.confirmation()})
	if err != nil || result.Join.Outcome == calls.OutcomeRefuse {
		h.refuse(w, c, err, result.Join)
		return
	}
	setCallsRetryAfter(w, result.Join.RetryAfter)
	h.write(w, http.StatusOK, callsStartReply{
		Call: callsLinksReply{
			ID: memql.BareShortId(result.Call.ID), GuestToken: result.Call.GuestToken, HostToken: result.Call.HostToken,
		},
		Join: joinReply(result.Join),
	})
}

// callsBookingBody is a booking as schedule takes it: when it starts (RFC 3339), how many
// minutes it is booked for, and its title.
type callsBookingBody struct {
	QuoteID        string    `json:"quoteId"`
	IdempotencyKey string    `json:"idempotencyKey"`
	Start          time.Time `json:"start"`
	Minutes        int       `json:"minutes"`
	Title          string    `json:"title"`
}

// callsMoveBody is a reschedule: the call it moves, and the booking it moves to.
type callsMoveBody struct {
	ID string `json:"id"`
	callsBookingBody
}

// serveSchedule books a call or moves one. The two take the same booking, and only a move
// names a call: an id on a booking is a field the route does not take.
func (h *callsHandler) serveSchedule(w http.ResponseWriter, r *http.Request, c callsCall) {
	var move callsMoveBody
	var into any = &move.callsBookingBody
	if c.route.op == callsOpReschedule {
		into = &move
	}
	if !h.decode(w, r, c, into) || !h.requireSession(w, c) || !h.resolveSite(w, r, &c) {
		return
	}
	req := calls.ScheduleRequest{QuoteID: move.QuoteID, IdempotencyKey: move.IdempotencyKey, Start: move.Start, Minutes: move.Minutes, Title: move.Title}
	var booked calls.ScheduledCall
	var err error
	if c.route.op == callsOpSchedule {
		booked, err = h.svc.Schedule(r.Context(), c.session(), req)
	} else {
		booked, err = h.svc.Reschedule(r.Context(), c.session(), move.ID, req)
	}
	if err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	h.write(w, http.StatusOK, callsScheduledReply{
		CallConfiguration: booked.Configuration,
		ID:                memql.BareShortId(booked.ID),
		Start:             booked.Start.UTC().Format(time.RFC3339Nano), End: booked.End.UTC().Format(time.RFC3339Nano),
		Title:      booked.Title,
		GuestToken: booked.GuestToken, HostToken: booked.HostToken,
		GuestLink: booked.GuestLink, HostLink: booked.HostLink,
		ICS: string(booked.ICS),
	})
}

func (h *callsHandler) serveCancel(w http.ResponseWriter, r *http.Request, c callsCall) {
	var body struct {
		ID string `json:"id"`
	}
	if !h.decode(w, r, c, &body) || !h.requireSession(w, c) || !h.resolveSite(w, r, &c) {
		return
	}
	if err := h.svc.Cancel(r.Context(), c.session(), body.ID); err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveJoin seats a person by a link, or in the call the session they hold is seated in.
//
// THE CREDENTIAL IS A LINK OR THE COOKIE, and either of the right shape is worth the reads.
// With neither, the request is answered for its credential as the service answers one: a
// link that could not be one is a link that opens nothing, a cookie that could not be a
// session is no session, and a join that names neither names no call. (The service checks a
// joiner's name and confirmations first, so a request wrong in both ways is told about its
// credential here and would have been told about its name there; both are refusals.)
//
// BOTH LINKS GO THROUGH AS SENT. A host link's fragment carries both tokens and a page sends
// the host token alone; which one a join meant is the service's to decide, and it refuses a
// join that presents both.
func (h *callsHandler) serveJoin(w http.ResponseWriter, r *http.Request, c callsCall) {
	var body struct {
		FamilyInvitationID string           `json:"familyInvitationId"`
		Family             bool             `json:"family"`
		FamilyToken        string           `json:"familyToken"`
		Token              string           `json:"token"`
		HostToken          string           `json:"hostToken"`
		HistoryID          string           `json:"historyId"`
		Name               string           `json:"name"`
		Confirm            callsConfirmBody `json:"confirm"`
	}
	if !h.decode(w, r, c, &body) {
		return
	}
	if body.HistoryID != "" && !h.requireSession(w, c) {
		return
	}
	guest, host := strings.TrimSpace(body.Token), strings.TrimSpace(body.HostToken)
	if !calls.TokenWellFormed(guest) && !calls.TokenWellFormed(host) && !c.cookie.wellFormed {
		switch {
		case guest != "" || host != "":
			h.refuse(w, c, calls.ErrNotFound, calls.JoinResult{})
		case c.cookie.presented:
			h.refuse(w, c, calls.ErrNoSession, calls.JoinResult{})
		default:
			h.refuse(w, c, calls.ErrInvalid, calls.JoinResult{})
		}
		return
	}
	if !h.resolveSite(w, r, &c) {
		return
	}
	// A COOKIE THAT COULD NOT BE A SESSION IS NOT ONE, and is not handed on: the service
	// would read it as none, and a join by link makes the person a session of their own.
	var held *calls.Session
	if c.cookie.wellFormed {
		sess := c.session()
		held = &sess
	}
	result, out, err := h.svc.Join(r.Context(), c.site, held, calls.JoinBody{
		FamilyInvitationID: body.FamilyInvitationID, Family: body.Family, FamilyToken: body.FamilyToken, Token: body.Token, HostToken: body.HostToken, HistoryID: body.HistoryID, Name: body.Name, Confirm: body.Confirm.confirmation(),
	})
	// THE SESSION THE JOINER NOW HOLDS is handed over whenever it is not the one they
	// brought, whatever else the answer says: the service has written it, and the person's
	// next request should carry it rather than open another.
	handed := out.Token != "" && (held == nil || out.Token != held.Token)
	if handed {
		setCallsCookie(w, out.Token)
	}
	if err != nil || result.Outcome == calls.OutcomeRefuse {
		// A new session was just handed over, so nothing presented is to be taken back.
		if handed {
			c.cookie.presented = false
		}
		h.refuse(w, c, err, result)
		return
	}
	setCallsRetryAfter(w, result.RetryAfter)
	h.write(w, http.StatusOK, joinReply(result))
}

func (h *callsHandler) serveQueue(w http.ResponseWriter, r *http.Request, c callsCall) {
	if !h.requireSession(w, c) || !h.resolveSite(w, r, &c) {
		return
	}
	result, err := h.svc.Queue(r.Context(), c.session())
	if err != nil || result.Outcome == calls.OutcomeRefuse {
		h.refuse(w, c, err, result)
		return
	}
	setCallsRetryAfter(w, result.RetryAfter)
	h.write(w, http.StatusOK, joinReply(result))
}

// serveHost extends the existing closed host-control surface with invitation effects.
// Link recovery binds the current host session to the media identity on the requesting
// call screen. It returns only a guest capability; delivery still takes just a mailbox
// and that capability, and the service chooses the fixed template and transport.
func (h *callsHandler) serveHost(w http.ResponseWriter, r *http.Request, c callsCall) {
	var body struct {
		Spotlight    *calls.SpotlightRequest         `json:"spotlight"`
		Processing   *calls.ProcessingConsentRequest `json:"processing"`
		Action       string                          `json:"action"`
		RequestID    *string                         `json:"requestId"`
		ReviewDigest *string                         `json:"reviewDigest"`
		Seat         *string                         `json:"seat"`
		Email        *string                         `json:"email"`
		Token        *string                         `json:"token"`
		Identity     *string                         `json:"identity"`
		Title        *string                         `json:"title"`
	}
	if !h.decode(w, r, c, &body) {
		return
	}
	inviting := body.Action == "invite"
	recovering := body.Action == "invitation_link"
	renaming := body.Action == "rename"
	reviewing := body.Action == "agent_approve" || body.Action == "agent_reject"
	invalid := false
	switch {
	case body.Action == "spotlight":
		invalid = body.Spotlight == nil || body.RequestID != nil || body.ReviewDigest != nil || body.Title != nil || body.Seat != nil || body.Identity != nil || body.Email != nil || body.Token != nil
	case body.Action == "processing_consent":
		invalid = body.Processing == nil || body.RequestID != nil || body.ReviewDigest != nil || body.Title != nil || body.Seat != nil || body.Identity != nil || body.Email != nil || body.Token != nil
	case reviewing:
		invalid = body.RequestID == nil || body.ReviewDigest == nil || body.Title != nil || body.Seat != nil || body.Identity != nil || body.Email != nil || body.Token != nil
	case inviting:
		invalid = body.Title != nil || body.Seat != nil || body.Identity != nil || body.Email == nil || body.Token == nil
		if !invalid {
			invalid = !calls.TokenWellFormed(*body.Token)
		}
	case recovering:
		invalid = body.Title != nil || body.Seat != nil || body.Email != nil || body.Token != nil || body.Identity == nil
		if !invalid {
			invalid = !calls.InvitationIdentityWellFormed(*body.Identity)
		}
	case renaming:
		invalid = body.Title == nil || body.Seat != nil || body.Email != nil || body.Token != nil || body.Identity == nil
		if !invalid {
			invalid = !calls.InvitationIdentityWellFormed(*body.Identity)
		}
	default:
		invalid = body.Title != nil || body.Email != nil || body.Token != nil || body.Identity != nil
	}
	if body.Action != "processing_consent" && body.Processing != nil {
		invalid = true
	}
	if body.Action != "spotlight" && body.Spotlight != nil {
		invalid = true
	}
	if !reviewing && (body.RequestID != nil || body.ReviewDigest != nil) {
		invalid = true
	}
	if invalid {
		h.refuse(w, c, calls.ErrInvalid, calls.JoinResult{})
		return
	}
	if !h.requireSession(w, c) || !h.resolveSite(w, r, &c) {
		return
	}
	if renaming {
		svc, ok := h.svc.(interface {
			RenameCall(context.Context, calls.Session, string, string) (string, error)
		})
		if !ok {
			h.refuse(w, c, calls.ErrNotFound, calls.JoinResult{})
			return
		}
		title, err := svc.RenameCall(r.Context(), c.session(), *body.Title, *body.Identity)
		if err != nil {
			h.refuse(w, c, err, calls.JoinResult{})
			return
		}
		h.write(w, http.StatusOK, struct {
			Title string `json:"title"`
		}{title})
		return
	}
	if recovering {
		result, err := h.svc.InvitationLink(r.Context(), c.session(), calls.InvitationLinkRequest{Identity: *body.Identity})
		if err != nil {
			h.refuse(w, c, err, calls.JoinResult{})
			return
		}
		h.write(w, http.StatusOK, struct {
			GuestToken string `json:"guestToken"`
		}{result.GuestToken})
		return
	}
	if inviting {
		result, err := h.svc.Invite(r.Context(), c.session(), calls.InviteRequest{Email: *body.Email, Token: *body.Token})
		if err != nil {
			h.refuse(w, c, err, calls.JoinResult{})
			return
		}
		h.write(w, http.StatusOK, struct {
			Status    string `json:"status"`
			Duplicate bool   `json:"duplicate"`
		}{result.Status, result.Duplicate})
		return
	}
	seat := ""
	if body.Seat != nil {
		seat = *body.Seat
	}
	requestID, reviewDigest := "", ""
	if body.RequestID != nil {
		requestID = *body.RequestID
	}
	if body.ReviewDigest != nil {
		reviewDigest = *body.ReviewDigest
	}
	if err := h.svc.Host(r.Context(), c.session(), calls.HostActionRequest{Action: body.Action, Seat: seat, RequestID: requestID, ReviewDigest: reviewDigest, Processing: body.Processing, Spotlight: body.Spotlight}); err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveLeave is the hang-up: the session's seat leaves its call. The session itself stands
// (the file note), so the cookie stays.
func (h *callsHandler) serveLeave(w http.ResponseWriter, r *http.Request, c callsCall) {
	var body calls.LeaveRequest
	if !h.decode(w, r, c, &body) || !h.requireSession(w, c) || !h.resolveSite(w, r, &c) {
		return
	}
	var err error
	if body == (calls.LeaveRequest{}) {
		err = h.svc.Leave(r.Context(), c.session())
	} else if svc, ok := h.svc.(interface {
		LeaveWithOptions(context.Context, calls.Session, calls.LeaveRequest) error
	}); ok {
		err = svc.LeaveWithOptions(r.Context(), c.session(), body)
	} else {
		err = calls.ErrInvalid
	}
	if err != nil {
		h.refuse(w, c, err, calls.JoinResult{})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// the answers
// ---------------------------------------------------------------------------

// callsErrorReply is every refusal's body: what kind of refusal it is, the service's stable
// reason when it gave one, and when to ask again when it says.
type callsErrorReply struct {
	Error        string `json:"error"`
	Reason       string `json:"reason,omitempty"`
	RetryAfterMs int64  `json:"retryAfterMs,omitempty"`
}

// callsAnswer is how one refusal reads on the wire. known is false for a failure the service
// did not classify, which is logged.
type callsAnswer struct {
	status int
	body   callsErrorReply
	known  bool
}

// callsAnswerFor is THE ONE PLACE an answer becomes a status. The class of the error decides,
// in this order; a refusal's reason is the refusal's own (calls.ReasonOf) or, for an act that
// answers a join, the JoinResult's; and the join window is the one conflict that is a rate
// limit.
//
//   - 401 no_session: no session, or one that has ended. Never a reason: one answer.
//   - 404 not_found: no stamp, a link or a booking that opens nothing, a forged owner, a
//     switch that is off. Never a reason, so every miss is one answer, identical to the byte.
//   - 401 invalid_key, for an unlock only: a key is a credential traded for a session, so a
//     key that opens nothing is a failed credential and not a miss.
//   - 410 ended: call_ended, cancelled.
//   - 403 forbidden: removed, key_not_active, not_host, and a session that may not do this.
//   - 400 invalid: a body that could not be read, a missing confirmation, and in_past,
//     beyond_horizon, too_long, title_too_long, minutes_invalid.
//   - 409 conflict: room_full, agent_seats_full, in_another_call, key_in_call,
//     no_media_server, overlaps, too_many_upcoming, started, not_waiting, and not_yet with
//     the time until the call opens.
//   - 429 limited: rate, the room's join window, with the time until it opens again.
//   - 413 too_large, 415 not_json: the route's own refusals.
//   - 502 failed: anything else, logged by the caller.
//
// A REFUSAL THE SERVICE GAVE NO ERROR FOR IS STILL A REFUSAL, so a 200 never says refuse.
func callsAnswerFor(op string, err error, result calls.JoinResult) callsAnswer {
	if err == nil && result.Outcome == calls.OutcomeRefuse {
		err = calls.ErrConflict
	}
	reason := calls.ReasonOf(err)
	if reason == "" {
		reason = result.Reason
	}
	retryAfter := callsMs(result.RetryAfter)
	var invitationLimit *calls.InvitationLimit
	if errors.As(err, &invitationLimit) {
		retryAfter = callsMs(invitationLimit.RetryAfter)
	}
	switch {
	case errors.Is(err, errCallsNotHere):
		return callsAnswer{http.StatusNotFound, callsErrorReply{Error: "not_found"}, true}
	case errors.Is(err, errCallsTooLarge):
		return callsAnswer{http.StatusRequestEntityTooLarge, callsErrorReply{Error: "too_large"}, true}
	case errors.Is(err, errCallsNotJSON):
		return callsAnswer{http.StatusUnsupportedMediaType, callsErrorReply{Error: "not_json"}, true}
	case errors.Is(err, calls.ErrNoSession):
		return callsAnswer{http.StatusUnauthorized, callsErrorReply{Error: "no_session"}, true}
	case errors.Is(err, calls.ErrNotFound) && op == callsOpUnlock:
		return callsAnswer{http.StatusUnauthorized, callsErrorReply{Error: "invalid_key"}, true}
	case errors.Is(err, calls.ErrNotFound):
		return callsAnswer{http.StatusNotFound, callsErrorReply{Error: "not_found"}, true}
	case errors.Is(err, calls.ErrEnded):
		return callsAnswer{http.StatusGone, callsErrorReply{Error: "ended", Reason: reason}, true}
	case errors.Is(err, calls.ErrForbidden):
		return callsAnswer{http.StatusForbidden, callsErrorReply{Error: "forbidden", Reason: reason}, true}
	case errors.Is(err, calls.ErrInvitationUnavailable):
		return callsAnswer{http.StatusServiceUnavailable, callsErrorReply{Error: "unavailable"}, true}
	case errors.Is(err, calls.ErrInvalid):
		return callsAnswer{http.StatusBadRequest, callsErrorReply{Error: "invalid", Reason: reason}, true}
	case errors.Is(err, calls.ErrConflict) && reason == calls.ReasonRate:
		return callsAnswer{http.StatusTooManyRequests, callsErrorReply{Error: "limited", Reason: reason, RetryAfterMs: retryAfter}, true}
	case errors.Is(err, calls.ErrConflict):
		return callsAnswer{http.StatusConflict, callsErrorReply{Error: "conflict", Reason: reason, RetryAfterMs: retryAfter}, true}
	}
	return callsAnswer{http.StatusBadGateway, callsErrorReply{Error: "failed"}, false}
}

// refuse answers a refusal. A 401 takes back the cookie the request presented, since the
// service has said it opens nothing; a cookie the request did not present is left alone.
func (h *callsHandler) refuse(w http.ResponseWriter, c callsCall, err error, result calls.JoinResult) {
	answer := callsAnswerFor(c.route.op, err, result)
	if !answer.known {
		// THE ROUTE AND THE SITE, and what failed, which the CallsService contract keeps free
		// of secrets. Nothing the request carried.
		h.logger.Warn("calls surface: a request failed",
			"component", "server", "op", c.route.op, logger.Subject(callsSiteConcept, c.siteID()), "err", err)
	}
	if answer.status == http.StatusUnauthorized && answer.body.Error == "no_session" && c.cookie.presented && c.route.op != callsOpAgent && c.route.op != callsOpStripeWebhook && c.route.op != callsOpDiditWebhook {
		clearCallsCookie(w)
	}
	if answer.body.RetryAfterMs > 0 {
		setCallsRetryAfter(w, time.Duration(answer.body.RetryAfterMs)*time.Millisecond)
	}
	h.write(w, answer.status, answer.body)
}

func (h *callsHandler) write(w http.ResponseWriter, status int, body any) {
	// Strings, numbers and booleans, which do not fail to encode.
	payload, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// callsMs is a duration in whole milliseconds, ROUNDED UP, so a wait that is not over never
// reads as zero and a client never asks early.
func callsMs(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

// setCallsRetryAfter says when to ask again in the header too, in whole seconds rounded up,
// whenever an answer says it in its body: a wait, a place in line, not_yet and the join
// window. A client that polls at the server's Retry-After then never asks early.
func setCallsRetryAfter(w http.ResponseWriter, d time.Duration) {
	if d <= 0 {
		return
	}
	seconds := int64((d + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}

// callsSessionReply is what a session says about itself: its kind, what it holds, and when
// it stops opening anything. NEVER ITS TOKEN, which rides the HttpOnly cookie so a page's
// script never holds it.
//
// `expiresAt` AND `expiresInMs` ARE ONE INSTANT, stated twice. The remainder is the one a
// client schedules from, so a wrong client clock does not move the moment; it also tells a
// page the server's now (expiresAt less expiresInMs), which is what `countdownEndsAt` is
// counted down from and an occupying seat's elapsed call time is measured from.
type callsSessionReply struct {
	Spotlight         *calls.Spotlight         `json:"spotlight,omitempty"`
	Reports           calls.ReportCapabilities `json:"reports"`
	ParticipationMode string                   `json:"participationMode,omitempty"`
	Family            *calls.FamilySession     `json:"family,omitempty"`
	Identity          string                   `json:"identity,omitempty"`
	LeaveToken        string                   `json:"leaveToken,omitempty"`
	CountdownKind     string                   `json:"countdownKind,omitempty"`
	ParticipantLimit  int                      `json:"participantLimit,omitempty"`
	Insights          bool                     `json:"insights,omitempty"`
	Resources         bool                     `json:"resources,omitempty"`
	DurationMinutes   int                      `json:"durationMinutes,omitempty"`
	ReservedCents     *int64                   `json:"reservedCents,omitempty"`
	CostCents         *int64                   `json:"costCents,omitempty"`
	Kind              string                   `json:"kind"`
	KeyID             string                   `json:"keyId,omitempty"`
	SeatID            string                   `json:"seatId,omitempty"`
	RoomID            string                   `json:"roomId,omitempty"`
	ExpiresAt         string                   `json:"expiresAt"`
	ExpiresInMs       int64                    `json:"expiresInMs"`
	// Host is true while the session's seat hosts its open call (a host seat admitted or in
	// the call), and absent otherwise.
	Host bool `json:"host,omitempty"`
	// OpenedAt is the room's persisted start in RFC 3339, only for an admitted or
	// in-call seat of an open room. Rejoining never resets it.
	OpenedAt string `json:"openedAt,omitempty"`
	Title    string `json:"title,omitempty"`
	// CountdownEndsAt is the shared server deadline in RFC 3339. host_left is
	// cancelled by host return; wrap_up is firm even with a host present. Both
	// remain visible after expiry until reconciliation ends the room.
	CountdownEndsAt string `json:"countdownEndsAt,omitempty"`
	// AllowanceEndsAt is independent of host presence: returning to the room
	// cannot extend its admitted simulated pass budget.
	AllowanceEndsAt string `json:"allowanceEndsAt,omitempty"`
	FundingMode     string `json:"fundingMode,omitempty"`
	// Roster is there for a host seat in an open call and absent for every other session.
	Roster        *callsRosterReply        `json:"roster,omitempty"`
	Processing    *calls.ProcessingStatus  `json:"processing,omitempty"`
	PendingAgents []calls.PendingAgent     `json:"pendingAgents,omitempty"`
	Agents        []calls.AgentParticipant `json:"agents,omitempty"`
}

// callsRosterReply is a host's roster on the wire: whether the call is locked, the seat ids
// POST /host takes, bare, and an in-call seat's media identity, so a page matches the media
// server's participants to seats. `locked` is in every roster, false included, so a host's
// page offers lock or unlock from what the service read rather than from a missing field.
// NO SECRET IS IN IT: no token, no digest and no key id, whose presence would tell a page
// which seats could take the call over. Two lists, each a list when empty.
type callsRosterReply struct {
	Locked  bool                     `json:"locked"`
	InCall  []callsRosterSeatReply   `json:"inCall"`
	Waiting []callsRosterWaiterReply `json:"waiting"`
}

type callsRosterSeatReply struct {
	Seat        string `json:"seat"`
	DisplayName string `json:"displayName"`
	Kind        string `json:"kind"`
	Host        bool   `json:"host"`
	Status      string `json:"status"`
	Identity    string `json:"identity"`
}

type callsRosterWaiterReply struct {
	Seat        string `json:"seat"`
	DisplayName string `json:"displayName"`
	Kind        string `json:"kind"`
	Since       string `json:"since,omitempty"`
}

func (h *callsHandler) sessionReply(sess calls.Session, call calls.SeatCall) callsSessionReply {
	reply := callsSessionReply{
		Spotlight:         call.Spotlight,
		ParticipationMode: call.ParticipationMode, Family: call.Family,
		Identity:      call.Identity,
		LeaveToken:    call.LeaveToken,
		CountdownKind: call.CountdownKind,
		Kind:          sess.Kind,
		KeyID:         memql.BareShortId(sess.KeyID),
		SeatID:        memql.BareShortId(sess.SeatID),
		RoomID:        memql.BareShortId(sess.RoomID),
		ExpiresAt:     sess.ExpiresAt.UTC().Format(time.RFC3339Nano),
		ExpiresInMs:   callsMs(sess.ExpiresAt.Sub(h.now())),
		Host:          call.Host,
		Roster:        rosterReply(call.Roster),
		Agents:        call.Agents,
		PendingAgents: call.PendingAgents,
		Reports:       call.Reports,
		Processing:    call.Processing,
	}
	if !call.CountdownEndsAt.IsZero() {
		reply.CountdownEndsAt = call.CountdownEndsAt.UTC().Format(time.RFC3339Nano)
	}
	if !call.AllowanceEndsAt.IsZero() {
		reply.AllowanceEndsAt = call.AllowanceEndsAt.UTC().Format(time.RFC3339Nano)
		reply.FundingMode = call.FundingMode
		reply.ParticipantLimit, reply.DurationMinutes = call.ParticipantLimit, call.DurationMinutes
		reply.Insights, reply.Resources = call.Insights, call.Resources
		if call.Host {
			reply.ReservedCents, reply.CostCents = call.ReservedCents, call.CostCents
		}
	}
	reply.Title = call.Title
	if !call.OpenedAt.IsZero() {
		reply.OpenedAt = call.OpenedAt.UTC().Format(time.RFC3339Nano)
	}
	return reply
}

func rosterReply(roster *calls.Roster) *callsRosterReply {
	if roster == nil {
		return nil
	}
	out := &callsRosterReply{Locked: roster.Locked, InCall: []callsRosterSeatReply{}, Waiting: []callsRosterWaiterReply{}}
	for _, seat := range roster.InCall {
		out.InCall = append(out.InCall, callsRosterSeatReply{
			Seat: memql.BareShortId(seat.Seat), DisplayName: seat.DisplayName, Kind: seat.Kind,
			Host: seat.Host, Status: seat.Status, Identity: seat.Identity,
		})
	}
	for _, waiter := range roster.Waiting {
		since := ""
		if !waiter.Since.IsZero() {
			since = waiter.Since.UTC().Format(time.RFC3339Nano)
		}
		out.Waiting = append(out.Waiting, callsRosterWaiterReply{
			Seat: memql.BareShortId(waiter.Seat), DisplayName: waiter.DisplayName, Kind: waiter.Kind, Since: since,
		})
	}
	return out
}

// callsJoinReply is a JoinResult on the wire, its durations in milliseconds. An admission
// carries the media server's address, the pass and the seat's identity; a wait or a place in
// line, when to ask again and, in line, the place counted from one.
type callsJoinReply struct {
	Outcome      string `json:"outcome"`
	URL          string `json:"url,omitempty"`
	Pass         string `json:"pass,omitempty"`
	Identity     string `json:"identity,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Host         bool   `json:"host"`
	ExpiresInMs  int64  `json:"expiresInMs,omitempty"`
	RetryAfterMs int64  `json:"retryAfterMs,omitempty"`
	Position     int    `json:"position,omitempty"`
}

func joinReply(result calls.JoinResult) callsJoinReply {
	return callsJoinReply{
		Outcome: string(result.Outcome), URL: result.URL, Pass: result.Pass, Identity: result.Identity,
		Reason: result.Reason, Host: result.Host,
		ExpiresInMs: callsMs(result.ExpiresIn), RetryAfterMs: callsMs(result.RetryAfter), Position: result.Position,
	}
}

// callsStartReply is a started call: its id and its two links' secrets, handed out once, and
// what became of its host.
type callsStartReply struct {
	Call callsLinksReply `json:"call"`
	Join callsJoinReply  `json:"join"`
}

type callsLinksReply struct {
	ID         string `json:"id"`
	GuestToken string `json:"guestToken"`
	HostToken  string `json:"hostToken"`
}

// callsScheduledReply is a booked call as its scheduler is handed it, once: the secrets of
// its links, both links, and its calendar file as text.
type callsScheduledReply struct {
	*calls.CallConfiguration
	ID         string `json:"id"`
	Start      string `json:"start"`
	End        string `json:"end"`
	Title      string `json:"title"`
	GuestToken string `json:"guestToken"`
	HostToken  string `json:"hostToken"`
	GuestLink  string `json:"guestLink"`
	HostLink   string `json:"hostLink"`
	ICS        string `json:"ics"`
}

// setCallsCookie hands the browser its session, exactly as setAccessCookie does: a __Host-
// cookie, HttpOnly, Secure, SameSite=Strict, Path=/, and no Max-Age, because the server is
// the authority on when a session ends.
func setCallsCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: CallsCookieName, Value: token, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// clearCallsCookie takes it back, with the attributes it was given: a browser will not let a
// weaker cookie overwrite a Secure one, and it refuses a __Host- cookie that lacks them.
func clearCallsCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CallsCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}
