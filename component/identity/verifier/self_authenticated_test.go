package verifier

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// self_authenticated_test.go -- memql#3062, split out of memql#2957.
//
// The inbound receiver is reached by a third party (Shopify, Amazon SP-API, a
// POS) that holds no memQL identity and authenticates with a per-source HMAC
// over the request body. Before this tier existed the route fitted NEITHER
// unauthenticated tier:
//
//   - PublicPaths() is consulted by this middleware, but adding "/inbound/"
//     there is an open PREFIX bypass -- it would exempt anything mounted
//     beneath it later -- and it would make the route unauthenticated rather
//     than differently-authenticated.
//   - HandlerAuthorizedPaths() is consulted ONLY on a binary with no verifier.
//     The bff installs one, so on the documented default configuration
//     (MEMQL_IDENTITY_ENABLED=true) every genuine delivery was 401'd BEFORE the
//     handler's allowlist and HMAC ever ran.
//
// So the feature could not function where it is deployed. These tests pin both
// halves of the fix: the route is reachable, and the exemption is BOUNDED.

// bypassed reports whether the middleware let the request through to the
// handler. A verifier is deliberately not constructed: every request here
// carries no credentials, so anything not skipped is rejected before
// verification is attempted, which is exactly the behaviour under test.
func bypassed(t *testing.T, opts MiddlewareOptions, path string) bool {
	t.Helper()

	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })

	publicPaths := normalizePublicPathSet(opts.PublicPaths)
	selfAuth := normalizeSelfAuthSet(opts.SelfAuthenticatedPaths)

	req := httptest.NewRequest(http.MethodPost, path, nil)
	if shouldBypassAuth(req, publicPaths) || isSelfAuthenticated(req, selfAuth) {
		next.ServeHTTP(httptest.NewRecorder(), req)
	}
	return reached
}

// The case that was broken: no credentials, on a node that DOES install the
// verifier, must reach the handler.
func TestSelfAuthenticatedRouteIsReachableWithoutAMemqlBearer(t *testing.T) {
	opts := MiddlewareOptions{SelfAuthenticatedPaths: []string{"/inbound/"}}

	if !bypassed(t, opts, "/inbound/shopify") {
		t.Error("POST /inbound/shopify with no credentials did not reach the handler.\n\n" +
			"This is the defect memql#3062 was filed for: the route's credential is a vendor " +
			"HMAC, not a memQL bearer, so the middleware must step aside and let the handler " +
			"do the authenticating. While this fails, the inbound receiver cannot function on " +
			"the documented default configuration -- its allowlist and signature check never " +
			"execute, because every delivery is 401'd first.")
	}
}

// And the half that keeps it from being a prefix bypass in disguise. Each of
// these must still be gated.
func TestSelfAuthenticatedExemptionIsBoundedToOneSegment(t *testing.T) {
	opts := MiddlewareOptions{SelfAuthenticatedPaths: []string{"/inbound/"}}

	for _, tc := range []struct{ name, path, why string }{
		{
			name: "a nested path does not inherit the exemption",
			path: "/inbound/shopify/admin",
			why: "this is the whole reason the tier is not PublicPaths(). An open prefix walk " +
				"would exempt every route mounted under /inbound/ later, including ones nobody " +
				"has written yet. The handler refuses nested paths too (splitSource), but the " +
				"middleware must not be the layer that lets them past.",
		},
		{
			name: "two levels deep does not either",
			path: "/inbound/shopify/admin/orders",
			why:  "same rule, one level further -- the bound is on segment count, not depth-1.",
		},
		{
			name: "the bare prefix is not a route",
			path: "/inbound/",
			why:  "an empty source names nothing; the handler 404s it and the middleware must not treat it as exempt.",
		},
		{
			name: "a sibling sharing the prefix as a substring is untouched",
			path: "/inboundx/shopify",
			why:  "matching is on path segments, not on string prefixes -- /inboundx is a different route.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if bypassed(t, opts, tc.path) {
				t.Errorf("%s was exempted from bearer verification, and must not be.\n\n%s", tc.path, tc.why)
			}
		})
	}
}

// The tier must not quietly widen the PUBLIC surface: a route is exempt from
// bearer verification, which is not the same as being public. Pinned because
// the cheap way to "fix" the original defect was to add "/inbound/" to
// PublicPaths(), and this asserts that was not what happened.
func TestSelfAuthenticatedPathsAreNotPublicPaths(t *testing.T) {
	onlySelfAuth := MiddlewareOptions{SelfAuthenticatedPaths: []string{"/inbound/"}}
	if bypassed(t, onlySelfAuth, "/inbound/shopify/admin") {
		t.Fatal("control broken: the bounded matcher is behaving like a prefix walk")
	}

	// The same entry in PublicPaths() DOES exempt the nested path -- which is
	// the behaviour being avoided, asserted here so the difference between the
	// two tiers is a measured fact rather than a claim in a comment.
	asPublic := MiddlewareOptions{PublicPaths: []string{"/inbound/"}}
	if !bypassed(t, asPublic, "/inbound/shopify/admin") {
		t.Error("PublicPaths() no longer prefix-matches. If that is deliberate, the argument " +
			"for a separate SelfAuthenticatedPaths tier has changed and memql#3062 should be " +
			"re-read; if it is accidental, other routes have silently become gated.")
	}
}

// An encoded separator makes the decoded path and the escaped path disagree,
// and this function decides on the decoded one while the mux routes on the
// escaped one. "/inbound%2Fx" decodes to the one-segment "/inbound/x" -- which
// the segment rule would exempt -- while the mux sees the single literal
// segment "inbound%2Fx" and dispatches somewhere else entirely.
//
// It 404s today only because no binary registers a "/" catch-all. That is a
// property of the current route table rather than of this matcher, so it is
// closed here instead: where the two spellings differ, the request is not
// exempted.
func TestEncodedSeparatorInTheMountSegmentIsNotExempt(t *testing.T) {
	opts := MiddlewareOptions{SelfAuthenticatedPaths: []string{"/inbound/"}}
	selfAuth := normalizeSelfAuthSet(opts.SelfAuthenticatedPaths)

	req := httptest.NewRequest(http.MethodPost, "/inbound%2Fx", nil)
	if req.URL.RawPath == "" || req.URL.RawPath == req.URL.Path {
		t.Skipf("control: this Go version did not keep a distinct RawPath for %q (path=%q, raw=%q)",
			"/inbound%2Fx", req.URL.Path, req.URL.RawPath)
	}
	if isSelfAuthenticated(req, selfAuth) {
		t.Errorf("%q was exempted. Its decoded path (%q) satisfies the one-segment rule, but the "+
			"mux routes on the escaped form (%q) and would dispatch elsewhere -- so the exemption "+
			"would apply to a request this route never handles.",
			"/inbound%2Fx", req.URL.Path, req.URL.RawPath)
	}
}

// And the ordinary encoded-separator case, which was already safe because
// decoding only adds separators: the decoded view is the stricter one.
func TestEncodedSeparatorInsideTheSourceSegmentIsNotExempt(t *testing.T) {
	opts := MiddlewareOptions{SelfAuthenticatedPaths: []string{"/inbound/"}}
	selfAuth := normalizeSelfAuthSet(opts.SelfAuthenticatedPaths)

	req := httptest.NewRequest(http.MethodPost, "/inbound/a%2Fb", nil)
	if isSelfAuthenticated(req, selfAuth) {
		t.Errorf("%q was exempted; it decodes to two segments under the mount and must be gated",
			"/inbound/a%2Fb")
	}
}

// TestHTTPMiddlewareLetsASelfAuthenticatedRouteThrough drives the ASSEMBLED
// middleware, and it is the only test here that does.
//
// Every other test in this file calls `bypassed`, which re-implements the
// production condition (`shouldBypassAuth(...) || isSelfAuthenticated(...)`)
// rather than building the middleware. That pins the PREDICATE and pins nothing
// about the WIRING -- so deleting `|| isSelfAuthenticated(...)` from
// HTTPMiddleware, the single line that makes this tier exist, left the verifier,
// app and component/server suites all green while `POST /inbound/{source}` went
// back to 401 on the bff. That 401 is verbatim the defect memql#3062 was filed
// for. Measured, not imagined.
//
// So this one constructs HTTPMiddleware itself, serves a request carrying NO
// Authorization header through it, and asserts the handler ran. A verifier is
// constructed too, because "the node installs a verifier" is half the property
// under test -- the old helper deliberately built none, which is why it could
// not have caught this.
func TestHTTPMiddlewareLetsASelfAuthenticatedRouteThrough(t *testing.T) {
	const exempt = "/inbound/shopify"

	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusAccepted)
	})

	mw := HTTPMiddleware(&Verifier{}, MiddlewareOptions{
		SelfAuthenticatedPaths: []string{"/inbound/"},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, exempt, nil) // no Authorization header
	mw(next).ServeHTTP(rec, req)

	if !reached {
		t.Fatalf("%s did not reach the handler through the real middleware (status %d). The "+
			"third-tier exemption is not wired: a third-party webhook carries no memQL bearer, so "+
			"this is the 401 memql#3062 exists to remove.", exempt, rec.Code)
	}

	// And the other half, through the same assembled middleware: a sibling under
	// the same prefix must still be gated. Asserting only the positive would
	// pass just as well on a middleware that skipped everything.
	gated := false
	nextGated := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { gated = true })
	recGated := httptest.NewRecorder()
	mw(nextGated).ServeHTTP(recGated, httptest.NewRequest(http.MethodPost, exempt+"/evil", nil))
	if gated {
		t.Errorf("%s/evil reached the handler with no credentials -- the exemption is behaving as "+
			"a prefix bypass, which is the repair memql#3062 explicitly rejected", exempt)
	}
}
