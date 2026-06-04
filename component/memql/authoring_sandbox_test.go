package memql

import (
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestSandboxCompileBundle_ValidConstructs: a context-spec + an actor shape
// (both concept-free) compile + bind cleanly, so the bundle is OK.
func TestSandboxCompileBundle_ValidConstructs(t *testing.T) {
	rep := SandboxCompileBundle([]SandboxConstruct{
		{
			Kind: "spec",
			Name: "specSandboxAdmin",
			Source: `@description("Caller is admin")
spec specSandboxAdmin {
  actor.role == "admin"
}`,
		},
		{
			Kind: "shape",
			Name: "sandboxActorEnv",
			Source: `@actor
@description("Actor envelope projection")
shape sandboxActorEnv {
  actor.userId
  actor.role
}`,
		},
	})

	if !rep.OK {
		t.Fatalf("expected bundle OK, got report: %+v", rep.Diagnostics)
	}
	if len(rep.Diagnostics) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d", len(rep.Diagnostics))
	}
	for _, d := range rep.Diagnostics {
		if !d.OK || d.Skipped || d.Error != "" {
			t.Errorf("construct %s/%s: expected clean compile, got %+v", d.Kind, d.Name, d)
		}
	}
}

// TestSandboxCompileBundle_SyntaxErrorFailsBundle: a malformed construct
// fails the bundle and carries an error diagnostic for the offending one.
func TestSandboxCompileBundle_SyntaxErrorFailsBundle(t *testing.T) {
	rep := SandboxCompileBundle([]SandboxConstruct{
		{
			Kind: "spec",
			Name: "specOK",
			Source: `@description("ok")
spec specOK {
  actor.role == "admin"
}`,
		},
		{
			Kind: "shape",
			Name: "shapeBroken",
			// Missing closing brace -> parse error.
			Source: `@actor
shape shapeBroken {
  actor.userId`,
		},
	})

	if rep.OK {
		t.Fatalf("expected bundle to FAIL on the broken shape, got OK: %+v", rep.Diagnostics)
	}
	var broken *SandboxDiagnostic
	for i := range rep.Diagnostics {
		if rep.Diagnostics[i].Name == "shapeBroken" {
			broken = &rep.Diagnostics[i]
		}
	}
	if broken == nil {
		t.Fatalf("missing diagnostic for shapeBroken")
	}
	if broken.OK || broken.Skipped || broken.Error == "" {
		t.Errorf("shapeBroken: expected a hard failure with an error, got %+v", *broken)
	}
}

// TestSandboxCompileBundle_UnsupportedKindSkipped: a kind this pass doesn't
// handle is reported as skipped and does NOT fail the bundle.
func TestSandboxCompileBundle_UnsupportedKindSkipped(t *testing.T) {
	rep := SandboxCompileBundle([]SandboxConstruct{
		{
			Kind:   "automation",
			Name:   "autoSandbox",
			Source: `automation autoSandbox { step run { logic noop { event: event } } }`,
		},
	})

	if !rep.OK {
		t.Fatalf("a skipped construct must not fail the bundle, got: %+v", rep.Diagnostics)
	}
	if len(rep.Diagnostics) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(rep.Diagnostics))
	}
	d := rep.Diagnostics[0]
	if !d.Skipped || d.OK {
		t.Errorf("expected skipped+!OK for automation, got %+v", d)
	}
	if d.Error == "" {
		t.Errorf("skipped construct should carry a skip reason")
	}
}

// TestSandboxCompileBundle_NoMutationOfConceptRegistry: the sandbox must not
// add/remove anything from the live concept registry.
func TestSandboxCompileBundle_NoMutationOfConceptRegistry(t *testing.T) {
	before := len(memoryNodes.List())
	_ = SandboxCompileBundle([]SandboxConstruct{
		{Kind: "spec", Name: "specSandboxAdmin", Source: `spec specSandboxAdmin { actor.role == "admin" }`},
		{Kind: "shape", Name: "sandboxActorEnv", Source: "@actor\nshape sandboxActorEnv {\n  actor.userId\n}"},
		{Kind: "automation", Name: "autoX", Source: "automation autoX { }"},
	})
	after := len(memoryNodes.List())
	if before != after {
		t.Fatalf("sandbox mutated the live concept registry: before=%d after=%d", before, after)
	}
}

// TestSandboxCompileBundle_AggregatesOK: bundle OK is the AND over all
// non-skipped constructs (one failure flips the whole bundle).
func TestSandboxCompileBundle_AggregatesOK(t *testing.T) {
	rep := SandboxCompileBundle([]SandboxConstruct{
		{Kind: "spec", Name: "good", Source: `spec good { actor.role == "admin" }`},
		{Kind: "spec", Name: "bad", Source: `spec bad { actor.role == }`}, // dangling operator
		{Kind: "automation", Name: "skip", Source: "automation skip { }"},
	})
	if rep.OK {
		t.Fatalf("expected bundle FAIL (one bad spec), got OK: %+v", rep.Diagnostics)
	}
}
