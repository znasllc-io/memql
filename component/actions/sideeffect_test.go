package actions

import (
	"strings"
	"testing"
)

// A declared @sideEffect that disagrees with the capability's authoritative
// class is rejected at load (it cannot be spoofed, ADR §7).
func TestSideEffectMismatchRejected(t *testing.T) {
	src := `@kind("primitive")
@sideEffect("read")
action spoofBad {
  capability "shell.exec"
  intent "..."
  params {}
  argTemplate {}
}`
	if _, err := LoadSource(src, "t.memql"); err == nil {
		t.Fatal("expected load error: @sideEffect(read) over shell.exec (class exec)")
	} else if !strings.Contains(err.Error(), "sideEffect") {
		t.Fatalf("error should mention sideEffect mismatch; got %v", err)
	}
}

// A declared @sideEffect that matches the capability's class loads cleanly.
func TestSideEffectMatchAccepted(t *testing.T) {
	src := `@sideEffect("read")
action readManifest {
  capability "fs.readFile"
  intent "..."
  params { path string @required }
  argTemplate { path: "$params.path" }
}`
	if _, err := LoadSource(src, "t.memql"); err != nil {
		t.Fatalf("matching @sideEffect must load: %v", err)
	}
}

// An omitted @sideEffect is DERIVED from the capability's class.
func TestSideEffectDerivedFromCapability(t *testing.T) {
	src := `action tag {
  capability "integration.github.tagRelease"
  intent "..."
  params { repo string @required }
  argTemplate { repo: "$params.repo" }
}`
	acts, err := LoadSource(src, "t.memql")
	if err != nil {
		t.Fatalf("LoadSource: %v", err)
	}
	if acts[0].SideEffect != "write" {
		t.Errorf("derived sideEffect = %q, want write", acts[0].SideEffect)
	}
}

// A capability outside the namespace vocabulary is rejected at load.
func TestUnknownNamespaceRejected(t *testing.T) {
	src := `action weird {
  capability "weird.doThing"
  intent "..."
  params {}
  argTemplate {}
}`
	if _, err := LoadSource(src, "t.memql"); err == nil {
		t.Fatal("expected load error for non-vocabulary capability namespace")
	}
}
