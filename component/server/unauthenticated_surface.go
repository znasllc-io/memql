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
		// GET /memql/ws -- the WebSocket upgrade itself performs no auth check,
		// and it is deliberately NOT in PublicPaths(): on a verifier-consuming
		// node the middleware gates it, extracting the bearer from the WS
		// subprotocol. It is listed here because this list is consulted ONLY on
		// a binary with no verifier, and there the bridge tunnels to
		// MemqlService.Stream, whose interceptor is OperatorAware(RejectAll)
		// (app/transport.go) -- every stream without an operator credential is
		// refused, so the surface fails closed at the next hop rather than in
		// the handler.
		//
		// Recorded plainly because it is weaker than the two entries above:
		// this declares a known posture, not a claim that the upgrade
		// authenticates. If identity's gRPC chain ever stops rejecting by
		// default, this entry stops being true.
		"/memql/ws",
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
	return append([]string(nil), contractRoutes...)
}

var contractRoutes = []string{
	"/healthz",
	"/readyz",
	"/livez",
	"/automations/",
	"/automations/resume",
}

// AssertUnauthenticatedSurfaceDeclared reports whether every supplied route is
// accounted for by PublicPaths() or HandlerAuthorizedPaths().
//
// Call it on a binary that installs no auth middleware, passing the routes that
// binary actually serves. It authenticates nothing; it refuses to let a route
// be unauthenticated by omission.
//
// Patterns may carry a leading method verb ("GET /memql/ws"); it is stripped
// before comparison, since the declarations are about paths.
func AssertUnauthenticatedSurfaceDeclared(routes []string) error {
	return assertSurfaceDeclared(routes, PublicPaths(), HandlerAuthorizedPaths())
}

// stripMethod removes a leading HTTP verb from a ServeMux pattern.
func stripMethod(pattern string) string {
	if i := strings.IndexByte(strings.TrimSpace(pattern), ' '); i >= 0 {
		return strings.TrimSpace(strings.TrimSpace(pattern)[i+1:])
	}
	return strings.TrimSpace(pattern)
}

// assertSurfaceDeclared holds the rule itself, separated from the live lists so
// the rule can be exercised against fixtures. Testing only the real lists would
// prove the current tree is clean without proving the check would catch a route
// that is not.
func assertSurfaceDeclared(routes, public, handlerAuthorized []string) error {
	declared := make([]string, 0, 2*(len(public)+len(handlerAuthorized)))
	for _, p := range append(append([]string(nil), public...), handlerAuthorized...) {
		declared = append(declared, surfacePathForms(p)...)
	}

	var undeclared []string
	for _, route := range routes {
		if !surfaceDeclaredBy(surfacePathForms(stripMethod(route)), declared) {
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

// surfaceDeclaredBy reports whether route is covered by any declaration.
//
// A trailing slash makes a declaration a PREFIX -- it covers the path and
// everything beneath it -- matching how registerAutomationTriggerRoute mounts
// "/automations/". Without one the declaration is EXACT.
//
// This is deliberately STRICTER than verifier.shouldBypassAuth, which is not
// the same rule: normalizePath strips the trailing slash from both the request
// path and every declaration, so the verifier treats EVERY PublicPaths() entry
// as a prefix whether or not it ends in one. What this function covers is
// therefore a strict subset of what the verifier bypasses. The asymmetry is
// safe in the only direction that matters -- this check can refuse to bless a
// route the verifier would let through, never the reverse -- so it fails
// closed, as a loud boot fatal rather than a silent exposure.
//
// The distinction is load-bearing. An earlier version normalized trailing
// slashes away, which conflated the two: the prefix declaration "/automations/"
// then also covered the exact path "/automations", so a new `GET /automations`
// route -- the most obvious next route on that subtree -- inherited a blessing
// whose justification is the trigger handler's owner-or-admin check, which it
// does not have. It passed every gate.
//
// Root is never a prefix declaration. "/" is a prefix of every absolute path,
// so honouring it here would bless the entire surface from a single declaration
// and make this whole check pass vacuously -- it would fail OPEN, which is the
// one direction a security assertion must not fail. verifier.shouldBypassAuth
// skips `allowed == "/"` for the same reason; this mirrors it. Exact equality
// against "/" still matches, so a genuinely-declared root route is unaffected.
func surfaceDeclaredBy(routeForms []string, declarations []string) bool {
	for _, route := range routeForms {
		for _, d := range declarations {
			if d == route {
				return true
			}
			if d == "/" {
				continue
			}
			if strings.HasSuffix(d, "/") && strings.HasPrefix(route, d) {
				return true
			}
		}
	}
	return false
}

// surfacePathForms returns every spelling a path may legitimately take: as
// written, and with a configured base path stripped.
//
// BOTH forms are kept rather than reducing to one. An earlier version stripped
// the base from declarations as well as routes, which silently broke whenever
// the base was a path-prefix of a declared path: with SERVER_PUBLIC_PATH=/memql
// the declaration "/memql/ws" reduced to "/ws" while the registered route
// "/memql/memql/ws" reduced to "/memql/ws", so they no longer matched and the
// identity binary fatally refused to boot. Comparing every form against every
// form cannot produce that mismatch.
//
// The base is trimmed as a path SEGMENT, not a substring: trimming "/heal" off
// "/healthz" would otherwise yield "thz". Trailing slashes are PRESERVED --
// they carry the prefix-vs-exact distinction (see surfaceDeclaredBy).
//
// A stripped form that reduces to "/" is DROPPED rather than emitted, because
// surfaceDeclaredBy honours a trailing-slash declaration as a prefix and "/"
// prefixes every absolute path -- one such declaration blessed the entire
// surface and the whole assertion passed vacuously.
//
// Two spellings produced it, and they were fixed by two different edits:
//
//	p == base            -> "/"   e.g. base=/metrics, /healthz, /readyz,
//	                              /livez, /memql/ws. Fixed by deleting the
//	                              p == base branch that appended "/" outright.
//	p == base + "/"      -> "/"   e.g. base=/automations against the prefix
//	                              declaration "/automations/". Fixed by the
//	                              rest != "" guard below.
//
// Nothing is lost by dropping the form: the as-written spelling is kept on
// both sides and still matches. surfaceDeclaredBy independently refuses "/" as
// a prefix, so the two are belt-and-braces; each is tested on its own, since a
// guard whose only coverage comes from the other guard is not covered at all.
func surfacePathForms(p string) []string {
	p = strings.TrimSpace(p)
	if p == "" {
		return []string{"/"}
	}
	forms := []string{p}
	if base := sanitizeBaseURLFromEnv(); base != "" {
		if rest := strings.TrimPrefix(p, base+"/"); rest != p && rest != "" {
			forms = append(forms, "/"+rest)
		}
	}
	return forms
}
