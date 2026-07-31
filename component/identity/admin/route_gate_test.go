// Asserts the precondition the internal-origin stamp rests on
// (znasllc-io/memql#2934).
//
// handlers.go stamps auth.ContextWithInternalOrigin onto an INBOUND REQUEST
// context in two places -- queryUsers (activeUsers) and userById
// (userByIdSystem, with a caller-supplied userId). ContextWithInternalOrigin's
// own doc forbids exactly that: "Never call it in a request handler on a
// context derived from an inbound request." The root call_origin_conformance
// gate records component/identity/admin as the one allowed exception, which by
// design makes that gate PASS.
//
// That exception is only sound because every route reaching those two stamps
// is behind requireAdmin. Nothing asserted the two facts were connected: if the
// gate were loosened, or a route registered with `wrap` instead of `gated`, or
// a handler reused from an ungated path, nothing failed -- and the blast radius
// is a cross-user read of any user row through an @serverOnly query.
//
// These tests are that missing link, from both directions:
//
//   - behavioural: `gated` really does reject an unauthenticated caller and a
//     caller whose role is neither owner nor admin, WITHOUT reaching the
//     handler. Loosening requireAdmin fails here.
//   - structural: every route Mount registers goes through `gated`, bar the
//     three auth-establishment routes that cannot require a session to work.
//     Registering a new route with `wrap` fails here.
//
// Note the gate admits owner OR admin (auth.go:62), not cluster-owner only.
package admin

import (
	"context"
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	identityweb "github.com/znasllc-io/memql/component/identity/web"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// routeGateEngine counts Execute calls. Status codes alone are a weak
// assertion -- a 403 from the CSRF middleware looks identical to a 403 from the
// auth gate -- so every rejection assertion also proves the handler body never
// ran by checking this stayed at zero.
type routeGateEngine struct{ calls atomic.Int64 }

func (e *routeGateEngine) Execute(context.Context, string) (*memqlengine.ExecuteResult, error) {
	e.calls.Add(1)
	return &memqlengine.ExecuteResult{}, nil
}

// routeGateSeed is a throwaway 32-byte Ed25519 seed. NewKeyManagerFromSeed
// derives the key in-process, so the test needs no disk and no Load().
var routeGateSeed = base64.StdEncoding.EncodeToString([]byte("memql-2934-route-gate-test-seed!"))

func routeGateConfig() identity.Config {
	return identity.Config{
		BaseURL:     "https://identity.test",
		JWTAudience: "memql-test",
	}
}

// newRouteGateServer builds a fully-wired AdminServer through the real New()
// constructor. The Issuer is concrete (*identity.JWTIssuer, unexported fields),
// so it cannot be faked -- the test mints against the same issuer the gate
// verifies with.
func newRouteGateServer(t *testing.T) (*AdminServer, *identity.JWTIssuer, *routeGateEngine) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := routeGateConfig()

	km, err := identity.NewKeyManagerFromSeed(routeGateSeed)
	if err != nil {
		t.Fatalf("NewKeyManagerFromSeed: %v", err)
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("NewJWTIssuer: %v", err)
	}
	web, err := identityweb.NewServer(cfg, logger, nil)
	if err != nil {
		t.Fatalf("identityweb.NewServer: %v", err)
	}
	eng := &routeGateEngine{}
	srv, err := New(&AdminServer{
		Cfg:       cfg,
		Logger:    logger,
		Issuer:    iss,
		Engine:    eng,
		Keys:      km,
		WebServer: web,
	})
	if err != nil {
		t.Fatalf("admin.New: %v", err)
	}
	return srv, iss, eng
}

func routeGateToken(t *testing.T, iss *identity.JWTIssuer, role string) string {
	t.Helper()
	tok, _, err := iss.IssueAccessToken(identity.IssueInput{
		UserId: "v1:identity:user:route-gate",
		Email:  "route-gate@test.invalid",
		Role:   role,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("IssueAccessToken(role=%q): %v", role, err)
	}
	return tok
}

type adminRoute struct {
	method string
	path   string
}

// gatedRoutes is every route Mount registers behind `gated`.
// TestMountRegistersOnlyKnownRoutes keeps this in step with server.go, so a
// route added there without being added here fails rather than going untested.
var gatedRoutes = []adminRoute{
	{"GET", "/admin/"},
	{"GET", "/admin/users"},
	{"GET", "/admin/users/detail"},
	{"POST", "/admin/users/profile"},
	{"POST", "/admin/users/role"},
	{"POST", "/admin/users/suspend"},
	{"POST", "/admin/users/unsuspend"},
	{"GET", "/admin/audit"},
	{"GET", "/admin/deployments"},
	{"POST", "/admin/deployments/deploy-staging"},
	{"POST", "/admin/deployments/promote"},
	{"POST", "/admin/deployments/rollback"},
	{"POST", "/admin/deployments/rollout"},
	{"GET", "/admin/tokens"},
	{"POST", "/admin/tokens/revoke"},
	{"POST", "/admin/tokens/node/revoke"},
	{"GET", "/admin/jwks"},
	{"POST", "/admin/jwks/rotate"},
	{"GET", "/admin/settings"},
	{"POST", "/admin/settings"},
	{"GET", "/admin/sessions"},
	{"GET", "/admin/invitations"},
	{"GET", "/admin/partition-grants"},
	{"GET", "/admin/access-requests"},
}

// ungatedRoutes cannot require a session: they are how one is established.
// handleEstablish does its own token validation, and /admin/establish is the
// sole CSRF-exempt path for the same reason.
var ungatedRoutes = []adminRoute{
	{"GET", "/admin/login"},
	{"POST", "/admin/establish"},
	{"POST", "/admin/logout"},
}

// routeGateRequest builds a request that isolates the AUTH gate as the thing
// under test.
//
// Two middlewares sit outside requireAdmin and would otherwise reject first,
// making the test pass while proving nothing:
//
//   - CSRF runs before requireAdmin, so a POST with no CSRF pair returns 403
//     from the CSRF layer whether or not the route is auth-gated -- the
//     assertion would still pass with requireAdmin deleted outright.
//   - content negotiation: without Accept: application/json an unauthenticated
//     request is a 303 redirect to /admin/login, not a typed 401.
func routeGateRequest(r adminRoute, token string) *http.Request {
	req := httptest.NewRequest(r.method, r.path, strings.NewReader(""))
	req.Header.Set("Accept", "application/json")
	if r.method != http.MethodGet {
		const csrf = "route-gate-csrf-token"
		req.AddCookie(&http.Cookie{Name: identityweb.CSRFCookieName, Value: csrf})
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestGatedAdminRoutesRejectUnauthenticated(t *testing.T) {
	srv, _, eng := newRouteGateServer(t)
	mux := http.NewServeMux()
	srv.Mount(mux)

	for _, r := range gatedRoutes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			before := eng.calls.Load()
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, routeGateRequest(r, ""))

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("no credentials: got %d, want 401 -- this route reaches the "+
					"internal-origin stamp only because requireAdmin turns it away first",
					rec.Code)
			}
			if got := eng.calls.Load(); got != before {
				t.Errorf("handler body ran despite rejection: engine Execute called %d time(s)",
					got-before)
			}
		})
	}
}

func TestGatedAdminRoutesRejectNonAdminRole(t *testing.T) {
	srv, iss, eng := newRouteGateServer(t)
	mux := http.NewServeMux()
	srv.Mount(mux)

	// A VALID, correctly-signed token for the right audience -- it fails only
	// on role. This is the assertion that would catch requireAdmin's role check
	// being loosened, which a no-credentials test cannot see.
	token := routeGateToken(t, iss, "reader")

	for _, r := range gatedRoutes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			before := eng.calls.Load()
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, routeGateRequest(r, token))

			if rec.Code != http.StatusForbidden {
				t.Errorf("role=reader: got %d, want 403", rec.Code)
			}
			if got := eng.calls.Load(); got != before {
				t.Errorf("handler body ran for a non-admin role: engine Execute called %d time(s)",
					got-before)
			}
		})
	}
}

// Positive control. Without it the two rejection tests above would pass just as
// happily against a mux that rejects everything, or a request builder whose
// URLs never match a route -- neither of which proves the gate does anything.
func TestAdminGateAdmitsAdminRole(t *testing.T) {
	for _, role := range []string{"owner", "admin"} {
		t.Run(role, func(t *testing.T) {
			srv, iss, eng := newRouteGateServer(t)
			mux := http.NewServeMux()
			srv.Mount(mux)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, routeGateRequest(adminRoute{"GET", "/admin/users"}, routeGateToken(t, iss, role)))

			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Fatalf("role=%q was rejected with %d; the gate must admit owner and admin, "+
					"otherwise the rejection tests above prove nothing", role, rec.Code)
			}
			if eng.calls.Load() == 0 {
				t.Error("handler body never ran for an admitted role -- the reach sentinel " +
					"cannot distinguish a working gate from an unreachable route")
			}
		})
	}
}

// The structural half. requireAdmin being correct is worth nothing if a route
// is registered without it, which is a one-word difference at the call site
// (`wrap` vs `gated`) and invisible in review.
func TestMountRegistersOnlyKnownRoutes(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}

	want := map[string]string{}
	for _, r := range gatedRoutes {
		p := r.path
		if p == "/admin/" {
			p = "/admin/{$}" // the registered pattern; "/admin/" is what a client requests
		}
		want[r.method+" "+p] = "gated"
	}
	for _, r := range ungatedRoutes {
		want[r.method+" "+r.path] = "wrap"
	}

	found := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		// The wrapper is the identifier being applied to the handler:
		// gated(s.handleX) / wrap(s.handleX).
		wrapper := "<none>"
		if w, ok := call.Args[1].(*ast.CallExpr); ok {
			if id, ok := w.Fun.(*ast.Ident); ok {
				wrapper = id.Name
			}
		}
		found[pattern] = wrapper
		return true
	})

	if len(found) == 0 {
		t.Fatal("no mux.HandleFunc registrations found in server.go -- this gate has " +
			"stopped resolving routes and would now pass vacuously")
	}

	for pattern, wrapper := range found {
		expected, known := want[pattern]
		if !known {
			t.Errorf("route %q is registered but not covered by this test. Add it to "+
				"gatedRoutes (and it will be checked against the auth gate), or to "+
				"ungatedRoutes with a reason it cannot require a session.", pattern)
			continue
		}
		if wrapper != expected {
			t.Errorf("route %q is registered with %s(...), want %s(...). Routes reaching "+
				"the internal-origin stamp are safe only behind `gated`; `wrap` applies "+
				"CSRF and headers but NO auth (#2934).", pattern, wrapper, expected)
		}
	}
	for pattern := range want {
		if _, ok := found[pattern]; !ok {
			t.Errorf("route %q is expected by this test but no longer registered in "+
				"server.go -- remove it here too", pattern)
		}
	}
}
