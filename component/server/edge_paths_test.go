//go:build !edge

package server

import "testing"

// PublicPaths() has two consumers with different matching rules over the
// same list (see EdgePaths' doc comment in nethttp.go): the boot-time
// unauthenticated-surface check treats a bare "/" as an exact declaration
// only and is safe, but verifier.shouldBypassAuth's exact-match branch
// (component/identity/verifier/middleware.go) has NO "/" guard at all -- the
// guard that skips "/" there lives only in the PREFIX loop below the
// exact-match check. An unconditional "/" entry would therefore bypass
// authentication for a root request on EVERY verifier-consuming node, not
// just the edge. EdgePaths' contribution is build-tag-scoped
// (edgeRootPaths, edge_paths_edge.go / edge_paths_default.go) precisely to
// prevent that -- this pins the scoping on every binary that must NOT carry
// it, i.e. every build except -tags edge. The mirror case (the edge build,
// where it must be present) is TestPublicPathsIncludesBareRootOnEdge in
// edge_paths_edge_test.go.
func TestPublicPathsDoesNotIncludeBareRootOutsideEdge(t *testing.T) {
	t.Parallel()
	for _, p := range PublicPaths() {
		if p == "/" {
			t.Fatalf(`PublicPaths() contains a bare "/" on a non-edge build. verifier.shouldBypassAuth's `+
				`exact-match branch has no "/" guard, so this would bypass authentication for a root `+
				`request on every verifier-consuming node -- see EdgePaths' doc comment in nethttp.go: got %q`, p)
		}
	}
	if got := EdgePaths(); len(got) != 0 {
		t.Fatalf("EdgePaths() = %v on a non-edge build, want empty", got)
	}
}
