package app

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/server"
)

// The declaration machinery in component/server is tested there. What this
// covers is the wiring: that createHTTPServer actually consults it, and only
// where it should.
//
// Without this, deleting the call is invisible -- the component/server tests
// still pass, because they exercise the function directly. The check would
// become dead code and the identity binary would go back to inheriting its
// unauthenticated surface by omission (memql#2939).

func newSurfaceWiringApp(t *testing.T) (*App, *[]string) {
	t.Helper()
	fatals := []string{}
	a := &App{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.overrides.FatalWithLogger = func(_ *slog.Logger, msg string, args ...any) {
		parts := []string{msg}
		for _, v := range args {
			if s, ok := v.(string); ok {
				parts = append(parts, s)
			} else if err, ok := v.(error); ok {
				parts = append(parts, err.Error())
			}
		}
		fatals = append(fatals, strings.Join(parts, " "))
	}
	// createHTTPServer builds a server after the assertion; a nil-returning
	// override keeps the test on the assertion rather than the transport.
	a.overrides.NewHTTPServer = func(...server.ServerArg) (*server.Server, error) {
		return nil, nil
	}
	// Mirror newApp's defaulting; individual tests substitute their own.
	a.overrides.AssertUnauthenticatedSurface = server.AssertUnauthenticatedSurfaceDeclared
	a.overrides.AssertSelfAuthenticatedSurface = server.AssertSelfAuthenticatedRoutesFailClosed
	return a, &fatals
}

// The live tree is declared, so a nil verifier must NOT fatal. If this fails,
// the identity binary cannot boot.
func TestCreateHTTPServerAcceptsDeclaredSurface(t *testing.T) {
	a, fatals := newSurfaceWiringApp(t)
	a.identityVerifier = nil

	a.createHTTPServer()

	if len(*fatals) != 0 {
		t.Fatalf("a declared surface must boot; got fatal(s): %v", *fatals)
	}
}

// The assertion must be reached on the no-middleware path. Pointing it at an
// undeclared route set proves createHTTPServer consults the declarations rather
// than merely containing a call that never fires.
func TestCreateHTTPServerRejectsUndeclaredSurface(t *testing.T) {
	if err := server.AssertUnauthenticatedSurfaceDeclared(server.ContractRoutes()); err != nil {
		t.Fatalf("precondition: the live surface should be declared, got %v", err)
	}

	a, fatals := newSurfaceWiringApp(t)
	a.identityVerifier = nil

	// Drive the error path rather than merely confirming a good tree boots --
	// that stays true if the assertion is deleted or downgraded to a log line.
	a.overrides.AssertUnauthenticatedSurface = func([]string) error {
		return errors.New("undeclared: /internal/undeclared")
	}

	a.createHTTPServer()

	if len(*fatals) == 0 {
		t.Fatal("an undeclared route must fail the boot -- a warning is exactly the " +
			"signal that went unread when the automations routes were added (#2937)")
	}
	joined := strings.Join(*fatals, " | ")
	if !strings.Contains(joined, "/internal/undeclared") {
		t.Errorf("the fatal must name the offending route so the fix is obvious; got: %s", joined)
	}
}

// The injected seam only proves the wiring if the real assertion is what gets
// injected by default. Without this, replacing the default with a no-op would
// leave every test above green.
func TestNewAppDefaultsToTheRealSurfaceAssertion(t *testing.T) {
	a := newApp(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", Overrides{})
	if a.overrides.AssertUnauthenticatedSurface == nil {
		t.Fatal("newApp must default AssertUnauthenticatedSurface; a nil default would " +
			"panic at boot or, if guarded, skip the check entirely")
	}
	if err := a.overrides.AssertUnauthenticatedSurface(server.ContractRoutes()); err != nil {
		t.Errorf("the default assertion must be the real one and must pass on this tree: %v", err)
	}
}

// The whole-mux branch is chosen from verifierRequired, a build-tag const, so
// in this binary only the contract-only branch would ever execute. Testing the
// assembly directly covers the identity path -- the one where an undeclared
// mux route is actually reachable.
func TestUnauthenticatedSurfaceRoutesIncludeMuxRoutesForIdentity(t *testing.T) {
	a := &App{registeredRoutes: []string{"POST /internal/dump-secrets"}}

	contractOnly := a.unauthenticatedSurfaceRoutes(false)
	for _, r := range contractOnly {
		if strings.Contains(r, "/internal/dump-secrets") {
			t.Fatal("the contract-only branch must not pull in mux routes")
		}
	}

	whole := a.unauthenticatedSurfaceRoutes(true)
	var found bool
	for _, r := range whole {
		if strings.Contains(r, "/internal/dump-secrets") {
			found = true
		}
	}
	if !found {
		t.Fatal("the identity branch must include routes registered on the mux -- " +
			"otherwise a route mounted there is reachable unauthenticated and " +
			"invisible to the check (#2939)")
	}

	// ...and such a route must actually fail the assertion.
	if err := server.AssertUnauthenticatedSurfaceDeclared(whole); err == nil {
		t.Fatal("an undeclared mux route must fail the surface assertion")
	}
}

// Being recorded is not enough -- being recorded IN TIME is the requirement.
//
// createHTTPServer reads a.registeredRoutes once, and every Build runs later
// phases after it (a.cluster() follows it in build_identity, build_default and
// build_cognition). A route mounted after that point is served by the same mux
// having never been checked, and because a compliant a.handleRoute call
// satisfies the AST gate in mux_registration_test.go, nothing else catches it:
// adding one to cluster.go left the whole suite green on both builds. The gate
// constrained HOW a route registers but not WHEN.
func TestRouteRegisteredAfterTheAssertionIsRefused(t *testing.T) {
	a, fatals := newSurfaceWiringApp(t)
	a.identityVerifier = nil
	a.mux = http.NewServeMux()

	noop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	// Before the assertion: the ordinary path, and it must stay quiet.
	a.handleRoute("GET /metrics", noop)
	a.createHTTPServer()
	if len(*fatals) != 0 {
		t.Fatalf("registering before the assertion must be accepted; got fatal(s): %v", *fatals)
	}

	// After it: refused, by both spellings.
	a.handleRoute("GET /internal/late", noop)
	if len(*fatals) == 0 {
		t.Fatal("a route registered after the surface assertion must be refused -- it is " +
			"served by the same mux and the check has already run")
	}
	if !strings.Contains((*fatals)[0], "/internal/late") {
		t.Errorf("the fatal must name the offending route so it can be found, got: %v", *fatals)
	}

	before := len(*fatals)
	a.handleRouteFunc("GET /internal/later", func(http.ResponseWriter, *http.Request) {})
	if len(*fatals) == before {
		t.Error("handleRouteFunc must refuse a late registration too -- otherwise the seal " +
			"is a one-spelling gate and the other spelling walks around it")
	}
}

// The edge node runs with no identity verifier at all (verifierRequired=false,
// app/auth_mode_edge.go, memql#3710) because it is not an auth boundary -- so
// its one registered route, the site-serving mount at "/"
// (app/transport_edge.go's mountEdgeEndpoints), has to survive the same
// unauthenticated-surface boot check every other undeclared route fails.
//
// This does NOT go through createHTTPServer(): the wholeMux branch it would
// take is chosen by `!verifierRequired`, a build-tag const, so a test binary
// compiled without -tags edge would run the identity-only-shaped
// contract-only branch and never look at a.registeredRoutes at all --
// exactly the gap TestUnauthenticatedSurfaceRoutesIncludeMuxRoutesForIdentity
// above already works around for the identity binary's own mux routes, and
// exactly what silently passed here on the first attempt at this test
// (`go test ./app/`, no tags: wholeMux=false, the fatal never fired, for the
// wrong reason). Assembling the whole-mux route list directly, as that test
// does, makes this meaningful under any tag the suite happens to run with.
//
// Exercises the REAL, live PublicPaths() (via server.AssertUnauthenticatedSurfaceDeclared,
// not a stand-in) -- if component/server's EdgePaths() declaration is ever
// removed, this is what catches it.
func TestEdgeRootMountSurvivesTheUnauthenticatedSurfaceAssertion(t *testing.T) {
	// The exact pattern mountEdgeEndpoints registers: no method verb, because
	// the /_memql/* reverse proxy (Task 7, #3712) needs every HTTP method
	// routed through this same mount, not just GET.
	a := &App{registeredRoutes: []string{"/"}}

	whole := a.unauthenticatedSurfaceRoutes(true)
	if err := server.AssertUnauthenticatedSurfaceDeclared(whole); err != nil {
		t.Fatalf("the edge's root mount must be declared public (component/server.EdgePaths) -- %v", err)
	}
}
