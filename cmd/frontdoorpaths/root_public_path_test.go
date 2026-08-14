//go:build !edge

// This file exists for its build constraint, which is the whole point of it
// being a file rather than one more test in main_test.go.
//
// THE PROPERTY IS TRUE IN THE UNTAGGED BUILD AND FALSE BY DESIGN IN THE EDGE
// BUILD. `PublicPaths()` legitimately contains "/" under `-tags edge`, because a
// public website server serves root -- that is what the edge node is for. So the
// assertion below is not universally true and must not claim to be. Asserting it
// unconditionally is the same defect this whole change has been cataloguing: a
// safety claim whose real scope lives in a comment instead of in a constraint.
//
// NOTHING EXERCISES IT WRONGLY TODAY, AND THAT IS A FACT ABOUT A CI ARGUMENT
// LIST, NOT ABOUT THIS CODE. The edge lane is scoped to two packages:
//
//	.github/workflows/ci.yml:877-879
//	  - name: go test -tags edge (build-tagged suites)
//	    run: go test -tags edge -timeout=300s ./app/ ./component/server/
//
// so cmd/frontdoorpaths is never compiled with that tag. The first person to
// widen that list to ./... would get a failure that looks like a real
// authentication bypass and is not one. A false alarm on a security assertion is
// expensive twice: it costs the investigation, and it teaches the next reader to
// distrust the check -- which is worse, because the check is the only thing
// standing between a latent bypass and a shipped one.
//
// WHY A BUILD TAG RATHER THAN A RUNTIME GUARD, which is the part that will not be
// obvious: there is no runtime guard available. The tempting one is to ask
// server.EdgePaths() whether this is an edge build and skip when it is. That
// cannot work. From inside the package, "this is an edge build" and "someone
// deleted the tag scoping on the contribution" are INDISTINGUISHABLE -- both
// present as EdgePaths() returning "/". The second is precisely the regression
// this assertion exists to catch, so a guard derived from EdgePaths() would
// switch itself off in exactly the case it is needed. Only the compiler knows
// which build this is, so the scope has to be a build constraint. The mechanism
// is not a stylistic preference over a cleverer check; it is the only honest one.
package main

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/server"
)

// PublicPaths() must not contain "/" -- an auth bypass, not a routing concern,
// asserted here because this is where the declarations are already audited.
//
// component/identity/verifier/middleware.go:161-177 checks two ways and only one
// of them is guarded:
//
//	path := normalizePath(r.URL.Path)
//	if _, ok := publicPaths[path]; ok {
//	        return true          // EXACT match, reached FIRST, no guard
//	}
//	for allowed := range publicPaths {
//	        if allowed == "" || allowed == "/" || allowed == path {
//	                continue     // the famous guard -- PREFIX loop only
//	        }
//
// The `allowed == "/"` skip that everyone cites protects the prefix walk. The
// exact-match branch above it does not, so "/" in PublicPaths() means a request
// to exactly "/" bypasses bearer verification on EVERY verifier-consuming node --
// bff, identity, mcp. The effect is nil today only because only the edge
// registers "/" and every other node's mux 404s, and
// AssertUnauthenticatedSurfaceDeclared -- which refuses "/" as a prefix
// declaration for this very reason -- does not run on a node that has a verifier
// (app/transport.go:265).
//
// The edge node genuinely serves the root, and it takes exactly the shape this
// test was written to permit: server.EdgePaths() is unconditionally compiled --
// its doc comment says explicitly that this is so it stays visible to a source
// scan -- while its RETURN VALUE is tag-scoped via edgeRootPaths, which is
// []string{"/"} under `//go:build edge` and nil under `!edge`
// (edge_paths_edge.go / edge_paths_default.go, memql#3710). So in this build
// PublicPaths() carries no "/" and this passes, while an unconditional "/" fails.
// Both halves are live and both are asserted: this test covers the contribution,
// and notServedByTheBFF["EdgePaths"] covers the routing.
func TestPublicPathsDoesNotDeclareRoot(t *testing.T) {
	for _, p := range server.PublicPaths() {
		if strings.TrimSpace(p) == "/" {
			t.Error(`server.PublicPaths() contains "/", which bypasses bearer verification ` +
				`for a request to exactly "/" on every verifier-consuming node: ` +
				`verifier.shouldBypassAuth's exact-match branch has no "/" guard, only its ` +
				`prefix loop does (component/identity/verifier/middleware.go:161-177). ` +
				`A node that legitimately serves the root must contribute "/" from a ` +
				`build-tagged declaration, not unconditionally.`)
		}
	}
}
