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

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/verifier"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/metrics"
)

// access_handler_test.go -- the bff half of the access surface, against fakes
// for the store and the engine and an injected clock (design record 2026-10-29,
// sections 6 to 9).
//
// What only a real engine can say -- who the tier admits, that a regenerate
// really replaces a digest, what a mutation's node really carries -- is said by
// app/access_surface_db_test.go. These hold the handler to its own decisions:
// which answer each state gets, what is written, what is never rendered and what
// never reaches a log.

const (
	accessTestOwner      = "user-owner"
	accessTestMember     = "user-member"
	accessTestSite       = "site1"
	accessTestOtherSite  = "site2"
	accessTestAccount    = "acct1"
	accessTestDeployable = "client-desk"
	accessTestDomain     = "desk"
	accessTestCode       = "K7Q4M-2XPDN-8RT3V-WJ6HC-9F01A"
	accessTestCodeID     = "code1"
	accessTestSubject    = "acme"
)

// accessT0 is when every test starts, at a whole second so a remainingMs reads as
// the arithmetic it is. TestSessionIssuedAtIsWholeSecondUTC starts somewhere less
// convenient, on purpose.
var accessT0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// accessFakeStore models what the real store's reads answer, and records every
// call, so "nothing was written" and "that was never read" are assertions rather
// than inferences.
//
// IT DOES NOT FILTER A CODE BY ITS SITE, though the real query does: the handler
// checks the row's own site as well, and a fake that filtered would leave that
// check unexercised by anything.
//
// NOR DOES IT ASK WHO IS ASKING, because the real one no longer does: a code and a
// session are read as the cluster acting, so anything that gets past the handler's own
// comparisons would be answered. Nothing else stands behind those comparisons, and a
// fake that kept a tier for these rows would hide that.
type accessFakeStore struct {
	site *AccessSite
	// admits is who the composite tier lets read the site. The owner, and a member
	// of its organization through the account arm, which is the whole reason the
	// handler compares the stamp with the row.
	admits map[string]bool
	// answersAnySite makes the read of the site hand back its row whatever id it was
	// asked for, for the handler's comparison of the row with the stamp.
	answersAnySite bool
	codes          map[string]*AccessCode
	digests        map[string]string
	sessions       map[string]*AccessSession

	created []accessCreatedSession
	ended   []string
	endedAt []time.Time
	calls   []string
	owners  []string
	fail    map[string]error
}

type accessCreatedSession struct {
	in        AccessSession
	tokenHash string
}

func (f *accessFakeStore) note(name string) error {
	f.calls = append(f.calls, name)
	return f.fail[name]
}

func (f *accessFakeStore) called(name string) int {
	n := 0
	for _, call := range f.calls {
		if call == name {
			n++
		}
	}
	return n
}

func (f *accessFakeStore) AccessSite(_ context.Context, siteID, owner string) (*AccessSite, error) {
	if err := f.note("AccessSite"); err != nil {
		return nil, err
	}
	if f.site == nil || (f.site.ID != siteID && !f.answersAnySite) || !f.admits[owner] {
		return nil, nil
	}
	site := *f.site
	return &site, nil
}

func (f *accessFakeStore) CodeByHash(_ context.Context, _, codeHash string) (*AccessCode, error) {
	if err := f.note("CodeByHash"); err != nil {
		return nil, err
	}
	code, found := f.codes[f.digests[codeHash]]
	if !found {
		return nil, nil
	}
	copied := *code
	return &copied, nil
}

func (f *accessFakeStore) CodeByID(_ context.Context, codeID string) (*AccessCode, error) {
	if err := f.note("CodeByID"); err != nil {
		return nil, err
	}
	code, found := f.codes[codeID]
	if !found {
		return nil, nil
	}
	copied := *code
	return &copied, nil
}

func (f *accessFakeStore) SessionByTokenHash(_ context.Context, tokenHash string) (*AccessSession, error) {
	if err := f.note("SessionByTokenHash"); err != nil {
		return nil, err
	}
	session, found := f.sessions[tokenHash]
	if !found {
		return nil, nil
	}
	copied := *session
	return &copied, nil
}

func (f *accessFakeStore) CreateSession(_ context.Context, owner string, in AccessSession, tokenHash string) error {
	if err := f.note("CreateSession"); err != nil {
		return err
	}
	f.owners = append(f.owners, owner)
	f.created = append(f.created, accessCreatedSession{in: in, tokenHash: tokenHash})
	stored := in
	if stored.ID == "" {
		stored.ID = "session-" + string(rune('a'+len(f.created)))
	}
	f.sessions[tokenHash] = &stored
	return nil
}

func (f *accessFakeStore) EndSession(_ context.Context, sessionID string, at time.Time) error {
	if err := f.note("EndSession"); err != nil {
		return err
	}
	f.ended = append(f.ended, sessionID)
	f.endedAt = append(f.endedAt, at)
	for _, session := range f.sessions {
		if session.ID == sessionID {
			session.Ended = true
		}
	}
	return nil
}

// regenerate is what a regenerate does to the row: the old digest finds nothing
// and the generation moves on.
func (f *accessFakeStore) regenerate(codeID, newCode string) {
	for digest, id := range f.digests {
		if id == codeID {
			delete(f.digests, digest)
		}
	}
	f.digests[memql.AccessCodeDigest(newCode)] = codeID
	f.codes[codeID].Generation++
}

// accessFakeEngine records every call and the identity it ran under.
type accessFakeEngine struct {
	calls   []string
	actors  []string
	origins []auth.CallOrigin
	result  any
	err     error
	during  func()
}

func (e *accessFakeEngine) Execute(ctx context.Context, query string) (any, error) {
	e.calls = append(e.calls, query)
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		actor = ac.UserId
	}
	e.actors = append(e.actors, actor)
	e.origins = append(e.origins, auth.OriginFromContext(ctx))
	if e.during != nil {
		e.during()
	}
	if e.err != nil {
		return nil, e.err
	}
	if e.result != nil {
		return e.result, nil
	}
	return memql.NewResultWithOutput([]any{}), nil
}

type accessFakeClock struct{ at time.Time }

func (c *accessFakeClock) now() time.Time { return c.at }

// accessRig is one test's handler and everything around it.
type accessRig struct {
	t      *testing.T
	h      *AccessHandler
	store  *accessFakeStore
	engine *accessFakeEngine
	clock  *accessFakeClock
	logs   *bytes.Buffer
	stamp  map[string]string
}

func newAccessRig(t *testing.T) *accessRig {
	t.Helper()
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	t.Cleanup(func() { memql.SetAccessGatesForTest() })
	dashboard := func(entry memql.AccessGateEntry) memql.AccessGateEntry {
		entry.Deployable, entry.Gate, entry.Domain, entry.Namespace = accessTestDeployable, "dashboard", accessTestDomain, accessTestDomain
		return entry
	}
	memql.SetAccessGatesForTest(
		dashboard(memql.AccessGateEntry{Construct: "deskBriefs", Kind: memql.AccessKindQuery,
			Args: []string{"window"}, Stamped: []string{memql.AccessArgCodeID}}),
		dashboard(memql.AccessGateEntry{Construct: "deskStamped", Kind: memql.AccessKindQuery,
			Args: []string{"window"}, Stamped: append([]string(nil), memql.ReservedAccessArgs...)}),
		dashboard(memql.AccessGateEntry{Construct: "deskNote", Kind: memql.AccessKindMutation,
			Args: []string{"noteId", "title"}, Stamped: []string{memql.AccessArgCodeID}}),
		// HAND-BUILT, since the load never lets a reserved name into Args: what stops a
		// caller naming one is the reserved list, and not only that Args omits it.
		dashboard(memql.AccessGateEntry{Construct: "deskListsReserved", Kind: memql.AccessKindQuery,
			Args: append([]string(nil), memql.ReservedAccessArgs...)}),
		// A construct on another gate, of another deployable, and of a domain this
		// site's package does not ship. Each is a real entry that must not be reached.
		memql.AccessGateEntry{Deployable: accessTestDeployable, Gate: "clients", Domain: accessTestDomain, Namespace: accessTestDomain,
			Construct: "deskClients", Kind: memql.AccessKindQuery},
		memql.AccessGateEntry{Deployable: "another-deployable", Gate: "dashboard", Domain: accessTestDomain, Namespace: accessTestDomain,
			Construct: "anotherDeployableBriefs", Kind: memql.AccessKindQuery},
		memql.AccessGateEntry{Deployable: accessTestDeployable, Gate: "dashboard", Domain: "anotherproduct", Namespace: "anotherproduct",
			Construct: "anotherProductBriefs", Kind: memql.AccessKindQuery},
	)
	store := &accessFakeStore{
		site: &AccessSite{ID: accessTestSite, OwnerUserID: accessTestOwner, AccountID: accessTestAccount,
			DeployableName: accessTestDeployable, PackageDomains: []string{accessTestDomain}},
		admits:   map[string]bool{accessTestOwner: true, accessTestMember: true},
		codes:    map[string]*AccessCode{},
		digests:  map[string]string{},
		sessions: map[string]*AccessSession{},
		fail:     map[string]error{},
	}
	store.codes[accessTestCodeID] = &AccessCode{
		ID: accessTestCodeID, SiteID: accessTestSite, AccountID: accessTestAccount, Gate: "dashboard",
		Subject: accessTestSubject, Generation: 1, ExpiresAt: accessT0.Add(time.Hour),
	}
	store.digests[memql.AccessCodeDigest(accessTestCode)] = accessTestCodeID
	engine := &accessFakeEngine{}
	clock := &accessFakeClock{at: accessT0}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &accessRig{
		t: t, h: NewAccessHandler(engine, store, logger, clock.now),
		store: store, engine: engine, clock: clock, logs: logs,
		stamp: map[string]string{AccessStampSiteHeader: accessTestSite, AccessStampOwnerHeader: accessTestOwner},
	}
}

func (r *accessRig) send(method, path, body, token string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for key, value := range r.stamp {
		req.Header.Set(key, value)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: AccessCookieName, Value: token})
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r *accessRig) unlock(code string) *httptest.ResponseRecorder {
	r.t.Helper()
	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		r.t.Fatal(err)
	}
	return r.send(http.MethodPost, "/access/unlock", string(body), "")
}

func (r *accessRig) session(token string) *httptest.ResponseRecorder {
	r.t.Helper()
	return r.send(http.MethodGet, "/access/session", "", token)
}

func (r *accessRig) leave(token string) *httptest.ResponseRecorder {
	r.t.Helper()
	return r.send(http.MethodPost, "/access/leave", "{}", token)
}

func (r *accessRig) call(token, name string, args map[string]any) *httptest.ResponseRecorder {
	r.t.Helper()
	body, err := json.Marshal(map[string]any{"call": name, "args": args})
	if err != nil {
		r.t.Fatal(err)
	}
	return r.send(http.MethodPost, "/access/call", string(body), token)
}

// open unlocks and answers the token the browser now holds.
func (r *accessRig) open() string {
	r.t.Helper()
	rec := r.unlock(accessTestCode)
	if rec.Code != http.StatusOK {
		r.t.Fatalf("unlocking a good code answered %d: %s", rec.Code, rec.Body.String())
	}
	return accessTokenFrom(r.t, rec)
}

var accessCookiePattern = regexp.MustCompile(`^memql_access=(mql_acs_[A-Za-z0-9_-]{43}); Path=/_memql/access; HttpOnly; Secure; SameSite=Strict$`)

func accessTokenFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	raw := rec.Header().Get("Set-Cookie")
	match := accessCookiePattern.FindStringSubmatch(raw)
	if match == nil {
		t.Fatalf("Set-Cookie = %q, want exactly the cookie Global Constraints names", raw)
	}
	return match[1]
}

func assertAccessCookieCleared(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	raw := rec.Header().Get("Set-Cookie")
	// The same Path, because a browser only overwrites the cookie it was given, and
	// the same attributes, because it refuses a clear that is weaker than the cookie.
	for _, want := range []string{"memql_access=;", "Path=/_memql/access", "Max-Age=0", "HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("Set-Cookie = %q, which does not clear the cookie: missing %q", raw, want)
		}
	}
}

func assertNoAccessCookie(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if raw := rec.Header().Get("Set-Cookie"); raw != "" {
		t.Fatalf("Set-Cookie = %q, want none", raw)
	}
}

func assertAccessReply(t *testing.T, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if rec.Code != status || rec.Body.String() != body {
		t.Fatalf("reply = %d %q, want %d %q", rec.Code, rec.Body.String(), status, body)
	}
	if got := rec.Header().Get("Content-Type"); body != "" && !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q on a JSON reply", got)
	}
}

func accessSessionBody(t *testing.T, rec *httptest.ResponseRecorder) (gate, subject, expiresAt string, remainingMs int64) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("not an object: %v: %s", err, rec.Body.String())
	}
	if len(fields) != 4 {
		t.Fatalf("the reply carries %d fields, want gate, subject, expiresAt and remainingMs: %s", len(fields), rec.Body.String())
	}
	var body struct {
		Gate        string `json:"gate"`
		Subject     string `json:"subject"`
		ExpiresAt   string `json:"expiresAt"`
		RemainingMs int64  `json:"remainingMs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Gate, body.Subject, body.ExpiresAt, body.RemainingMs
}

// ---------------------------------------------------------------------------
// unlock
// ---------------------------------------------------------------------------

func TestUnlockSetsTheCookieAndAnswersRemaining(t *testing.T) {
	rig := newAccessRig(t)

	rec := rig.unlock(accessTestCode)

	gate, subject, expiresAt, remainingMs := accessSessionBody(t, rec)
	if gate != "dashboard" || subject != accessTestSubject {
		t.Fatalf("gate = %q, subject = %q", gate, subject)
	}
	if want := accessT0.Add(time.Hour).Format(time.RFC3339); expiresAt != want {
		t.Fatalf("expiresAt = %q, want %q", expiresAt, want)
	}
	if remainingMs != time.Hour.Milliseconds() {
		t.Fatalf("remainingMs = %d, want an hour", remainingMs)
	}
	raw := rec.Header().Get("Set-Cookie")
	for _, forbidden := range []string{"Domain", "Max-Age", "Expires"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("the cookie carries %s: %q. The server is the authority on expiry, and a host-only cookie travels with nothing else", forbidden, raw)
		}
	}
	token := accessTokenFrom(t, rec)

	if len(rig.store.created) != 1 {
		t.Fatalf("wrote %d sessions, want 1", len(rig.store.created))
	}
	written := rig.store.created[0]
	if written.in.SiteID != accessTestSite || written.in.CodeID != accessTestCodeID || written.in.Generation != 1 || !written.in.IssuedAt.Equal(accessT0) {
		t.Fatalf("session row = %+v", written.in)
	}
	if written.tokenHash != memql.AccessSessionTokenDigest(token) {
		t.Fatal("the row does not hold the digest of the cookie, so the cookie would find nothing")
	}
	if written.tokenHash == token || strings.Contains(written.tokenHash, token) {
		t.Fatal("the row holds the plain token, so a row read would hand over a working session")
	}
	// THE ONE THING STILL GIVEN AN OWNER: a session is stamped with the site's at birth.
	if len(rig.store.owners) != 1 || rig.store.owners[0] != accessTestOwner {
		t.Fatalf("the row was written as %v, want the site's owner", rig.store.owners)
	}

	// The next request finds the session the cookie names.
	if _, _, _, remaining := accessSessionBody(t, rig.session(token)); remaining != time.Hour.Milliseconds() {
		t.Fatalf("remainingMs on the next request = %d", remaining)
	}
}

// Two unlocks are two sessions. A token is minted on every unlock, so nothing a
// visitor is handed before unlocking can become their session.
func TestUnlockMintsAFreshTokenEveryTime(t *testing.T) {
	rig := newAccessRig(t)
	if rig.open() == rig.open() {
		t.Fatal("two unlocks produced one token")
	}
}

func TestUnlockForgivesTyping(t *testing.T) {
	for name, typed := range map[string]string{
		"as shown":                accessTestCode,
		"lower case":              strings.ToLower(accessTestCode),
		"surrounding whitespace":  "\n\t  " + accessTestCode + "  \r\n",
		"spaces for hyphens":      "K7Q4M 2XPDN 8RT3V WJ6HC 9F01A",
		"no separators":           "K7Q4M2XPDN8RT3VWJ6HC9F01A",
		"O for zero":              "K7Q4M-2XPDN-8RT3V-WJ6HC-9FO1A",
		"I for one":               "K7Q4M-2XPDN-8RT3V-WJ6HC-9F0IA",
		"L for one":               "K7Q4M-2XPDN-8RT3V-WJ6HC-9F0LA",
		"lower case o, i and l":   "k7q4m-2xpdn-8rt3v-wj6hc-9foia",
		"en dashes from an email": "K7Q4M\u20132XPDN\u20138RT3V\u2013WJ6HC\u20139F01A",
		"a zero-width space":      "K7Q4M-\u200b2XPDN-8RT3V-WJ6HC-9F01A",
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t)
			rec := rig.unlock(typed)
			if rec.Code != http.StatusOK {
				t.Fatalf("%q answered %d: %s", typed, rec.Code, rec.Body.String())
			}
			accessTokenFrom(t, rec)
		})
	}
}

func TestUnlockInvalidIsOneAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(*accessRig)
		body    string
	}{
		"an unknown code": {body: `{"code":"00000-00000-00000-00000-00000"}`},
		"another site's code": {arrange: func(r *accessRig) {
			r.store.codes[accessTestCodeID].SiteID = accessTestOtherSite
		}},
		"a removed code": {arrange: func(r *accessRig) { r.store.codes[accessTestCodeID].Revoked = true }},
		"a superseded code": {arrange: func(r *accessRig) {
			r.store.regenerate(accessTestCodeID, "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ")
		}},
		// A removed code past its expiry is still a removed code: `expired` is for
		// somebody holding a code that was good, and a code that was ended by an
		// operator must not start answering differently once its date passes.
		"a removed code past its expiry": {arrange: func(r *accessRig) {
			r.store.codes[accessTestCodeID].Revoked = true
			r.store.codes[accessTestCodeID].ExpiresAt = accessT0.Add(-time.Hour)
		}},
		"no code at all":      {body: `{}`},
		"a blank code":        {body: `{"code":"   "}`},
		"not a string":        {body: `{"code":12345}`},
		"not JSON":            {body: `code=K7Q4M-2XPDN-8RT3V-WJ6HC-9F01A`},
		"an empty body":       {body: ""},
		"JSON and then more":  {body: `{"code":"` + accessTestCode + `"}{"code":"x"}`},
		"a body over the cap": {body: `{"code":"` + accessTestCode + `","pad":"` + strings.Repeat("x", 2<<10) + `"}`},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t)
			if tc.arrange != nil {
				tc.arrange(rig)
			}
			body := tc.body
			if tc.body == "" && tc.arrange != nil {
				encoded, _ := json.Marshal(map[string]string{"code": accessTestCode})
				body = string(encoded)
			}
			rec := rig.send(http.MethodPost, "/access/unlock", body, "")
			assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"invalid"}`)
			assertNoAccessCookie(t, rec)
			if len(rig.store.created) != 0 {
				t.Fatalf("a failed attempt wrote a session: %+v", rig.store.created)
			}
		})
	}
}

func TestACodeWhoseAccountDiffersFromItsSiteOpensNothing(t *testing.T) {
	t.Run("unlock answers invalid, even past its expiry", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.store.codes[accessTestCodeID].AccountID = "acct-the-site-left"
		rec := rig.unlock(accessTestCode)
		assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"invalid"}`)
		assertNoAccessCookie(t, rec)
		if len(rig.store.created) != 0 {
			t.Fatal("a stale code wrote a session")
		}

		rig.store.codes[accessTestCodeID].ExpiresAt = accessT0.Add(-time.Minute)
		assertAccessReply(t, rig.unlock(accessTestCode), http.StatusUnauthorized, `{"state":"invalid"}`)
	})

	t.Run("a live session ends", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		rig.store.site.AccountID = "acct-the-site-moved-to"

		rec := rig.session(token)
		assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
		assertAccessCookieCleared(t, rec)

		rec = rig.call(token, "deskBriefs", nil)
		assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
		if len(rig.engine.calls) != 0 {
			t.Fatalf("a stale code reached the engine: %v", rig.engine.calls)
		}
	})

	t.Run("one id spelled two ways is not a move", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.store.site.AccountID = "v1:accounts:account:" + accessTestAccount
		token := rig.open()
		if rec := rig.session(token); rec.Code != http.StatusOK {
			t.Fatalf("a canonical spelling read as a move: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("two blanks are not a stale copy", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.store.site.AccountID = ""
		rig.store.codes[accessTestCodeID].AccountID = ""
		token := rig.open()
		if rec := rig.session(token); rec.Code != http.StatusOK {
			t.Fatalf("a site with no organization opened nothing: %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a code with a copy on a site with none is a stale copy", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.store.site.AccountID = ""
		assertAccessReply(t, rig.unlock(accessTestCode), http.StatusUnauthorized, `{"state":"invalid"}`)
	})
}

func TestUnlockExpired(t *testing.T) {
	for name, at := range map[string]time.Time{
		"at the instant": accessT0.Add(time.Hour),
		"long past":      accessT0.Add(48 * time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t)
			rig.clock.at = at
			rec := rig.unlock(accessTestCode)
			assertAccessReply(t, rec, http.StatusForbidden, `{"state":"expired"}`)
			assertNoAccessCookie(t, rec)
			if len(rig.store.created) != 0 {
				t.Fatal("an expired code wrote a session")
			}
		})
	}

	t.Run("a code with no readable expiry is expired, not eternal", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.store.codes[accessTestCodeID].ExpiresAt = time.Time{}
		assertAccessReply(t, rig.unlock(accessTestCode), http.StatusForbidden, `{"state":"expired"}`)
	})

	t.Run("the millisecond before is not expired", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.clock.at = accessT0.Add(time.Hour - time.Millisecond)
		if rec := rig.unlock(accessTestCode); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

// THE LATEST-SESSION SORT COMPARES DATETIMES AS TEXT, so `...:20Z` would sort after
// `...:20.5Z` and an offset by its digits. The store enforces the form as well; the
// handler hands it the form so what it answers and what it stored agree.
func TestSessionIssuedAtIsWholeSecondUTC(t *testing.T) {
	rig := newAccessRig(t)
	zone := time.FixedZone("somewhere", 2*60*60)
	rig.clock.at = time.Date(2026, 10, 30, 14, 0, 7, 987654321, zone)

	rig.open()

	got := rig.store.created[0].in.IssuedAt
	want := time.Date(2026, 10, 30, 12, 0, 7, 0, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC || got.Nanosecond() != 0 {
		t.Fatalf("issuedAt = %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

// ---------------------------------------------------------------------------
// the session
// ---------------------------------------------------------------------------

func TestSessionEndsAtTheInstant(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()
	expiry := accessT0.Add(time.Hour)
	rig.engine.result = memql.NewResultWithOutput([]any{map[string]any{"id": "brief1"}})

	rig.clock.at = expiry.Add(-time.Millisecond)
	if _, _, _, remaining := accessSessionBody(t, rig.session(token)); remaining != 1 {
		t.Fatalf("remainingMs a millisecond before the end = %d", remaining)
	}
	if rec := rig.call(token, "deskBriefs", nil); rec.Code != http.StatusOK {
		t.Fatalf("a call a millisecond before the end answered %d", rec.Code)
	}

	for name, at := range map[string]time.Time{"at the instant": expiry, "after": expiry.Add(time.Millisecond)} {
		t.Run(name, func(t *testing.T) {
			rig.clock.at = at
			rec := rig.session(token)
			assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
			assertAccessCookieCleared(t, rec)
			rec = rig.call(token, "deskBriefs", nil)
			assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
			assertAccessCookieCleared(t, rec)
		})
	}
}

func TestSessionCapIs24Hours(t *testing.T) {
	rig := newAccessRig(t)
	rig.store.codes[accessTestCodeID].ExpiresAt = accessT0.Add(30 * 24 * time.Hour)
	token := rig.open()

	rig.clock.at = accessT0.Add(time.Hour)
	_, _, expiresAt, remaining := accessSessionBody(t, rig.session(token))
	if remaining != (23 * time.Hour).Milliseconds() {
		t.Fatalf("remainingMs = %d, want 23 hours", remaining)
	}
	// THE SAME INSTANT TWO WAYS. A reply that said a month beside a remainder of a
	// day would leave a client holding two answers to when it is taken out.
	if want := accessT0.Add(24 * time.Hour).Format(time.RFC3339); expiresAt != want {
		t.Fatalf("expiresAt = %q, want the moment the session ends, %q", expiresAt, want)
	}

	rig.clock.at = accessT0.Add(24*time.Hour - time.Millisecond)
	if rec := rig.session(token); rec.Code != http.StatusOK {
		t.Fatalf("the session ended before its day was up: %d", rec.Code)
	}

	rig.clock.at = accessT0.Add(24 * time.Hour)
	rec := rig.session(token)
	assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
	assertAccessCookieCleared(t, rec)

	// The code is still good, so typing it again opens a new day.
	fresh := rig.open()
	if _, _, _, remaining := accessSessionBody(t, rig.session(fresh)); remaining != (24 * time.Hour).Milliseconds() {
		t.Fatalf("a fresh unlock has %d left", remaining)
	}
}

func TestSessionEndedByRegenerateRemoveAndLeave(t *testing.T) {
	for name, end := range map[string]func(*accessRig, string){
		"regenerate": func(r *accessRig, _ string) {
			r.store.regenerate(accessTestCodeID, "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ")
		},
		"remove": func(r *accessRig, _ string) { r.store.codes[accessTestCodeID].Revoked = true },
		"leave": func(r *accessRig, token string) {
			if rec := r.leave(token); rec.Code != http.StatusNoContent {
				r.t.Fatalf("leave answered %d", rec.Code)
			}
		},
		"the code's row is gone": func(r *accessRig, _ string) { delete(r.store.codes, accessTestCodeID) },
		"the code moved to another site": func(r *accessRig, _ string) {
			r.store.codes[accessTestCodeID].SiteID = accessTestOtherSite
		},
		"the session belongs to another site": func(r *accessRig, token string) {
			r.store.sessions[memql.AccessSessionTokenDigest(token)].SiteID = accessTestOtherSite
		},
		"a cookie with no row": func(r *accessRig, token string) {
			delete(r.store.sessions, memql.AccessSessionTokenDigest(token))
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t)
			token := rig.open()
			if rec := rig.session(token); rec.Code != http.StatusOK {
				t.Fatalf("the session was not live to begin with: %d", rec.Code)
			}

			end(rig, token)

			rec := rig.session(token)
			assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
			assertAccessCookieCleared(t, rec)
			rec = rig.call(token, "deskBriefs", nil)
			assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
			assertAccessCookieCleared(t, rec)
			if len(rig.engine.calls) != 0 {
				t.Fatalf("an ended session reached the engine: %v", rig.engine.calls)
			}
		})
	}

	t.Run("a session with no readable issuedAt has no day left", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		rig.store.sessions[memql.AccessSessionTokenDigest(token)].IssuedAt = time.Time{}
		assertAccessReply(t, rig.session(token), http.StatusUnauthorized, `{"state":"ended"}`)
	})

	t.Run("a code with no readable expiry ends its sessions", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		rig.store.codes[accessTestCodeID].ExpiresAt = time.Time{}
		assertAccessReply(t, rig.session(token), http.StatusUnauthorized, `{"state":"ended"}`)
	})

	t.Run("a session of a later generation than its code is not a live one", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		rig.store.sessions[memql.AccessSessionTokenDigest(token)].Generation = 2
		assertAccessReply(t, rig.session(token), http.StatusUnauthorized, `{"state":"ended"}`)
	})
}

// Regenerated, the code that replaced it opens a NEW session and the old cookie
// stays ended: the two generations share a row and nothing else.
func TestRegenerateLeavesTheOldCookieEndedAndTheNewCodeGood(t *testing.T) {
	const fresh = "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ"
	rig := newAccessRig(t)
	old := rig.open()
	rig.store.regenerate(accessTestCodeID, fresh)

	assertAccessReply(t, rig.unlock(accessTestCode), http.StatusUnauthorized, `{"state":"invalid"}`)
	rec := rig.unlock(fresh)
	token := accessTokenFrom(t, rec)
	if rig.store.created[len(rig.store.created)-1].in.Generation != 2 {
		t.Fatal("the new session is not of the new generation")
	}
	if rec := rig.session(token); rec.Code != http.StatusOK {
		t.Fatalf("the new code's session answered %d", rec.Code)
	}
	assertAccessReply(t, rig.session(old), http.StatusUnauthorized, `{"state":"ended"}`)
}

func TestExtendingAnExpiryMovesRemaining(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()

	rig.store.codes[accessTestCodeID].ExpiresAt = accessT0.Add(3 * time.Hour)
	if _, _, expiresAt, remaining := accessSessionBody(t, rig.session(token)); remaining != (3*time.Hour).Milliseconds() || expiresAt != accessT0.Add(3*time.Hour).Format(time.RFC3339) {
		t.Fatalf("extended: remainingMs = %d, expiresAt = %q", remaining, expiresAt)
	}

	rig.store.codes[accessTestCodeID].ExpiresAt = accessT0.Add(time.Minute)
	if _, _, _, remaining := accessSessionBody(t, rig.session(token)); remaining != time.Minute.Milliseconds() {
		t.Fatalf("shortened: remainingMs = %d", remaining)
	}

	// PAST ITS END, AND THEN EXTENDED: the row says it is good again, and nothing
	// about the session recorded that it had lapsed, because nothing wrote to it.
	rig.clock.at = accessT0.Add(2 * time.Hour)
	assertAccessReply(t, rig.session(token), http.StatusUnauthorized, `{"state":"ended"}`)
	rig.store.codes[accessTestCodeID].ExpiresAt = accessT0.Add(3 * time.Hour)
	if _, _, _, remaining := accessSessionBody(t, rig.session(token)); remaining != time.Hour.Milliseconds() {
		t.Fatalf("revived: remainingMs = %d", remaining)
	}
	if len(rig.store.ended) != 0 {
		t.Fatal("an expiry wrote to the session; ending one is a comparison, not a write")
	}
}

func TestLeavingEndsThisSessionOnly(t *testing.T) {
	rig := newAccessRig(t)
	first, second := rig.open(), rig.open()

	rec := rig.leave(first)
	assertAccessReply(t, rec, http.StatusNoContent, "")
	assertAccessCookieCleared(t, rec)

	assertAccessReply(t, rig.session(first), http.StatusUnauthorized, `{"state":"ended"}`)
	if rec := rig.session(second); rec.Code != http.StatusOK {
		t.Fatalf("leaving one browser took the other out: %d %s", rec.Code, rec.Body.String())
	}
	if len(rig.store.ended) != 1 {
		t.Fatalf("ended %v, want one session", rig.store.ended)
	}
	if !rig.store.endedAt[0].Equal(accessT0) {
		t.Fatalf("ended at %s, want the injected clock's time", rig.store.endedAt[0])
	}
}

func TestLeaveIsIdempotent(t *testing.T) {
	t.Run("twice writes once", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		for range 2 {
			rec := rig.leave(token)
			assertAccessReply(t, rec, http.StatusNoContent, "")
			assertAccessCookieCleared(t, rec)
		}
		if len(rig.store.ended) != 1 {
			t.Fatalf("ended %d times; a second write would move the time it says the visitor left", len(rig.store.ended))
		}
	})

	t.Run("no cookie", func(t *testing.T) {
		rig := newAccessRig(t)
		rec := rig.leave("")
		assertAccessReply(t, rec, http.StatusNoContent, "")
		assertAccessCookieCleared(t, rec)
		if rig.store.called("SessionByTokenHash") != 0 || rig.store.called("EndSession") != 0 {
			t.Fatalf("store calls = %v", rig.store.calls)
		}
	})

	t.Run("a cookie with no row", func(t *testing.T) {
		rig := newAccessRig(t)
		rec := rig.leave("mql_acs_nothing")
		assertAccessReply(t, rec, http.StatusNoContent, "")
		assertAccessCookieCleared(t, rec)
		if len(rig.store.ended) != 0 {
			t.Fatalf("ended %v", rig.store.ended)
		}
	})

	t.Run("another site's session is not this site's to end", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		rig.store.sessions[memql.AccessSessionTokenDigest(token)].SiteID = accessTestOtherSite
		assertAccessReply(t, rig.leave(token), http.StatusNoContent, "")
		if len(rig.store.ended) != 0 {
			t.Fatalf("ended %v", rig.store.ended)
		}
	})

	t.Run("a session whose code is gone still ends", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		delete(rig.store.codes, accessTestCodeID)
		assertAccessReply(t, rig.leave(token), http.StatusNoContent, "")
		if len(rig.store.ended) != 1 {
			t.Fatalf("ended %v", rig.store.ended)
		}
	})
}

func TestNoCookieAnswersNone(t *testing.T) {
	for name, token := range map[string]string{"no cookie": "", "a blank cookie": " "} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t)
			rec := rig.session(token)
			assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"none"}`)
			assertAccessCookieCleared(t, rec)
			for _, leaked := range []string{"dashboard", accessTestDeployable, accessTestSubject, accessTestCodeID} {
				if strings.Contains(rec.Body.String(), leaked) {
					t.Fatalf("a stranger learned %q: %s", leaked, rec.Body.String())
				}
			}
			if rig.store.called("SessionByTokenHash") != 0 || rig.store.called("CodeByID") != 0 {
				t.Fatalf("store calls = %v", rig.store.calls)
			}
		})
	}

	t.Run("a call with no cookie has one word on the wire", func(t *testing.T) {
		rig := newAccessRig(t)
		rec := rig.call("", "deskBriefs", nil)
		assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
		if len(rig.engine.calls) != 0 {
			t.Fatalf("a stranger reached the engine: %v", rig.engine.calls)
		}
	})
}

// ---------------------------------------------------------------------------
// the call
// ---------------------------------------------------------------------------

func TestCallRunsTheDeclaredConstructUnderTheOwner(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()

	rec := rig.call(token, "deskBriefs", map[string]any{"window": "24h"})

	assertAccessReply(t, rec, http.StatusOK, `{"data":[]}`)
	assertNoAccessCookie(t, rec)
	want := `query desk.deskBriefs(accessCodeId: "code1", window: "24h")`
	if len(rig.engine.calls) != 1 || rig.engine.calls[0] != want {
		t.Fatalf("calls = %v, want %q", rig.engine.calls, want)
	}
	if rig.engine.actors[0] != accessTestOwner {
		t.Fatalf("the construct ran as %q, want the site's owner", rig.engine.actors[0])
	}
	// NEVER INTERNAL ORIGIN, which would put every @serverOnly construct in reach of
	// whoever holds a code.
	if rig.engine.origins[0] == auth.OriginInternal {
		t.Fatal("the construct ran with internal origin")
	}
}

// The owner the construct runs as is the ROW'S, and a canonical stamp and a bare
// row name one person.
func TestCallRunsAsTheRowsOwnerHoweverTheStampSpellsIt(t *testing.T) {
	rig := newAccessRig(t)
	canonical := "v1:identity:user:" + accessTestOwner
	rig.store.admits[canonical] = true
	rig.stamp[AccessStampOwnerHeader] = canonical
	token := rig.open()

	rig.call(token, "deskBriefs", nil)

	if rig.engine.actors[0] != accessTestOwner {
		t.Fatalf("ran as %q", rig.engine.actors[0])
	}
	for _, owner := range rig.store.owners {
		if owner != accessTestOwner {
			t.Fatalf("the session was written as %q", owner)
		}
	}
}

func TestCallStampsReservedArgumentsLast(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()

	rig.call(token, "deskStamped", map[string]any{"window": "24h"})

	want := `query desk.deskStamped(accessCodeId: "code1", accessGate: "dashboard", accessSubject: "acme", siteId: "site1", window: "24h")`
	if len(rig.engine.calls) != 1 || rig.engine.calls[0] != want {
		t.Fatalf("calls = %v, want %q", rig.engine.calls, want)
	}
}

// A CODE WITH NO SUBJECT STAMPS NONE, and the empty string is the mistake this
// holds off. Measured on the engine: a filter of `row.subject == args.accessSubject`
// with "" matches every row whose subject was never set, so a subject-less code
// would read rows that belong to no client, while an absent argument is refused by
// the engine. The refusal is the fail-closed answer, and the database test has it.
func TestACodeWithNoSubjectStampsNoSubject(t *testing.T) {
	rig := newAccessRig(t)
	rig.store.codes[accessTestCodeID].Subject = ""
	token := rig.open()

	rig.call(token, "deskStamped", nil)

	if len(rig.engine.calls) != 1 || strings.Contains(rig.engine.calls[0], memql.AccessArgSubject) {
		t.Fatalf("calls = %v", rig.engine.calls)
	}
}

func TestCallReturnsBareIds(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()
	const canonicalBrief = "v1:desk:deskBrief:brief1"
	nested := []map[string]any{{"id": "v1:desk:deskNote:note1", "concept": "v1:desk:deskNote"}}
	output := []map[string]any{{
		"id":        canonicalBrief,
		"title":     "Ops",
		"siteId":    "v1:platform:site:" + accessTestSite,
		"concept":   "v1:desk:deskBrief",
		"count":     7,
		"notes":     nested,
		"reviewers": []string{"v1:identity:user:reviewer1"},
		"author":    map[string]any{"userId": "v1:identity:user:author1"},
	}}
	rig.engine.result = memql.NewResultWithOutput(output)

	rec := rig.call(token, "deskBriefs", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{
		"id":        "brief1",
		"title":     "Ops",
		"siteId":    accessTestSite,
		"concept":   "v1:desk:deskBrief",
		"count":     float64(7),
		"notes":     []any{map[string]any{"id": "note1", "concept": "v1:desk:deskNote"}},
		"reviewers": []any{"reviewer1"},
		"author":    map[string]any{"userId": "author1"},
	}}
	if !reflect.DeepEqual(body.Data, want) {
		t.Fatalf("data = %#v, want %#v", body.Data, want)
	}
	if memql.WireCanonicalIdPattern().MatchString(body.Data[0]["id"].(string)) {
		t.Fatalf("a canonical id left the handler: %s", rec.Body.String())
	}

	// THE ENGINE'S OWN ROWS STAY CANONICAL. A shaped result can be a cached one, and
	// a later in-process reader must not find a bare id where it left a canonical one.
	if output[0]["id"] != canonicalBrief || nested[0]["id"] != "v1:desk:deskNote:note1" {
		t.Fatalf("the engine's rows were rewritten in place: %#v", output)
	}
}

func TestAGatedMutationAnswersOnlyItsBareId(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()
	payload, err := structpb.NewStruct(map[string]any{
		"title": "somebody else's", "accessCodeId": "another-code", "ownerUserId": accessTestOwner, "secretField": "sekrit",
	})
	if err != nil {
		t.Fatal(err)
	}
	schema, err := structpb.NewStruct(map[string]any{"description": "what the concept is for, in its author's words"})
	if err != nil {
		t.Fatal(err)
	}
	result := memql.NewResultWithOutput(nil)
	result.Bundle = &memqlv1.GraphBundle{
		RootIds: []string{"v1:desk:deskNote:note1"},
		Nodes: []*memqlv1.MemoryNode{{
			Id: "v1:desk:deskNote:note1", Concept: "v1:desk:deskNote",
			CreatedBy: "v1:identity:user:" + accessTestOwner, Payload: payload, Schema: schema,
		}},
	}
	rig.engine.result = result

	rec := rig.call(token, "deskNote", map[string]any{"noteId": "note1", "title": "mine"})

	assertAccessReply(t, rec, http.StatusOK, `{"data":[{"id":"note1"}]}`)
	want := `desk.deskNote(accessCodeId: "code1", noteId: "note1", title: "mine")`
	if rig.engine.calls[0] != want {
		t.Fatalf("call = %q, want %q", rig.engine.calls[0], want)
	}
}

// `data` IS ALWAYS AN ARRAY, so a client reading it never has to tell "nothing" from
// null: a shape with no rows, a construct that wrote nothing, and a result that did not
// come from the engine's own type at all.
func TestAReplyWithNoRowsIsAnEmptyArray(t *testing.T) {
	for name, tc := range map[string]struct {
		construct string
		result    any
	}{
		"a shaped query with no rows": {"deskBriefs", memql.NewResultWithOutput([]any{})},
		"an unshaped query with no rows": {"deskBriefs", func() any {
			result := memql.NewResultWithOutput(nil)
			result.Bundle = &memqlv1.GraphBundle{}
			return result
		}()},
		"a mutation with no bundle": {"deskNote", memql.NewResultWithOutput(nil)},
		"a result of no known type": {"deskBriefs", map[string]any{"id": "v1:desk:deskBrief:brief1", "title": "secret"}},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t)
			token := rig.open()
			rig.engine.result = tc.result
			args := map[string]any{}
			if tc.construct == "deskNote" {
				args = map[string]any{"noteId": "note1", "title": "mine"}
			}
			assertAccessReply(t, rig.call(token, tc.construct, args), http.StatusOK, `{"data":[]}`)
		})
	}
}

// A MUTATION NEVER ANSWERS WITH ROWS, whatever its result carries: being a mutation is
// the reason, and not what the result happens to look like today.
func TestAGatedMutationWithAnOutputStillAnswersOnlyItsBareId(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()
	result := memql.NewResultWithOutput([]any{map[string]any{"id": "v1:desk:deskNote:note1", "title": "somebody else's"}})
	result.Bundle = &memqlv1.GraphBundle{RootIds: []string{"v1:desk:deskNote:note1"}}
	rig.engine.result = result

	rec := rig.call(token, "deskNote", map[string]any{"noteId": "note1", "title": "mine"})

	assertAccessReply(t, rec, http.StatusOK, `{"data":[{"id":"note1"}]}`)
}

// A QUERY WITH NO SHAPE CLAUSE HANDS OVER WHAT A MUTATION WOULD: the node, with its
// payload, its creator and the concept's description. The spec says a query answers
// with its shape's rows and a construct with no shape answers with its ids, so this
// is that rule read off the result rather than off the declaration, and the warning
// is how its author finds out why the fields are missing.
//
// ITS BUNDLE CARRIES WHAT ITS ROWS RELATE TO AS WELL, so the second node here is a
// related site, which is what a real bundle holds: it is no row of this query, and
// its id is not an answer.
func TestAnUnshapedQueryAnswersOnlyItsBareIds(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()
	payload, err := structpb.NewStruct(map[string]any{"title": "t", "secretField": "sekrit"})
	if err != nil {
		t.Fatal(err)
	}
	result := memql.NewResultWithOutput(nil)
	result.Bundle = &memqlv1.GraphBundle{
		RootIds: []string{"v1:desk:deskBrief:brief1"},
		Nodes: []*memqlv1.MemoryNode{
			{
				Id: "v1:desk:deskBrief:brief1", Concept: "v1:desk:deskBrief",
				CreatedBy: "v1:identity:user:" + accessTestOwner, Payload: payload,
			},
			{Id: "v1:platform:site:" + accessTestSite, Concept: "v1:platform:site", Payload: payload},
		},
	}
	rig.engine.result = result

	rec := rig.call(token, "deskBriefs", nil)

	assertAccessReply(t, rec, http.StatusOK, `{"data":[{"id":"brief1"}]}`)
	logged := rig.logs.String()
	if !strings.Contains(logged, "deskBriefs") || !strings.Contains(logged, "shape") {
		t.Fatalf("nothing tells the author why the fields are missing: %s", logged)
	}
}

func TestCallRefusesForgedReservedArguments(t *testing.T) {
	for _, reserved := range memql.ReservedAccessArgs {
		// The construct that declares all four, and one that does not declare this
		// one: a caller sending a reserved name is refused either way, so a forged
		// argument cannot tell anybody what a construct declares.
		for _, construct := range []string{"deskStamped", "deskNote", "deskListsReserved"} {
			t.Run(reserved+" on "+construct, func(t *testing.T) {
				rig := newAccessRig(t)
				token := rig.open()
				rec := rig.call(token, construct, map[string]any{reserved: "forged"})
				assertAccessReply(t, rec, http.StatusBadRequest, `{"error":"invalid_args"}`)
				if len(rig.engine.calls) != 0 {
					t.Fatalf("a forged argument reached the engine: %v", rig.engine.calls)
				}
			})
		}
	}
}

func TestCallRefusesUndeclaredArgumentNames(t *testing.T) {
	for _, name := range []string{
		"bogus",
		"x: 1) mutation evil(",
		`window: "24h") mutation evil(x`,
		"Window",
		"window ",
		" window",
		"window\n",
		"",
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t)
			token := rig.open()
			rec := rig.call(token, "deskBriefs", map[string]any{name: 1})
			assertAccessReply(t, rec, http.StatusBadRequest, `{"error":"invalid_args"}`)
			if len(rig.engine.calls) != 0 {
				t.Fatalf("an undeclared name reached the engine: %v", rig.engine.calls)
			}
		})
	}
}

// A VALUE IS DATA, however much it looks like the language. dslCall JSON-encodes
// values, so this holds the encoding to that; the database test holds the engine to
// reading it back as the same string.
func TestCallEncodesArgumentValues(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()

	rig.call(token, "deskBriefs", map[string]any{"window": `24h") mutation evil("`})

	want := `query desk.deskBriefs(accessCodeId: "code1", window: "24h\") mutation evil(\"")`
	if len(rig.engine.calls) != 1 || rig.engine.calls[0] != want {
		t.Fatalf("calls = %v, want %q", rig.engine.calls, want)
	}
}

// A long integer reaches the engine as the digits it was sent as. Read through a
// float64 it would arrive as a different number, or as 1.2345678901234567e+19.
func TestCallKeepsALongNumberExact(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()

	rec := rig.send(http.MethodPost, "/access/call", `{"call":"deskBriefs","args":{"window":12345678901234567890}}`, token)

	if rec.Code != http.StatusOK || !strings.Contains(rig.engine.calls[0], "window: 12345678901234567890") {
		t.Fatalf("status = %d, calls = %v", rec.Code, rig.engine.calls)
	}
}

func TestCallNotFoundIsOneAnswer(t *testing.T) {
	const want = `{"error":"not_found"}`
	for name, tc := range map[string]struct {
		construct string
		arrange   func(*accessRig)
	}{
		"an unknown construct":        {construct: "nobodyDeclaredThis"},
		"a construct of another gate": {construct: "deskClients"},
		"a construct of another deployable": {
			construct: "anotherDeployableBriefs",
		},
		"a domain the site's package does not ship": {construct: "anotherProductBriefs"},
		"no recorded domains": {construct: "deskBriefs", arrange: func(r *accessRig) {
			r.store.site.PackageDomains = nil
		}},
		"a gate no construct declares": {construct: "deskBriefs", arrange: func(r *accessRig) {
			r.store.codes[accessTestCodeID].Gate = "gate-dropped-from-the-bundle"
		}},
		"an empty name":          {construct: ""},
		"a name almost declared": {construct: "deskBriefs "},
		"a qualified name":       {construct: "desk.deskBriefs"},
		"a name with a keyword":  {construct: "query deskBriefs"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t)
			token := rig.open()
			if tc.arrange != nil {
				tc.arrange(rig)
			}
			// EVEN A FORGED ARGUMENT, since a 400 on something out of reach would say
			// the difference between a name that is there and one that is not.
			rec := rig.call(token, tc.construct, map[string]any{memql.AccessArgCodeID: "forged"})
			assertAccessReply(t, rec, http.StatusNotFound, want)
			assertNoAccessCookie(t, rec)
			if len(rig.engine.calls) != 0 {
				t.Fatalf("reached the engine: %v", rig.engine.calls)
			}
		})
	}
}

func TestCallAfterExpiryNeverReachesTheEngine(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()
	rig.clock.at = accessT0.Add(2 * time.Hour)

	rec := rig.call(token, "deskBriefs", map[string]any{"window": "24h"})

	assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
	assertAccessCookieCleared(t, rec)
	if len(rig.engine.calls) != 0 {
		t.Fatalf("reached the engine: %v", rig.engine.calls)
	}
}

// NOTHING LEAVES UNTIL THE SESSION HAS BEEN READ AGAIN (section 8). The check ahead of
// a call has no opinion about what happened while the call ran, so each of these lands
// from inside Execute, with the rows already in the engine's hands: an operator's act,
// the clock, and a second read that cannot say. Every one of them delivers nothing and
// answers as a call after the end does.
func TestAnEndLandingDuringACallDeliversNothing(t *testing.T) {
	const secretRow = "a-row-this-visitor-may-no-longer-read"
	for name, act := range map[string]func(r *accessRig, token string){
		"a regenerate": func(r *accessRig, _ string) {
			r.store.regenerate(accessTestCodeID, "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ")
		},
		"a removal": func(r *accessRig, _ string) { r.store.codes[accessTestCodeID].Revoked = true },
		"an expiry moved into the past": func(r *accessRig, _ string) {
			r.store.codes[accessTestCodeID].ExpiresAt = accessT0.Add(-time.Minute)
		},
		"the clock reaching the expiry": func(r *accessRig, _ string) { r.clock.at = accessT0.Add(time.Hour) },
		"the clock reaching the day's cap": func(r *accessRig, _ string) {
			r.store.codes[accessTestCodeID].ExpiresAt = accessT0.Add(30 * 24 * time.Hour)
			r.clock.at = accessT0.Add(24 * time.Hour)
		},
		"the visitor leaving in another tab": func(r *accessRig, token string) {
			r.store.sessions[memql.AccessSessionTokenDigest(token)].Ended = true
		},
		"the session's row going": func(r *accessRig, token string) {
			delete(r.store.sessions, memql.AccessSessionTokenDigest(token))
		},
		"the code's row going": func(r *accessRig, _ string) { delete(r.store.codes, accessTestCodeID) },
		"the code's copy of the account going stale": func(r *accessRig, _ string) {
			r.store.codes[accessTestCodeID].AccountID = "acct-the-site-left"
		},
		// THE ONE FAILED READ THAT TAKES SOMEBODY OUT. The rows are in hand and nothing can
		// say they may leave. The first read of a call still answers 502, which is
		// TestAStoreFailureOpensNothingAndEndsNothing.
		"the second read of the session failing": func(r *accessRig, _ string) {
			r.store.fail["SessionByTokenHash"] = errors.New("the store failed")
		},
		"the second read of the code failing": func(r *accessRig, _ string) {
			r.store.fail["CodeByID"] = errors.New("the store failed")
		},
	} {
		for _, construct := range []string{"deskBriefs", "deskNote"} {
			t.Run(name+", on "+construct, func(t *testing.T) {
				rig := newAccessRig(t)
				token := rig.open()
				rig.engine.result = memql.NewResultWithOutput([]any{map[string]any{"id": "brief1", "title": secretRow}})
				rig.engine.during = func() { act(rig, token) }
				args := map[string]any{}
				if construct == "deskNote" {
					args = map[string]any{"noteId": "note1", "title": "mine"}
				}

				rec := rig.call(token, construct, args)

				if len(rig.engine.calls) != 1 {
					t.Fatalf("the construct did not run, so nothing was in hand: %v", rig.engine.calls)
				}
				assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
				assertAccessCookieCleared(t, rec)
			})
		}
	}

	// The control, and the price: with nothing landing the rows leave, and a call has read
	// the session and its code twice each.
	t.Run("with nothing landing, the rows leave after a second read", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		rig.engine.result = memql.NewResultWithOutput([]any{map[string]any{"id": "brief1", "title": secretRow}})
		sessions, codes := rig.store.called("SessionByTokenHash"), rig.store.called("CodeByID")

		rec := rig.call(token, "deskBriefs", nil)

		assertAccessReply(t, rec, http.StatusOK, `{"data":[{"id":"brief1","title":"`+secretRow+`"}]}`)
		assertNoAccessCookie(t, rec)
		if got := rig.store.called("SessionByTokenHash") - sessions; got != 2 {
			t.Fatalf("the session was read %d times, want 2", got)
		}
		if got := rig.store.called("CodeByID") - codes; got != 2 {
			t.Fatalf("the code was read %d times, want 2", got)
		}
	})
}

// A GENERATION STARTS AT 1, and an unreadable one comes back as 0. Equality is not enough:
// 0 on a code and 0 on its session compare equal, so with both read as absent a regenerate
// would stop ending sessions. It fails as a zero expiry does.
func TestACodeOfNoGenerationOpensNothing(t *testing.T) {
	for name, generation := range map[string]int{"zero": 0, "below zero": -1} {
		t.Run(name, func(t *testing.T) {
			t.Run("on unlock", func(t *testing.T) {
				rig := newAccessRig(t)
				rig.store.codes[accessTestCodeID].Generation = generation
				rec := rig.unlock(accessTestCode)
				assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"invalid"}`)
				assertNoAccessCookie(t, rec)
				if len(rig.store.created) != 0 {
					t.Fatalf("a code of no generation wrote a session: %+v", rig.store.created)
				}
			})

			t.Run("with its session of the same", func(t *testing.T) {
				rig := newAccessRig(t)
				token := rig.open()
				rig.store.codes[accessTestCodeID].Generation = generation
				rig.store.sessions[memql.AccessSessionTokenDigest(token)].Generation = generation

				rec := rig.session(token)
				assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
				assertAccessCookieCleared(t, rec)
				rec = rig.call(token, "deskBriefs", nil)
				assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"ended"}`)
				if len(rig.engine.calls) != 0 {
					t.Fatalf("reached the engine: %v", rig.engine.calls)
				}
			})
		})
	}

	// The first generation is the first that counts: what a mint writes has to open.
	t.Run("generation 1 opens", func(t *testing.T) {
		rig := newAccessRig(t)
		if rig.store.codes[accessTestCodeID].Generation != 1 {
			t.Fatal("the fixture's code is not of generation 1, so this proves nothing")
		}
		token := rig.open()
		if rec := rig.session(token); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestARefusedCallIsRefusedAndTheSessionStands(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()
	rig.engine.err = errors.New("the engine refused")

	rec := rig.call(token, "deskBriefs", nil)

	assertAccessReply(t, rec, http.StatusBadGateway, `{"error":"refused"}`)
	assertNoAccessCookie(t, rec)
}

func TestCallBodies(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		status int
		want   string
	}{
		"not JSON":            {body: `call=deskBriefs`, status: http.StatusBadRequest, want: `{"error":"invalid_args"}`},
		"an empty body":       {body: "", status: http.StatusBadRequest, want: `{"error":"invalid_args"}`},
		"args not an object":  {body: `{"call":"deskBriefs","args":["window"]}`, status: http.StatusBadRequest, want: `{"error":"invalid_args"}`},
		"call not a string":   {body: `{"call":1}`, status: http.StatusBadRequest, want: `{"error":"invalid_args"}`},
		"JSON and then more":  {body: `{"call":"deskBriefs"}{"call":"deskClients"}`, status: http.StatusBadRequest, want: `{"error":"invalid_args"}`},
		"no call named":       {body: `{"args":{}}`, status: http.StatusNotFound, want: `{"error":"not_found"}`},
		"no args":             {body: `{"call":"deskBriefs"}`, status: http.StatusOK, want: `{"data":[]}`},
		"null args":           {body: `{"call":"deskBriefs","args":null}`, status: http.StatusOK, want: `{"data":[]}`},
		"an escaped forgery":  {body: `{"call":"deskBriefs","args":{"accessCodeId":"forged"}}`, status: http.StatusBadRequest, want: `{"error":"invalid_args"}`},
		"a repeated forgery":  {body: `{"call":"deskBriefs","args":{"accessCodeId":"forged","accessCodeId":"x"}}`, status: http.StatusBadRequest, want: `{"error":"invalid_args"}`},
		"a body over the cap": {body: `{"call":"deskBriefs","args":{"window":"` + strings.Repeat("x", 65<<10) + `"}}`, status: http.StatusBadRequest, want: `{"error":"invalid_args"}`},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t)
			token := rig.open()
			rec := rig.send(http.MethodPost, "/access/call", tc.body, token)
			assertAccessReply(t, rec, tc.status, tc.want)
			if tc.status != http.StatusOK && len(rig.engine.calls) != 0 {
				t.Fatalf("reached the engine: %v", rig.engine.calls)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the bound on the body
// ---------------------------------------------------------------------------

// THE EDGE CAPS TOO, and this does not lean on it: the normalizer and the digests
// run over whatever is read, so the cap has to hold before any of them does.
func TestAnOversizedBodyIsRefusedBeforeItIsHashed(t *testing.T) {
	sized := func(prefix, suffix string, size int) string {
		return prefix + strings.Repeat(" ", size-len(prefix)-len(suffix)) + suffix
	}

	t.Run("unlock, one byte over 1 KiB", func(t *testing.T) {
		rig := newAccessRig(t)
		body := sized(`{"code":"`+accessTestCode+`"`, "}", 1<<10+1)
		assertAccessReply(t, rig.send(http.MethodPost, "/access/unlock", body, ""), http.StatusUnauthorized, `{"state":"invalid"}`)
		if rig.store.called("CodeByHash") != 0 {
			t.Fatal("an oversized body was hashed and looked up")
		}
	})

	t.Run("unlock, exactly 1 KiB", func(t *testing.T) {
		rig := newAccessRig(t)
		body := sized(`{"code":"`+accessTestCode+`"`, "}", 1<<10)
		if rec := rig.send(http.MethodPost, "/access/unlock", body, ""); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("call, one byte over 64 KiB", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		body := sized(`{"call":"deskBriefs"`, "}", 64<<10+1)
		assertAccessReply(t, rig.send(http.MethodPost, "/access/call", body, token), http.StatusBadRequest, `{"error":"invalid_args"}`)
		if len(rig.engine.calls) != 0 {
			t.Fatal("an oversized body reached the engine")
		}
	})

	t.Run("call, exactly 64 KiB", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		body := sized(`{"call":"deskBriefs"`, "}", 64<<10)
		if rec := rig.send(http.MethodPost, "/access/call", body, token); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("leave reads no body, however long", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		body := sized("{", "}", 2<<10)
		assertAccessReply(t, rig.send(http.MethodPost, "/access/leave", body, token), http.StatusNoContent, "")
		if len(rig.store.ended) != 1 {
			t.Fatalf("ended %v", rig.store.ended)
		}
	})
}

// ---------------------------------------------------------------------------
// what never leaves
// ---------------------------------------------------------------------------

func TestRefusalIsNotLoggedWithArguments(t *testing.T) {
	rig := newAccessRig(t)
	const secretArgument = "a-value-only-the-caller-knows"
	token := rig.open()
	// An engine error can repeat its statement, which holds every argument and the
	// stamped ids, so the error's words are the one thing here that must not be logged.
	rig.engine.err = errors.New(`parse error near query desk.deskBriefs(window: "` + secretArgument + `")`)

	rig.call(token, "deskBriefs", map[string]any{"window": secretArgument})
	rig.call(token, "deskBriefs", map[string]any{"undeclared": secretArgument})
	rig.call(token, "deskBriefs", map[string]any{memql.AccessArgCodeID: secretArgument})
	rig.call(token, "nobodyDeclaredThis", map[string]any{"window": secretArgument})
	rig.unlock("00000-00000-00000-00000-00000")
	rig.store.fail["CreateSession"] = errors.New("the store failed")
	rig.unlock(accessTestCode)
	delete(rig.store.fail, "CreateSession")
	rig.session(token)
	rig.leave(token)

	logged := rig.logs.String()
	for name, secret := range map[string]string{
		"the code":             accessTestCode,
		"the normalized code":  memql.NormalizeAccessCode(accessTestCode),
		"the code's digest":    memql.AccessCodeDigest(accessTestCode),
		"a mistyped code":      "00000-00000-00000-00000-00000",
		"the token":            token,
		"the token's digest":   memql.AccessSessionTokenDigest(token),
		"a call argument":      secretArgument,
		"the rendered call":    "desk.deskBriefs(",
		"the stamped code id":  `accessCodeId: "`,
		"the stamped subject":  accessTestSubject,
		"the engine's message": "parse error near",
	} {
		if strings.Contains(logged, secret) {
			t.Fatalf("the log holds %s: %s", name, logged)
		}
	}
	// IT STILL SAYS WHAT HAPPENED: a refusal an operator cannot find is no better
	// than one that leaks. The site and the construct, and nothing about the caller.
	if !strings.Contains(logged, accessTestSite) || !strings.Contains(logged, "deskBriefs") {
		t.Fatalf("the refusal does not name the site and the construct: %s", logged)
	}
}

func TestRepliesAreNoStore(t *testing.T) {
	rig := newAccessRig(t)
	token := rig.open()
	replies := map[string]*httptest.ResponseRecorder{}

	replies["unlock 200"] = rig.unlock(accessTestCode)
	replies["unlock 401"] = rig.unlock("00000-00000-00000-00000-00000")
	replies["session 200"] = rig.session(token)
	replies["session none"] = rig.session("")
	replies["session ended"] = rig.session("mql_acs_nothing")
	replies["call 200"] = rig.call(token, "deskBriefs", nil)
	replies["call 400"] = rig.call(token, "deskBriefs", map[string]any{"bogus": 1})
	replies["call 404"] = rig.call(token, "nobodyDeclaredThis", nil)
	rig.engine.err = errors.New("refused")
	replies["call 502"] = rig.call(token, "deskBriefs", nil)
	rig.engine.err = nil
	rig.store.fail["SessionByTokenHash"] = errors.New("the store failed")
	replies["session 502"] = rig.session(token)
	delete(rig.store.fail, "SessionByTokenHash")
	replies["leave 204"] = rig.leave(token)
	replies["an unknown route"] = rig.send(http.MethodGet, "/access/nope", "", "")
	rig.clock.at = accessT0.Add(2 * time.Hour)
	replies["unlock 403"] = rig.unlock(accessTestCode)
	rig.stamp = nil
	replies["an unstamped request"] = rig.session("")

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
// the stamp
// ---------------------------------------------------------------------------

func accessEveryOperation(r *accessRig, token string) map[string]*httptest.ResponseRecorder {
	return map[string]*httptest.ResponseRecorder{
		"unlock":  r.unlock(accessTestCode),
		"session": r.session(token),
		"leave":   r.leave(token),
		"call":    r.call(token, "deskBriefs", nil),
	}
}

func TestUnstampedRequestIs404(t *testing.T) {
	for name, stamp := range map[string]map[string]string{
		"no stamp":      nil,
		"no owner":      {AccessStampSiteHeader: accessTestSite},
		"no site":       {AccessStampOwnerHeader: accessTestOwner},
		"a blank owner": {AccessStampSiteHeader: accessTestSite, AccessStampOwnerHeader: "   "},
		"a blank site":  {AccessStampSiteHeader: "   ", AccessStampOwnerHeader: accessTestOwner},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t)
			rig.stamp = stamp
			for _, rec := range accessEveryOperation(rig, "mql_acs_whatever") {
				assertAccessReply(t, rec, http.StatusNotFound, `{"error":"not_found"}`)
				assertNoAccessCookie(t, rec)
			}
			if len(rig.store.calls) != 0 || len(rig.engine.calls) != 0 {
				t.Fatalf("an unstamped request reached store %v, engine %v", rig.store.calls, rig.engine.calls)
			}
		})
	}
}

func TestForgedOwnerReadsNothing(t *testing.T) {
	t.Run("a stranger reads zero rows", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		before := len(rig.store.calls)
		rig.stamp[AccessStampOwnerHeader] = "user-stranger"
		for op, rec := range accessEveryOperation(rig, token) {
			t.Run(op, func(t *testing.T) {
				assertAccessReply(t, rec, http.StatusNotFound, `{"error":"not_found"}`)
				assertNoAccessCookie(t, rec)
			})
		}
		for _, call := range rig.store.calls[before:] {
			if call != "AccessSite" {
				t.Fatalf("a stranger's request read %s", call)
			}
		}
	})

	// THE TIER'S ACCOUNT ARM ADMITS HIM, so "the read returned a row" says nothing, and
	// past this comparison nothing asks who he is: the codes and sessions are read as the
	// cluster. It is the check on the owner, and there is no other.
	t.Run("a member of the site's organization reads the row and is refused anyway", func(t *testing.T) {
		rig := newAccessRig(t)
		token := rig.open()
		before := len(rig.store.calls)
		rig.stamp[AccessStampOwnerHeader] = accessTestMember
		sessions := len(rig.store.created)
		for op, rec := range accessEveryOperation(rig, token) {
			t.Run(op, func(t *testing.T) {
				assertAccessReply(t, rec, http.StatusNotFound, `{"error":"not_found"}`)
				assertNoAccessCookie(t, rec)
			})
		}
		for _, call := range rig.store.calls[before:] {
			if call != "AccessSite" {
				t.Fatalf("a forged owner's request read %s", call)
			}
		}
		if len(rig.engine.calls) != 0 {
			t.Fatalf("a forged owner reached the engine: %v", rig.engine.calls)
		}
		if len(rig.store.created) != sessions || len(rig.store.ended) != 0 {
			t.Fatalf("a forged owner wrote: created %v, ended %v", rig.store.created[sessions:], rig.store.ended)
		}
		// Whose session is untouched, since nothing about the forgery reached it.
		rig.stamp[AccessStampOwnerHeader] = accessTestOwner
		if rec := rig.session(token); rec.Code != http.StatusOK {
			t.Fatalf("a forged owner's leave ended the session: %d", rec.Code)
		}
	})

	t.Run("a stamp naming another site reads nothing of it", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.stamp[AccessStampSiteHeader] = accessTestOtherSite
		assertAccessReply(t, rig.unlock(accessTestCode), http.StatusNotFound, `{"error":"not_found"}`)
	})

	// THE ROW IS HELD TO THE STAMP, not only the read to its argument: every code and
	// session below is compared with this row's id, so a row that is not the stamped site
	// would move what all of those comparisons mean.
	t.Run("a read that answers with another site's row is refused", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.store.answersAnySite = true
		rig.stamp[AccessStampSiteHeader] = accessTestOtherSite
		before := len(rig.store.calls)
		for op, rec := range accessEveryOperation(rig, "mql_acs_whatever") {
			t.Run(op, func(t *testing.T) {
				assertAccessReply(t, rec, http.StatusNotFound, `{"error":"not_found"}`)
			})
		}
		for _, call := range rig.store.calls[before:] {
			if call != "AccessSite" {
				t.Fatalf("a row that is not the stamped site went on to read %s", call)
			}
		}
	})

	// The edge stamps the site bare and the owner as the row spells it, which is canonical,
	// and neither spelling is what makes a stamp good or bad.
	t.Run("a canonical spelling of the right site and owner is the right site and owner", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.store.answersAnySite = true
		canonicalOwner := "v1:identity:user:" + accessTestOwner
		rig.store.admits[canonicalOwner] = true
		rig.stamp[AccessStampSiteHeader] = "v1:platform:site:" + accessTestSite
		rig.stamp[AccessStampOwnerHeader] = canonicalOwner
		if rec := rig.unlock(accessTestCode); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a row with no owner refuses every stamp", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.store.site.OwnerUserID = ""
		assertAccessReply(t, rig.unlock(accessTestCode), http.StatusNotFound, `{"error":"not_found"}`)
	})

	t.Run("a site with no package deployable has no codes to open", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.store.site.DeployableName = "  "
		assertAccessReply(t, rig.unlock(accessTestCode), http.StatusNotFound, `{"error":"not_found"}`)
	})
}

// A SITE WHOSE OWNER WAS REMOVED FROM ITS ORGANIZATION reads as zero rows under that
// owner, so all its codes answer 404. That fails closed and stays that way, and the 404
// has to stay the one a stranger gets, but an operator has to be able to see why a site's
// codes stopped working, and without this line nothing anywhere says so.
func TestAStampThatDoesNotCheckOutIsLoggedAndStillTheOne404(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(*accessRig)
		reason  string
	}{
		"the stamped owner cannot read the site, as one removed from its organization cannot": {
			arrange: func(r *accessRig) { r.store.admits = map[string]bool{} },
			reason:  "the stamped owner cannot read this site",
		},
		"the site is read but the stamp does not name its owner": {
			arrange: func(r *accessRig) { r.stamp[AccessStampOwnerHeader] = accessTestMember },
			reason:  "the stamped owner is not this site's owner",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t)
			token := rig.open()
			rig.logs.Reset()
			tc.arrange(rig)

			const visitorWrote = "something-only-the-visitor-sent"
			for op, rec := range map[string]*httptest.ResponseRecorder{
				"unlock":  rig.unlock(visitorWrote),
				"session": rig.session(token),
				"leave":   rig.leave(token),
				"call":    rig.call(token, visitorWrote, map[string]any{"window": visitorWrote}),
			} {
				t.Run(op, func(t *testing.T) {
					assertAccessReply(t, rec, http.StatusNotFound, `{"error":"not_found"}`)
					assertNoAccessCookie(t, rec)
				})
			}

			logged := rig.logs.String()
			if got := strings.Count(logged, "level=WARN"); got != 4 {
				t.Fatalf("logged %d WARN lines for 4 requests:\n%s", got, logged)
			}
			for _, want := range []string{tc.reason, "subject.id=" + accessTestSite, "subject.concept=v1:platform:site"} {
				if !strings.Contains(logged, want) {
					t.Fatalf("the line does not carry %q:\n%s", want, logged)
				}
			}
			for what, secret := range map[string]string{
				"what the visitor sent": visitorWrote,
				"the token":             token,
				"the stamped owner":     accessTestMember,
				"the token's digest":    memql.AccessSessionTokenDigest(token),
			} {
				if strings.Contains(logged, secret) {
					t.Fatalf("the line holds %s:\n%s", what, logged)
				}
			}
		})
	}

	// NOT EVERY 404 IS THIS ONE. A site with no package deployable has no codes to stop
	// working, a request with no stamp is not about a site at all, and an operator told
	// about those would learn to read past the one that matters.
	t.Run("a site with no package deployable says nothing", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.store.site.DeployableName = ""
		assertAccessReply(t, rig.session(""), http.StatusNotFound, `{"error":"not_found"}`)
		if logged := rig.logs.String(); logged != "" {
			t.Fatalf("logged:\n%s", logged)
		}
	})

	t.Run("a request with no stamp says nothing", func(t *testing.T) {
		rig := newAccessRig(t)
		rig.stamp = nil
		assertAccessReply(t, rig.session(""), http.StatusNotFound, `{"error":"not_found"}`)
		if logged := rig.logs.String(); logged != "" {
			t.Fatalf("logged:\n%s", logged)
		}
	})
}

func TestOnlyTheFourRoutesAnswer(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/access/unlock"},
		{http.MethodPost, "/access/session"},
		{http.MethodGet, "/access/leave"},
		{http.MethodGet, "/access/call"},
		{http.MethodDelete, "/access/call"},
		{http.MethodPut, "/access/unlock"},
		{http.MethodOptions, "/access/unlock"},
		{http.MethodHead, "/access/session"},
		{http.MethodPost, "/access/"},
		{http.MethodGet, "/access/"},
		{http.MethodPost, "/access/unlock/"},
		{http.MethodPost, "/access/unlock/deeper"},
		{http.MethodPost, "/access/nope"},
		{http.MethodPost, "/accessories/unlock"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rig := newAccessRig(t)
			rec := rig.send(tc.method, tc.path, "{}", "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d", rec.Code)
			}
			if tc.method != http.MethodHead && rec.Body.String() != `{"error":"not_found"}` {
				t.Fatalf("body = %q", rec.Body.String())
			}
			if len(rig.store.calls) != 0 {
				t.Fatalf("store calls = %v", rig.store.calls)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// when the store fails
// ---------------------------------------------------------------------------

// A FAILED READ IS NOT AN ANSWER. It opens nothing, ends nothing and clears nothing:
// a database blip must not read as "your code is wrong", and a cookie cleared on a
// blip would take out a visitor whose session the row still honours. The second read of
// a call, with rows in hand, is the one exception, and it has its own cases in
// TestAnEndLandingDuringACallDeliversNothing.
func TestAStoreFailureOpensNothingAndEndsNothing(t *testing.T) {
	failure := errors.New("the store failed")
	for _, tc := range []struct {
		name, fails, op string
	}{
		{"the site read, on unlock", "AccessSite", "unlock"},
		{"the site read, on the session", "AccessSite", "session"},
		{"the site read, on leave", "AccessSite", "leave"},
		{"the site read, on a call", "AccessSite", "call"},
		{"the code read, on unlock", "CodeByHash", "unlock"},
		{"the session write", "CreateSession", "unlock"},
		{"the session read, on the session", "SessionByTokenHash", "session"},
		{"the session read, on a call", "SessionByTokenHash", "call"},
		{"the session read, on leave", "SessionByTokenHash", "leave"},
		{"the code read, on the session", "CodeByID", "session"},
		{"the code read, on a call", "CodeByID", "call"},
		{"the end of the session", "EndSession", "leave"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAccessRig(t)
			token := rig.open()
			created := len(rig.store.created)
			rig.store.fail[tc.fails] = failure

			rec := accessEveryOperationOf(rig, tc.op, token)

			assertAccessReply(t, rec, http.StatusBadGateway, `{"error":"refused"}`)
			assertNoAccessCookie(t, rec)
			if len(rig.engine.calls) != 0 {
				t.Fatalf("reached the engine: %v", rig.engine.calls)
			}
			if len(rig.store.created) != created {
				t.Fatal("a failure wrote a session")
			}
			delete(rig.store.fail, tc.fails)
			if rec := rig.session(token); rec.Code != http.StatusOK {
				t.Fatalf("the session did not survive the failure: %d", rec.Code)
			}
		})
	}
}

func accessEveryOperationOf(r *accessRig, op, token string) *httptest.ResponseRecorder {
	switch op {
	case "unlock":
		return r.unlock(accessTestCode)
	case "session":
		return r.session(token)
	case "leave":
		return r.leave(token)
	default:
		return r.call(token, "deskBriefs", nil)
	}
}

// ---------------------------------------------------------------------------
// reaching the handler at all
// ---------------------------------------------------------------------------

func accessAuthenticatedHandler(h http.Handler) http.Handler {
	return verifier.HTTPMiddleware(&verifier.Verifier{}, verifier.MiddlewareOptions{
		PublicPaths: PublicPaths(), SelfAuthenticatedPaths: SelfAuthenticatedPaths(),
	})(h)
}

// A VISITOR CARRIES NO MEMQL CREDENTIAL AND NEVER WILL (app/auth_mode_edge.go), and
// the bff installs a verifier ahead of its mux. HandlerAuthorizedPaths() does not
// make it step aside, since that list is read only where no verifier runs, so
// without this every visitor's request is a 401 that names nothing.
func TestAccessRoutesReachTheHandlerWithoutABearer(t *testing.T) {
	for name, tc := range map[string]struct {
		method, path, body string
		status             int
	}{
		"unlock":  {http.MethodPost, "/access/unlock", `{"code":"` + accessTestCode + `"}`, http.StatusOK},
		"session": {http.MethodGet, "/access/session", "", http.StatusUnauthorized},
		"leave":   {http.MethodPost, "/access/leave", "{}", http.StatusNoContent},
		"call":    {http.MethodPost, "/access/call", `{"call":"deskBriefs"}`, http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			for key, value := range rig.stamp {
				req.Header.Set(key, value)
			}
			rec := httptest.NewRecorder()
			accessAuthenticatedHandler(rig.h).ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if rig.store.called("AccessSite") != 1 {
				t.Fatal("the request did not reach the handler")
			}
		})
	}
}

// THE VERIFIER STEPS ASIDE FOR FOUR EXACT ROUTES AND NOTHING ELSE, so a route mounted
// beneath the prefix later does not inherit a bypass nobody decided on.
func TestAccessSelfAuthenticationDoesNotExemptOtherPaths(t *testing.T) {
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	for _, path := range []string{
		"/access", "/access/", "/access/nope", "/access/unlock/", "/access/unlock/deeper",
		"/access/call/x", "/access%2Funlock", "/%61ccess/unlock", "/access/unlock%20",
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

func TestAccessSurfaceDeclarations(t *testing.T) {
	t.Run("the bff mounts one base", func(t *testing.T) {
		t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
		if got, want := AccessSurfacePaths(), []string{"/access/"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("AccessSurfacePaths() = %v, want %v", got, want)
		}
	})

	t.Run("the exact routes carry the base", func(t *testing.T) {
		t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "/tenant")
		want := []string{
			"/access/unlock", "/tenant/access/unlock",
			"/access/session", "/tenant/access/session",
			"/access/leave", "/tenant/access/leave",
			"/access/call", "/tenant/access/call",
		}
		if got := AccessSelfAuthenticatedPaths(); !reflect.DeepEqual(got, want) {
			t.Fatalf("AccessSelfAuthenticatedPaths() = %v, want %v", got, want)
		}
		if err := AssertSelfAuthenticatedRoutesFailClosed(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("under a base, the based path answers too", func(t *testing.T) {
		rig := newAccessRig(t)
		t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "/tenant")
		rec := rig.send(http.MethodGet, "/tenant/access/session", "", "")
		assertAccessReply(t, rec, http.StatusUnauthorized, `{"state":"none"}`)
	})

	t.Run("the boot assertion accounts for the mount", func(t *testing.T) {
		t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
		var routes []string
		for _, path := range AccessSurfacePaths() {
			routes = append(routes, "GET "+path, "POST "+path)
		}
		if err := AssertUnauthenticatedSurfaceDeclared(routes); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("it is not public", func(t *testing.T) {
		t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
		for _, path := range PublicPaths() {
			if strings.HasPrefix(path, "/access") {
				t.Fatalf("%q is in PublicPaths(), which every verifier-consuming node walks as an open prefix", path)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// the counter
// ---------------------------------------------------------------------------

func TestAccessRequestsAreCounted(t *testing.T) {
	counted := func(op, result string, act func()) {
		t.Helper()
		before := metrics.SiteAccessRequestValue(op, result)
		act()
		if after := metrics.SiteAccessRequestValue(op, result); after != before+1 {
			t.Fatalf("memql_site_access_requests_total{op=%q,result=%q} moved from %v to %v", op, result, before, after)
		}
	}
	rig := newAccessRig(t)
	var token string
	counted("unlock", "ok", func() { token = rig.open() })
	counted("unlock", "invalid", func() { rig.unlock("00000-00000-00000-00000-00000") })
	counted("session", "ok", func() { rig.session(token) })
	counted("session", "none", func() { rig.session("") })
	counted("session", "ended", func() { rig.session("mql_acs_nothing") })
	counted("call", "ok", func() { rig.call(token, "deskBriefs", nil) })
	counted("call", "not_found", func() { rig.call(token, "nobodyDeclaredThis", nil) })
	counted("call", "invalid_args", func() { rig.call(token, "deskBriefs", map[string]any{"bogus": 1}) })
	rig.engine.err = errors.New("refused")
	counted("call", "refused", func() { rig.call(token, "deskBriefs", nil) })
	rig.engine.err = nil
	counted("leave", "ok", func() { rig.leave(token) })
	rig.clock.at = accessT0.Add(2 * time.Hour)
	counted("unlock", "expired", func() { rig.unlock(accessTestCode) })
}
