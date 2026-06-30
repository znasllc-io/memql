package actions

import (
	"strings"
	"testing"
)

// @sideEffect is RETIRED on actions (construct-invocation ADR Decision 3): the
// authoritative class lives on the capability declaration now. An action that
// still declares one is rejected at parse with a migration-pointing error.
func TestSideEffectAnnotationRejected(t *testing.T) {
	src := `use capabilities.shell.{ script }
@sideEffect("read")
action spoofBad {
  args { }
  capability script(script: "x")
}`
	if _, err := LoadSource(src, "t.memql"); err == nil {
		t.Fatal("expected load error: @sideEffect is retired on actions")
	} else if !strings.Contains(err.Error(), "sideEffect") {
		t.Fatalf("error should mention sideEffect; got %v", err)
	}
}

// The action's side-effect class is DERIVED from the capability it calls
// (shell.* -> exec, fs.readFile -> read, integration.github.tagRelease -> write).
func TestSideEffectDerivedFromCapability(t *testing.T) {
	src := `use capabilities.integration.github.{ tagRelease }
action tag {
  args { repo string @required }
  capability tagRelease(repo: args.repo)
}`
	acts, err := LoadSource(src, "t.memql")
	if err != nil {
		t.Fatalf("LoadSource: %v", err)
	}
	if acts[0].Capability != "integration.github.tagRelease" {
		t.Errorf("capability = %q, want integration.github.tagRelease", acts[0].Capability)
	}
	if acts[0].SideEffect != "write" {
		t.Errorf("derived sideEffect = %q, want write", acts[0].SideEffect)
	}
}

// A capability whose namespace is outside the vocabulary is rejected at load.
func TestUnknownNamespaceRejected(t *testing.T) {
	src := `use capabilities.weird.{ doThing }
action weird {
  args { }
  capability doThing(x: "y")
}`
	if _, err := LoadSource(src, "t.memql"); err == nil {
		t.Fatal("expected load error for non-vocabulary capability namespace")
	}
}

// A bare capability verb that is not imported cannot resolve to a full dotted
// capability name -- a loud load error.
func TestUnimportedVerbRejected(t *testing.T) {
	src := `action orphan {
  args { }
  capability script(script: "x")
}`
	if _, err := LoadSource(src, "t.memql"); err == nil {
		t.Fatal("expected load error for an unimported capability verb")
	}
}
