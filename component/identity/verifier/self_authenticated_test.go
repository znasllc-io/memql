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
