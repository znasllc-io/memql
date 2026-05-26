package memql

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// representativeRuntimeQueries is the equivalence-test corpus for
// #248: a small set of shapes that cover every category of runtime
// query the 123 live `engine.Execute(ctx, string)` call sites use.
// The corpus is derived from a literature search of those sites:
//
//   - 121 of 123 sites go through SDK builders that produce
//     `funcName({k: v, ...})` invocations (Mutation/Query/Builtin/
//     Logic). MutationProvisionWorkspaceBuild is a representative
//     mutation, queryDueTrainAgentRetryPlans is representative of a
//     no-arg query, trainAgent is a builtin with mixed arg types.
//   - 2 of 123 sites pass hand-written string literals:
//     "concept==v1:cluster:node" (filter) and
//     "queryDueTrainAgentRetryPlans({})" (already covered).
//   - The filter form joined by `;` (AND) shows up in
//     hand-written admin queries via `BrowseConcept`.
//   - actor./args./payload. accessors flow through the
//     #244-shared classifier and should produce identical
//     reference nodes through either path.
//
// Add a new representative shape here if a future caller adopts
// one this corpus doesn't cover.
var representativeRuntimeQueries = []struct {
	name  string
	query string
}{
	{"concept-only filter", `concept==v1:cluster:node`},
	{"compound filter joined by ;", `concept==v1:cluster:node; payload.name=="bff"`},
	{"compound filter joined by &&", `concept==v1:cluster:node && payload.name=="bff"`},
	{"actor accessor in RHS", `id==actor.userId`},
	{"args accessor in RHS", `payload.spaceId==args.spaceId`},
	{"no-arg query invocation", `queryDueTrainAgentRetryPlans({})`},
	{"single-arg mutation invocation", `mutationCreateRecord({recordId: "v1:data:record:abc"})`},
	{"mixed-arg builtin invocation",
		`trainAgent({agentId: "v1:agents:agent:abc", domains: ["x", "y"], tools: []})`},
	// Bare-parens no-arg invocation -- some integrations write
	// `queryFoo()` instead of `queryFoo({})`. Found at
	// integrations/agents/factory.go:238 (queryActiveAgentRoles()).
	{"bare-parens no-arg invocation", `queryActiveAgentRoles()`},
	// Modern single-paren directive calls -- the langparser now
	// emits the specialised AST nodes directly (#254). reflect.DeepEqual
	// holds against the memql parser for every shape below.
	{"paginate limit only", `paginate(concept==v1:cluster:node, 10)`},
	{"paginate limit + offset", `paginate(concept==v1:cluster:node, 10, 5)`},
	{"sort single field default desc", `sort(concept==v1:cluster:node, "createdAt")`},
	{"sort field + explicit desc", `sort(concept==v1:cluster:node, "createdAt", "desc")`},
	{"sort field + explicit asc", `sort(concept==v1:cluster:node, "createdAt", "asc")`},
	{"sort two fields with directions", `sort(concept==v1:cluster:node, "createdAt", "desc", "payload.name", "asc")`},
	{"select single field", `select(concept==v1:cluster:node, "payload.name")`},
	{"select multiple fields", `select(concept==v1:cluster:node, "payload.name", "payload.role")`},
	{"asOf latest", `asOf(concept==v1:cluster:node, latest)`},
	{"asOf rfc3339", `asOf(concept==v1:cluster:node, "2026-01-01T00:00:00Z")`},
	{"withDepth", `withDepth(concept==v1:cluster:node, 2)`},
	// shape() runtime usages always carry an inline template object;
	// the bare-string "named shape" form exists in DSL contexts only
	// and isn't part of the runtime equivalence contract.
	{"shape inline template", `shape(concept==v1:cognition:space;payload.active==true, {"id": node("id")})`},
	{"shape composite paginate + sort", `shape(paginate(sort(concept==v1:cognition:space:context; payload.spaceId=="spc1", "createdAt", "desc"), 1), {"snapshot": node("payload.snapshot")})`},
	// Single-paren relationship wrappers -- these used to ride the
	// generic wrapperFunctions branch in the langparser; they now
	// also have equivalence coverage so the contract is locked in.
	{"relationship parentOf", `parentOf(concept==v1:cluster:node)`},
	{"relationship childOf", `childOf(concept==v1:cluster:node)`},
	{"relationship aliasOf", `aliasOf(concept==v1:cluster:node)`},
	{"relationship equals", `equals(concept==v1:cluster:node)`},
	{"relationship interactsWith", `interactsWith(concept==v1:cluster:node)`},
	{"relationship contains (single-arg)", `contains(concept==v1:cluster:node)`},
	{"relationship owns", `owns(concept==v1:cluster:node)`},
	{"relationship createdBy", `createdBy(concept==v1:cluster:node)`},
	{"relationship ids", `ids(concept==v1:cluster:node)`},
}

// TestParseViaLangparser_CoversCorpus asserts the opt-in langparser
// path parses every shape in the representative corpus without
// erroring. The trickier equivalence assertion -- that the produced
// engine AST matches what the memql parser produces for the same
// input -- lives in TestParseViaLangparser_Equivalence below.
func TestParseViaLangparser_CoversCorpus(t *testing.T) {
	for _, tc := range representativeRuntimeQueries {
		t.Run(tc.name, func(t *testing.T) {
			node, err := parseViaLangparser(tc.query)
			if err != nil {
				t.Fatalf("parseViaLangparser(%q): %v", tc.query, err)
			}
			if node == nil {
				t.Fatalf("parseViaLangparser(%q): returned nil node + nil error", tc.query)
			}
		})
	}
}

// TestParseViaLangparser_Equivalence is the #248 acceptance test:
// for every representative runtime query shape, the langparser path
// must produce the SAME engine AST as the memql parser. If a
// future change to either parser silently diverges from the other,
// this test fails.
//
// The comparison uses reflect.DeepEqual on the engine ExpressionNode
// tree. Both paths emit the exact same engine AST types
// (*ComparisonExpression, *FunctionCallExpression, *LogicalExpression,
// *ActorReference, *ArgReference, ...) so DeepEqual is a precise check
// -- a structural divergence would surface as a Go-level inequality.
func TestParseViaLangparser_Equivalence(t *testing.T) {
	for _, tc := range representativeRuntimeQueries {
		t.Run(tc.name, func(t *testing.T) {
			langNode, err := parseViaLangparser(tc.query)
			if err != nil {
				t.Fatalf("parseViaLangparser(%q): %v", tc.query, err)
			}
			tokens, err := tokenize(tc.query)
			if err != nil {
				t.Fatalf("tokenize(%q): %v", tc.query, err)
			}
			memqlNode, err := newParser(tokens, nil).parse()
			if err != nil {
				t.Fatalf("memql parse(%q): %v", tc.query, err)
			}
			if !reflect.DeepEqual(langNode, memqlNode) {
				t.Errorf("cross-parser AST divergence on %q:\n  langparser path: %#v\n  memql path:      %#v",
					tc.query, langNode, memqlNode)
			}
		})
	}
}

// TestParseViaLangparser_RejectsTimestampSuffix asserts the upfront
// langparserPathUnsupported detector catches the `@latest` /
// `@"..."` suffix shape and returns the sentinel error so e.Parse
// can fall back to the memql parser. Without this guard the
// langparser path would surface a confusing "unexpected token after
// expression" instead.
func TestParseViaLangparser_RejectsTimestampSuffix(t *testing.T) {
	for _, src := range []string{
		`concept==v1:cluster:node @latest`,
		`concept==v1:cluster:node @"2026-01-01T00:00:00Z"`,
	} {
		t.Run(src, func(t *testing.T) {
			_, err := parseViaLangparser(src)
			if !errors.Is(err, errLangparserUnsupported) {
				t.Errorf("got %v, want errLangparserUnsupported", err)
			}
		})
	}
}

// TestParseViaLangparser_RejectsInlineSpec asserts inline-spec
// definitions (`name := expr`) fall back too.
func TestParseViaLangparser_RejectsInlineSpec(t *testing.T) {
	src := `myFilter := concept==v1:cluster:node`
	_, err := parseViaLangparser(src)
	if !errors.Is(err, errLangparserUnsupported) {
		t.Errorf("got %v, want errLangparserUnsupported", err)
	}
}

// TestLangparserPathDirectivesNotFlagged confirms the upfront
// detector no longer rejects modern single-paren directive calls.
// The langparser produces the specialised AST types directly (#254),
// so these queries take the opt-in path and equivalence is exercised
// by TestParseViaLangparser_Equivalence above.
func TestLangparserPathDirectivesNotFlagged(t *testing.T) {
	for _, src := range []string{
		`paginate(concept==v1:cluster:node, 10)`,
		`sort(concept==v1:cluster:node, "createdAt", "desc")`,
		`select(concept==v1:cluster:node, "payload.name")`,
		`asOf(concept==v1:cluster:node, latest)`,
		`withDepth(concept==v1:cluster:node, 2)`,
		`shape(concept==v1:cognition:space, "spaceCard")`,
		`mutationPaginateFoo({id: "x"})`,
		`payload.shape=="cylinder"`,
	} {
		t.Run(src, func(t *testing.T) {
			if langparserPathUnsupported(src) {
				t.Errorf("detector flagged %q after #254 removed directive-name guard", src)
			}
		})
	}
}

// TestLangparserPathUnsupported_QuoteAware confirms the upfront
// detector skips `@` and `:=` inside string literals so a query
// like `... payload.title=="@latest"` doesn't trigger a spurious
// fallback. Only structural occurrences of the trailing-feature
// tokens should fall back.
func TestLangparserPathUnsupported_QuoteAware(t *testing.T) {
	for _, src := range []string{
		`payload.title=="@latest"`,
		`payload.title=="x := y"`,
	} {
		t.Run(src, func(t *testing.T) {
			if langparserPathUnsupported(src) {
				t.Errorf("detector flagged %q (quoted content) as unsupported", src)
			}
		})
	}
}

// TestEngineUseLangparserRuntimeToggle is a smoke test for the
// (*MemQLEngine).UseLangparserRuntime / LangparserRuntimeEnabled
// pair -- the bootstrap-side opt-in handle for #248. Default OFF;
// flipping ON is reflected in LangparserRuntimeEnabled. The flag's
// effect on Parse is exercised by the equivalence test above.
//
// The planned default-flip (#249) is now down to its remaining
// architectural prerequisites: #255 (number literal int/float
// standardisation), #256 (introspection builtin dispatch), and #257
// (parser_test error-text assertions). #254 closed the directive +
// relationship AST gap that used to dominate the divergence list.
func TestEngineUseLangparserRuntimeToggle(t *testing.T) {
	e := &MemQLEngine{}
	if e.LangparserRuntimeEnabled() {
		t.Fatal("default should be OFF")
	}
	e.UseLangparserRuntime(true)
	if !e.LangparserRuntimeEnabled() {
		t.Fatal("UseLangparserRuntime(true) did not enable")
	}
	e.UseLangparserRuntime(false)
	if e.LangparserRuntimeEnabled() {
		t.Fatal("UseLangparserRuntime(false) did not disable")
	}
}

// TestParseViaLangparser_DoesNotMaskRealErrors confirms the
// fall-back contract: errLangparserUnsupported is the ONLY sentinel
// e.Parse should swallow; every other parse error must propagate
// unchanged so the caller doesn't get a misleading message from
// the wrong parser.
func TestParseViaLangparser_DoesNotMaskRealErrors(t *testing.T) {
	// Genuinely malformed input -- nothing about it triggers the
	// upfront-unsupported detector, so it reaches
	// langparser.ParseExpression which returns its own real error.
	_, err := parseViaLangparser(`concept==`)
	if err == nil {
		t.Fatal("expected real parse error, got nil")
	}
	if errors.Is(err, errLangparserUnsupported) {
		t.Errorf("real parse error masquerading as errLangparserUnsupported: %v", err)
	}
	if strings.Contains(err.Error(), "not yet supported") {
		t.Errorf("real parse error formatted like the sentinel: %v", err)
	}
}
