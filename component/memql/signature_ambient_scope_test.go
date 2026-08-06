package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// signature_ambient_scope_test.go -- memql#3084.
//
// memql#3026 fixed the ambient rule for `canonicalId`. The SIGNATURE-CONCEPT
// resolution path in the same file kept both defects that issue removed from
// its neighbour:
//
//   - the namespace hint came from DomainFromFilePath(origin) -- the LAST path
//     segment -- while boot assembles a concept id from the FIRST
//     (unified_loader.go: `dir := firstPathSegment(p)`). For
//     `agents/tools/askSpecialist.memql` the hint was `tools` where assembly
//     used `agents`.
//   - the hint was then handed to resolveBareConceptNameWithNamespace with NO
//     scope check -- no equivalent of the idIsInDomainAmbientScope gate the
//     canonicalId path now carries.
//
// So a nested `dsl/agents/tools/*.memql` carrying `mutate widget ...` could
// bind a foreign `v1:tools:widget` with no import: the same widening #3026
// closed, one construct over.
//
// Latent in-tree (no concept assembles under v1:tools / v1:roles / v1:skills),
// and live the moment a MEMQL_DSL_PATH bundle claims a domain named `tools`,
// `roles` or `skills` -- none of which is a core domain, so RegisterTree
// permits it. That cross-repo case is what #3026 was about.

// twoDomainRegistry registers the same trailing name under two namespaces, so
// resolution has a real choice to get wrong.
func twoDomainRegistry(t *testing.T) memoryNodes.Registry {
	t.Helper()
	return newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:agents:widget": {Name: "v1:agents:widget"},
		"v1:tools:widget":  {Name: "v1:tools:widget"},
	})
}

// THE REPRODUCTION. A nested file under `agents/` whose SUBDIRECTORY is named
// `tools` must not bind `v1:tools:widget` -- the file's domain is `agents`.
func TestNestedSignatureDoesNotBindAForeignNamespace(t *testing.T) {
	src := `mutate widget nestedWidgetWrite {
  args {
    widgetId  string  @required
  }
  insert {
    id: args.widgetId
  }
}`

	fn, err := tryParseNewFunctionSyntax(
		"nestedWidgetWrite", "mutation", src,
		"agents/tools/askSpecialist.memql", // NESTED: domain agents, subdir tools
		twoDomainRegistry(t),
	)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if fn.BoundConcept == "v1:tools:widget" {
		t.Errorf("a nested file under `agents/` bound the FOREIGN concept %q with no import.\n"+
			"The namespace hint came from the LAST path segment (`tools`) while boot assembles "+
			"this file's ids from the FIRST (`agents`), and nothing gated the ambient bind. A "+
			"MEMQL_DSL_PATH bundle claiming a domain named `tools` makes this live (memql#3084).",
			fn.BoundConcept)
	}
	if fn.BoundConcept != "v1:agents:widget" {
		t.Errorf("BoundConcept = %q, want the file's own domain concept v1:agents:widget",
			fn.BoundConcept)
	}
}

// The ordinary case must keep working: a NON-nested file binds its own
// domain's concept ambiently, with no import. This is what #2617 made ambient
// and what the tightening must not break.
func TestFlatSignatureStillBindsItsOwnDomainAmbiently(t *testing.T) {
	src := `mutate widget flatWidgetWrite {
  args {
    widgetId  string  @required
  }
  insert {
    id: args.widgetId
  }
}`

	fn, err := tryParseNewFunctionSyntax(
		"flatWidgetWrite", "mutation", src,
		"agents/mutations.memql", // the ordinary flat layout
		twoDomainRegistry(t),
	)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if fn.BoundConcept != "v1:agents:widget" {
		t.Errorf("BoundConcept = %q, want v1:agents:widget -- ambient same-domain binding (#2617) "+
			"must survive the #3084 tightening", fn.BoundConcept)
	}
}

// A foreign namespace stays reachable WITH an explicit import. The tightening
// removes the ambient path across namespaces, not the ability to say so.
func TestForeignNamespaceStillReachableViaImport(t *testing.T) {
	src := `use tools.concepts.{ widget }

mutate widget importedWidgetWrite {
  args {
    widgetId  string  @required
  }
  insert {
    id: args.widgetId
  }
}`

	fn, err := tryParseNewFunctionSyntax(
		"importedWidgetWrite", "mutation", src,
		"agents/tools/askSpecialist.memql",
		twoDomainRegistry(t),
	)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if fn.BoundConcept != "v1:tools:widget" {
		t.Errorf("BoundConcept = %q, want v1:tools:widget -- an EXPLICIT import must still reach "+
			"a foreign namespace; only the ambient path is tightened (memql#3084)",
			fn.BoundConcept)
	}
}

// An unambiguous trailing name resolves regardless of nesting -- there is only
// one candidate, so no scope decision arises. Guards against the tightening
// refusing binds it has no reason to.
func TestUnambiguousNameResolvesFromANestedOrigin(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:agents:gadget": {Name: "v1:agents:gadget"},
	})
	src := `mutate gadget nestedGadgetWrite {
  args {
    gadgetId  string  @required
  }
  insert {
    id: args.gadgetId
  }
}`
	fn, err := tryParseNewFunctionSyntax(
		"nestedGadgetWrite", "mutation", src,
		"agents/tools/askSpecialist.memql", registry,
	)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if fn.BoundConcept != "v1:agents:gadget" {
		t.Errorf("BoundConcept = %q, want v1:agents:gadget", fn.BoundConcept)
	}
}

// The hint itself must come from the ROOT domain, pin-aware -- asserted
// directly so a regression in the hint is distinguishable from a regression in
// the scope gate. They are separate defects and #3084 fixes both.
func TestSignatureHintUsesTheRootDomainNotTheLastSegment(t *testing.T) {
	for _, tc := range []struct{ origin, want string }{
		{"agents/tools/askSpecialist.memql", "agents"},
		{"agents/roles/foundational.memql", "agents"},
		{"agents/mutations.memql", "agents"},
		{"cognition/queries.memql", "cognition"},
	} {
		if got := RootDomainFromFilePath(tc.origin); got != tc.want {
			t.Errorf("root domain of %q = %q, want %q -- the LAST segment is what #3084 fixes",
				tc.origin, got, tc.want)
		}
	}
	// And the last-segment form really does differ, or the test above proves
	// nothing about the defect.
	if DomainFromFilePath("agents/tools/askSpecialist.memql") == "agents" {
		t.Skip("DomainFromFilePath no longer returns the last segment; the defect shape changed")
	}
	if !strings.EqualFold(DomainFromFilePath("agents/tools/askSpecialist.memql"), "tools") {
		t.Errorf("control: DomainFromFilePath should return the LAST segment %q, got %q",
			"tools", DomainFromFilePath("agents/tools/askSpecialist.memql"))
	}
}
