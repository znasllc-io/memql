package memql

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// InterceptedPaths is a hand-written list, and a hand-written list of what code
// does drifts from what it does. memql#3004 exists because a middleware-mounted
// route was in no list at all; replacing that with a list that is merely
// plausible would be a smaller version of the same defect.
//
// So this drives the REAL predicate. shouldHandle is what decides whether the
// middleware claims a request ahead of the mux, and every declared path must
// actually be claimed by it.
func TestInterceptedPathsMatchShouldHandle(t *testing.T) {
	paths := InterceptedPaths()
	if len(paths) == 0 {
		t.Fatal("InterceptedPaths is empty. If the gateway genuinely intercepts nothing, " +
			"Middleware() should not be installed -- an empty list here silently removes the " +
			"middleware channel from the boot assertion again (memql#3004)")
	}

	g := &Gateway{}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader("{}"))
		if !g.shouldHandle(req) {
			t.Errorf("InterceptedPaths declares %q but shouldHandle refuses POST to it. The "+
				"declaration would put a path into the boot assertion that the middleware never "+
				"serves -- and, worse, imply the real one is covered.", p)
		}
	}

	// The converse direction, to the extent a predicate can be probed: a path
	// the gateway does NOT claim must not be declared, or the assertion is
	// asserting over noise.
	for _, notOurs := range []string{"/healthz", "/memql/ws", "/auth/login"} {
		req := httptest.NewRequest(http.MethodPost, notOurs, strings.NewReader("{}"))
		if g.shouldHandle(req) {
			continue // genuinely claimed; nothing to assert
		}
		for _, p := range paths {
			if p == notOurs {
				t.Errorf("InterceptedPaths declares %q, which shouldHandle does not claim", p)
			}
		}
	}
}

// shouldHandle matches on a SUFFIX, so a base-path deployment serves the
// gateway at `<base>/memql/query` too. Pinned because the declaration is the
// canonical un-prefixed form, and the comparison in
// AssertUnauthenticatedSurfaceDeclared strips a configured base uniformly --
// that only works if the suffix behaviour here is what it looks like.
func TestGatewayClaimsBasePathPrefixedRequests(t *testing.T) {
	g := &Gateway{}
	for _, p := range []string{
		"/memql/query",
		"/api/memql/query",
		"/memql/query/", // trailing slash is trimmed before comparison
	} {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader("{}"))
		if !g.shouldHandle(req) {
			t.Errorf("gateway does not claim POST %q; the declared path would not cover the "+
				"surface actually served", p)
		}
	}

	// A GET is not claimed -- the declaration covers the POST surface only,
	// which is what the HandlerAuthorizedPaths note says.
	if g.shouldHandle(httptest.NewRequest(http.MethodGet, "/memql/query", nil)) {
		t.Error("gateway claims a GET to /memql/query; the declaration and its rationale are " +
			"written for the POST surface")
	}
}
