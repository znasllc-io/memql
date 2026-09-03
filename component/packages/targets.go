package packages

import "strings"

// targets.go is the engine's half of the target model (epic memql#4885, D9;
// the build surface is epic memql#4900, task memql#4904).
//
// ===========================================================================
// A TARGET SAYS WHERE ITS KIND IS BUILT, AND THE PIPELINE ASKS
// ===========================================================================
// The Compose epic fixed what a target IS -- an address stop, a build surface,
// a set of live states, a row -- and registered exactly one, `web`. This file
// holds the half the build stage needs: which SURFACE a kind is built on, so
// the pipeline's choice is a lookup rather than an `if kind == "ios"`.
//
// Every OFFERED kind builds on the workbench. The fleet route exists, has a
// hop test, and has no registered kind that reaches it -- which is the shape
// the epic asked for and the reason this is a table: turning it on for the
// first mobile target is one entry, not a code path.
//
// ===========================================================================
// AN UNKNOWN KIND HAS NO SURFACE, AND THAT IS NOT A DEFAULT
// ===========================================================================
// BuildSurfaceFor answers "" for a kind no target claims. The analysis has
// already refused such a package (deployable_kind_unknown) or reported it
// (deployable_target_not_offered), so the build stage never sees one -- and if
// it somehow did, "" reaches no builder and is refused by name rather than
// being quietly built on whichever surface happened to be the default.

// buildSurfaceByKind maps a deployable kind to the surface it builds on.
//
// The three unoffered kinds are PRESENT and map to the fleet, which is a
// statement rather than a stub: iOS and macOS need Xcode, Xcode needs macOS,
// and macOS in this product means a machine in the person's own Fleet. Android
// is here for the opposite reason -- it needs a JDK and an SDK, which a
// workbench flavour could carry, so it maps to the workbench the day one does
// and to the fleet until then.
var buildSurfaceByKind = map[string]string{
	// The three registered web kinds (v1:platform:site.kind).
	KindSPA:        SurfaceWorkbench,
	KindStatic:     SurfaceWorkbench,
	KindStorefront: SurfaceWorkbench,
	// Known, not offered. No manifest declaring one reaches the build stage;
	// the analysis reports it against the app and the rest of the package
	// deploys.
	"ios":     SurfaceFleet,
	"android": SurfaceFleet,
	"macos":   SurfaceFleet,
}

// BuildSurfaceFor reports the surface a kind builds on, or "" for a kind no
// target claims.
func BuildSurfaceFor(kind string) string {
	return buildSurfaceByKind[strings.TrimSpace(kind)]
}

// OfferedKindsBuildOnTheWorkbench reports whether every kind this cluster
// actually offers builds in-cluster.
//
// Exported for the gate that pins it (build_surface_test.go): the acceptance
// for the fleet route is that NO offered kind reaches it, and a boolean a test
// can read is what makes that assertion about the table rather than about a
// list the test would otherwise have to keep in step by hand.
func OfferedKindsBuildOnTheWorkbench() bool {
	for _, kind := range []string{KindSPA, KindStatic, KindStorefront} {
		if BuildSurfaceFor(kind) != SurfaceWorkbench {
			return false
		}
	}
	return true
}
