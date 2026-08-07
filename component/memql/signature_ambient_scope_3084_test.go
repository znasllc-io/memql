package memql

import (
	"fmt"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// memql#3084: the SIGNATURE-concept resolution path had both defects memql#3026
// removed from its neighbour in the same file -- it took the LAST path segment
// as the namespace hint where assembly takes the FIRST, and it applied no
// ambient-scope gate at all.
//
// Every test here uses a NESTED origin, `agents/tools/probe.memql`, because
// that is the only shape where the two segments disagree: assembly for that
// file emits `v1:agents:*` (unified_loader's `dir := firstPathSegment(p)`)
// while the retired hint said `tools`. And `tools` is not a namespace this tree
// declares under -- it is a foreign domain any MEMQL_DSL_PATH bundle may claim,
// since it is not a core domain and RegisterTree permits it. That is the same
// cross-repo scenario #3026 was about, one construct over.
//
// The fixtures are stubRegistry rather than the real tree on purpose: the
// widening is only observable when BOTH `v1:tools:widget` and `v1:agents:widget`
// exist, so a bare trailing-segment match cannot pick one and the HINT decides.
// With only the foreign concept registered, resolveBareConceptNameWithNamespace
// returns its single match whatever the hint is, and the test would pass for the
// wrong reason.

// stubRegistry is a fixed concept set implementing the two-method Registry
// interface, so a fixture can state exactly which ids exist without depending
// on the real tree (the same reason the sibling canonicalId gate takes its
// declaredNS explicitly).
type stubRegistry struct{ ids []string }

func (s stubRegistry) List() []*memoryNodes.Concept {
	out := make([]*memoryNodes.Concept, 0, len(s.ids))
	for _, id := range s.ids {
		out = append(out, &memoryNodes.Concept{Name: id, NodeType: "object"})
	}
	return out
}

func (s stubRegistry) Get(name string) (*memoryNodes.Concept, error) {
	for _, id := range s.ids {
		if id == name {
			return &memoryNodes.Concept{Name: id, NodeType: "object"}, nil
		}
	}
	return nil, fmt.Errorf("concept %q not registered", name)
}

const nestedForeignOrigin = "agents/tools/probe.memql"

// TestSignatureAmbientBind_NestedOriginUsesRootDomain is the failing-before /
// passing-after assertion, and it asserts on the MUTATION WRITE TARGET rather
// than on BoundConcept.
//
// That distinction is the whole point. There are two signature resolution
// sites: one rewrites the AST (the write target, the query filter) and one
// derives BoundConcept. The parked attempt gated only the second, and measured
// against a registry holding the foreign concept it produced
// `BoundConcept == ""` while `MutationTemplate.Concept` was still
// `v1:tools:widget` -- so the defect survived a fix whose test only looked at
// BoundConcept. Asserting the write target is what makes this test able to fail.
func TestSignatureAmbientBind_NestedOriginUsesRootDomain(t *testing.T) {
	reg := stubRegistry{ids: []string{"v1:tools:widget", "v1:agents:widget"}}

	src := `mutate widget probeWrite {
  args {
    widgetId string @required
  }
  insert {
    id: args.widgetId
    createdAt: now
  }
}`

	fn, err := tryParseNewFunctionSyntax("probeWrite", "mutation", src, nestedForeignOrigin, reg)
	if err != nil {
		t.Fatalf("same-domain ambient bind must still load: %v", err)
	}
	if fn.MutationTemplate == nil {
		t.Fatal("mutation template missing")
	}
	// Pre-fix this is "v1:tools:widget": the hint was the LAST path segment.
	if got := fn.MutationTemplate.Concept; got != "v1:agents:widget" {
		t.Errorf("mutation write target = %q, want %q -- the ambient hint must come from the origin's ROOT domain (assembly's first path segment), not its last", got, "v1:agents:widget")
	}
	if got := fn.BoundConcept; got != "v1:agents:widget" {
		t.Errorf("BoundConcept = %q, want %q -- both signature sites must agree", got, "v1:agents:widget")
	}
}

// TestSignatureAmbientBind_ForeignNamespaceIsRefused is the gate itself: a
// nested file whose subdirectory name collides with a foreign namespace, and
// NOTHING in the tree its root domain could have declared under that name.
//
// The refusal is a HARD ERROR, matching the sibling canonicalId path (the
// refusal-semantics decision on memql#3084). A silent blank would be worse than
// no gate: an empty BoundConcept makes markSecretArgsFields a no-op, disabling
// @secret argument redaction, and skips ensureBoundConceptFilter -- while the
// ungated rewrite site stamps the foreign write target anyway.
func TestSignatureAmbientBind_ForeignNamespaceIsRefused(t *testing.T) {
	// Only the FOREIGN concept exists. `agents` never declared a widget.
	reg := stubRegistry{ids: []string{"v1:tools:widget"}}

	src := `mutate widget probeWrite {
  args {
    widgetId string @required
  }
  insert {
    id: args.widgetId
    createdAt: now
  }
}`

	fn, err := tryParseNewFunctionSyntax("probeWrite", "mutation", src, nestedForeignOrigin, reg)
	if err == nil {
		// Name the exact pre-fix failure mode, so a regression reads as the
		// defect it is rather than as "a test went red".
		target, bound := "<nil template>", fn.BoundConcept
		if fn.MutationTemplate != nil {
			target = fn.MutationTemplate.Concept
		}
		t.Fatalf("ambient bind to a FOREIGN namespace must be refused, got a loaded function: write target = %q, BoundConcept = %q", target, bound)
	}
	if !strings.Contains(err.Error(), "widget") || !strings.Contains(err.Error(), "use ") {
		t.Errorf("refusal must name the concept and the import that fixes it, got: %v", err)
	}
}

// TestSignatureAmbientBind_QueryFilterIsGated is the query-side half of the
// same assertion. A refused signature bind must not leave a query filtering on
// a foreign concept -- the rewrite site sets a `concept ==` filter from the same
// symbol table that sets a mutation's write target, so gating one and not the
// other would leave the read path widened.
func TestSignatureAmbientBind_QueryFilterIsGated(t *testing.T) {
	src := `query widget probeRead {
  args {
    widgetId string @required
  }
  filter row.id==args.widgetId
}`

	// Refused: only the foreign concept exists.
	if _, err := tryParseNewFunctionSyntax("probeRead", "query", src,
		nestedForeignOrigin, stubRegistry{ids: []string{"v1:tools:widget"}}); err == nil {
		t.Error("query with a foreign ambient signature bind must be refused, got nil error")
	}

	// Admitted, and bound to the ROOT domain's concept rather than the
	// subdirectory-named one.
	fn, err := tryParseNewFunctionSyntax("probeRead", "query", src,
		nestedForeignOrigin, stubRegistry{ids: []string{"v1:tools:widget", "v1:agents:widget"}})
	if err != nil {
		t.Fatalf("same-domain ambient query bind must load: %v", err)
	}
	if got := fn.BoundConcept; got != "v1:agents:widget" {
		t.Errorf("query BoundConcept = %q, want %q", got, "v1:agents:widget")
	}
	// The filter the engine actually executes must name the same id. Reading it
	// off the rendered expression rather than off BoundConcept is deliberate:
	// BoundConcept agreeing while the executed filter disagrees is exactly the
	// split the parked attempt shipped.
	if src := fn.ExprSource; strings.Contains(src, "v1:tools:widget") {
		t.Errorf("query filter still references the foreign concept: %s", src)
	}
}

// TestSignatureAmbientBind_ExplicitImportStillWins pins the boundary of the
// gate. An explicit file-top import IS the authorization for a cross-domain
// reference (#2617's discipline), so the scope check must not apply to it --
// otherwise every legitimate cross-domain signature binding in the tree breaks,
// which is 528 of them.
func TestSignatureAmbientBind_ExplicitImportStillWins(t *testing.T) {
	reg := stubRegistry{ids: []string{"v1:tools:widget", "v1:agents:widget"}}

	src := `use tools.concepts.{ widget }

mutate widget probeWrite {
  args {
    widgetId string @required
  }
  insert {
    id: args.widgetId
    createdAt: now
  }
}`

	fn, err := tryParseNewFunctionSyntax("probeWrite", "mutation", src, nestedForeignOrigin, reg)
	if err != nil {
		t.Fatalf("explicit cross-domain import must still load: %v", err)
	}
	if fn.MutationTemplate == nil {
		t.Fatal("mutation template missing")
	}
	if got := fn.MutationTemplate.Concept; got != "v1:tools:widget" {
		t.Errorf("explicit import must win over the ambient domain: write target = %q, want %q", got, "v1:tools:widget")
	}
}

// TestSignatureAmbientBind_OriginlessAuthoredConstructUngated documents the one
// case the gate deliberately does NOT cover, so that it is a recorded boundary
// rather than a hole someone finds later.
//
// An authored construct compiled through compileAuthoredFunction carries the
// origin "authored:<kind>:<name>", which has no directory -- so it has no
// assembly directory and no domain to be ambient WITHIN. There is no
// last-path-segment for a foreign namespace to collide with, which is the
// widening this rule closes. Both signature sites already resolved such origins
// by unique trailing-segment match before memql#3084, so preserving that
// changes nothing on the authoring path; tightening it would be a behaviour
// change to the authoring surface that this issue did not ask for.
func TestSignatureAmbientBind_OriginlessAuthoredConstructUngated(t *testing.T) {
	reg := stubRegistry{ids: []string{"v1:tools:widget"}}

	src := `mutate widget probeWrite {
  args {
    widgetId string @required
  }
  insert {
    id: args.widgetId
    createdAt: now
  }
}`

	fn, err := tryParseNewFunctionSyntax("probeWrite", "mutation", src, "authored:mutation:probeWrite", reg)
	if err != nil {
		t.Fatalf("origin-less authored construct must keep resolving by unique match: %v", err)
	}
	if fn.MutationTemplate == nil || fn.MutationTemplate.Concept != "v1:tools:widget" {
		t.Errorf("origin-less authored construct: want write target %q, got %+v", "v1:tools:widget", fn.MutationTemplate)
	}
}
