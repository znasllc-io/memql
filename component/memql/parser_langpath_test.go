package memql

import (
	"errors"
	"reflect"
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
	// select entries removed from the equivalence corpus in #249:
	// langparser parses select() correctly (the AST is equivalent
	// to memql's), but doesn't populate Plan.Fields/Plan.Metadata
	// the way the memql parser's select handler does. Until that
	// post-parse plumbing reaches parity, select() falls back.
	// The fallback contract for select() lives in
	// selectFallsBackToMemqlQueries below.
	{"asOf latest", `asOf(concept==v1:cluster:node, latest)`},
	{"asOf rfc3339", `asOf(concept==v1:cluster:node, "2026-01-01T00:00:00Z")`},
	{"withDepth", `withDepth(concept==v1:cluster:node, 2)`},
	// shape() entries removed from the equivalence corpus in #249:
	// the langparser's shape grammar has two gaps the executor tests
	// surface (array templates `[...]` parse-error; nested-object
	// templates parse but execute differently). Until parity is
	// reached, `langparserPathUnsupported` short-circuits shape()
	// to the memql parser; the previously-equivalent shape entries
	// now live in `shapeFallsBackToMemqlQueries` below where the
	// fallback contract is asserted explicitly.
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

// shapeFallsBackToMemqlQueries covers the runtime shape() usages
// that #249 forces back to the memql parser via the targeted
// shape() detector in langparserPathUnsupported. Each entry MUST:
//
//  1. Trigger the upfront unsupported detector
//     (`langparserPathUnsupported == true` -> parseViaLangparser
//     returns errLangparserUnsupported).
//  2. Round-trip cleanly through the memql parser so the fallback
//     produces a usable AST.
//
// Move entries back into representativeRuntimeQueries (and delete
// here) once langparser shape parity ships -- the corpus then asserts
// the langparser path handles them equivalently. The follow-up issue
// for that parity work is tracked under epic #218.
var shapeFallsBackToMemqlQueries = []struct {
	name  string
	query string
}{
	{"shape inline template", `shape(concept==v1:cognition:space;payload.active==true, {"id": node("id")})`},
	{"shape composite paginate + sort", `shape(paginate(sort(concept==v1:cognition:space:context; payload.spaceId=="spc1", "createdAt", "desc"), 1), {"snapshot": node("payload.snapshot")})`},
	// Array-form template (the parse-error failure mode):
	{"shape array template", `shape(concept==v1:conversation;id=="conv-1",[node("id"),children(node("id")),"done"])`},
	// Nested-object template with relationships (the
	// execute-differently failure mode):
	{"shape nested relations template", `shape(concept==v1:conversation;id=="conv-1",{"conversation":node("payload.title","id"),"messages":children({"id":node("id"),"author":createdBy(node("payload.name"))})})`},
}

// selectFallsBackToMemqlQueries covers select() runtime usages.
// See note on shapeFallsBackToMemqlQueries for the contract.
var selectFallsBackToMemqlQueries = []struct {
	name  string
	query string
}{
	{"select single field", `select(concept==v1:cluster:node, "payload.name")`},
	{"select multiple fields", `select(concept==v1:cluster:node, "payload.name", "payload.role")`},
	{"select with metadata wildcard", `select(concept==v1:assistant,"meta.*")`},
}

// memqlVersionCompatFallsBackQueries covers the
// `concept==memql:version` compat shape that the memql parser
// rewrites to memqlVersion BuiltinFunctionExpression. Langparser
// produces a generic ComparisonExpression -- different downstream
// dispatch. Falls back until the rewrite migrates to the
// meta-command dispatch (filed under epic #218).
var memqlVersionCompatFallsBackQueries = []struct {
	name  string
	query string
}{
	{"concept==memql:version", `concept==memql:version`},
	{"concept == memql:version with space", `concept == memql:version`},
}

// inOperatorFallsBackQueries covers the parenthesised collection-
// form of the `in` operator that langparser doesn't recognise.
var inOperatorFallsBackQueries = []struct {
	name  string
	query string
}{
	{"in with quoted list", `payload.stage in ("new","open")`},
	{"in with space variations", `payload.status  in  ( "active" , "live" )`},
}

// TestLangparserPathFallsBackOnShape pins the #249 shape() fallback:
// every shape() form in shapeFallsBackToMemqlQueries must short-
// circuit to the memql parser via the upfront detector. Catches
// regressions in BOTH directions: if a future fix lets shape()
// flow through the langparser cleanly, this test fails + the entry
// moves back into the equivalence corpus.
func TestLangparserPathFallsBackOnShape(t *testing.T) {
	for _, tc := range shapeFallsBackToMemqlQueries {
		t.Run(tc.name, func(t *testing.T) {
			if !langparserPathUnsupported(tc.query) {
				t.Errorf("langparserPathUnsupported(%q) returned false; shape() should short-circuit until parity ships", tc.query)
			}
			// And the public entry must return the sentinel
			// (not a stray parser error that would propagate).
			_, err := parseViaLangparser(tc.query)
			if !errors.Is(err, errLangparserUnsupported) {
				t.Errorf("parseViaLangparser(%q) -> %v; want errLangparserUnsupported", tc.query, err)
			}
		})
	}
}

// TestLangparserPathFallsBackOnSelect mirrors the shape test for
// select() -- pins the detector + sentinel for every select()
// entry in selectFallsBackToMemqlQueries.
func TestLangparserPathFallsBackOnSelect(t *testing.T) {
	for _, tc := range selectFallsBackToMemqlQueries {
		t.Run(tc.name, func(t *testing.T) {
			if !langparserPathUnsupported(tc.query) {
				t.Errorf("langparserPathUnsupported(%q) returned false; select() should short-circuit until Plan.Fields parity ships", tc.query)
			}
			_, err := parseViaLangparser(tc.query)
			if !errors.Is(err, errLangparserUnsupported) {
				t.Errorf("parseViaLangparser(%q) -> %v; want errLangparserUnsupported", tc.query, err)
			}
		})
	}
}

// TestLangparserPathFallsBackOnMemqlVersionCompat pins the
// `concept==memql:version` compat shape fallback.
func TestLangparserPathFallsBackOnMemqlVersionCompat(t *testing.T) {
	for _, tc := range memqlVersionCompatFallsBackQueries {
		t.Run(tc.name, func(t *testing.T) {
			if !langparserPathUnsupported(tc.query) {
				t.Errorf("langparserPathUnsupported(%q) returned false; concept==memql:version compat should short-circuit", tc.query)
			}
			_, err := parseViaLangparser(tc.query)
			if !errors.Is(err, errLangparserUnsupported) {
				t.Errorf("parseViaLangparser(%q) -> %v; want errLangparserUnsupported", tc.query, err)
			}
		})
	}
}

// TestLangparserPathFallsBackOnInOperator pins the `<expr> in (...)`
// collection-operator fallback.
func TestLangparserPathFallsBackOnInOperator(t *testing.T) {
	for _, tc := range inOperatorFallsBackQueries {
		t.Run(tc.name, func(t *testing.T) {
			if !langparserPathUnsupported(tc.query) {
				t.Errorf("langparserPathUnsupported(%q) returned false; in (...) collection operator should short-circuit", tc.query)
			}
			_, err := parseViaLangparser(tc.query)
			if !errors.Is(err, errLangparserUnsupported) {
				t.Errorf("parseViaLangparser(%q) -> %v; want errLangparserUnsupported", tc.query, err)
			}
		})
	}
}

// TestContainsRuntimeCallBoundary pins the word-boundary semantics
// of the containsRuntimeCall helper -- a substring match would
// false-positive on `payload.shapeId` / `myShape(` / a quoted
// "shape(" string and silently force fallback for queries the
// langparser handles fine.
func TestContainsRuntimeCallBoundary(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		fn     string
		expect bool
	}{
		{"bare call matches", `shape(x, y)`, "shape", true},
		{"prefix-bounded matches", `; shape(x, y)`, "shape", true},
		{"identifier suffix does not match", `payload.shapeId == "abc"`, "shape", false},
		{"identifier prefix does not match", `myShape(x)`, "shape", false},
		{"inside quoted string does not match", `payload.title=="shape(foo)"`, "shape", false},
		{"no occurrence", `concept==v1:cluster:node`, "shape", false},
		{"end-of-string without paren does not match", `payload.shape`, "shape", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := containsRuntimeCall(c.query, c.fn)
			if got != c.expect {
				t.Errorf("containsRuntimeCall(%q, %q) = %v; want %v", c.query, c.fn, got, c.expect)
			}
		})
	}
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
		// select( and shape( are intentionally NOT in this list
		// post-#249 -- they're in the fallback bucket
		// (shapeFallsBackToMemqlQueries / selectFallsBackToMemqlQueries)
		// until langparser parity reaches the Plan.Fields population
		// + shape grammar gaps.
		`asOf(concept==v1:cluster:node, latest)`,
		`withDepth(concept==v1:cluster:node, 2)`,
		`mutationPaginateFoo({id: "x"})`,
		`payload.shape=="cylinder"`,
		// Identifiers and function-call args that contain the
		// substring `in` must NOT trip the in-operator detector
		// (would force unnecessary fallback). Pin the boundary.
		`payload.inside == true`,
		`integerField == 5`,
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
// pair -- the bootstrap-side opt-out handle for #249's flipped
// default.
//
// **#249 flipped the default:** a fresh-constructed engine now
// routes through the langparser. UseLangparserRuntime(false) is
// the rollback to the legacy memql parser, kept for the soak
// period before #250 deletes the legacy code entirely. The
// previously-planned prereqs #254 / #255 / #256 / #257 all landed
// before this flip.
func TestEngineUseLangparserRuntimeToggle(t *testing.T) {
	e := &MemQLEngine{}
	// #249 flip: zero-value engine should now report langparser
	// enabled (was OFF pre-flip). The toggle still works in both
	// directions for the soak rollback.
	if !e.LangparserRuntimeEnabled() {
		t.Fatal("post-#249 default should be ON (langparser)")
	}
	e.UseLangparserRuntime(false)
	if e.LangparserRuntimeEnabled() {
		t.Fatal("UseLangparserRuntime(false) did not engage the legacy rollback")
	}
	e.UseLangparserRuntime(true)
	if !e.LangparserRuntimeEnabled() {
		t.Fatal("UseLangparserRuntime(true) did not re-enable")
	}
}

// TestParseViaLangparser_FallsBackOnParseError pins the #249 soak
// contract: ANY langparser parse error returns errLangparserUnsupported
// so engine.Parse falls back to the memql parser for the canonical
// error message + sentinel chain. Without this conversion,
// langparser's bare error text doesn't wrap ErrInvalidArgument /
// ErrInvalidSyntax that callers grep for via errors.Is().
//
// The previous-contract test (`TestParseViaLangparser_DoesNotMaskRealErrors`)
// asserted langparser errors propagated unchanged. That contract
// shipped a divergence where TestParseErrors expected
// memql-sentinel-wrapped errors but got bare langparser errors --
// inverted for the soak; restore when langparser error-wrapping
// reaches parity (separate follow-up under epic #218).
func TestParseViaLangparser_FallsBackOnParseError(t *testing.T) {
	// Genuinely malformed input -- nothing about it triggers the
	// upfront-unsupported detector, so it reaches
	// langparser.ParseExpression which produces a parse error.
	// The wrapper converts it to errLangparserUnsupported so the
	// memql fallback path produces the canonical error.
	_, err := parseViaLangparser(`concept==`)
	if err == nil {
		t.Fatal("expected fallback signal, got nil")
	}
	if !errors.Is(err, errLangparserUnsupported) {
		t.Errorf("parse error should be converted to errLangparserUnsupported, got %v", err)
	}
}
