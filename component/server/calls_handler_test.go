package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// calls_handler_test.go -- the bff half of the calls surface, against a fake calls service
// and an injected clock.
//
// What the calls rows really answer -- who the site's tier admits, that a token's digest
// finds its session, that a join takes a seat -- is said by integrations/calls' own tests,
// and end to end through the edge by app's. These hold the handler to its own decisions:
// what it reads before it trusts anything, which site it hands the service, which status
// each answer gets, which cookie it sets, and what never reaches a log.

const (
	callsTestOwner      = "user-owner"
	callsTestMember     = "user-member"
	callsTestStranger   = "user-stranger"
	callsTestSite       = "site1"
	callsTestOtherSite  = "site2"
	callsTestDeployable = "meetings"
	callsTestKeyID      = "key1"
	callsTestRoomID     = "room1"
	callsTestSeatID     = "seat1"
	callsTestCallID     = "booked1"
)

// callsT0 is when every test starts.
var callsT0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

// callsSecrets are the bearer secrets a rig's service honours. They are minted at run time,
// so no secret-shaped literal sits in the source, and each test can ask that none of them
// reached a log.
type callsSecrets struct {
	key          string // a key of the site's, which unlocks a host session
	hostSession  string // the session that key opened
	guestSession string // the session a guest link's first join opens
	linkSession  string // the session a host link's first join opens
	guestLink    string // the call's guest link token
	hostLink     string // the call's host link token
	unheld       string // a token of the right shape that no session holds
	pass         string // the media pass an admission carries
}

func callsToken(t *testing.T, fill byte) string {
	t.Helper()
	token, err := calls.NewToken(bytes.NewReader(bytes.Repeat([]byte{fill}, calls.TokenBytes)), calls.TokenBytes)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func callsKey(fill byte) string {
	var raw [16]byte
	for i := range raw {
		raw[i] = fill + byte(i)
	}
	return calls.FormatKey(raw)
}

func newCallsSecrets(t *testing.T) callsSecrets {
	t.Helper()
	return callsSecrets{
		key:          callsKey(0x11),
		hostSession:  callsToken(t, 0x21),
		guestSession: callsToken(t, 0x22),
		linkSession:  callsToken(t, 0x23),
		guestLink:    callsToken(t, 0x31),
		hostLink:     callsToken(t, 0x32),
		unheld:       callsToken(t, 0x41),
		pass:         "pass." + callsToken(t, 0x51),
	}
}

// all is every secret, by what it is, for asserting that none of them was logged.
func (s callsSecrets) all() map[string]string {
	return map[string]string{
		"the key": s.key, "the host session": s.hostSession, "the guest session": s.guestSession,
		"the host link's session": s.linkSession, "the guest link": s.guestLink,
		"the host link": s.hostLink, "an unheld token": s.unheld, "the media pass": s.pass,
	}
}

// callsFakeCall is one call the handler made of the service, and what it was handed.
type callsFakeCall struct {
	op             string
	site           calls.SiteRef  // the site the call was handed, as an argument or on its session
	session        *calls.Session // the session it was handed, if any
	siteID         string         // SiteAsOwner's arguments
	owner          string
	key            string
	start          calls.StartBody
	schedule       calls.ScheduleRequest
	callID         string
	join           calls.JoinBody
	host           calls.HostActionRequest
	invite         calls.InviteRequest
	invitationLink calls.InvitationLinkRequest
}

// callsFakeRefusal is what a test makes one op answer instead of its default.
type callsFakeRefusal struct {
	err    error
	result calls.JoinResult
	// session is what Join answers as the session the joiner now holds, in place of the
	// one it was handed.
	session *calls.Session
}

// callsFakeService models what the real service and the site's tier answer, and records
// every call, so "nothing was read" and "that was handed the row's site" are assertions
// rather than inferences.
//
// THE SITE READ MODELS THE TIER: the row comes back to its owner and to a member of its
// organization (the account arm), and to nobody else, so a forged owner who is a member
// gets the row and only the handler's comparison stops him.
type callsFakeService struct {
	t       *testing.T
	secrets callsSecrets
	site    *CallsSite
	admits  map[string]bool
	// answersAnySite hands back the site's row whatever id was asked for, for the handler's
	// comparison of the row with the stamp.
	answersAnySite bool
	siteErr        error
	// sessions are the tokens the service honours, and what each one is.
	sessions map[string]calls.Session
	// rosters are the roster SessionCall answers, by session token: the call a host seat is
	// in, so a token with one is a host's. A token with none answers no roster and no host,
	// as the service answers a guest's seat, a host seat outside the call and an ended call.
	rosters map[string]*calls.Roster
	// countdowns are when the host-left countdown of the token's call ends, for a token
	// whose call is counting down.
	countdowns map[string]time.Time
	openedAt   map[string]time.Time
	refusals   map[string]callsFakeRefusal
	got        []callsFakeCall
}

func (f *callsFakeService) record(c callsFakeCall) {
	if c.session != nil {
		c.site = c.session.Site
	}
	f.got = append(f.got, c)
}

func (f *callsFakeService) ops() []string {
	out := []string{}
	for _, c := range f.got {
		out = append(out, c.op)
	}
	return out
}

func (f *callsFakeService) last() callsFakeCall {
	f.t.Helper()
	if len(f.got) == 0 {
		f.t.Fatal("the service was never called")
	}
	return f.got[len(f.got)-1]
}

func (f *callsFakeService) refusal(op string) (callsFakeRefusal, bool) {
	r, ok := f.refusals[op]
	return r, ok
}

// known is the session a token names, as the real service resolves it: by its token, on
// the deployable it was handed, and nowhere else.
func (f *callsFakeService) known(sess calls.Session) (calls.Session, error) {
	held, ok := f.sessions[sess.Token]
	if !ok || held.Site.SiteID != sess.Site.SiteID {
		return calls.Session{}, calls.ErrNoSession
	}
	held.Token, held.Site = sess.Token, sess.Site
	return held, nil
}

func (f *callsFakeService) SiteAsOwner(_ context.Context, siteID, owner string) (*CallsSite, error) {
	f.record(callsFakeCall{op: "SiteAsOwner", siteID: siteID, owner: owner})
	if f.siteErr != nil {
		return nil, f.siteErr
	}
	if f.site == nil || !f.admits[memql.BareShortId(owner)] {
		return nil, nil
	}
	if !f.answersAnySite && memql.BareShortId(siteID) != f.site.ID {
		return nil, nil
	}
	row := *f.site
	return &row, nil
}

func (f *callsFakeService) Unlock(_ context.Context, site calls.SiteRef, key string) (calls.Session, error) {
	f.record(callsFakeCall{op: "Unlock", site: site, key: key})
	if r, ok := f.refusal("Unlock"); ok {
		return calls.Session{}, r.err
	}
	if key != f.secrets.key {
		return calls.Session{}, calls.ErrNotFound
	}
	sess := calls.Session{Token: f.secrets.hostSession, Kind: calls.SessionHost, KeyID: callsTestKeyID,
		ExpiresAt: callsT0.Add(calls.SessionLifetime), Site: site}
	// One row per key: an unlock with the rig's key answers its session as a new one, and
	// leaves the row the rig seated alone, so the order tests run their routes in is not a
	// test of its own.
	if _, held := f.sessions[sess.Token]; !held {
		f.sessions[sess.Token] = sess
	}
	return sess, nil
}

func (f *callsFakeService) Session(_ context.Context, site calls.SiteRef, token string) (calls.Session, error) {
	f.record(callsFakeCall{op: "Session", site: site, session: &calls.Session{Token: token, Site: site}})
	if r, ok := f.refusal("Session"); ok {
		return calls.Session{}, r.err
	}
	return f.known(calls.Session{Token: token, Site: site})
}

func (f *callsFakeService) SessionCall(_ context.Context, sess calls.Session) (calls.SeatCall, error) {
	f.record(callsFakeCall{op: "SessionCall", session: &sess})
	if r, ok := f.refusal("SessionCall"); ok {
		return calls.SeatCall{}, r.err
	}
	if _, err := f.known(sess); err != nil {
		return calls.SeatCall{}, err
	}
	roster := f.rosters[sess.Token]
	return calls.SeatCall{Host: roster != nil, Roster: roster, CountdownEndsAt: f.countdowns[sess.Token], OpenedAt: f.openedAt[sess.Token]}, nil
}

func (f *callsFakeService) Start(_ context.Context, sess calls.Session, body calls.StartBody) (calls.StartResult, error) {
	f.record(callsFakeCall{op: "Start", session: &sess, start: body})
	if r, ok := f.refusal("Start"); ok {
		return calls.StartResult{Join: r.result}, r.err
	}
	if _, err := f.known(sess); err != nil {
		return calls.StartResult{}, err
	}
	return calls.StartResult{
		Call: calls.CallLinks{ID: callsTestRoomID, GuestToken: f.secrets.guestLink, HostToken: f.secrets.hostLink},
		Join: f.admission(true),
	}, nil
}

func (f *callsFakeService) admission(host bool) calls.JoinResult {
	return calls.JoinResult{Outcome: calls.OutcomeAdmit, URL: "wss://media.example.test", Pass: f.secrets.pass,
		Identity: "identity1", Host: host, ExpiresIn: 120 * time.Second}
}

func (f *callsFakeService) booking() calls.ScheduledCall {
	start := callsT0.Add(48 * time.Hour)
	return calls.ScheduledCall{
		ID: callsTestCallID, Start: start, End: start.Add(30 * time.Minute), Title: "Weekly review",
		GuestToken: f.secrets.guestLink, HostToken: f.secrets.hostLink,
		GuestLink: calls.GuestLink("meet.example.test", f.secrets.guestLink),
		HostLink:  calls.HostLink("meet.example.test", f.secrets.guestLink, f.secrets.hostLink),
		ICS:       []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"),
	}
}

func (f *callsFakeService) Schedule(_ context.Context, sess calls.Session, req calls.ScheduleRequest) (calls.ScheduledCall, error) {
	f.record(callsFakeCall{op: "Schedule", session: &sess, schedule: req})
	if r, ok := f.refusal("Schedule"); ok {
		return calls.ScheduledCall{}, r.err
	}
	if _, err := f.known(sess); err != nil {
		return calls.ScheduledCall{}, err
	}
	return f.booking(), nil
}

func (f *callsFakeService) Reschedule(_ context.Context, sess calls.Session, callID string, req calls.ScheduleRequest) (calls.ScheduledCall, error) {
	f.record(callsFakeCall{op: "Reschedule", session: &sess, callID: callID, schedule: req})
	if r, ok := f.refusal("Reschedule"); ok {
		return calls.ScheduledCall{}, r.err
	}
	if _, err := f.known(sess); err != nil {
		return calls.ScheduledCall{}, err
	}
	return f.booking(), nil
}

func (f *callsFakeService) Cancel(_ context.Context, sess calls.Session, callID string) error {
	f.record(callsFakeCall{op: "Cancel", session: &sess, callID: callID})
	if r, ok := f.refusal("Cancel"); ok {
		return r.err
	}
	_, err := f.known(sess)
	return err
}

// Join honours the call's two links. A joiner who brings a session it honours keeps it; one
// who brings none, or one it does not honour, is handed a new one of their own, as the real
// service does.
func (f *callsFakeService) Join(_ context.Context, site calls.SiteRef, sess *calls.Session, body calls.JoinBody) (calls.JoinResult, calls.Session, error) {
	var handed *calls.Session
	if sess != nil {
		copied := *sess
		handed = &copied
	}
	f.record(callsFakeCall{op: "Join", site: site, session: handed, join: body})
	f.got[len(f.got)-1].site = site
	if r, ok := f.refusal("Join"); ok {
		out := calls.Session{}
		if r.session != nil {
			out = *r.session
		} else if sess != nil {
			if held, err := f.known(*sess); err == nil {
				out = held
			}
		}
		return r.result, out, r.err
	}
	var fresh string
	host := false
	switch {
	case body.Token == f.secrets.guestLink && body.HostToken == "":
		fresh = f.secrets.guestSession
	case body.HostToken == f.secrets.hostLink && body.Token == "":
		fresh, host = f.secrets.linkSession, true
	default:
		return calls.JoinResult{}, calls.Session{}, calls.ErrNotFound
	}
	if sess != nil {
		if held, err := f.known(*sess); err == nil {
			held.SeatID, held.RoomID = callsTestSeatID, callsTestRoomID
			return f.admission(host), held, nil
		}
	}
	out := calls.Session{Token: fresh, Kind: calls.SessionGuest, SeatID: callsTestSeatID, RoomID: callsTestRoomID,
		ExpiresAt: callsT0.Add(calls.SessionLifetime), Site: site}
	if host {
		out.Kind = calls.SessionHost
	}
	f.sessions[fresh] = out
	return f.admission(host), out, nil
}

func (f *callsFakeService) Queue(_ context.Context, sess calls.Session) (calls.JoinResult, error) {
	f.record(callsFakeCall{op: "Queue", session: &sess})
	if r, ok := f.refusal("Queue"); ok {
		return r.result, r.err
	}
	if _, err := f.known(sess); err != nil {
		return calls.JoinResult{}, err
	}
	return calls.JoinResult{Outcome: calls.OutcomeQueue, Reason: "capacity", Position: 2, RetryAfter: 3 * time.Second}, nil
}

func (f *callsFakeService) Host(_ context.Context, sess calls.Session, req calls.HostActionRequest) error {
	f.record(callsFakeCall{op: "Host", session: &sess, host: req})
	if r, ok := f.refusal("Host"); ok {
		return r.err
	}
	_, err := f.known(sess)
	return err
}

func (f *callsFakeService) Invite(_ context.Context, sess calls.Session, req calls.InviteRequest) (calls.InviteResult, error) {
	f.record(callsFakeCall{op: "Invite", session: &sess, invite: req})
	if r, ok := f.refusal("Invite"); ok {
		return calls.InviteResult{}, r.err
	}
	if _, err := f.known(sess); err != nil {
		return calls.InviteResult{}, err
	}
	return calls.InviteResult{Status: "captured"}, nil
}

func (f *callsFakeService) InvitationLink(_ context.Context, sess calls.Session, req calls.InvitationLinkRequest) (calls.InvitationLinkResult, error) {
	f.record(callsFakeCall{op: "InvitationLink", session: &sess, invitationLink: req})
	if r, ok := f.refusal("InvitationLink"); ok {
		return calls.InvitationLinkResult{}, r.err
	}
	if _, err := f.known(sess); err != nil {
		return calls.InvitationLinkResult{}, err
	}
	return calls.InvitationLinkResult{GuestToken: f.secrets.guestLink}, nil
}

func (f *callsFakeService) Leave(_ context.Context, sess calls.Session) error {
	f.record(callsFakeCall{op: "Leave", session: &sess})
	if r, ok := f.refusal("Leave"); ok {
		return r.err
	}
	_, err := f.known(sess)
	return err
}

type callsFakeClock struct{ at time.Time }

func (c *callsFakeClock) now() time.Time { return c.at }

// callsRig is one test's handler and everything around it.
type callsRig struct {
	t       *testing.T
	h       http.Handler
	svc     *callsFakeService
	secrets callsSecrets
	clock   *callsFakeClock
	logs    *bytes.Buffer
	stamp   map[string]string
}

func newCallsRig(t *testing.T) *callsRig {
	t.Helper()
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	secrets := newCallsSecrets(t)
	svc := &callsFakeService{
		t: t, secrets: secrets,
		site:       &CallsSite{ID: callsTestSite, OwnerUserID: callsTestOwner, DeployableName: callsTestDeployable, Calls: true},
		admits:     map[string]bool{callsTestOwner: true, callsTestMember: true},
		sessions:   map[string]calls.Session{},
		rosters:    map[string]*calls.Roster{},
		countdowns: map[string]time.Time{},
		openedAt:   map[string]time.Time{},
		refusals:   map[string]callsFakeRefusal{},
	}
	// THE SITE'S KEY HOLDER IS ALREADY IN, so every route that takes a session has one to
	// take. unlock mints the same token again, which is the fake keeping one row per key.
	site := calls.SiteRef{SiteID: callsTestSite, OwnerUserID: callsTestOwner, Deployable: callsTestDeployable}
	svc.sessions[secrets.hostSession] = calls.Session{Kind: calls.SessionHost, KeyID: callsTestKeyID,
		SeatID: callsTestSeatID, RoomID: callsTestRoomID, ExpiresAt: callsT0.Add(calls.SessionLifetime), Site: site}
	clock := &callsFakeClock{at: callsT0}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &callsRig{
		t: t, h: NewCallsHandler(svc, logger, clock.now), svc: svc, secrets: secrets, clock: clock, logs: logs,
		stamp: map[string]string{CallsStampSiteHeader: callsTestSite, CallsStampOwnerHeader: callsTestOwner},
	}
}

// send is one request as the edge forwards it: the stamp, a JSON content type on a POST,
// and the session cookie when one is given.
func (r *callsRig) send(method, path, body, cookie string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range r.stamp {
		req.Header.Set(key, value)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: CallsCookieName, Value: cookie})
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r *callsRig) post(path, body, cookie string) *httptest.ResponseRecorder {
	r.t.Helper()
	return r.send(http.MethodPost, path, body, cookie)
}

func (r *callsRig) get(path, cookie string) *httptest.ResponseRecorder {
	r.t.Helper()
	return r.send(http.MethodGet, path, "", cookie)
}

// costOf is the service calls one act made, in order, and nothing from before it.
func (r *callsRig) costOf(act func()) []string {
	r.t.Helper()
	before := len(r.svc.got)
	act()
	out := []string{}
	for _, c := range r.svc.got[before:] {
		out = append(out, c.op)
	}
	return out
}

func callsJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

const (
	callsStartBody    = `{"name":"Ada","confirm":{"adult":true,"noMinors":true,"terms":"2026-11"}}`
	callsScheduleBody = `{"start":"2026-11-20T15:00:00Z","minutes":30,"title":"Weekly review"}`
)

func (r *callsRig) joinBody(token, hostToken string) string {
	r.t.Helper()
	return callsJSON(r.t, map[string]any{
		"token": token, "hostToken": hostToken, "name": "Grace",
		"confirm": map[string]any{"adult": true, "noMinors": true, "terms": "2026-11"},
	})
}

// callsEveryRoute is the ten routes, each sent as a person holding what it needs: a key for
// unlock, the guest link for a join, and the session cookie for everything else.
func callsEveryRoute(r *callsRig, cookie string) map[string]*httptest.ResponseRecorder {
	r.t.Helper()
	return map[string]*httptest.ResponseRecorder{
		"unlock":     r.post("/calls/unlock", callsJSON(r.t, map[string]string{"key": r.secrets.key}), ""),
		"session":    r.get("/calls/session", cookie),
		"start":      r.post("/calls/start", callsStartBody, cookie),
		"schedule":   r.post("/calls/schedule", callsScheduleBody, cookie),
		"reschedule": r.post("/calls/reschedule", `{"id":"`+callsTestCallID+`","start":"2026-11-21T15:00:00Z","minutes":45}`, cookie),
		"cancel":     r.post("/calls/cancel", `{"id":"`+callsTestCallID+`"}`, cookie),
		"join":       r.post("/calls/join", r.joinBody(r.secrets.guestLink, ""), cookie),
		"queue":      r.get("/calls/queue", cookie),
		"host":       r.post("/calls/host", `{"action":"mute","seat":"seat2"}`, cookie),
		"leave":      r.post("/calls/leave", `{}`, cookie),
	}
}

var callsCookiePattern = regexp.MustCompile(`^__Host-memql_calls=([a-z2-7]{32}); Path=/; HttpOnly; Secure; SameSite=Strict$`)

// callsCookieFrom is the session token the reply handed the browser, held to exactly the
// cookie a __Host- prefix requires and setAccessCookie sets.
func callsCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	got := rec.Header().Values("Set-Cookie")
	if len(got) != 1 {
		t.Fatalf("Set-Cookie = %q, want exactly one", got)
	}
	match := callsCookiePattern.FindStringSubmatch(got[0])
	if match == nil {
		t.Fatalf("Set-Cookie = %q, want __Host-memql_calls=<token>; Path=/; HttpOnly; Secure; SameSite=Strict", got[0])
	}
	return match[1]
}

func assertNoCallsCookie(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("Set-Cookie = %q, want none", got)
	}
}

func assertCallsCookieCleared(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	// EXACTLY: a browser overwrites only the cookie it was given, and refuses a __Host-
	// cookie with another Path, a Domain or no Secure, so a clear that was almost right
	// would leave the cookie where it is with nothing saying so.
	const want = "__Host-memql_calls=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Strict"
	if got := rec.Header().Values("Set-Cookie"); len(got) != 1 || got[0] != want {
		t.Fatalf("Set-Cookie = %q, want exactly [%s]", got, want)
	}
}

func assertCallsReply(t *testing.T, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if rec.Code != status || rec.Body.String() != body {
		t.Fatalf("reply = %d %q, want %d %q", rec.Code, rec.Body.String(), status, body)
	}
	if got := rec.Header().Get("Content-Type"); body != "" && !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q on a JSON reply", got)
	}
}

const callsNotFound = `{"error":"not_found"}`

// ---------------------------------------------------------------------------
// the stamp
// ---------------------------------------------------------------------------

func TestUnstampedCallsRequestIs404(t *testing.T) {
	for name, stamp := range map[string]map[string]string{
		"no stamp":      nil,
		"no owner":      {CallsStampSiteHeader: callsTestSite},
		"no site":       {CallsStampOwnerHeader: callsTestOwner},
		"a blank owner": {CallsStampSiteHeader: callsTestSite, CallsStampOwnerHeader: "   "},
		"a blank site":  {CallsStampSiteHeader: "   ", CallsStampOwnerHeader: callsTestOwner},
		// THE ACCESS SURFACE'S STAMP IS NOT THIS ONE. The edge sets each surface's on its own
		// routes, so a request carrying the other's arrived some way the edge did not send it.
		"the access surface's stamp": {AccessStampSiteHeader: callsTestSite, AccessStampOwnerHeader: callsTestOwner},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newCallsRig(t)
			rig.stamp = stamp
			for op, rec := range callsEveryRoute(rig, rig.secrets.hostSession) {
				t.Run(op, func(t *testing.T) {
					assertCallsReply(t, rec, http.StatusNotFound, callsNotFound)
					assertNoCallsCookie(t, rec)
				})
			}
			if len(rig.svc.got) != 0 {
				t.Fatalf("an unstamped request reached the service: %v", rig.svc.ops())
			}
		})
	}
}

// callsOnlyReadTheSite is what a request whose stamp does not check out may cost: the read
// of the site that turned it away, and nothing the service does with a site.
func callsOnlyReadTheSite(t *testing.T, cost []string) {
	t.Helper()
	for _, op := range cost {
		if op != "SiteAsOwner" {
			t.Fatalf("a stamp that does not check out went on to %s: %v", op, cost)
		}
	}
}

func TestForgedCallsOwnerReadsNothing(t *testing.T) {
	// THE SITE IS READ UNDER THE STAMPED OWNER, so a stranger is a read that finds nothing.
	t.Run("a stranger reads nothing past the read that turns him away", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.stamp[CallsStampOwnerHeader] = callsTestStranger
		var replies map[string]*httptest.ResponseRecorder
		cost := rig.costOf(func() { replies = callsEveryRoute(rig, rig.secrets.hostSession) })
		for op, rec := range replies {
			t.Run(op, func(t *testing.T) {
				assertCallsReply(t, rec, http.StatusNotFound, callsNotFound)
				assertNoCallsCookie(t, rec)
			})
		}
		callsOnlyReadTheSite(t, cost)
		for _, c := range rig.svc.got {
			if c.owner != callsTestStranger {
				t.Fatalf("the site was read as %q: the read under the stamped owner IS the check", c.owner)
			}
		}
	})

	// A MEMBER OF THE SITE'S ORGANIZATION READS THE ROW, through the tier's account arm, so
	// "the read returned a row" says nothing: the row's own owner has to be the one stamped.
	t.Run("a member of the site's organization reads the row and is refused anyway", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.stamp[CallsStampOwnerHeader] = callsTestMember
		var replies map[string]*httptest.ResponseRecorder
		cost := rig.costOf(func() { replies = callsEveryRoute(rig, rig.secrets.hostSession) })
		for op, rec := range replies {
			t.Run(op, func(t *testing.T) {
				assertCallsReply(t, rec, http.StatusNotFound, callsNotFound)
				assertNoCallsCookie(t, rec)
			})
		}
		callsOnlyReadTheSite(t, cost)
	})

	t.Run("a stamp naming another site reads nothing of it", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.stamp[CallsStampSiteHeader] = callsTestOtherSite
		cost := rig.costOf(func() {
			assertCallsReply(t, rig.get("/calls/session", rig.secrets.hostSession), http.StatusNotFound, callsNotFound)
		})
		callsOnlyReadTheSite(t, cost)
	})

	// THE ROW IS HELD TO THE STAMP, not only the read to its argument: the site the service
	// is handed is this row's, so a row that is not the stamped site would move every row
	// the service reads after it.
	t.Run("a read that answers with another site's row is refused", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.answersAnySite = true
		rig.stamp[CallsStampSiteHeader] = callsTestOtherSite
		var replies map[string]*httptest.ResponseRecorder
		cost := rig.costOf(func() { replies = callsEveryRoute(rig, rig.secrets.hostSession) })
		for op, rec := range replies {
			t.Run(op, func(t *testing.T) { assertCallsReply(t, rec, http.StatusNotFound, callsNotFound) })
		}
		callsOnlyReadTheSite(t, cost)
	})

	// THE SWITCH IS READ HERE TOO, although the edge reads it: the edge's copy of the site
	// can be older than the row, and the service trusts the site it is handed.
	t.Run("a site whose calls switch is off is the one 404", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.site.Calls = false
		var replies map[string]*httptest.ResponseRecorder
		cost := rig.costOf(func() { replies = callsEveryRoute(rig, rig.secrets.hostSession) })
		for op, rec := range replies {
			t.Run(op, func(t *testing.T) {
				assertCallsReply(t, rec, http.StatusNotFound, callsNotFound)
				assertNoCallsCookie(t, rec)
			})
		}
		callsOnlyReadTheSite(t, cost)
	})

	t.Run("a row with no owner refuses every stamp", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.site.OwnerUserID = ""
		cost := rig.costOf(func() {
			assertCallsReply(t, rig.get("/calls/session", rig.secrets.hostSession), http.StatusNotFound, callsNotFound)
		})
		callsOnlyReadTheSite(t, cost)
	})

	// The edge stamps the owner as the row spells it, and neither spelling is what makes a
	// stamp good or bad.
	t.Run("a canonical spelling of the right site and owner is the right site and owner", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.stamp[CallsStampSiteHeader] = "v1:platform:site:" + callsTestSite
		rig.stamp[CallsStampOwnerHeader] = "v1:identity:user:" + callsTestOwner
		for op, rec := range callsEveryRoute(rig, rig.secrets.hostSession) {
			if rec.Code >= 300 {
				t.Errorf("%s answered %d: %s", op, rec.Code, rec.Body.String())
			}
		}
	})
}

// A STAMP THAT DOES NOT CHECK OUT is the one 404, and an operator has to be able to see why,
// because the reply never will. Nothing the request carried goes in the line, and neither
// does the owner the stamp named.
func TestACallsStampThatDoesNotCheckOutIsLoggedAndStillTheOne404(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(*callsRig)
		reason  string
	}{
		"the stamped owner reads no row": {
			arrange: func(r *callsRig) { r.stamp[CallsStampOwnerHeader] = callsTestStranger },
			reason:  "the stamped owner reads no row of the stamped site",
		},
		"the stamped owner is not the site's owner": {
			arrange: func(r *callsRig) { r.stamp[CallsStampOwnerHeader] = callsTestMember },
			reason:  "the stamped owner is not this site's owner",
		},
		"the read answers another site": {
			arrange: func(r *callsRig) {
				r.svc.answersAnySite = true
				r.stamp[CallsStampSiteHeader] = callsTestOtherSite
			},
			reason: "the read answered a site other than the stamped one",
		},
		"the switch is off": {
			arrange: func(r *callsRig) { r.svc.site.Calls = false },
			reason:  "the site's calls switch is off",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newCallsRig(t)
			tc.arrange(rig)
			assertCallsReply(t, rig.get("/calls/session", rig.secrets.hostSession), http.StatusNotFound, callsNotFound)
			logged := rig.logs.String()
			if got := strings.Count(logged, "level=WARN"); got != 1 {
				t.Fatalf("logged %d WARN lines for one request:\n%s", got, logged)
			}
			if !strings.Contains(logged, tc.reason) || !strings.Contains(logged, "subject.concept=v1:platform:site") {
				t.Fatalf("the line does not say %q about the site:\n%s", tc.reason, logged)
			}
			for what, secret := range rig.secrets.all() {
				if strings.Contains(logged, secret) {
					t.Fatalf("the line holds %s:\n%s", what, logged)
				}
			}
			for _, owner := range []string{callsTestStranger, callsTestMember} {
				if strings.Contains(logged, owner) {
					t.Fatalf("the line names the stamped owner %q:\n%s", owner, logged)
				}
			}
		})
	}

	t.Run("a request with no stamp says nothing", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.stamp = nil
		assertCallsReply(t, rig.get("/calls/session", rig.secrets.hostSession), http.StatusNotFound, callsNotFound)
		if logged := rig.logs.String(); logged != "" {
			t.Fatalf("logged:\n%s", logged)
		}
	})
}

// A FAILED READ OF THE SITE IS NOT A MISS. It opens nothing and clears nothing, so a
// database blip costs a retry and takes nobody out of a call.
func TestAFailedSiteReadOpensNothingAndClearsNothing(t *testing.T) {
	rig := newCallsRig(t)
	rig.svc.siteErr = errors.New("the site read failed")
	var replies map[string]*httptest.ResponseRecorder
	cost := rig.costOf(func() { replies = callsEveryRoute(rig, rig.secrets.hostSession) })
	for op, rec := range replies {
		t.Run(op, func(t *testing.T) {
			assertCallsReply(t, rec, http.StatusBadGateway, `{"error":"failed"}`)
			assertNoCallsCookie(t, rec)
		})
	}
	callsOnlyReadTheSite(t, cost)
}

// ---------------------------------------------------------------------------
// what a request costs
// ---------------------------------------------------------------------------

// A STRANGER COSTS NO CALLS READS. Anybody can send these, so what a request with no
// credential of the right shape costs is the floor of what the cluster can be made to
// spend: nothing is read until it carries a session cookie, a key or a link token that
// could have been issued. Each answer is the one the service would give, without the
// reads.
func TestAStrangerCostsNoCallsReads(t *testing.T) {
	type request struct {
		method, path, body, cookie string
	}
	malformed := "not-a-token"
	for name, tc := range map[string]struct {
		send   func(*callsRig) request
		status int
		body   string
		clears bool
	}{
		"a status check with no cookie": {
			send:   func(r *callsRig) request { return request{http.MethodGet, "/calls/session", "", ""} },
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`,
		},
		"a status check with a cookie no session could have": {
			send:   func(r *callsRig) request { return request{http.MethodGet, "/calls/session", "", malformed} },
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`, clears: true,
		},
		"a start with no cookie": {
			send:   func(r *callsRig) request { return request{http.MethodPost, "/calls/start", callsStartBody, ""} },
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`,
		},
		"a schedule with no cookie": {
			send:   func(r *callsRig) request { return request{http.MethodPost, "/calls/schedule", callsScheduleBody, ""} },
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`,
		},
		"a reschedule with no cookie": {
			send: func(r *callsRig) request {
				return request{http.MethodPost, "/calls/reschedule", `{"id":"booked1","start":"2026-11-21T15:00:00Z","minutes":45}`, ""}
			},
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`,
		},
		"a cancel with no cookie": {
			send:   func(r *callsRig) request { return request{http.MethodPost, "/calls/cancel", `{"id":"booked1"}`, ""} },
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`,
		},
		"a poll with no cookie": {
			send:   func(r *callsRig) request { return request{http.MethodGet, "/calls/queue", "", ""} },
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`,
		},
		"a poll with a cookie no session could have": {
			send:   func(r *callsRig) request { return request{http.MethodGet, "/calls/queue", "", malformed} },
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`, clears: true,
		},
		"a host control with no cookie": {
			send:   func(r *callsRig) request { return request{http.MethodPost, "/calls/host", `{"action":"lock"}`, ""} },
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`,
		},
		"a leave with no cookie": {
			send:   func(r *callsRig) request { return request{http.MethodPost, "/calls/leave", `{}`, ""} },
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`,
		},
		"an unlock naming no key": {
			send:   func(r *callsRig) request { return request{http.MethodPost, "/calls/unlock", `{}`, ""} },
			status: http.StatusBadRequest, body: `{"error":"invalid"}`,
		},
		"an unlock with what could not be a key": {
			send:   func(r *callsRig) request { return request{http.MethodPost, "/calls/unlock", `{"key":"let-me-in"}`, ""} },
			status: http.StatusBadRequest, body: `{"error":"invalid"}`,
		},
		// A TYPO IS CAUGHT BY THE CHECKSUM, which is public: saying so tells nobody which keys
		// exist, and it tells a person their key is mistyped rather than wrong.
		"an unlock with a key one symbol off": {
			send: func(r *callsRig) request {
				key := []byte(r.secrets.key)
				key[len(key)-5] ^= 1
				return request{http.MethodPost, "/calls/unlock", callsJSON(r.t, map[string]string{"key": string(key)}), ""}
			},
			status: http.StatusBadRequest, body: `{"error":"invalid"}`,
		},
		"a join naming no call": {
			send: func(r *callsRig) request {
				return request{http.MethodPost, "/calls/join", r.joinBody("", ""), ""}
			},
			status: http.StatusBadRequest, body: `{"error":"invalid"}`,
		},
		"a join with a guest link no call could have": {
			send: func(r *callsRig) request {
				return request{http.MethodPost, "/calls/join", r.joinBody(malformed, ""), ""}
			},
			status: http.StatusNotFound, body: callsNotFound,
		},
		"a join with a host link no call could have": {
			send: func(r *callsRig) request {
				return request{http.MethodPost, "/calls/join", r.joinBody("", malformed), ""}
			},
			status: http.StatusNotFound, body: callsNotFound,
		},
		"a join by a cookie no session could have": {
			send: func(r *callsRig) request {
				return request{http.MethodPost, "/calls/join", r.joinBody("", ""), malformed}
			},
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`, clears: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newCallsRig(t)
			req := tc.send(rig)
			rec := rig.send(req.method, req.path, req.body, req.cookie)
			assertCallsReply(t, rec, tc.status, tc.body)
			if tc.clears {
				assertCallsCookieCleared(t, rec)
			} else {
				assertNoCallsCookie(t, rec)
			}
			if len(rig.svc.got) != 0 {
				t.Fatalf("a stranger reached the service: %v", rig.svc.ops())
			}
		})
	}
}

// A BODY IS REFUSED BEFORE ANYTHING IS READ, and the caps are enforced while reading: the
// edge caps too, and this does not lean on it.
func TestACallsBodyIsRefusedBeforeAnythingIsRead(t *testing.T) {
	big := func(n int) string { return `{"title":"` + strings.Repeat("a", n) + `"}` }
	for name, tc := range map[string]struct {
		path, contentType, body string
		status                  int
		reply                   string
	}{
		"not JSON":             {"/calls/start", "text/plain", callsStartBody, http.StatusUnsupportedMediaType, `{"error":"not_json"}`},
		"a form post":          {"/calls/join", "application/x-www-form-urlencoded", "token=x", http.StatusUnsupportedMediaType, `{"error":"not_json"}`},
		"no content type":      {"/calls/leave", "", `{}`, http.StatusUnsupportedMediaType, `{"error":"not_json"}`},
		"an empty body":        {"/calls/leave", "application/json", "", http.StatusBadRequest, `{"error":"invalid"}`},
		"not an object":        {"/calls/start", "application/json", `["Ada"]`, http.StatusBadRequest, `{"error":"invalid"}`},
		"null":                 {"/calls/leave", "application/json", `null`, http.StatusBadRequest, `{"error":"invalid"}`},
		"a string":             {"/calls/leave", "application/json", `"{}"`, http.StatusBadRequest, `{"error":"invalid"}`},
		"two values":           {"/calls/leave", "application/json", `{}{}`, http.StatusBadRequest, `{"error":"invalid"}`},
		"a field nobody reads": {"/calls/start", "application/json", `{"name":"Ada","admin":true}`, http.StatusBadRequest, `{"error":"invalid"}`},
		"a field of the wrong type": {"/calls/schedule", "application/json", `{"start":"2026-11-20T15:00:00Z","minutes":"thirty"}`,
			http.StatusBadRequest, `{"error":"invalid"}`},
		"a start over the small cap":    {"/calls/start", "application/json", big(int(memql.CallsSmallBodyBytes)), http.StatusRequestEntityTooLarge, `{"error":"too_large"}`},
		"a join over the small cap":     {"/calls/join", "application/json", big(int(memql.CallsSmallBodyBytes)), http.StatusRequestEntityTooLarge, `{"error":"too_large"}`},
		"a schedule over its cap":       {"/calls/schedule", "application/json", big(int(memql.CallsScheduleBodyBytes)), http.StatusRequestEntityTooLarge, `{"error":"too_large"}`},
		"a reschedule over its cap":     {"/calls/reschedule", "application/json", big(int(memql.CallsScheduleBodyBytes)), http.StatusRequestEntityTooLarge, `{"error":"too_large"}`},
		"padding past the cap after it": {"/calls/leave", "application/json", `{}` + strings.Repeat(" ", int(memql.CallsSmallBodyBytes)), http.StatusRequestEntityTooLarge, `{"error":"too_large"}`},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newCallsRig(t)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			for key, value := range rig.stamp {
				req.Header.Set(key, value)
			}
			req.AddCookie(&http.Cookie{Name: CallsCookieName, Value: rig.secrets.hostSession})
			rec := httptest.NewRecorder()
			rig.h.ServeHTTP(rec, req)
			assertCallsReply(t, rec, tc.status, tc.reply)
			assertNoCallsCookie(t, rec)
			if len(rig.svc.got) != 0 {
				t.Fatalf("a refused body reached the service: %v", rig.svc.ops())
			}
		})
	}

	// THE SCHEDULE CAP IS THE LARGER ONE, and a booking's title is what needs it.
	t.Run("a schedule between the two caps is read", func(t *testing.T) {
		rig := newCallsRig(t)
		title := strings.Repeat("a", int(memql.CallsSmallBodyBytes))
		body := callsJSON(t, map[string]any{"start": "2026-11-20T15:00:00Z", "minutes": 30, "title": title})
		rec := rig.post("/calls/schedule", body, rig.secrets.hostSession)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if got := rig.svc.last().schedule.Title; got != title {
			t.Fatalf("the title reached the service as %d characters", len(got))
		}
	})

	t.Run("a JSON content type with a charset is JSON", func(t *testing.T) {
		rig := newCallsRig(t)
		req := httptest.NewRequest(http.MethodPost, "/calls/leave", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		for key, value := range rig.stamp {
			req.Header.Set(key, value)
		}
		req.AddCookie(&http.Cookie{Name: CallsCookieName, Value: rig.secrets.hostSession})
		rec := httptest.NewRecorder()
		rig.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// A CREDENTIAL OF THE RIGHT SHAPE PAYS FOR THE SITE, and then the service does the rest:
// one read of the site, then the one call the route makes. A session is not resolved first,
// because the service reads the row its token names on every call it is handed. A status
// check of a session seated in a call asks for the call's roster as well, which the service
// answers only to a host seat in it.
func TestWhatACallsRequestCosts(t *testing.T) {
	rig := newCallsRig(t)
	want := map[string][]string{
		"unlock": {"SiteAsOwner", "Unlock"}, "session": {"SiteAsOwner", "Session", "SessionCall"},
		"start": {"SiteAsOwner", "Start"}, "schedule": {"SiteAsOwner", "Schedule"},
		"reschedule": {"SiteAsOwner", "Reschedule"}, "cancel": {"SiteAsOwner", "Cancel"},
		"join": {"SiteAsOwner", "Join"}, "queue": {"SiteAsOwner", "Queue"},
		"host": {"SiteAsOwner", "Host"}, "leave": {"SiteAsOwner", "Leave"},
	}
	for op, send := range map[string]func() *httptest.ResponseRecorder{
		"unlock": func() *httptest.ResponseRecorder {
			return rig.post("/calls/unlock", callsJSON(t, map[string]string{"key": rig.secrets.key}), "")
		},
		"session": func() *httptest.ResponseRecorder { return rig.get("/calls/session", rig.secrets.hostSession) },
		"start": func() *httptest.ResponseRecorder {
			return rig.post("/calls/start", callsStartBody, rig.secrets.hostSession)
		},
		"schedule": func() *httptest.ResponseRecorder {
			return rig.post("/calls/schedule", callsScheduleBody, rig.secrets.hostSession)
		},
		"reschedule": func() *httptest.ResponseRecorder {
			return rig.post("/calls/reschedule", `{"id":"booked1","start":"2026-11-21T15:00:00Z","minutes":45}`, rig.secrets.hostSession)
		},
		"cancel": func() *httptest.ResponseRecorder {
			return rig.post("/calls/cancel", `{"id":"booked1"}`, rig.secrets.hostSession)
		},
		"join": func() *httptest.ResponseRecorder {
			return rig.post("/calls/join", rig.joinBody(rig.secrets.guestLink, ""), "")
		},
		"queue": func() *httptest.ResponseRecorder { return rig.get("/calls/queue", rig.secrets.hostSession) },
		"host": func() *httptest.ResponseRecorder {
			return rig.post("/calls/host", `{"action":"mute","seat":"seat2"}`, rig.secrets.hostSession)
		},
		"leave": func() *httptest.ResponseRecorder { return rig.post("/calls/leave", `{}`, rig.secrets.hostSession) },
	} {
		var rec *httptest.ResponseRecorder
		cost := rig.costOf(func() { rec = send() })
		if rec.Code >= 300 {
			t.Errorf("%s answered %d: %s", op, rec.Code, rec.Body.String())
		}
		if !reflect.DeepEqual(cost, want[op]) {
			t.Errorf("%s cost %v, want %v", op, cost, want[op])
		}
	}
}

// ---------------------------------------------------------------------------
// the routes
// ---------------------------------------------------------------------------

func TestOnlyTheTenCallsRoutesAnswer(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/calls/unlock"},
		{http.MethodPost, "/calls/session"},
		{http.MethodGet, "/calls/start"},
		{http.MethodGet, "/calls/schedule"},
		{http.MethodPut, "/calls/reschedule"},
		{http.MethodDelete, "/calls/cancel"},
		{http.MethodGet, "/calls/join"},
		{http.MethodPost, "/calls/queue"},
		{http.MethodGet, "/calls/host"},
		{http.MethodGet, "/calls/leave"},
		{http.MethodHead, "/calls/session"},
		{http.MethodOptions, "/calls/join"},
		{http.MethodPatch, "/calls/host"},
		{http.MethodPost, "/calls/"},
		{http.MethodGet, "/calls/"},
		{http.MethodPost, "/calls"},
		{http.MethodPost, "/calls/join/"},
		{http.MethodPost, "/calls/join/deeper"},
		{http.MethodPost, "/calls/end"},
		{http.MethodPost, "/callsx/join"},
		{http.MethodPost, "/access/unlock"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rig := newCallsRig(t)
			rec := rig.send(tc.method, tc.path, "{}", rig.secrets.hostSession)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d", rec.Code)
			}
			if tc.method != http.MethodHead && rec.Body.String() != callsNotFound {
				t.Fatalf("body = %q", rec.Body.String())
			}
			if len(rig.svc.got) != 0 {
				t.Fatalf("service calls = %v", rig.svc.ops())
			}
		})
	}
}

// A NODE WITH NO CALLS SERVICE HAS NO CALLS SURFACE, and says so the way it says there is
// nothing at any other path: the route still exists, so the request does not fall through
// to a protocol error, and the one 404 is what a stranger and a holder both get.
func TestNoCallsServiceIsTheOne404(t *testing.T) {
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	h := NewCallsHandler(nil, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), nil)
	req := httptest.NewRequest(http.MethodGet, "/calls/session", nil)
	req.Header.Set(CallsStampSiteHeader, callsTestSite)
	req.Header.Set(CallsStampOwnerHeader, callsTestOwner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertCallsReply(t, rec, http.StatusNotFound, callsNotFound)
}

// ---------------------------------------------------------------------------
// cookies
// ---------------------------------------------------------------------------

func TestJoinSetsTheSeatCookieAndUnlockSetsTheHostCookie(t *testing.T) {
	t.Run("unlock hands the browser the host session", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.post("/calls/unlock", callsJSON(t, map[string]string{"key": rig.secrets.key}), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if got := callsCookieFrom(t, rec); got != rig.secrets.hostSession {
			t.Fatal("the cookie is not the session the key opened")
		}
		if strings.Contains(rec.Body.String(), rig.secrets.hostSession) {
			t.Fatal("the session token is in the body, where a page's script can read it")
		}
	})

	t.Run("a guest's first join hands the browser the seat's session", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.post("/calls/join", rig.joinBody(rig.secrets.guestLink, ""), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if got := callsCookieFrom(t, rec); got != rig.secrets.guestSession {
			t.Fatal("the cookie is not the session the join opened")
		}
		if rig.svc.last().session != nil {
			t.Fatal("a join with no cookie was handed a session")
		}
	})

	t.Run("a host link's first join hands the browser its session", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.post("/calls/join", rig.joinBody("", rig.secrets.hostLink), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if got := callsCookieFrom(t, rec); got != rig.secrets.linkSession {
			t.Fatal("the cookie is not the session the host link opened")
		}
	})

	// A REJOIN KEEPS ITS SEAT, and its session with it, so there is nothing to hand over.
	t.Run("a join that keeps the session it brought sets nothing", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.post("/calls/join", rig.joinBody(rig.secrets.guestLink, ""), rig.secrets.hostSession)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		assertNoCallsCookie(t, rec)
		if got := rig.svc.last().session; got == nil || got.Token != rig.secrets.hostSession {
			t.Fatalf("the join was handed %+v, want the session its cookie names", got)
		}
	})

	// A SESSION THE SERVICE NO LONGER HONOURS is no session at all: a link's join makes a new
	// one, and the browser is handed it in place of the dead one.
	t.Run("a join that replaces the session it brought hands over the new one", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.post("/calls/join", rig.joinBody(rig.secrets.guestLink, ""), rig.secrets.unheld)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if got := callsCookieFrom(t, rec); got != rig.secrets.guestSession {
			t.Fatal("the cookie is not the session the join opened")
		}
	})

	// A WAITER'S POLLS CARRY THE SEAT'S SESSION, so a join that waits or queues hands it over
	// exactly as one that admits does: without it the person's next poll has no session.
	t.Run("a join that queues hands the browser the seat's session", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.refusals["Join"] = callsFakeRefusal{
			result:  calls.JoinResult{Outcome: calls.OutcomeQueue, Reason: "capacity", Position: 4, RetryAfter: 3 * time.Second},
			session: &calls.Session{Token: rig.secrets.guestSession, Kind: calls.SessionGuest},
		}
		rec := rig.post("/calls/join", rig.joinBody(rig.secrets.guestLink, ""), "")
		assertCallsReply(t, rec, http.StatusOK, `{"outcome":"queue","reason":"capacity","host":false,"retryAfterMs":3000,"position":4}`)
		if got := callsCookieFrom(t, rec); got != rig.secrets.guestSession {
			t.Fatal("the cookie is not the seat's session")
		}
	})

	// A NEW SESSION IS NOT TAKEN BACK IN THE SAME REPLY: had the answer that came with it said
	// there was no session, a clear after the set would leave the browser with neither.
	t.Run("a refusal that came with a new session hands it over and clears nothing", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.refusals["Join"] = callsFakeRefusal{
			err:     calls.ErrNoSession,
			session: &calls.Session{Token: rig.secrets.guestSession, Kind: calls.SessionGuest},
		}
		rec := rig.post("/calls/join", rig.joinBody(rig.secrets.guestLink, ""), rig.secrets.unheld)
		assertCallsReply(t, rec, http.StatusUnauthorized, `{"error":"no_session"}`)
		if got := callsCookieFrom(t, rec); got != rig.secrets.guestSession {
			t.Fatal("the cookie is not the new session")
		}
	})

	t.Run("a start seats the session it brought and sets nothing", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.post("/calls/start", callsStartBody, rig.secrets.hostSession)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		assertNoCallsCookie(t, rec)
	})

	// LEAVING A CALL IS NOT ENDING THE SESSION: a key holder starts the next call on it, and
	// a guest who hung up rejoins on the same seat.
	t.Run("a leave keeps the cookie", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.post("/calls/leave", `{}`, rig.secrets.hostSession)
		assertCallsReply(t, rec, http.StatusNoContent, "")
		assertNoCallsCookie(t, rec)
	})

	// AN ENDED SESSION IS TAKEN BACK, but only one the request presented: no cookie on a
	// request does not mean the browser holds none, since under SameSite=Strict a cross-site
	// navigation arrives without it.
	t.Run("a session the service says has ended is cleared", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.get("/calls/session", rig.secrets.unheld)
		assertCallsReply(t, rec, http.StatusUnauthorized, `{"error":"no_session"}`)
		assertCallsCookieCleared(t, rec)
	})

	t.Run("a failed unlock leaves the cookie the browser holds", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.post("/calls/unlock", callsJSON(t, map[string]string{"key": callsKey(0x61)}), rig.secrets.hostSession)
		assertCallsReply(t, rec, http.StatusUnauthorized, `{"error":"invalid_key"}`)
		assertNoCallsCookie(t, rec)
	})
}

// ---------------------------------------------------------------------------
// the roster a host polls
// ---------------------------------------------------------------------------

// callsRosterReplyBody is a session reply's roster as a client reads it. Locked is a pointer
// so a roster that leaves it out reads as missing, not as unlocked.
type callsRosterReplyBody struct {
	Roster *struct {
		Locked *bool `json:"locked"`
		InCall []struct {
			Seat, DisplayName, Kind, Status, Identity string
			Host                                      bool
		} `json:"inCall"`
		Waiting []struct {
			Seat, DisplayName, Kind, Since string
		} `json:"waiting"`
	} `json:"roster"`
}

func callsRosterFrom(t *testing.T, rec *httptest.ResponseRecorder) callsRosterReplyBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body callsRosterReplyBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not an object: %v: %s", err, rec.Body.String())
	}
	return body
}

// THE HOST'S CLIENT POLLS ITS SESSION TO SEE WHO IS KNOCKING, and the roster is what names
// the seats its controls act on: the ids it lists are the ids POST /host takes.
func TestAHostsSessionCarriesTheCallsRoster(t *testing.T) {
	hostSeat := calls.RosterSeat{Seat: callsTestSeatID, DisplayName: "Ada", Kind: "person", Host: true, Status: "in_call", Identity: "identity1"}
	guestSeat := calls.RosterSeat{Seat: "seat2", DisplayName: "Grace", Kind: "person", Status: "admitted", Identity: "identity2"}
	waiter := calls.RosterWaiter{Seat: "seat3", DisplayName: "Lin", Kind: "person", Since: callsT0.Add(-30 * time.Second)}

	t.Run("a host's session reply lists a waiting seat behind the lock, and POST /host admit with that seat id lets it in", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.rosters[rig.secrets.hostSession] = &calls.Roster{InCall: []calls.RosterSeat{hostSeat}, Waiting: []calls.RosterWaiter{waiter}}
		roster := callsRosterFrom(t, rig.get("/calls/session", rig.secrets.hostSession)).Roster
		if roster == nil || len(roster.Waiting) != 1 || roster.Waiting[0].Seat != "seat3" {
			t.Fatalf("roster = %+v, want the waiting seat", roster)
		}
		rec := rig.post("/calls/host", callsJSON(t, map[string]string{"action": calls.HostAdmit, "seat": roster.Waiting[0].Seat}), rig.secrets.hostSession)
		assertCallsReply(t, rec, http.StatusNoContent, "")
		if got, want := rig.svc.last().host, (calls.HostActionRequest{Action: calls.HostAdmit, Seat: "seat3"}); got != want {
			t.Fatalf("host control = %+v, want %+v", got, want)
		}
	})

	t.Run("it lists an in-call seat that the host can then mute and remove by that id", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.rosters[rig.secrets.hostSession] = &calls.Roster{InCall: []calls.RosterSeat{hostSeat, guestSeat}}
		roster := callsRosterFrom(t, rig.get("/calls/session", rig.secrets.hostSession)).Roster
		if roster == nil || len(roster.InCall) != 2 || roster.InCall[1].Seat != "seat2" || roster.InCall[1].Identity != "identity2" {
			t.Fatalf("roster = %+v, want the guest in the call with its media identity", roster)
		}
		for _, action := range []string{calls.HostMute, calls.HostRemove} {
			rec := rig.post("/calls/host", callsJSON(t, map[string]string{"action": action, "seat": roster.InCall[1].Seat}), rig.secrets.hostSession)
			assertCallsReply(t, rec, http.StatusNoContent, "")
			if got, want := rig.svc.last().host, (calls.HostActionRequest{Action: action, Seat: "seat2"}); got != want {
				t.Fatalf("host control = %+v, want %+v", got, want)
			}
		}
	})

	// THE SERVICE DECIDES WHO IS A HOST, from the seat and never the session's kind: a
	// guest's session can hold a host seat by the host link, and a key's can hold a guest's.
	// So the handler asks, and a roster it is not given is a field it does not write.
	t.Run("a guest's session reply has no roster", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.rosters[rig.secrets.hostSession] = &calls.Roster{InCall: []calls.RosterSeat{hostSeat, guestSeat}}
		rig.svc.sessions[rig.secrets.guestSession] = calls.Session{Kind: calls.SessionGuest, SeatID: "seat2", RoomID: callsTestRoomID,
			ExpiresAt: callsT0.Add(calls.SessionLifetime), Site: calls.SiteRef{SiteID: callsTestSite}}
		rec := rig.get("/calls/session", rig.secrets.guestSession)
		assertCallsReply(t, rec, http.StatusOK,
			`{"kind":"guest","seatId":"seat2","roomId":"room1","expiresAt":"2026-11-19T12:00:00Z","expiresInMs":86400000}`)
	})

	t.Run("an ended call's host session has no roster", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.get("/calls/session", rig.secrets.hostSession)
		assertCallsReply(t, rec, http.StatusOK,
			`{"kind":"host","keyId":"key1","seatId":"seat1","roomId":"room1","expiresAt":"2026-11-19T12:00:00Z","expiresInMs":86400000}`)
		if got := rig.svc.ops(); !reflect.DeepEqual(got, []string{"SiteAsOwner", "Session", "SessionCall"}) {
			t.Fatalf("calls = %v: the service was not asked, so the handler decided who is a host", got)
		}
	})

	// The Go names in lowerCamel, ids bare as every wire has them, a time in RFC 3339, and
	// an empty list a list. Nothing in it is a secret: no token, no digest, no key id.
	t.Run("the roster on the wire", func(t *testing.T) {
		rig := newCallsRig(t)
		canonical := guestSeat
		canonical.Seat = "v1:calls:seat:seat2"
		rig.svc.rosters[rig.secrets.hostSession] = &calls.Roster{InCall: []calls.RosterSeat{hostSeat, canonical}}
		rec := rig.get("/calls/session", rig.secrets.hostSession)
		assertCallsReply(t, rec, http.StatusOK,
			`{"kind":"host","keyId":"key1","seatId":"seat1","roomId":"room1","expiresAt":"2026-11-19T12:00:00Z","expiresInMs":86400000,`+
				`"host":true,"roster":{"locked":false,"inCall":[`+
				`{"seat":"seat1","displayName":"Ada","kind":"person","host":true,"status":"in_call","identity":"identity1"},`+
				`{"seat":"seat2","displayName":"Grace","kind":"person","host":false,"status":"admitted","identity":"identity2"}],`+
				`"waiting":[]}}`)

		rig.svc.rosters[rig.secrets.hostSession] = &calls.Roster{InCall: []calls.RosterSeat{hostSeat}, Waiting: []calls.RosterWaiter{waiter}}
		rec = rig.get("/calls/session", rig.secrets.hostSession)
		if !strings.Contains(rec.Body.String(), `"waiting":[{"seat":"seat3","displayName":"Lin","kind":"person","since":"2026-11-18T11:59:30Z"}]`) {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})

	// A HOST IS OFFERED LOCK OR UNLOCK, NOT BOTH: the roster says which the call is, as the
	// service read it, and says it in every roster, unlocked included, so a page never
	// guesses from a missing field.
	t.Run("it says whether the call is locked", func(t *testing.T) {
		rig := newCallsRig(t)
		for _, locked := range []bool{true, false} {
			rig.svc.rosters[rig.secrets.hostSession] = &calls.Roster{Locked: locked, InCall: []calls.RosterSeat{hostSeat}, Waiting: []calls.RosterWaiter{waiter}}
			roster := callsRosterFrom(t, rig.get("/calls/session", rig.secrets.hostSession)).Roster
			if roster == nil || roster.Locked == nil || *roster.Locked != locked {
				t.Fatalf("roster = %+v, want locked %v", roster, locked)
			}
		}
	})

	// A SESSION SEATED NOWHERE HAS NO CALL TO LIST, which the session itself says, so its
	// status check costs no roster read.
	t.Run("a session seated nowhere is not asked for a roster", func(t *testing.T) {
		rig := newCallsRig(t)
		unseated := rig.secrets.linkSession
		rig.svc.sessions[unseated] = calls.Session{Kind: calls.SessionHost, KeyID: callsTestKeyID,
			ExpiresAt: callsT0.Add(calls.SessionLifetime), Site: calls.SiteRef{SiteID: callsTestSite}}
		var rec *httptest.ResponseRecorder
		cost := rig.costOf(func() { rec = rig.get("/calls/session", unseated) })
		assertCallsReply(t, rec, http.StatusOK, `{"kind":"host","keyId":"key1","expiresAt":"2026-11-19T12:00:00Z","expiresInMs":86400000}`)
		if want := []string{"SiteAsOwner", "Session"}; !reflect.DeepEqual(cost, want) {
			t.Fatalf("cost = %v, want %v", cost, want)
		}
	})

	t.Run("a roster that could not be read is answered as any failure", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.refusals["SessionCall"] = callsFakeRefusal{err: calls.ErrNoSession}
		rec := rig.get("/calls/session", rig.secrets.hostSession)
		assertCallsReply(t, rec, http.StatusUnauthorized, `{"error":"no_session"}`)
		assertCallsCookieCleared(t, rec)

		rig.svc.refusals["SessionCall"] = callsFakeRefusal{err: errors.New("calls: read the room's seats: the engine did not complete the statement")}
		rec = rig.get("/calls/session", rig.secrets.hostSession)
		assertCallsReply(t, rec, http.StatusBadGateway, `{"error":"failed"}`)
		assertNoCallsCookie(t, rec)
	})
}

// EVERY SEAT'S SESSION SAYS WHETHER IT HOSTS ITS CALL AND WHEN THE HOST-LEFT COUNTDOWN ENDS
// (design section 8, "End": a countdown on screen). The service decides both from the
// rows (integrations/calls host.go SessionCall); the reply carries `host` only when it is
// true and `countdownEndsAt` only while a countdown runs, in RFC 3339 on the service's
// clock, which a page counts down from against the reply's own expiresAt and expiresInMs.
func TestASeatsSessionSaysWhetherItHostsAndWhenTheCountdownEnds(t *testing.T) {
	guest := func(rig *callsRig) string {
		rig.svc.sessions[rig.secrets.guestSession] = calls.Session{Kind: calls.SessionGuest, SeatID: "seat2", RoomID: callsTestRoomID,
			ExpiresAt: callsT0.Add(calls.SessionLifetime), Site: calls.SiteRef{SiteID: callsTestSite}}
		return rig.secrets.guestSession
	}

	t.Run("a guest whose host left is told when the call ends, and is not told it hosts", func(t *testing.T) {
		rig := newCallsRig(t)
		token := guest(rig)
		rig.svc.countdowns[token] = callsT0.Add(4*time.Minute + 30*time.Second)
		assertCallsReply(t, rig.get("/calls/session", token), http.StatusOK,
			`{"kind":"guest","seatId":"seat2","roomId":"room1","expiresAt":"2026-11-19T12:00:00Z","expiresInMs":86400000,`+
				`"countdownEndsAt":"2026-11-18T12:04:30Z"}`)
	})

	t.Run("a host's session says it hosts, beside its roster, and no countdown", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.rosters[rig.secrets.hostSession] = &calls.Roster{InCall: []calls.RosterSeat{{Seat: callsTestSeatID, Host: true, Status: "in_call"}}}
		var body map[string]json.RawMessage
		rec := rig.get("/calls/session", rig.secrets.hostSession)
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("not an object: %v: %s", err, rec.Body.String())
		}
		if string(body["host"]) != "true" || body["roster"] == nil {
			t.Fatalf("a host's session reply is %s, want host true beside the roster", rec.Body.String())
		}
		if _, counting := body["countdownEndsAt"]; counting {
			t.Fatalf("a host's session reply carries a countdown: %s", rec.Body.String())
		}
	})

	t.Run("a seat with neither is told neither", func(t *testing.T) {
		rig := newCallsRig(t)
		token := guest(rig)
		rec := rig.get("/calls/session", token)
		for _, field := range []string{`"host"`, `"countdownEndsAt"`} {
			if strings.Contains(rec.Body.String(), field) {
				t.Fatalf("a guest's session with no countdown carries %s: %s", field, rec.Body.String())
			}
		}
	})
}

// ---------------------------------------------------------------------------
// what the service is handed
// ---------------------------------------------------------------------------

// THE SITE THE SERVICE IS HANDED IS THE ROW'S, never the stamp's or anything a request
// sent: the service trusts the SiteRef it is given, and reads and writes every calls row of
// the site as that owner.
func TestTheCallsServiceIsHandedTheRowsSite(t *testing.T) {
	rig := newCallsRig(t)
	rig.stamp[CallsStampSiteHeader] = "v1:platform:site:" + callsTestSite
	rig.stamp[CallsStampOwnerHeader] = "v1:identity:user:" + callsTestOwner
	callsEveryRoute(rig, rig.secrets.hostSession)

	want := calls.SiteRef{SiteID: callsTestSite, OwnerUserID: callsTestOwner, Deployable: callsTestDeployable}
	served := 0
	for _, c := range rig.svc.got {
		if c.op == "SiteAsOwner" {
			// Bare, both of them, whatever the stamp's spelling: the read takes ids.
			if c.siteID != callsTestSite || c.owner != callsTestOwner {
				t.Errorf("the site was read as (%q, %q)", c.siteID, c.owner)
			}
			continue
		}
		served++
		if c.site != want {
			t.Errorf("%s was handed the site %+v, want the row's %+v", c.op, c.site, want)
		}
		if c.op != "Unlock" && c.op != "Join" && (c.session == nil || c.session.Token != rig.secrets.hostSession) {
			t.Errorf("%s was handed the session %+v, want the token the cookie carried", c.op, c.session)
		}
	}
	// Ten routes, and the roster the status check of a seated session asks for as well.
	if served != 11 {
		t.Fatalf("the service served %d calls for the ten routes and the roster: %v", served, rig.svc.ops())
	}
}

// WHAT A REQUEST SAYS REACHES THE SERVICE AS IT WAS SENT, in the service's own types, and
// the names on the wire are the Go names in lowerCamel.
func TestCallsRequestsReachTheServiceAsSent(t *testing.T) {
	confirm := calls.Confirmation{Adult: true, NoMinors: true, Terms: "2026-11"}

	t.Run("unlock", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.post("/calls/unlock", callsJSON(t, map[string]string{"key": strings.ToUpper(rig.secrets.key)}), "")
		// As typed: tidying a key is ParseKey's, and the service parses it again.
		if got := rig.svc.last().key; got != strings.ToUpper(rig.secrets.key) {
			t.Fatalf("the key reached the service as %q", got)
		}
	})

	t.Run("start", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.post("/calls/start", callsStartBody, rig.secrets.hostSession)
		if got, want := rig.svc.last().start, (calls.StartBody{Name: "Ada", Confirm: confirm}); got != want {
			t.Fatalf("start = %+v, want %+v", got, want)
		}
	})

	t.Run("schedule", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.post("/calls/schedule", callsScheduleBody, rig.secrets.hostSession)
		want := calls.ScheduleRequest{Start: time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC), Minutes: 30, Title: "Weekly review"}
		if got := rig.svc.last().schedule; !got.Start.Equal(want.Start) || got.Minutes != want.Minutes || got.Title != want.Title {
			t.Fatalf("schedule = %+v, want %+v", got, want)
		}
	})

	t.Run("reschedule names the call", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.post("/calls/reschedule", `{"id":"booked1","start":"2026-11-21T09:30:00-05:00","minutes":45}`, rig.secrets.hostSession)
		got := rig.svc.last()
		if got.callID != callsTestCallID || got.schedule.Minutes != 45 || got.schedule.Title != "" ||
			!got.schedule.Start.Equal(time.Date(2026, 11, 21, 14, 30, 0, 0, time.UTC)) {
			t.Fatalf("reschedule = %q %+v", got.callID, got.schedule)
		}
	})

	t.Run("cancel names the call", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.post("/calls/cancel", `{"id":"booked1"}`, rig.secrets.hostSession)
		if got := rig.svc.last().callID; got != callsTestCallID {
			t.Fatalf("cancel named %q", got)
		}
	})

	t.Run("join", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.post("/calls/join", rig.joinBody(rig.secrets.guestLink, ""), "")
		want := calls.JoinBody{Token: rig.secrets.guestLink, Name: "Grace", Confirm: confirm}
		if got := rig.svc.last().join; got != want {
			t.Fatalf("join = %+v, want %+v", got, want)
		}
	})

	// BOTH LINKS GO THROUGH, and the service refuses them: a host link's fragment carries
	// both tokens, a page sends the host token alone, and which of the two a join meant is
	// the service's to say.
	t.Run("a join carrying both links is the service's to refuse", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.refusals["Join"] = callsFakeRefusal{err: calls.ErrInvalid}
		rec := rig.post("/calls/join", rig.joinBody(rig.secrets.guestLink, rig.secrets.hostLink), "")
		assertCallsReply(t, rec, http.StatusBadRequest, `{"error":"invalid"}`)
		if got := rig.svc.last().join; got.Token != rig.secrets.guestLink || got.HostToken != rig.secrets.hostLink {
			t.Fatalf("join = %+v, want both links as sent", got)
		}
	})

	t.Run("host", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.post("/calls/host", `{"action":"remove","seat":"seat2"}`, rig.secrets.hostSession)
		if got, want := rig.svc.last().host, (calls.HostActionRequest{Action: calls.HostRemove, Seat: "seat2"}); got != want {
			t.Fatalf("host = %+v, want %+v", got, want)
		}
	})

	t.Run("a confirmation left out reaches the service as not given", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.refusals["Start"] = callsFakeRefusal{err: calls.ErrInvalid}
		rec := rig.post("/calls/start", `{"name":"Ada","confirm":{"adult":true,"terms":"2026-11"}}`, rig.secrets.hostSession)
		assertCallsReply(t, rec, http.StatusBadRequest, `{"error":"invalid"}`)
		if got := rig.svc.last().start.Confirm; got.NoMinors {
			t.Fatalf("confirm = %+v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// the replies
// ---------------------------------------------------------------------------

func TestCallsRepliesSpeakLowerCamel(t *testing.T) {
	t.Run("the session", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.clock.at = callsT0.Add(time.Hour)
		rec := rig.get("/calls/session", rig.secrets.hostSession)
		// expiresAt and expiresInMs are one instant, stated twice: the remainder is the one a
		// client schedules from, so a wrong client clock does not move the moment.
		assertCallsReply(t, rec, http.StatusOK,
			`{"kind":"host","keyId":"key1","seatId":"seat1","roomId":"room1","expiresAt":"2026-11-19T12:00:00Z","expiresInMs":82800000}`)
	})

	t.Run("an unlock answers the session it opened", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.post("/calls/unlock", callsJSON(t, map[string]string{"key": rig.secrets.key}), "")
		assertCallsReply(t, rec, http.StatusOK,
			`{"kind":"host","keyId":"key1","expiresAt":"2026-11-19T12:00:00Z","expiresInMs":86400000}`)
	})

	t.Run("an admission", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.post("/calls/join", rig.joinBody(rig.secrets.guestLink, ""), "")
		assertCallsReply(t, rec, http.StatusOK, `{"outcome":"admit","url":"wss://media.example.test","pass":"`+rig.secrets.pass+
			`","identity":"identity1","host":false,"expiresInMs":120000}`)
		if got := rec.Header().Get("Retry-After"); got != "" {
			t.Fatalf("Retry-After = %q on an admission", got)
		}
	})

	// A REPLY THAT SAYS WHEN TO ASK AGAIN SAYS IT IN THE HEADER TOO, in whole seconds rounded
	// up, so a client that polls at the server's Retry-After never asks early.
	t.Run("a place in the queue", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.get("/calls/queue", rig.secrets.hostSession)
		assertCallsReply(t, rec, http.StatusOK, `{"outcome":"queue","reason":"capacity","host":false,"retryAfterMs":3000,"position":2}`)
		if got := rec.Header().Get("Retry-After"); got != "3" {
			t.Fatalf("Retry-After = %q", got)
		}
	})

	t.Run("a wait behind the lock", func(t *testing.T) {
		rig := newCallsRig(t)
		rig.svc.refusals["Queue"] = callsFakeRefusal{result: calls.JoinResult{Outcome: calls.OutcomeWait, Reason: "locked", RetryAfter: 2500 * time.Millisecond}}
		rec := rig.get("/calls/queue", rig.secrets.hostSession)
		assertCallsReply(t, rec, http.StatusOK, `{"outcome":"wait","reason":"locked","host":false,"retryAfterMs":2500}`)
		if got := rec.Header().Get("Retry-After"); got != "3" {
			t.Fatalf("Retry-After = %q", got)
		}
	})

	t.Run("a started call", func(t *testing.T) {
		rig := newCallsRig(t)
		rec := rig.post("/calls/start", callsStartBody, rig.secrets.hostSession)
		assertCallsReply(t, rec, http.StatusOK, `{"call":{"id":"room1","guestToken":"`+rig.secrets.guestLink+
			`","hostToken":"`+rig.secrets.hostLink+`"},"join":{"outcome":"admit","url":"wss://media.example.test","pass":"`+
			rig.secrets.pass+`","identity":"identity1","host":true,"expiresInMs":120000}}`)
	})

	t.Run("a booked call, with its calendar file as a string", func(t *testing.T) {
		rig := newCallsRig(t)
		for _, path := range []string{"/calls/schedule", "/calls/reschedule"} {
			body := callsScheduleBody
			if path == "/calls/reschedule" {
				body = `{"id":"booked1","start":"2026-11-21T15:00:00Z","minutes":45}`
			}
			rec := rig.post(path, body, rig.secrets.hostSession)
			s := rig.secrets
			assertCallsReply(t, rec, http.StatusOK, `{"id":"booked1","start":"2026-11-20T12:00:00Z","end":"2026-11-20T12:30:00Z",`+
				`"title":"Weekly review","guestToken":"`+s.guestLink+`","hostToken":"`+s.hostLink+`",`+
				`"guestLink":"https://meet.example.test/#j=`+s.guestLink+`","hostLink":"https://meet.example.test/#j=`+s.guestLink+`\u0026h=`+s.hostLink+`",`+
				`"ics":"BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"}`)
		}
	})

	t.Run("a cancel, a host control and a leave say nothing", func(t *testing.T) {
		rig := newCallsRig(t)
		for _, rec := range []*httptest.ResponseRecorder{
			rig.post("/calls/cancel", `{"id":"booked1"}`, rig.secrets.hostSession),
			rig.post("/calls/host", `{"action":"lock"}`, rig.secrets.hostSession),
			rig.post("/calls/leave", `{}`, rig.secrets.hostSession),
		} {
			assertCallsReply(t, rec, http.StatusNoContent, "")
		}
	})
}

// EVERY ANSWER THE SERVICE GIVES HAS ONE STATUS, decided in one place: the class of its
// error first, then, for a conflict, whether it is the join window. A refusal's reason rides
// beside it, from the refusal or from the join's answer, and a 404 is the one 404 whatever
// the service said, because every miss is one answer.
func TestCallsErrorsMapToTheirStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		op, route  string
		refusal    callsFakeRefusal
		status     int
		body       string
		retryAfter string
		clears     bool
	}{
		"no session": {
			op: "Start", route: "start", refusal: callsFakeRefusal{err: calls.ErrNoSession},
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`, clears: true,
		},
		"a session that ended under a poll": {
			op: "Queue", route: "queue", refusal: callsFakeRefusal{err: fmtWrap(calls.ErrNoSession, "the session ended")},
			status: http.StatusUnauthorized, body: `{"error":"no_session"}`, clears: true,
		},
		"an unknown link": {
			op: "Join", route: "join", refusal: callsFakeRefusal{err: calls.ErrNotFound},
			status: http.StatusNotFound, body: callsNotFound,
		},
		"an unknown booking": {
			op: "Cancel", route: "cancel", refusal: callsFakeRefusal{err: fmtWrap(calls.ErrNotFound, "no call of that id is booked here")},
			status: http.StatusNotFound, body: callsNotFound,
		},
		// AN UNLOCK IS A CREDENTIAL TRADED FOR A SESSION, so a key that opens nothing is a
		// failed credential and not a miss.
		"a key that opens nothing": {
			op: "Unlock", route: "unlock", refusal: callsFakeRefusal{err: calls.ErrNotFound},
			status: http.StatusUnauthorized, body: `{"error":"invalid_key"}`,
		},
		"a frozen key": {
			op: "Unlock", route: "unlock", refusal: callsFakeRefusal{err: fmtWrap(calls.ErrForbidden, "the key is frozen")},
			status: http.StatusForbidden, body: `{"error":"forbidden"}`,
		},
		"a call that has ended": {
			op: "Join", route: "join", refusal: callsFakeRefusal{
				err: calls.ErrEnded, result: calls.JoinResult{Outcome: calls.OutcomeRefuse, Reason: calls.ReasonCallEnded}},
			status: http.StatusGone, body: `{"error":"ended","reason":"call_ended"}`,
		},
		"a call that was cancelled": {
			op: "Join", route: "join", refusal: callsFakeRefusal{
				err: calls.ErrEnded, result: calls.JoinResult{Outcome: calls.OutcomeRefuse, Reason: calls.ReasonCancelled}},
			status: http.StatusGone, body: `{"error":"ended","reason":"cancelled"}`,
		},
		"moving a cancelled booking": {
			op: "Reschedule", route: "reschedule", refusal: callsFakeRefusal{err: calls.Refuse(calls.ErrEnded, calls.ReasonCancelled, "a cancelled call is not moved")},
			status: http.StatusGone, body: `{"error":"ended","reason":"cancelled"}`,
		},
		"a host control in a call that ended": {
			op: "Host", route: "host", refusal: callsFakeRefusal{err: calls.Refuse(calls.ErrEnded, calls.ReasonCallEnded, "the call has ended")},
			status: http.StatusGone, body: `{"error":"ended","reason":"call_ended"}`,
		},
		"a seat the host removed": {
			op: "Join", route: "join", refusal: callsFakeRefusal{
				err: calls.ErrForbidden, result: calls.JoinResult{Outcome: calls.OutcomeRefuse, Reason: calls.ReasonRemoved}},
			status: http.StatusForbidden, body: `{"error":"forbidden","reason":"removed"}`,
		},
		"a control from a seat that is not the host's": {
			op: "Host", route: "host", refusal: callsFakeRefusal{err: calls.Refuse(calls.ErrForbidden, calls.ReasonNotHost, "only a host in the call makes a host control")},
			status: http.StatusForbidden, body: `{"error":"forbidden","reason":"not_host"}`,
		},
		"a start on a key that is not active": {
			op: "Start", route: "start", refusal: callsFakeRefusal{
				err: calls.ErrForbidden, result: calls.JoinResult{Outcome: calls.OutcomeRefuse, Reason: calls.ReasonKeyNotActive}},
			status: http.StatusForbidden, body: `{"error":"forbidden","reason":"key_not_active"}`,
		},
		"a missing confirmation": {
			op: "Join", route: "join", refusal: callsFakeRefusal{err: fmtWrap(calls.ErrInvalid, "the person has not confirmed they are 18 or older")},
			status: http.StatusBadRequest, body: `{"error":"invalid"}`,
		},
		"a booking in the past": {
			op: "Schedule", route: "schedule", refusal: callsFakeRefusal{err: calls.BookingRefusal(calls.ErrInPast)},
			status: http.StatusBadRequest, body: `{"error":"invalid","reason":"in_past"}`,
		},
		"a title too long": {
			op: "Schedule", route: "schedule", refusal: callsFakeRefusal{err: calls.Refuse(calls.ErrInvalid, calls.ReasonTitleTooLong, "a title is at most 80 characters")},
			status: http.StatusBadRequest, body: `{"error":"invalid","reason":"title_too_long"}`,
		},
		"a full room": {
			op: "Join", route: "join", refusal: callsFakeRefusal{
				err: calls.ErrConflict, result: calls.JoinResult{Outcome: calls.OutcomeRefuse, Reason: "room_full"}},
			status: http.StatusConflict, body: `{"error":"conflict","reason":"room_full"}`,
		},
		"a key already in a call": {
			op: "Start", route: "start", refusal: callsFakeRefusal{
				err: calls.ErrConflict, result: calls.JoinResult{Outcome: calls.OutcomeRefuse, Reason: calls.ReasonKeyInCall}},
			status: http.StatusConflict, body: `{"error":"conflict","reason":"key_in_call"}`,
		},
		"an overlapping booking": {
			op: "Schedule", route: "schedule", refusal: callsFakeRefusal{err: calls.BookingRefusal(calls.ErrOverlaps)},
			status: http.StatusConflict, body: `{"error":"conflict","reason":"overlaps"}`,
		},
		"admitting a seat that is not waiting": {
			op: "Host", route: "host", refusal: callsFakeRefusal{err: calls.Refuse(calls.ErrConflict, calls.ReasonNotWaiting, "only a seat waiting to come in is admitted")},
			status: http.StatusConflict, body: `{"error":"conflict","reason":"not_waiting"}`,
		},
		// NOT YET IS A CONFLICT THAT SAYS WHEN, and the header says it too.
		"a booked call that has not opened": {
			op: "Join", route: "join", refusal: callsFakeRefusal{
				err: calls.ErrConflict, result: calls.JoinResult{Outcome: calls.OutcomeRefuse, Reason: calls.ReasonNotYet, RetryAfter: 5 * time.Minute}},
			status: http.StatusConflict, body: `{"error":"conflict","reason":"not_yet","retryAfterMs":300000}`, retryAfter: "300",
		},
		// THE JOIN WINDOW IS THE ONE CONFLICT THAT IS A RATE LIMIT, and it says when it opens
		// again, rounded up so a client never asks early.
		"a full join window": {
			op: "Join", route: "join", refusal: callsFakeRefusal{
				err: calls.ErrConflict, result: calls.JoinResult{Outcome: calls.OutcomeRefuse, Reason: calls.ReasonRate, RetryAfter: 12300 * time.Millisecond}},
			status: http.StatusTooManyRequests, body: `{"error":"limited","reason":"rate","retryAfterMs":12300}`, retryAfter: "13",
		},
		"a full join window under a few milliseconds": {
			op: "Join", route: "join", refusal: callsFakeRefusal{
				err: calls.ErrConflict, result: calls.JoinResult{Outcome: calls.OutcomeRefuse, Reason: calls.ReasonRate, RetryAfter: 4 * time.Millisecond}},
			status: http.StatusTooManyRequests, body: `{"error":"limited","reason":"rate","retryAfterMs":4}`, retryAfter: "1",
		},
		// A REFUSAL WITH NO ERROR IS STILL A REFUSAL: a 200 never says refuse.
		"a refusal the service gave no error for": {
			op: "Queue", route: "queue", refusal: callsFakeRefusal{result: calls.JoinResult{Outcome: calls.OutcomeRefuse, Reason: "room_full"}},
			status: http.StatusConflict, body: `{"error":"conflict","reason":"room_full"}`,
		},
		"a failure the service could not classify": {
			op: "Leave", route: "leave", refusal: callsFakeRefusal{err: errors.New("calls: reach media server m1: connection refused")},
			status: http.StatusBadGateway, body: `{"error":"failed"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newCallsRig(t)
			rig.svc.refusals[tc.op] = tc.refusal
			rec := callsEveryRoute(rig, rig.secrets.hostSession)[tc.route]
			assertCallsReply(t, rec, tc.status, tc.body)
			if got := rec.Header().Get("Retry-After"); got != tc.retryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tc.retryAfter)
			}
			if tc.clears {
				assertCallsCookieCleared(t, rec)
			} else if tc.route != "join" {
				assertNoCallsCookie(t, rec)
			}
		})
	}
}

func fmtWrap(class error, detail string) error { return errors.Join(class, errors.New(detail)) }

// A FAILURE IS LOGGED BY THE ROUTE AND THE SITE, so an operator can find it, and by what the
// service said, which its own contract keeps free of secrets. NOTHING A REQUEST CARRIED
// GOES IN A LINE: not a key, a token, a link, a cookie or a pass, on any route, refused or
// answered.
func TestCallsNeverLogsASecret(t *testing.T) {
	rig := newCallsRig(t)
	// Every route answered, then every route refused by a failure, then by a stamp that does
	// not check out: between them, every line the handler can write.
	callsEveryRoute(rig, rig.secrets.hostSession)
	rig.post("/calls/join", rig.joinBody(rig.secrets.guestLink, rig.secrets.hostLink), rig.secrets.unheld)
	for _, op := range []string{"Unlock", "Session", "Start", "Schedule", "Reschedule", "Cancel", "Join", "Queue", "Host", "Leave"} {
		rig.svc.refusals[op] = callsFakeRefusal{err: errors.New("calls: the engine did not complete the statement")}
	}
	callsEveryRoute(rig, rig.secrets.hostSession)
	rig.stamp[CallsStampOwnerHeader] = callsTestMember
	callsEveryRoute(rig, rig.secrets.hostSession)

	logged := rig.logs.String()
	if !strings.Contains(logged, "op=leave") || !strings.Contains(logged, "subject.id="+callsTestSite) {
		t.Fatalf("a failure is not logged by its route and its site:\n%s", logged)
	}
	for what, secret := range rig.secrets.all() {
		if strings.Contains(logged, secret) {
			t.Fatalf("a line holds %s:\n%s", what, logged)
		}
	}
	for _, typed := range []string{"Grace", "Ada", "Weekly review"} {
		if strings.Contains(logged, typed) {
			t.Fatalf("a line holds %q, which a person typed:\n%s", typed, logged)
		}
	}
}

func TestCallsRepliesAreNoStore(t *testing.T) {
	rig := newCallsRig(t)
	replies := callsEveryRoute(rig, rig.secrets.hostSession)
	replies["a refusal"] = rig.get("/calls/session", "")
	replies["an unknown route"] = rig.get("/calls/nope", "")
	rig.stamp = nil
	replies["an unstamped request"] = rig.get("/calls/session", rig.secrets.hostSession)
	for name, rec := range replies {
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q", name, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", name, got)
		}
	}
}

// ---------------------------------------------------------------------------
// reaching the handler at all
// ---------------------------------------------------------------------------

// A PERSON IN A CALL CARRIES NO MEMQL CREDENTIAL AND NEVER WILL, and the bff installs a
// verifier ahead of its mux. HandlerAuthorizedPaths() does not make it step aside, since
// that list is read only where no verifier runs, so without the exact routes in
// SelfAuthenticatedPaths() every request from a browser is a 401 that names nothing.
func TestCallsRoutesReachTheHandlerWithoutABearer(t *testing.T) {
	rig := newCallsRig(t)
	for name, tc := range map[string]struct{ method, path, body string }{
		"unlock":     {http.MethodPost, "/calls/unlock", `{}`},
		"session":    {http.MethodGet, "/calls/session", ""},
		"start":      {http.MethodPost, "/calls/start", callsStartBody},
		"schedule":   {http.MethodPost, "/calls/schedule", callsScheduleBody},
		"reschedule": {http.MethodPost, "/calls/reschedule", `{"id":"booked1"}`},
		"cancel":     {http.MethodPost, "/calls/cancel", `{"id":"booked1"}`},
		"join":       {http.MethodPost, "/calls/join", `{}`},
		"queue":      {http.MethodGet, "/calls/queue", ""},
		"host":       {http.MethodPost, "/calls/host", `{"action":"lock"}`},
		"leave":      {http.MethodPost, "/calls/leave", `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			for key, value := range rig.stamp {
				req.Header.Set(key, value)
			}
			rec := httptest.NewRecorder()
			accessAuthenticatedHandler(rig.h).ServeHTTP(rec, req)
			// ONLY THE HANDLER SAYS THIS. The verifier's own 401 has the status a stranger's
			// reply has, and a stranger's reply reads nothing to show for it.
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("status %d, Cache-Control = %q: the request did not reach the handler", rec.Code, got)
			}
		})
	}
}

// THE VERIFIER STEPS ASIDE FOR TEN EXACT ROUTES AND NOTHING ELSE, so a route mounted beneath
// the prefix later does not inherit a bypass nobody decided on.
func TestCallsSelfAuthenticationDoesNotExemptOtherPaths(t *testing.T) {
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	for _, path := range []string{
		"/calls", "/calls/", "/calls/nope", "/calls/join/", "/calls/join/deeper", "/calls/host/x",
		"/calls%2Fjoin", "/%63alls/join", "/calls/join%20", "/calls/end",
	} {
		t.Run(path, func(t *testing.T) {
			called := false
			h := accessAuthenticatedHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
			if called || rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, handlerCalled = %v", rec.Code, called)
			}
		})
	}
}

func TestCallsSurfaceDeclarations(t *testing.T) {
	t.Run("the bff mounts one base", func(t *testing.T) {
		t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
		if got, want := CallsSurfacePaths(), []string{"/calls/"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("CallsSurfacePaths() = %v, want %v", got, want)
		}
	})

	t.Run("the exact routes carry the base", func(t *testing.T) {
		t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "/tenant")
		var want []string
		for _, name := range []string{"unlock", "session", "start", "schedule", "reschedule", "cancel", "join", "queue", "host", "leave"} {
			want = append(want, "/calls/"+name, "/tenant/calls/"+name)
		}
		if got := CallsSelfAuthenticatedPaths(); !reflect.DeepEqual(got, want) {
			t.Fatalf("CallsSelfAuthenticatedPaths() = %v, want %v", got, want)
		}
		if err := AssertSelfAuthenticatedRoutesFailClosed(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("under a base, the based path answers too", func(t *testing.T) {
		rig := newCallsRig(t)
		t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "/tenant")
		rec := rig.send(http.MethodGet, "/tenant/calls/session", "", "")
		assertCallsReply(t, rec, http.StatusUnauthorized, `{"error":"no_session"}`)
	})

	t.Run("the boot assertion accounts for the mount", func(t *testing.T) {
		t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
		var routes []string
		for _, path := range CallsSurfacePaths() {
			routes = append(routes, "GET "+path, "POST "+path)
		}
		if err := AssertUnauthenticatedSurfaceDeclared(routes); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("it is not public", func(t *testing.T) {
		t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
		for _, path := range PublicPaths() {
			if strings.HasPrefix(path, "/calls") {
				t.Fatalf("%q is in PublicPaths(), which every verifier-consuming node walks as an open prefix", path)
			}
		}
	})
}
