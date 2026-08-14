package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/adminops"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// cors_test.go -- memql#3716, and the first test of this middleware there has
// ever been.
//
// # WHAT IS ACTUALLY AT STAKE
//
// A matched origin gets `Access-Control-Allow-Origin: <origin>` AND
// `Access-Control-Allow-Credentials: true`, on the routes that carry the
// refresh-token cookie. So an entry on this allowlist is permission to make
// cookie-bearing requests to identity and READ THE RESPONSES -- session theft
// adjacent, not a convenience header.
//
// # THE ESCALATION THESE TESTS EXIST TO FORBID
//
// The obvious way to make the allowlist graph-backed is to derive it from the
// registered OAuth clients' redirect URIs. `POST /register` is RFC 7591 dynamic
// client registration: deliberately UNAUTHENTICATED (the endpoint exists so
// strangers can self-register), enabled by default, and it writes a
// v1:identity:oauthClient row. Derive the allowlist from those rows and a
// stranger POSTs `redirect_uris: ["https://evil.example/cb"]` and
// `https://evil.example` becomes a credentialed-CORS origin with no
// authentication anywhere in the path.
//
// So registration stays open and the ALLOWANCE is a separate, explicit
// owner/admin act on the same row. TestDCRRegisteredClientIsRefusedUntilGranted
// is that statement, asserted end to end through the real registration handler,
// the real owner/admin gate and the real middleware. It is the acceptance
// criterion of the issue: if it goes green for the wrong reason -- because the
// grant mechanism does not work at all -- the positive control inside it fails.

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// corsGraphEngine is the v1:identity:oauthClient table in a map. It answers the
// four statements this path issues -- the DCR insert, the by-id lookup, the
// admin CORS write, and the middleware's granted-origin read -- so one fixture
// drives registration, the gated grant and the middleware over one graph.
//
// A fake rather than a database because what is under test is the DECISION --
// who may set an allowance, and whether the middleware honours it live -- not
// the SQL. Same reasoning as component/identity/adminops/gate_test.go, which
// stubs its engine for the same surface.
type corsGraphEngine struct {
	mu sync.Mutex
	// rows is clientId -> corsOriginsJSON. A registered client with no
	// allowance is present with an empty value, which is exactly the state a
	// DCR row is created in.
	rows map[string]string
	// readErr fails the granted-origin read ONLY. Scoped that narrowly so a
	// test can ask what the middleware does with an unreachable graph without
	// breaking the rest of the fixture.
	readErr error
	// reads counts granted-origin reads, so the TTL window is observable
	// without sleeping.
	reads int
}

func newCORSGraphEngine() *corsGraphEngine {
	return &corsGraphEngine{rows: map[string]string{}}
}

func (e *corsGraphEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	switch {
	case strings.HasPrefix(q, "mutation createOAuthClient("):
		e.rows[extractField(q, "clientId")] = ""
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil

	case strings.HasPrefix(q, "mutation setOAuthClientCORSOrigins("):
		clientId := extractField(q, "clientId")
		if _, present := e.rows[clientId]; !present {
			return nil, errors.New("corsGraphEngine: update of a row that does not exist: " + clientId)
		}
		e.rows[clientId] = extractField(q, "corsOriginsJSON")
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil

	case strings.HasPrefix(q, "query oAuthClientByClientId("):
		clientId := extractField(q, "clientId")
		grant, present := e.rows[clientId]
		if !present {
			return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{
			Nodes: []*memqlv1.MemoryNode{oauthClientNode(clientId, grant)},
		}}, nil

	case strings.HasPrefix(q, "query oAuthClientCORSGrants("):
		e.reads++
		if e.readErr != nil {
			return nil, e.readErr
		}
		// The query's own filter is `corsOriginsJSON != ""`, so a row with no
		// allowance never reaches the caller. The fake applies the same
		// predicate: a fixture looser than the construct it stands in for
		// would let the middleware pass on rows production never hands it.
		var nodes []*memqlv1.MemoryNode
		for clientId, grant := range e.rows {
			if grant == "" {
				continue
			}
			nodes = append(nodes, oauthClientNode(clientId, grant))
		}
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}, nil
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

func oauthClientNode(clientId, corsOriginsJSON string) *memqlv1.MemoryNode {
	return &memqlv1.MemoryNode{
		Id: clientId,
		Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
			"clientId":        structpb.NewStringValue(clientId),
			"corsOriginsJSON": structpb.NewStringValue(corsOriginsJSON),
		}},
	}
}

func (e *corsGraphEngine) grantReadCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reads
}

func (e *corsGraphEngine) failGrantReads(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.readErr = err
}

// newCORSTestServer builds the Server under test with envOrigins as the
// boot-time MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS list.
//
// The granted-origin window is installed with ttl 0 -- read on every call --
// so "takes effect with no restart" is observable without a test sleeping
// through the production window. The window itself is covered separately by
// TestGrantedOriginReadIsBoundedByTheTTLWindow.
func newCORSTestServer(eng identity.EngineExecutor, envOrigins ...string) *Server {
	s := &Server{
		Cfg: identity.Config{
			OAuthDCREnabled:    true,
			CORSAllowedOrigins: envOrigins,
		},
		Store: &identity.Store{Engine: eng},
	}
	s.corsGrants = &grantedOrigins{read: s.readGrantedCORSOrigins}
	return s
}

// preflight drives one CORS preflight through the middleware and returns the
// recorder. OPTIONS with an Origin is the shape a browser actually sends first,
// and it is unauthenticated -- which is why the middleware cannot resolve the
// allowance from a caller.
func preflight(t *testing.T, s *Server, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodOptions, "/auth/refresh", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	s.cors(s.handleOptions)(rec, req)
	return rec
}

// assertOriginRefused checks that neither CORS header came back.
//
// READ THE TWO HALVES DIFFERENTLY. The Access-Control-Allow-Credentials half is
// load-bearing and true of the deployed chain: it is the header that lets the
// calling page read a cookie-bearing response, and this middleware is the only
// thing on these routes that ever emits it.
//
// The Access-Control-Allow-Origin half is true of THIS MIDDLEWARE IN ISOLATION
// and not of the deployed chain. Identity's routes also pass through
// component/server's generic corsMiddleware (SERVER_ALLOWED_ORIGINS), which is
// "*" on identity today -- and under a wildcard that layer echoes ACAO for any
// origin while deliberately withholding Credentials. So in production a refused
// origin may still see an ACAO it cannot do anything with. Do not read a green
// run here as "no ACAO reaches the browser"; read it as "this middleware did not
// grant it".
func assertOriginRefused(t *testing.T, s *Server, origin, why string) {
	t.Helper()
	rec := preflight(t, s, origin)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q for %s, want it ABSENT -- %s",
			got, origin, why)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q for %s, want it ABSENT -- %s\n"+
			"This header is the whole exposure: with it the browser hands the response "+
			"body to the calling page, cookies and all.", got, origin, why)
	}
}

func assertOriginAllowed(t *testing.T, s *Server, origin, why string) {
	t.Helper()
	rec := preflight(t, s, origin)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("Access-Control-Allow-Origin = %q for %s, want %q -- %s",
			got, origin, origin, why)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q for %s, want \"true\" -- %s",
			got, origin, why)
	}
}

// registerDCRClient self-registers through the REAL POST /register handler and
// returns the minted client_id. Unauthenticated, exactly as a stranger reaches
// it.
func registerDCRClient(t *testing.T, s *Server, redirectURI string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"client_name":   "Someone Else's Connector",
		"redirect_uris": []string{redirectURI},
	})
	if err != nil {
		t.Fatalf("marshal registration body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleRegister(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /register = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp registerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode registration response: %v; body=%s", err, rec.Body.String())
	}
	if resp.ClientId == "" {
		t.Fatal("POST /register returned no client_id")
	}
	return resp.ClientId
}

// capturingAudit collects the audit trail the grant leaves behind.
type capturingAudit struct {
	mu     sync.Mutex
	events []identity.AuditEvent
}

func (a *capturingAudit) Log(_ context.Context, ev identity.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
}

func (a *capturingAudit) all() []identity.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]identity.AuditEvent(nil), a.events...)
}

// newGrantService builds the real owner/admin-gated write surface over the same
// graph the middleware reads. Not a stand-in for it: this is the only path that
// can set an allowance.
func newGrantService(t *testing.T, eng identity.EngineExecutor) (*adminops.Service, *capturingAudit) {
	t.Helper()
	audit := &capturingAudit{}
	svc, err := adminops.New(&adminops.Service{Engine: eng, Audit: audit})
	if err != nil {
		t.Fatalf("adminops.New: %v", err)
	}
	return svc, audit
}

// ctxAsOwner is the stream context an authenticated cluster owner arrives on.
func ctxAsOwner() context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId:       "v1:identity:user:owner",
		PrimaryEmail: "owner@example.test",
		Role:         auth.RoleOwner,
		IdentityId:   "v1:identity:identity:owner",
	})
}

// ---------------------------------------------------------------------------
// Assertion 1 -- the acceptance criterion
// ---------------------------------------------------------------------------

// TestDCRRegisteredClientIsRefusedUntilGranted is the issue.
//
// A stranger self-registers and their origin is REFUSED credentialed CORS. Then
// an owner grants it on the same row, in the same process, and it is allowed.
//
// The grant half is a POSITIVE CONTROL, and it is not optional: without it the
// refusal is satisfied by a middleware that refuses everything, which is what
// the code did before this change and is not the property being claimed. Read
// the two halves together -- "refused until granted" is one statement.
func TestDCRRegisteredClientIsRefusedUntilGranted(t *testing.T) {
	const stranger = "https://evil.example"

	eng := newCORSGraphEngine()
	// The env list carries what a real deployment boots with. The stranger's
	// origin is deliberately not on it, and nothing in the flow below adds it.
	srv := newCORSTestServer(eng, "https://identity.example.test", "https://app.example.test")

	// The unauthenticated RFC 7591 registration a stranger can perform today.
	// The redirect URI's origin is the one they would want on the allowlist.
	clientId := registerDCRClient(t, srv, stranger+"/cb")

	assertOriginRefused(t, srv, stranger,
		"registering an OAuth client is UNAUTHENTICATED, so it must grant nothing. "+
			"An allowlist derived from redirectURIsJSON would hand this origin "+
			"credentialed read access to identity for the price of one anonymous POST.")

	// Positive control: the one path that CAN grant it.
	grants, audit := newGrantService(t, eng)
	res := grants.SetOAuthClientCORSOrigins(ctxAsOwner(), clientId, []string{stranger})
	if !res.OK {
		t.Fatalf("an owner's grant failed: code=%d %s", res.Code, res.ErrorMessage)
	}

	assertOriginAllowed(t, srv, stranger,
		"an owner granted this origin on the client's row, and the middleware reads "+
			"the granted set live -- so it must take effect on the very next request, "+
			"with no restart and no new Server")

	// The trail is the point of gating a trust decision: it must name WHO and
	// WHICH origins.
	events := audit.all()
	if len(events) != 1 {
		t.Fatalf("want exactly 1 audit event for the grant, got %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Outcome != identity.AuditOutcomeSuccess {
		t.Errorf("audit outcome = %q, want success", ev.Outcome)
	}
	if ev.ActorUserId != "v1:identity:user:owner" {
		t.Errorf("audit actor = %q, want the granting owner", ev.ActorUserId)
	}
	if detail, _ := ev.Detail["origins"].([]string); len(detail) != 1 || detail[0] != stranger {
		t.Errorf("audit detail origins = %#v, want [%q] -- a trust decision whose "+
			"trail does not record what was trusted is not audited", ev.Detail["origins"], stranger)
	}
}

// ---------------------------------------------------------------------------
// Assertions 3 + 4 -- a grant, and a revoke, take effect with no restart
// ---------------------------------------------------------------------------

// TestGrantAndRevokeTakeEffectWithoutRestart pins both directions against ONE
// Server: no second construction, no re-read of the environment, no reload of
// Cfg. That is what "without a restart" has to mean to be worth anything -- the
// bug being fixed is that `allowed := s.Cfg.CORSAllowedOrigins` was captured
// once, at wrap-registration time, outside the returned closure.
//
// Revoke is asserted in the same test as grant deliberately. A revoke that
// never lands is the dangerous failure -- an origin the operator believes they
// took away -- and testing it right after the grant, on the same Server, is the
// only arrangement that can tell "revoked" apart from "was never granted".
func TestGrantAndRevokeTakeEffectWithoutRestart(t *testing.T) {
	const customer = "https://shop.customer.test"

	eng := newCORSGraphEngine()
	srv := newCORSTestServer(eng, "https://identity.example.test")
	clientId := registerDCRClient(t, srv, customer+"/auth/callback")
	grants, _ := newGrantService(t, eng)

	assertOriginRefused(t, srv, customer, "no grant exists yet")

	if res := grants.SetOAuthClientCORSOrigins(ctxAsOwner(), clientId, []string{customer}); !res.OK {
		t.Fatalf("grant failed: code=%d %s", res.Code, res.ErrorMessage)
	}
	assertOriginAllowed(t, srv, customer, "the grant is live on the same Server")

	// An EMPTY list is the revoke: the argument is the whole allowance, not an
	// addition to it, so there is no second verb to get out of step with this
	// one.
	if res := grants.SetOAuthClientCORSOrigins(ctxAsOwner(), clientId, nil); !res.OK {
		t.Fatalf("revoke failed: code=%d %s", res.Code, res.ErrorMessage)
	}
	assertOriginRefused(t, srv, customer,
		"the allowance was revoked on the same Server -- if this still passes, an "+
			"operator cannot take back credentialed access without a redeploy")
}

// ---------------------------------------------------------------------------
// Assertion 5 -- the env list still works standalone
// ---------------------------------------------------------------------------

// TestEnvAllowlistWorksWithNoGrantedRows is the fresh-cluster case: nothing has
// been granted, no oauthClient row exists, and the origins a deployment boots
// with must work anyway. Otherwise this change breaks every cluster on upgrade.
func TestEnvAllowlistWorksWithNoGrantedRows(t *testing.T) {
	eng := newCORSGraphEngine()
	srv := newCORSTestServer(eng,
		"https://identity.example.test",
		"https://api.example.test",
		// Deliberately spaced + differently cased: the env list is a
		// comma-separated string an operator typed, and the pre-existing
		// TrimSpace / EqualFold semantics are part of the contract.
		"  https://Portal.Example.Test  ",
	)

	for _, origin := range []string{
		"https://identity.example.test",
		"https://api.example.test",
		"https://portal.example.test",
	} {
		assertOriginAllowed(t, srv, origin, "it is on the boot-time env list")
	}

	// Measured BEFORE the refusal below, which legitimately consults the graph:
	// an origin on neither list can only be refused once both have been checked.
	// What this pins is that a HIT on the env list never reaches the graph at
	// all -- a deployment whose engine is still coming up has to be able to
	// serve its own login page.
	if n := eng.grantReadCount(); n != 0 {
		t.Errorf("the graph was read %d time(s) for origins the env list already covers -- "+
			"the in-memory list is checked first precisely so it cannot be made to "+
			"depend on a database", n)
	}

	assertOriginRefused(t, srv, "https://nobody.example.test", "it is on neither list")
}

// ---------------------------------------------------------------------------
// Assertion 6 -- a graph read failure fails CLOSED
// ---------------------------------------------------------------------------

// TestGraphReadFailureFailsClosedAndLeavesEnvListWorking pins the failure
// direction. An unreachable engine must not start allowing origins, and must
// not stop allowing the ones the environment configured.
//
// It also pins the harder half: a previously-granted origin stops being
// allowed. Serving a set we can no longer verify would let an outage outlive a
// REVOKE, and revoking is half of what this surface exists to do. Refusing is
// recoverable -- the next preflight retries and succeeds the moment the graph
// is back.
func TestGraphReadFailureFailsClosedAndLeavesEnvListWorking(t *testing.T) {
	const customer = "https://shop.customer.test"

	eng := newCORSGraphEngine()
	srv := newCORSTestServer(eng, "https://identity.example.test")
	clientId := registerDCRClient(t, srv, customer+"/cb")
	grants, _ := newGrantService(t, eng)
	if res := grants.SetOAuthClientCORSOrigins(ctxAsOwner(), clientId, []string{customer}); !res.OK {
		t.Fatalf("grant failed: code=%d %s", res.Code, res.ErrorMessage)
	}
	assertOriginAllowed(t, srv, customer, "the grant is live before the graph goes away")

	eng.failGrantReads(errors.New("dial tcp 10.0.0.1:5432: connect: connection refused"))

	assertOriginRefused(t, srv, customer,
		"the granted set could not be read, so it is not in effect. Falling back to a "+
			"stale copy would make a revoke survivable by an outage")
	assertOriginAllowed(t, srv, "https://identity.example.test",
		"the env list is in memory and owes the graph nothing -- identity must keep "+
			"serving its own login page through a database outage")
}

// ---------------------------------------------------------------------------
// Assertion 7 -- "*" grants nothing on the credentialed path
// ---------------------------------------------------------------------------

// TestWildcardDoesNotGrantCredentialedAccess covers the bypass that made the
// whole gate decorative.
//
// originAllowed used to return true for a "*" entry under a comment saying the
// wildcard was "only ever used for non-credentialed requests". cors() sets
// Access-Control-Allow-Credentials unconditionally on every match, so that
// comment described a property the code did not have: one "*" anywhere granted
// credentialed cross-origin read access to every origin on the internet.
//
// Both sources are covered, because the wildcard has to be refused wherever it
// comes from -- an operator's env var and an admin's grant alike.
func TestWildcardDoesNotGrantCredentialedAccess(t *testing.T) {
	t.Run("from the env list", func(t *testing.T) {
		eng := newCORSGraphEngine()
		srv := newCORSTestServer(eng, "*", "https://identity.example.test")

		assertOriginRefused(t, srv, "https://evil.example",
			`a "*" entry must not match an arbitrary origin on a path that always sets `+
				`Access-Control-Allow-Credentials: true`)
		assertOriginAllowed(t, srv, "https://identity.example.test",
			`the explicit entry beside the "*" still works -- refusing the wildcard `+
				`must not poison the rest of the list`)
	})

	t.Run("from a granted row", func(t *testing.T) {
		eng := newCORSGraphEngine()
		srv := newCORSTestServer(eng, "https://identity.example.test")
		clientId := registerDCRClient(t, srv, "https://shop.customer.test/cb")
		grants, _ := newGrantService(t, eng)

		// Refused at the WRITE, which is where a human gets an error message.
		res := grants.SetOAuthClientCORSOrigins(ctxAsOwner(), clientId, []string{"*"})
		if res.OK {
			t.Fatal(`an admin was permitted to grant "*" -- that is one call away from ` +
				`credentialed CORS for every origin on the internet`)
		}
		if res.Code != adminops.CodeInvalidArgument {
			t.Errorf("code = %d, want %d (INVALID_ARGUMENT)", res.Code, adminops.CodeInvalidArgument)
		}
		assertOriginRefused(t, srv, "https://evil.example", "the write was refused, so nothing was granted")
	})

	t.Run("from a row that already holds one", func(t *testing.T) {
		// Write validation is not the last line: a row can hold a wildcard that
		// no longer reachable path put there, and the middleware must still
		// refuse it. Injected directly into the fixture graph for that reason --
		// the point is what the READ does with a value the write would reject.
		eng := newCORSGraphEngine()
		eng.rows["mcp_legacy"] = `["*"]`
		srv := newCORSTestServer(eng, "https://identity.example.test")

		assertOriginRefused(t, srv, "https://evil.example",
			`a "*" already sitting on a row must not be honoured either -- validating `+
				`only on the way in leaves the read trusting whatever is stored`)
	})
}

// ---------------------------------------------------------------------------
// The TTL window
// ---------------------------------------------------------------------------

// TestGrantedOriginReadIsBoundedByTheTTLWindow covers the reason the window
// exists at all.
//
// cors() wraps ~15 routes, including every unauthenticated OPTIONS preflight
// and POST /register. Without a window, a flood of unknown origins is one
// database query per request against the auth surface. The window bounds that
// to one query per interval while keeping "a grant needs no restart" true --
// the delay is bounded and does not require an operator to do anything.
//
// Clock-driven rather than sleeping: a test that sleeps through a real 10
// seconds is a test people delete.
func TestGrantedOriginReadIsBoundedByTheTTLWindow(t *testing.T) {
	eng := newCORSGraphEngine()
	eng.rows["mcp_customer"] = `["https://shop.customer.test"]`

	clock := time.Unix(1_700_000_000, 0).UTC()
	srv := &Server{
		Cfg:   identity.Config{CORSAllowedOrigins: []string{"https://identity.example.test"}},
		Store: &identity.Store{Engine: eng},
	}
	srv.corsGrants = &grantedOrigins{
		ttl: 10 * time.Second,
		now: func() time.Time { return clock },
	}
	srv.corsGrants.read = srv.readGrantedCORSOrigins

	for i := 0; i < 5; i++ {
		assertOriginAllowed(t, srv, "https://shop.customer.test", "the row grants it")
	}
	if n := eng.grantReadCount(); n != 1 {
		t.Fatalf("granted-origin reads = %d across 5 requests in one window, want 1 -- "+
			"the window is what keeps an unauthenticated preflight flood from being one "+
			"query per request", n)
	}

	clock = clock.Add(11 * time.Second)
	assertOriginAllowed(t, srv, "https://shop.customer.test", "the row still grants it")
	if n := eng.grantReadCount(); n != 2 {
		t.Fatalf("granted-origin reads = %d after the window elapsed, want 2 -- a window "+
			"that never expires is a boot-time snapshot, which is the bug being fixed", n)
	}
}

// TestNoStoreMeansEnvListOnly covers a binary wired without a graph handle at
// all. It must serve the env list and not error, which is the pre-memql#3716
// behaviour and correct for it.
//
// Both half-wired shapes are covered, and the second is the one that matters
// (fix round 1, item 3): a Store whose Engine is nil. identity.Store's
// executeAndExtract dereferences Engine unguarded, so before the guard this
// panicked -- and these are UNAUTHENTICATED routes, so the panic needed no
// credential to reach. Guarding only `Store == nil` was survivable while every
// caller of the store was an authenticated handler; the CORS path is the first
// that is not.
func TestNoStoreMeansEnvListOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store *identity.Store
	}{
		{"no store at all", nil},
		{"a store with no engine", &identity.Store{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{
				Cfg:   identity.Config{CORSAllowedOrigins: []string{"https://identity.example.test"}},
				Store: tc.store,
			}
			assertOriginAllowed(t, srv, "https://identity.example.test", "the env list needs no graph")
			assertOriginRefused(t, srv, "https://evil.example", "there is no graph to grant anything")
		})
	}
}

// TestGrantedOriginReadCarriesADeadline covers fix round 1, item 2.
//
// The read runs while `current` holds the mutex, and `sync.Mutex.Lock` is not
// context-aware -- a waiter cannot be cancelled by its own client hanging up. So
// the only thing that drains a queue of parked preflights is the holder
// finishing, which makes an unbounded holder unbounded goroutine accumulation on
// an unauthenticated surface during a database outage.
//
// Nothing else on the path supplies a deadline: `r.Context()` arrives without
// one, component/server's timeouts are connection-level, and what terminated the
// hang case before this was bun's pgdriver socket deadline -- a dependency's
// default rather than anything this package states.
func TestGrantedOriginReadCarriesADeadline(t *testing.T) {
	var (
		sawDeadline bool
		remaining   time.Duration
	)
	srv := &Server{Cfg: identity.Config{CORSAllowedOrigins: []string{"https://identity.example.test"}}}
	srv.corsGrants = &grantedOrigins{
		read: func(ctx context.Context) ([]string, error) {
			deadline, ok := ctx.Deadline()
			sawDeadline = ok
			if ok {
				remaining = time.Until(deadline)
			}
			return nil, nil
		},
	}

	// An origin the env list does NOT cover, so the graph read actually runs.
	assertOriginRefused(t, srv, "https://unknown.example", "nothing grants it")

	if !sawDeadline {
		t.Fatal("the granted-origin read ran with NO deadline. It holds a mutex that waiters " +
			"cannot be cancelled out of, so an unbounded read parks every queued preflight " +
			"until the database answers -- on routes that need no credential to reach.")
	}
	if remaining <= 0 || remaining > grantedOriginReadTimeout {
		t.Errorf("deadline leaves %v, want (0, %v] -- the bound must be this package's own, "+
			"not inherited from something further out", remaining, grantedOriginReadTimeout)
	}
	if grantedOriginReadTimeout >= 10*time.Second {
		t.Errorf("grantedOriginReadTimeout = %v, which is at or above bun's pgdriver ReadTimeout "+
			"default (10s). Above that the socket deadline fires first and the bound is the "+
			"dependency's again, which is the situation this constant exists to end.",
			grantedOriginReadTimeout)
	}
}
