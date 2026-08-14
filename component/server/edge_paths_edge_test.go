//go:build edge

package server

import "testing"

// The mirror of TestPublicPathsDoesNotIncludeBareRootOutsideEdge
// (edge_paths_test.go): on the edge binary specifically, the declaration
// MUST be present, or the edge's own boot-time unauthenticated-surface
// check (app/auth_mode_edge.go, verifierRequired=false) fatals on its own
// root mount -- see app/unauthenticated_surface_wiring_test.go's
// TestEdgeRootMountSurvivesTheUnauthenticatedSurfaceAssertion for that half.
func TestPublicPathsIncludesBareRootOnEdge(t *testing.T) {
	t.Parallel()
	got := EdgePaths()
	if len(got) != 1 || got[0] != "/" {
		t.Fatalf(`EdgePaths() = %v on an edge build, want ["/"]`, got)
	}
	var found bool
	for _, p := range PublicPaths() {
		if p == "/" {
			found = true
		}
	}
	if !found {
		t.Fatal(`PublicPaths() must include "/" on the edge build`)
	}
}
