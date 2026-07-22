package sense

// Tests for kind-filtered in-body invocation completion (#2733): after a
// query/mutation/logic verb in a behavioral body, offer only functions of that
// kind instead of the whole registry (the census gap where an automation body
// offered queries, tools, and mutations interchangeably).

import "testing"

func invocationRegistry() *stubRegistry {
	return &stubRegistry{functions: map[string]*FunctionInfo{
		"listOrders": {Name: "listOrders", Kind: "query"},
		"writeThing": {Name: "writeThing", Kind: "mutation"},
		"dayRollup":  {Name: "dayRollup", Kind: "logic"},
		"searchTool": {Name: "searchTool", Kind: "tool"},
	}}
}

func TestInvocation_QueryVerbOffersOnlyQueries(t *testing.T) {
	got := completeAtEnd(New(invocationRegistry()), "logic doThing {\n  body {\n    query ")
	if !got["listOrders"] {
		t.Errorf("query verb should offer the query listOrders, got %v", got)
	}
	for _, never := range []string{"writeThing", "dayRollup", "searchTool"} {
		if got[never] {
			t.Errorf("query verb must not offer %q (wrong kind)", never)
		}
	}
}

func TestInvocation_LogicVerbOffersOnlyLogic(t *testing.T) {
	got := completeAtEnd(New(invocationRegistry()), "automation sweep {\n  logic ")
	if !got["dayRollup"] {
		t.Errorf("logic verb should offer dayRollup, got %v", got)
	}
	for _, never := range []string{"listOrders", "writeThing", "searchTool"} {
		if got[never] {
			t.Errorf("logic verb must not offer %q", never)
		}
	}
}

func TestInvocation_MutationVerbOffersOnlyMutations(t *testing.T) {
	got := completeAtEnd(New(invocationRegistry()), "logic doThing {\n  body {\n    mutation ")
	if !got["writeThing"] || got["listOrders"] || got["dayRollup"] {
		t.Errorf("mutation verb should offer only writeThing, got %v", got)
	}
}

func TestInvocation_PrefixFilter(t *testing.T) {
	got := completeAtEnd(New(invocationRegistry()), "logic doThing {\n  body {\n    query list")
	if !got["listOrders"] {
		t.Errorf("`query list` should offer listOrders, got %v", got)
	}
}

// firedInvocation reports whether the result is the kind-filtered query
// invocation set -- listOrders (a query) present while writeThing (a mutation)
// is absent. Every fallthrough (func-body dump, block field completion,
// func-call args) either omits listOrders or also offers writeThing, so this
// cleanly distinguishes a fired invocation from a suppressed one.
func firedInvocation(got map[string]bool) bool {
	return got["listOrders"] && !got["writeThing"]
}

func TestInvocation_ConceptBodySuppressed(t *testing.T) {
	// A concept body holds field declarations; a field named `query` is not an
	// invocation (concept is not a behavioral construct).
	if firedInvocation(completeAtEnd(New(invocationRegistry()), "concept thing {\n  query ")) {
		t.Error("invocation fired in a concept body")
	}
}

func TestInvocation_MutateWriteBlockSuppressed(t *testing.T) {
	// mutate is not behavioral; an insert/accept key named `query` is a write
	// field, not an invocation.
	rp := &stubRegistry{
		functions: map[string]*FunctionInfo{
			"listOrders": {Name: "listOrders", Kind: "query"},
			"writeThing": {Name: "writeThing", Kind: "mutation"},
		},
		concepts: map[string]*ConceptInfo{"v1:orders:order": {Name: "v1:orders:order"}},
	}
	if firedInvocation(completeAtEnd(New(rp), "mutate order place {\n  insert {\n    query ")) {
		t.Error("invocation fired in a mutate insert block")
	}
}

func TestInvocation_CallArgsParenSuppressed(t *testing.T) {
	// Inside call-arg parens `foo(query ` the token is an argument, not an
	// invocation verb -- func-call-args completion must own it.
	if firedInvocation(completeAtEnd(New(invocationRegistry()), "logic doThing {\n  body {\n    foo(query ")) {
		t.Error("invocation fired inside call-arg parens")
	}
}

func TestInvocation_ArgsBlockSuppressed(t *testing.T) {
	// A field literally named `query` in an args block is a field declaration,
	// not an invocation: the invocation completer must NOT fire and hide the
	// field-type completion.
	rp := &stubRegistry{
		functions: map[string]*FunctionInfo{"listOrders": {Name: "listOrders", Kind: "query"}},
		concepts:  map[string]*ConceptInfo{"v1:todos:todo": {Name: "v1:todos:todo"}},
	}
	got := completeAtEnd(New(rp), "query todo todos {\n  args {\n    query ")
	if got["listOrders"] {
		t.Errorf("invocation completion fired inside an args block, hiding field types: %v", got)
	}
	if !got["string"] {
		t.Errorf("args block should offer field types (string), got %v", got)
	}
}

func TestInvocation_TopLevelDeclarationNotAffected(t *testing.T) {
	// `query ` at the top level (depth 0) is a construct declaration whose
	// signature binds a concept, NOT an invocation -- checkInvocationContext
	// requires body depth, so it must not fire here.
	ctx := analyzeCursorContext("query ", 1, len("query ")+1)
	if ctx.Kind == ContextInvocation {
		t.Errorf("top-level `query ` classified as invocation; want ConstructConcept")
	}
	if ctx.Kind != ContextConstructConcept {
		t.Errorf("top-level `query ` = %v, want ContextConstructConcept", ctx.Kind)
	}
}
