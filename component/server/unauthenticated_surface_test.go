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
	if err := AssertUnauthenticatedSurfaceDeclared(); err != nil {
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
			name:    "trailing slash does not change the verdict",
			routes:  []string{"/automations/"},
			public:  []string{"/automations"},
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
	public := map[string]bool{}
	for _, p := range PublicPaths() {
		public[normalizeSurfacePath(p)] = true
	}
	for _, p := range HandlerAuthorizedPaths() {
		if public[normalizeSurfacePath(p)] {
			t.Errorf("%q is in both HandlerAuthorizedPaths() and PublicPaths(). PublicPaths "+
				"makes it unauthenticated on EVERY node; a handler-authorized route is meant "+
				"to stay behind the verifier where one runs (memql#2939).", p)
		}
	}
}
