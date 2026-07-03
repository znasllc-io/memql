package memql

import (
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// negative_load_test.go -- S7 of the fail-loud syntax epic (memql#2383 /
// #2351). This is the LOAD/LINT + SLICER half of the systematic negative-syntax
// conformance suite; the parser/expression half lives in
// component/language/parser/negative_grammar_test.go.
//
// It drives the real lint pipeline (dslimports.Load, the same path
// cmd/memqllint and engine startup take) against in-memory malformed fixtures
// and asserts each one FAILS with a diagnostic that names the offending file --
// never a silent accept. It pins the S1 (#2356) behavior: with the strip step
// now a no-op, every construct kind parses through its native parser, so a
// broken body surfaces as a Load diagnostic instead of vanishing.
//
// dslimports_test.go (S1) already pins spec / trait / shape / builtin; this
// suite extends the matrix to ALL construct kinds for completeness, and adds
// the slicer-level edge cases (unbalanced braces, typo'd keyword, wrong-depth
// nesting, duplicate names) that no test previously exercised.
//
// Requires no database -- pure parse/load -- so it runs as part of the standard
// `go test ./...` CI job.

// loadMemFS runs the full lint pipeline over a single in-memory file and
// returns the aggregate error (nil on clean load).
func loadMemFS(name, body string) error {
	_, err := dslimports.Load(fstest.MapFS{name: {Data: []byte(body)}})
	return err
}

// ---------------------------------------------------------------------------
// 1. Malformed body per construct kind -> a Load diagnostic that names the
//    file. This is the core matrix: deliberately breaking ANY kind's body must
//    fail the lint gate.
// ---------------------------------------------------------------------------

func TestNegativeLoad_MalformedBodyPerKind(t *testing.T) {
	cases := []struct{ kind, file, body string }{
		{"concept", "x/concepts.memql", "@version(\"1.0.0\")\n@namespace(\"v1:x:y\")\nconcept c {\n  name string @@@ !!! broken\n}\n"},
		{"shape", "x/shapes.memql", "@row\nshape s {\n  row.id\n  123 456 789\n}\n"},
		{"spec", "x/specs.memql", "@enabled\nspec activeRowTrait s {\n  return status ==== \"x\" &&&& true\n}\n"},
		{"trait", "x/traits.memql", "@enabled\ntrait t {\n  return active ==== true\n}\n"},
		{"mutation", "x/mutations.memql", "use cognition.concepts.{ space }\nmutate space m {\n  ?? !! garbage\n}\n"},
		{"query", "x/queries.memql", "use cognition.concepts.{ space }\nquery space q {\n  filter @@@ !!! broken\n  shape spaceFull\n}\n"},
		{"logic", "x/logic.memql", "logic l {\n  args { event object @required }\n  return 1\n}\n"}, // missing body{}
		{"automation", "x/automations.memql", "@trigger(event=)\nautomation a {\n  step run { logic doThing { event: event } }\n}\n"},
		{"policy", "x/policies.memql", "@primary(\"x\")\npolicy p {\n"}, // unterminated brace
		{"provider", "x/providers.memql", "@extends(\"openai\")\n@model(\"m\")\nprovider pr {\n  params {\n    contextWindow @@@ broken\n  }\n}\n"},
		{"prompt", "x/prompts.memql", "@templateFile(\"x.tmpl\")\nprompt pr {\n  field @@@ !!! broken\n}\n"},
		{"builtin", "x/builtins.memql", "@executor(\"integration.x.y\")\nbuiltin b {\n  arg string @@@ !!! broken\n}\n"},
		{"tool", "x/tools.memql", "@handler(type=\"function\", name=\"fn\")\ntool tl {\n  arg string @@@ !!! broken\n}\n"},
		{"seed", "x/seeds.memql", "seed agent sd {\n  name: @@@ broken !!!\n}\n"},
		{"action", "x/actions.memql", "action doThing {\n  garbage !!! not-a-capability-call\n}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			err := loadMemFS(tc.file, tc.body)
			if err == nil {
				t.Fatalf("%s: Load returned no diagnostic for a malformed %s body (silently accepted)", tc.kind, tc.kind)
			}
			// The diagnostic must name the offending file (message-quality:
			// the operator/CI can locate the broken construct).
			if !strings.Contains(err.Error(), tc.file) {
				t.Errorf("%s: Load diagnostic did not name the file %q:\n  %v", tc.kind, tc.file, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. Slicer-level edge cases.
// ---------------------------------------------------------------------------

// 2a. Unbalanced braces. At the SLICER level (ExtractKeywordSlices) an
// unbalanced construct is silently dropped (returns no slice) -- the historical
// "construct silently invisible" behavior. The lint gate (dslimports.Load) is
// the loud safety net: the whole-file parse fails, so the file never loads
// clean. This test pins BOTH facts so the safety net can't quietly regress.
func TestNegativeLoad_UnbalancedBraces(t *testing.T) {
	const unbalanced = "@row\nshape s {\n  row.id\n" // no closing brace

	slices := ExtractKeywordSlices(unbalanced, "shape")
	if len(slices) != 0 {
		t.Errorf("slicer: expected 0 slices for an unbalanced-brace shape (silent drop), got %d", len(slices))
	}

	if err := loadMemFS("x/shapes.memql", unbalanced); err == nil {
		t.Fatal("Load returned no diagnostic for an unbalanced-brace shape -- the loud safety net regressed")
	}
}

// 2b. A typo'd top-level construct keyword surfaces a Load diagnostic carrying
// the S3 did-you-mean hint.
func TestNegativeLoad_TypoTopLevelKeyword(t *testing.T) {
	err := loadMemFS("x/concepts.memql", "@enabled\nconept foo { }\n")
	if err == nil {
		t.Fatal("Load accepted a typo'd top-level keyword `conept`")
	}
	if !strings.Contains(err.Error(), "did you mean 'concept'") {
		t.Errorf("expected a did-you-mean hint for `conept`; got: %v", err)
	}
}

// 2c. A construct nested at the wrong depth (a spec inside a shape body) is not
// a valid shape body line and must surface a diagnostic rather than be
// swallowed.
func TestNegativeLoad_ConstructNestedAtWrongDepth(t *testing.T) {
	const nested = "@row\nshape s {\n  row.id\n  spec activeRowTrait inner { return active == true }\n}\n"
	if err := loadMemFS("x/shapes.memql", nested); err == nil {
		t.Fatal("Load accepted a spec nested inside a shape body")
	}
}

// 2d. Duplicate construct names in one file. dslimports.Load does not register
// into runtime registries, so it does not catch a duplicate; the loud gate is
// the runtime loader, which logs a WARN (S1 raised register failures Debug->
// Warn) and registers only the first. This drives the exact production loader
// closure and asserts (a) only one construct registers and (b) the drop is
// surfaced as a WARN the load-clean gate (skipLike) catches.
func TestNegativeLoad_DuplicateNamesWarnSkip(t *testing.T) {
	const dup = "@enabled\nspec activeRowTrait sA {\n  return active == true\n}\n" +
		"@enabled\nspec activeRowTrait sA {\n  return active == false\n}\n"
	capture := newCaptureHandler()
	logger := slog.New(capture)
	files := []baseloader.RawFile{{Path: "x/specs.memql", Content: dup}}
	parse := func(origin string, raw []byte) (*Spec, error) {
		decl, err := languageParser.ParseSpecDecl(string(raw))
		if err != nil {
			return nil, err
		}
		return specDeclToSpec(decl, origin)
	}
	reg := newSpecRegistry()
	n, err := baseloader.LoadOne[Spec](logger, "memql.unifiedSpecLoader", "spec",
		files, extractAdapter, parse, reg.add)
	if err != nil {
		t.Fatalf("LoadOne returned a pipeline error: %v", err)
	}
	if n != 1 {
		t.Errorf("duplicate spec names should register exactly 1 construct, got %d", n)
	}
	// The dropped duplicate is surfaced loudly (WARN) and caught by skipLike.
	assertWarnCaptured(t, capture, "register failed", "sA")
}

// ===========================================================================
// KNOWN SILENT-ACCEPTANCE HOLES (surfaced by this suite, NOT fixed in S7).
// Reported to the coordinator on memql#2383 for triage into a fail-loud
// sub-story of epic #2351.
// ===========================================================================

// HOLE 3: two `insert { ... }` blocks in one mutation load CLEAN through
// dslimports.Load. docs/public/language/authoring-rules.md Rule #1 states this
// is "a parse-time error", but the lint pipeline (NormaliseAll + ParseFileSource)
// does not catch it -- the second write is only rejected deeper, at engine
// mutation-template registration, which the lint gate never reaches.
func TestHOLE_TwoWritesPerMutationNotCaughtAtLint(t *testing.T) {
	t.Skip("HOLE (memql#2383 report): two insert{} blocks in one mutation load clean through dslimports.Load; the one-write rule (authoring-rules #1) is only enforced at engine mutation-template registration, not the lint gate. Move the enforcement earlier (rewriter/parse) so `memqllint` catches it, then un-skip.")

	// Documented behavior today (would pass if asserted): loads clean.
	//   const twoWrite = "use cognition.concepts.{ space }\nmutate space m {\n  args { x string @required }\n  insert { name: args.x }\n  insert { name: args.x }\n}\n"
	//   require err == nil  // <-- the hole
}

// HOLE 4: a policy with a non-empty body (`policy p { garbage tokens }`) loads
// CLEAN. Live policies are empty-bodied provider-selection records; the policy
// parser ignores body content instead of rejecting a non-empty body.
func TestHOLE_PolicyGarbageBodyAccepted(t *testing.T) {
	t.Skip("HOLE (memql#2383 report): a policy with a non-empty body (`policy p { garbage tokens here }`) loads clean -- the policy parser ignores body content. Reject a non-empty policy body (policies are empty-bodied provider-selection records), then un-skip.")
}
