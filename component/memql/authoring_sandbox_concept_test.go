package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestSandboxCompileBundle_CandidateConceptCompiles: a bundle that declares
// a NEW concept compiles cleanly, and the concept is overlaid onto the
// isolated clone (NOT the global default registry).
func TestSandboxCompileBundle_CandidateConceptCompiles(t *testing.T) {
	before := len(memoryNodes.List())

	rep := SandboxCompileBundle([]SandboxConstruct{
		{Kind: "concept", Name: "sandboxWidget", Source: candidateWidgetConcept},
	})

	if !rep.OK {
		t.Fatalf("expected candidate concept to compile, got: %+v", rep.Diagnostics)
	}
	if len(rep.Diagnostics) != 1 || !rep.Diagnostics[0].OK {
		t.Fatalf("expected one clean concept diagnostic, got: %+v", rep.Diagnostics)
	}

	// No-mutation guarantee: the candidate concept must NOT have landed in
	// the global default registry.
	if after := len(memoryNodes.List()); after != before {
		t.Fatalf("candidate concept mutated the global registry: before=%d after=%d", before, after)
	}
	if _, err := memoryNodes.Get("v1:sandboxns:sandboxWidget"); err == nil {
		t.Fatal("candidate concept leaked into the global default registry")
	}
}

// candidateWidgetConcept is the candidate concept reused by the
// overlay-binding tests below.
const candidateWidgetConcept = `@version("1.0.0")
@namespace("sandboxns")
@description("Candidate widget concept")
concept sandboxWidget {
  ownerUserId  string  @required
  label        string
}`

// candidateWidgetMutation binds against sandboxWidget via canonicalId(...),
// which forces a registry lookup of v1:sandboxns:sandboxWidget at Gate 1.
const candidateWidgetMutation = `use sandboxns.concepts.{ sandboxWidget }

@description("Create a widget")
mutate sandboxWidget mutationCreateSandboxWidget {
  args {
    widgetId  string  @required
  }
  insert {
    id:    canonicalId(args.widgetId, sandboxWidget)
    label: "x"
  }
}`

// TestSandboxCompileBundle_ConstructBindsCandidateConcept: a mutation that
// resolves a concept ref (canonicalId(..., sandboxWidget)) against a concept
// the SAME bundle defines compiles cleanly, because the candidate concept is
// overlaid onto the clone before the mutation parses + resolves.
func TestSandboxCompileBundle_ConstructBindsCandidateConcept(t *testing.T) {
	rep := SandboxCompileBundle([]SandboxConstruct{
		{Kind: "concept", Name: "sandboxWidget", Source: candidateWidgetConcept},
		{Kind: "mutation", Name: "mutationCreateSandboxWidget", Source: candidateWidgetMutation},
	})

	if !rep.OK {
		t.Fatalf("expected a self-consistent bundle (mutation resolves bundle concept) to pass, got: %+v", rep.Diagnostics)
	}
	if len(rep.Diagnostics) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d: %+v", len(rep.Diagnostics), rep.Diagnostics)
	}
	for _, d := range rep.Diagnostics {
		if !d.OK {
			t.Errorf("construct %s/%s: expected clean, got %+v", d.Kind, d.Name, d)
		}
	}
}

// TestSandboxCompileBundle_MissingConceptRefFails: the SAME mutation WITHOUT
// the candidate concept in the bundle fails to resolve the concept ref --
// proving the overlay (not some ambient registration) is what made the
// positive case pass.
func TestSandboxCompileBundle_MissingConceptRefFails(t *testing.T) {
	rep := SandboxCompileBundle([]SandboxConstruct{
		{Kind: "mutation", Name: "mutationCreateSandboxWidget", Source: candidateWidgetMutation},
	})

	if rep.OK {
		t.Fatalf("expected bundle FAIL: mutation resolves a concept the bundle does not define, got OK: %+v", rep.Diagnostics)
	}
	d := rep.Diagnostics[0]
	if d.OK || d.Skipped || d.Error == "" {
		t.Errorf("expected a hard resolve failure, got %+v", d)
	}
}

// TestSandboxCompileBundle_ConceptMissingNamespaceFails: a candidate concept
// without @namespace cannot assemble a canonical id and is rejected with a
// precise diagnostic. Absent @version is NOT a failure -- it defaults to
// 1.0.0 (#2613), so @namespace alone compiles.
func TestSandboxCompileBundle_ConceptMissingNamespaceFails(t *testing.T) {
	rep := SandboxCompileBundle([]SandboxConstruct{
		{
			Kind: "concept",
			Name: "sandboxNoNamespace",
			Source: `@description("Missing namespace")
concept sandboxNoNamespace {
  label  string
}`,
		},
	})

	if rep.OK {
		t.Fatalf("expected bundle FAIL on concept without @namespace, got OK: %+v", rep.Diagnostics)
	}
	d := rep.Diagnostics[0]
	if d.OK || d.Skipped || !strings.Contains(d.Error, "namespace") {
		t.Errorf("expected a missing-namespace failure, got %+v", d)
	}

	rep = SandboxCompileBundle([]SandboxConstruct{
		{
			Kind: "concept",
			Name: "sandboxDefaultVersion",
			Source: `@namespace("sandboxns")
@description("No @version: the 1.0.0 default applies")
concept sandboxDefaultVersion {
  label  string
}`,
		},
	})
	if !rep.OK {
		t.Fatalf("concept with @namespace but no @version must compile under the #2613 default, got: %+v", rep.Diagnostics)
	}
}

// TestSandboxCompileBundle_ConceptNameMismatchFails: the declared
// construct.Name must equal the concept name in the source.
func TestSandboxCompileBundle_ConceptNameMismatchFails(t *testing.T) {
	rep := SandboxCompileBundle([]SandboxConstruct{
		{
			Kind: "concept",
			Name: "claimedConcept",
			Source: `@version("1.0.0")
@namespace("sandboxns")
concept actualConcept {
  label  string
}`,
		},
	})

	if rep.OK {
		t.Fatalf("expected bundle FAIL on concept name mismatch, got OK: %+v", rep.Diagnostics)
	}
	d := rep.Diagnostics[0]
	if d.OK || d.Skipped || !strings.Contains(d.Error, "does not match") {
		t.Errorf("expected a name-mismatch failure, got %+v", d)
	}
}

// TestSandboxCompileBundle_BrokenCandidateConceptFails: a syntactically
// broken concept fails the bundle.
func TestSandboxCompileBundle_BrokenCandidateConceptFails(t *testing.T) {
	rep := SandboxCompileBundle([]SandboxConstruct{
		{
			Kind: "concept",
			Name: "sandboxBroken",
			// Missing closing brace -> no parseable concept decl.
			Source: `@version("1.0.0")
@namespace("sandboxns")
concept sandboxBroken {
  label  string`,
		},
	})

	if rep.OK {
		t.Fatalf("expected bundle FAIL on broken concept, got OK: %+v", rep.Diagnostics)
	}
	d := rep.Diagnostics[0]
	if d.OK || d.Skipped || d.Error == "" {
		t.Errorf("expected a hard failure for the broken concept, got %+v", d)
	}
}
