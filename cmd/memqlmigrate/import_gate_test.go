package main

import (
	"os/exec"
	"strings"
	"testing"
)

// import_gate_test.go -- the escape hatch must outlive the door (memql#5390,
// fix round 1, MINOR 4).
//
// # What this protects
//
// A deprecated form refuses once its window is spent, and the refusal names
// `memqlmigrate --rewrite=<name>` as the way across. That instruction is only
// true while memqlmigrate itself still READS the form -- and whether it does is
// decided by one thing: `deprecation.Current()`, the release this process makes
// deprecation decisions at.
//
// `component/memql` sets that from the binary's link-time release stamp in an
// `init()`. So any build that imports `component/memql`, directly or through
// anything else, decides at its own release; a build that does not leaves the
// value empty, which window.go cannot read and therefore fails OPEN on. Today
// memqlmigrate is in the second group, which is why a 0.25.0-stamped
// memqlmigrate still rewrites `array(T)` that a 0.25.0 engine refuses to load.
//
// Add one import -- `component/memql` itself, or anything that reaches it --
// and a release build of this tool starts refusing the very spelling it exists
// to remove, at exactly the moment the refusal starts telling people to run it.
// The bundle is then stranded with no mechanical way across, which is the one
// thing a deprecation window promises never to happen. Nothing about that
// failure is visible in a diff: the import would look like a convenience, and
// every test would stay green on an unstamped test binary, because an unstamped
// binary reads no release and fails open.
//
// # Why a build-graph test rather than a comment
//
// The guarantee was a comment in component/memql/deprecated_uses.go and nothing
// else. `go list -deps` is the build graph, and the build graph does not have
// opinions. This is the same instrument component/proving uses to keep its pure
// sub-packages off the engine.
//
// # If this fails
//
// Do not add an exemption. Either drop the import, or -- if this tool genuinely
// must reach the engine -- give it back its release-blindness explicitly, by
// calling deprecation.SetCurrent("") at the top of main, and change this test to
// assert THAT instead. The property to keep is "memqlmigrate reads every form,
// whatever release it was cut from", not the absence of one edge.
const engineImportPath = "github.com/znasllc-io/memql/component/memql"

func TestMemqlmigrateDoesNotReachTheEngine(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps .: %v", err)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(dep) != engineImportPath {
			continue
		}
		t.Fatalf(`cmd/memqlmigrate now depends on %s.

That package sets deprecation.Current() from the binary's release stamp in an
init(), so a release build of THIS tool would start refusing a deprecated form
at the same release the engine does -- while the engine's refusal is telling
people to run this tool. A bundle past the window would have no mechanical way
across, which is the promise a deprecation window exists to keep.

Drop the import, or make the release-blindness explicit (see this file's
comment) and change this test to assert that instead.`, engineImportPath)
	}
}

// The gate is worth nothing if `go list -deps` is not actually reporting this
// tool's imports, so one edge that MUST be there is asserted beside the one
// that must not. Without this a broken command, a renamed package or an empty
// result would read as a pass.
func TestTheImportGateIsLookingAtThisTool(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps .: %v", err)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")
	const parser = "github.com/znasllc-io/memql/component/language/parser"
	for _, dep := range deps {
		if strings.TrimSpace(dep) == parser {
			return
		}
	}
	t.Fatalf("go list -deps . returned %d package(s) and none of them is %s, which memqlmigrate "+
		"certainly imports -- the gate above is reading nothing", len(deps), parser)
}
