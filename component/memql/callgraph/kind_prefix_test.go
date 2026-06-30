package callgraph

import "testing"

// Story 9 (#2335) -- the whole-tree gate's half of the kind-prefixed invocation
// rule (construct-invocation ADR Decision 1): a call to an imported construct of
// an invocation kind (query / mutation / logic / action / builtin / automation)
// MUST carry its kind keyword at the call site (`<kind> name(...)`). A bare
// `name(...)` is a `missing-kind-prefix` finding.

const kindPrefixUseHeader = `use cluster.queries.{ existingCluster }
use cluster.mutations.{ createNode }
`

// A bare construct call (no kind keyword) is flagged.
func TestKindPrefix_BareCallFlagged(t *testing.T) {
	src := kindPrefixUseHeader + `
logic decideThing {
  args { x string @required }
  body {
    existing := existingCluster()
    return existing.empty()
  }
}`
	fs := CheckFile("dsl/cluster/logic.memql", src, nil)
	if !has(fs, "missing-kind-prefix") {
		t.Fatalf("expected missing-kind-prefix finding for bare `existingCluster()`; got %v", rules(fs))
	}
}

// A correctly kind-prefixed call produces NO missing-kind-prefix finding.
func TestKindPrefix_PrefixedCallClean(t *testing.T) {
	src := kindPrefixUseHeader + `
logic decideThing {
  args { x string @required }
  body {
    existing := query existingCluster()
    return existing.empty()
  }
}`
	fs := CheckFile("dsl/cluster/logic.memql", src, nil)
	if has(fs, "missing-kind-prefix") {
		t.Fatalf("kind-prefixed `query existingCluster()` must NOT be flagged; got %v", rules(fs))
	}
}

// A bare mutation call (in an analysed construct -- here a logic body) is
// flagged with the called construct's kind. (The call-graph gate analyses
// logic / query / mutation / action constructs; automation step bodies are out
// of its scope, so the caller here is a logic.)
func TestKindPrefix_BareMutationFlagged(t *testing.T) {
	src := kindPrefixUseHeader + `
logic register {
  args { event object @required }
  body {
    node := createNode(id: args.event.payload.id)
    return node
  }
}`
	fs := CheckFile("dsl/cluster/logic.memql", src, nil)
	if !has(fs, "missing-kind-prefix") {
		t.Fatalf("expected missing-kind-prefix finding for bare `createNode(...)`; got %v", rules(fs))
	}
}

// A call-shaped token mentioned only in an @description string or a // comment
// is prose, not a call site, and must NOT be flagged.
func TestKindPrefix_ProseExempt(t *testing.T) {
	src := kindPrefixUseHeader + `
@description("Decides things. Historically returned existingCluster({foo: bar}); see createNode({...}).")
logic decideThing {
  args { x string @required }
  body {
    // a comment mentioning createNode(id: x) must not count as a call
    return query existingCluster().empty()
  }
}`
	fs := CheckFile("dsl/cluster/logic.memql", src, nil)
	if has(fs, "missing-kind-prefix") {
		t.Fatalf("call-shaped tokens in prose/comments must NOT be flagged; got %v", rules(fs))
	}
}
