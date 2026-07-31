package server

import (
	"fmt"
	"sort"
	"strings"
)

// Declares which HTTP routes may be reachable without authentication, and on
// what grounds (znasllc-io/memql#2939).
//
// On a verifier-consuming node, public is opt-in: the verifier middleware is
// installed with PublicPaths() and a route is unauthenticated only because
// someone put it on that list. On a binary with no verifier middleware -- the
// identity binary, and any node running MEMQL_IDENTITY_ENABLED=false -- that
// middleware is not configured differently, it is skipped entirely. PublicPaths
// is then never consulted, so public stops being a declared list and becomes
// the default, with no opt-out.
//
// That is how POST /automations/{name}/trigger and POST /automations/resume came
// to be unauthenticated on identity (#2937, #2908). Both now carry their own
// owner-or-admin checks (#2938), so the live exposure is closed. The defect this
// file addresses is the one that outlives them: the NEXT route registered in
// HandlerWithOptions is unauthenticated on those binaries by default, and its
// author has no reason to notice.
//
// The rule enforced at boot is that every route the contract registers must be
// accounted for by exactly one of two declarations:
//
//   - PublicPaths()            -- genuinely public on EVERY node.
//   - HandlerAuthorizedPaths() -- not public, but authorizes internally, so it
//     is safe where no middleware runs ahead of it.
//
// A new route belongs to one of those or the node refuses to boot. The point is
// not that the boot check authenticates anything new -- it does not -- but that
// leaving a route unauthenticated becomes an explicit, reviewable act instead of
// an omission.

// HandlerAuthorizedPaths returns routes that are NOT public but perform their
// own authorization inside the handler, and may therefore be registered on a
// binary that installs no auth middleware.
//
// These deliberately do NOT go in PublicPaths(): that list is consulted by the
// verifier middleware on every verifier-consuming node, so adding them there
// would make them unauthenticated everywhere -- the exact opposite of the fix.
//
// A route qualifies here only if it fails CLOSED with no credentials. Both
// entries below check for an owner-or-admin actor and reject when the request
// carries no claims at all, so the unauthenticated tier is rejected rather than
// admitted (#2908, #2938). "The handler looks at the actor" is not sufficient;
// it has to reject when there is no actor.
func HandlerAuthorizedPaths() []string {
	return []string{
		// POST /automations/{name}/trigger -- registered as a prefix by
		// registerAutomationTriggerRoute; owner-or-admin enforced in the
		// handler (#2937, #2938).
		"/automations/",
		// POST /automations/resume -- owner-or-admin enforced in the handler
		// (#2908, #2938).
		"/automations/resume",
	}
}

// ContractRoutes returns the request paths HandlerWithOptions registers.
//
// Hand-maintained lists drift, so this one is verified rather than trusted:
// TestContractRoutesMatchesRegistration registers the real handler against a
// recording ServeMux and asserts this set equals what was actually registered.
// A sixth route therefore fails that test before it can reach the boot check.
//
// Paths are the un-prefixed canonical forms. A configured base path is applied
// uniformly by joinPath at registration and by PublicPaths at comparison, so it
// cannot change whether a route is declared.
func ContractRoutes() []string {
	return []string{
		"/healthz",
		"/readyz",
		"/livez",
		"/automations/",
		"/automations/resume",
	}
}

// AssertUnauthenticatedSurfaceDeclared reports whether every contract route is
// accounted for by PublicPaths() or HandlerAuthorizedPaths().
//
// Call it on any binary that installs no auth middleware. It authenticates
// nothing; it refuses to let a route be unauthenticated by omission.
func AssertUnauthenticatedSurfaceDeclared() error {
	return assertSurfaceDeclared(ContractRoutes(), PublicPaths(), HandlerAuthorizedPaths())
}

// assertSurfaceDeclared holds the rule itself, separated from the live lists so
// the rule can be exercised against fixtures. Testing only the real lists would
// prove the current tree is clean without proving the check would catch a route
// that is not.
func assertSurfaceDeclared(routes, public, handlerAuthorized []string) error {
	declared := map[string]bool{}
	for _, p := range public {
		declared[normalizeSurfacePath(p)] = true
	}
	for _, p := range handlerAuthorized {
		declared[normalizeSurfacePath(p)] = true
	}

	var undeclared []string
	for _, route := range routes {
		if !declared[normalizeSurfacePath(route)] {
			undeclared = append(undeclared, route)
		}
	}
	if len(undeclared) == 0 {
		return nil
	}
	sort.Strings(undeclared)
	return fmt.Errorf(
		"unauthenticated HTTP surface is not declared: %s reachable without authentication "+
			"on a binary that installs no auth middleware, and listed in neither PublicPaths() "+
			"nor HandlerAuthorizedPaths(). Add it to PublicPaths() if it is public on every "+
			"node, or to HandlerAuthorizedPaths() if it authorizes internally and fails closed "+
			"with no credentials (see component/server/unauthenticated_surface.go, memql#2939)",
		strings.Join(undeclared, ", "))
}

// normalizeSurfacePath strips any configured base path and trailing slash so a
// route and its declaration compare equal regardless of how each was spelled.
// "/" is preserved, since trimming it would make every path match it.
func normalizeSurfacePath(p string) string {
	p = strings.TrimSpace(p)
	if base := sanitizeBaseURLFromEnv(); base != "" {
		p = strings.TrimPrefix(p, base)
	}
	if p == "" {
		return "/"
	}
	if p != "/" {
		p = strings.TrimSuffix(p, "/")
	}
	if p == "" {
		return "/"
	}
	return p
}
