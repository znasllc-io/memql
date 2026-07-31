package server

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// stubStrictServer satisfies StrictServerInterface so HandlerWithOptions will
// register its routes. The handlers are never invoked -- only the registration
// is under inspection.
type stubStrictServer struct{}

func (stubStrictServer) GetHealthz(context.Context, GetHealthzRequestObject) (GetHealthzResponseObject, error) {
	return nil, nil
}

func (stubStrictServer) PostAutomationTrigger(context.Context, PostAutomationTriggerRequestObject) (PostAutomationTriggerResponseObject, error) {
	return nil, nil
}

func (stubStrictServer) PostAutomationResume(context.Context, PostAutomationResumeRequestObject) (PostAutomationResumeResponseObject, error) {
	return nil, nil
}

// recordingMux captures the patterns HandlerWithOptions registers, so
// ContractRoutes() can be checked against reality instead of trusted.
type recordingMux struct{ patterns []string }

func (m *recordingMux) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
}

func (m *recordingMux) ServeHTTP(http.ResponseWriter, *http.Request) {}

// registeredContractPaths returns the distinct request paths HandlerWithOptions
// mounts, with any leading method verb stripped -- registerRoute registers
// "GET /healthz" while registerAutomationTriggerRoute registers both
// "POST /automations/" and the bare "/automations/".
func registeredContractPaths(t *testing.T) []string {
	t.Helper()
	mux := &recordingMux{}
	HandlerWithOptions(NewStrictHandler(stubStrictServer{}, nil), StdHTTPServerOptions{BaseRouter: mux})

	seen := map[string]bool{}
	var paths []string
	for _, p := range mux.patterns {
		if i := strings.IndexByte(p, ' '); i >= 0 {
			p = p[i+1:]
		}
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// The whole point of ContractRoutes() is that it cannot quietly fall behind the
// routes actually registered -- a hand-maintained list would drift, and a route
// missing from it would skip the boot check entirely.
func TestContractRoutesMatchesRegistration(t *testing.T) {
	got := registeredContractPaths(t)

	want := append([]string(nil), ContractRoutes()...)
	sort.Strings(want)

	if len(got) == 0 {
		t.Fatal("HandlerWithOptions registered no routes -- this check has stopped " +
			"resolving them and would now pass vacuously")
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ContractRoutes() is out of step with what HandlerWithOptions registers.\n"+
			"registered: %v\ndeclared:   %v\n\nA route missing from ContractRoutes() never "+
			"reaches AssertUnauthenticatedSurfaceDeclared, so it would be unauthenticated on "+
			"the identity binary with nothing failing (memql#2939).", got, want)
	}
}

// The live tree must satisfy the rule -- otherwise the identity binary cannot
// boot once the assertion is wired in.
func TestLiveUnauthenticatedSurfaceIsDeclared(t *testing.T) {
	if err := AssertUnauthenticatedSurfaceDeclared(ContractRoutes()); err != nil {
		t.Errorf("the routes registered today are not fully declared: %v", err)
	}
}

// Exercising only the live lists would show the current tree is clean without
// showing the check would catch a route that is not.
func TestAssertSurfaceDeclaredCatchesUndeclaredRoutes(t *testing.T) {
	cases := []struct {
		name              string
		routes            []string
		public            []string
		handlerAuthorized []string
		wantErr           bool
		wantMentions      string
	}{
		{
			name:    "route covered by PublicPaths",
			routes:  []string{"/healthz"},
			public:  []string{"/healthz"},
			wantErr: false,
		},
		{
			name:              "route covered by HandlerAuthorizedPaths",
			routes:            []string{"/automations/resume"},
			handlerAuthorized: []string{"/automations/resume"},
			wantErr:           false,
		},
		{
			name:         "a sixth route declared nowhere -- the case this exists for",
			routes:       []string{"/healthz", "/admin/secrets"},
			public:       []string{"/healthz"},
			wantErr:      true,
			wantMentions: "/admin/secrets",
		},
		{
			name:         "no declarations at all",
			routes:       []string{"/automations/resume"},
			wantErr:      true,
			wantMentions: "/automations/resume",
		},
		{
			// The case an earlier version got wrong: normalizing trailing
			// slashes away made the PREFIX declaration "/automations/" also
			// cover the EXACT path "/automations", so a new GET /automations
			// route inherited a blessing justified by the trigger handler's
			// owner-or-admin check, which it does not have.
			name:              "exact path is NOT covered by a prefix declaration",
			routes:            []string{"/automations"},
			handlerAuthorized: []string{"/automations/"},
			wantErr:           true,
			wantMentions:      "/automations",
		},
		{
			name:              "a path beneath a prefix declaration IS covered",
			routes:            []string{"/automations/resume"},
			handlerAuthorized: []string{"/automations/"},
			wantErr:           false,
		},
		{
			// The converse: an exact declaration must not bless a route that
			// serves an entire subtree.
			name:    "prefix route is NOT covered by an exact declaration",
			routes:  []string{"/automations/"},
			public:  []string{"/automations"},
			wantErr: true,
		},
		{
			// TrimPrefix on a raw substring would turn "/healthz" into "thz".
			name:    "base path is trimmed by segment, not substring",
			routes:  []string{"/healthz"},
			public:  []string{"/healthz"},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := assertSurfaceDeclared(tc.routes, tc.public, tc.handlerAuthorized)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil -- an undeclared route would boot silently")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.wantMentions != "" && !strings.Contains(err.Error(), tc.wantMentions) {
				t.Errorf("error must name the offending route %q so the fix is obvious; got: %v",
					tc.wantMentions, err)
			}
		})
	}
}

// The automations routes must NOT be public: PublicPaths is consulted by the
// verifier middleware on every verifier-consuming node, so listing them there
// would make them unauthenticated everywhere -- the opposite of the fix.
func TestHandlerAuthorizedPathsAreNotPublic(t *testing.T) {
	// This must mirror verifier.shouldBypassAuth, NOT surfaceDeclaredBy. The
	// verifier treats EVERY PublicPaths entry as a prefix -- HasPrefix(path,
	// allowed+"/") -- with the trailing slash irrelevant, whereas
	// surfaceDeclaredBy only treats a declaration as a prefix when it ends in
	// one. Using the latter here left the guard green while a PublicPaths entry
	// of "/automations" (no slash) bypassed auth for /automations/resume and
	// /automations/{name}/trigger on every verifier-consuming node -- verbatim
	// the scenario this test claims to catch.
	bypassed := func(path string) bool {
		for _, allowed := range PublicPaths() {
			for _, a := range surfacePathForms(allowed) {
				a = strings.TrimSuffix(a, "/")
				if a == "" {
					continue
				}
				if path == a || strings.HasPrefix(path, a+"/") {
					return true
				}
			}
		}
		return false
	}
	for _, p := range HandlerAuthorizedPaths() {
		if bypassed(strings.TrimSuffix(p, "/")) {
			t.Errorf("%q is in both HandlerAuthorizedPaths() and PublicPaths(). PublicPaths "+
				"makes it unauthenticated on EVERY node; a handler-authorized route is meant "+
				"to stay behind the verifier where one runs (memql#2939).", p)
		}
	}
}

// A configured base path must not change any verdict. An earlier version
// stripped the base from declarations as well as routes, so with
// SERVER_PUBLIC_PATH=/memql the declaration "/memql/ws" became "/ws" while the
// registered route "/memql/memql/ws" became "/memql/ws" -- they stopped
// matching and the identity binary fatally refused to boot. Nothing tested it,
// because the existing base-path case ran with the variable unset.
func TestBasePathDoesNotChangeTheVerdict(t *testing.T) {
	identitySurface := func() []string {
		routes := ContractRoutes()
		for _, p := range MetricsPaths() {
			routes = append(routes, "GET "+p)
		}
		for _, p := range MemqlWebsocketPaths() {
			routes = append(routes, "GET "+p)
		}
		return routes
	}

	for _, base := range []string{"", "/api", "/memql", "/heal"} {
		t.Run("base="+base, func(t *testing.T) {
			t.Setenv("SERVER_PUBLIC_PATH", base)
			if err := AssertUnauthenticatedSurfaceDeclared(identitySurface()); err != nil {
				t.Errorf("the identity binary's surface must stay declared under "+
					"SERVER_PUBLIC_PATH=%q, otherwise the node crash-loops on boot: %v",
					base, err)
			}
		})
	}
}
