package sense

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

func parseForTest(t *testing.T, src string) *parser.File {
	t.Helper()
	lex := parser.NewLexer(src)
	tokens, err := lex.Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	p := parser.NewParser(tokens)
	node, err := p.Parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	file, ok := node.(*parser.File)
	if !ok {
		t.Fatalf("parse returned %T, want *parser.File", node)
	}
	return file
}

func TestDirectivesInBodyRule_FlagsSortCall(t *testing.T) {
	src := `
@enabled
@description("bad")
func (Query) bad(args any) (any, error) {
  return sort(concept==v1:platform:partition, "payload.name", "asc"), nil
}
`
	file := parseForTest(t, src)
	diags := directivesInBodyRule(file, src)
	if len(diags) == 0 {
		t.Fatalf("expected directive-in-body diagnostic, got none")
	}
	if diags[0].Code != "directive-in-body" {
		t.Errorf("code = %q, want directive-in-body", diags[0].Code)
	}
	if !strings.Contains(diags[0].Message, "sort") {
		t.Errorf("message missing 'sort': %q", diags[0].Message)
	}
}

func TestDirectivesInBodyRule_IgnoresAllowedCall(t *testing.T) {
	src := `
@enabled
@description("ok")
func (Query) queryListPartitions(args any) (any, error) {
  return concept==v1:platform:partition, nil
}
`
	file := parseForTest(t, src)
	diags := directivesInBodyRule(file, src)
	if len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %v", diags)
	}
}

func TestNameShapeRule_FlagsLongName(t *testing.T) {
	longName := strings.Repeat("a", 55)
	src := `
@enabled
@description("too long")
func (Query) ` + longName + `(args any) (any, error) {
  return concept==v1:foo:bar, nil
}
`
	file := parseForTest(t, src)
	diags := nameShapeRule(file, src)
	if len(diags) == 0 {
		t.Fatalf("expected name-too-long diagnostic, got none")
	}
	if diags[0].Code != "name-too-long" {
		t.Errorf("code = %q, want name-too-long", diags[0].Code)
	}
}

func TestNameShapeRule_IgnoresShortCamelCase(t *testing.T) {
	src := `
@enabled
@description("fine")
func (Query) queryListPartitions(args any) (any, error) {
  return concept==v1:platform:partition, nil
}
`
	file := parseForTest(t, src)
	diags := nameShapeRule(file, src)
	if len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %v", diags)
	}
}

func TestArraySyntaxRule_FlagsArrayCall(t *testing.T) {
	src := `
@description("deprecated")
@input {
  names  array(string)
}
func (Prompt) foo(args any) {}
`
	diags := arraySyntaxRule(src)
	if len(diags) == 0 {
		t.Fatalf("expected deprecation hint for array(string), got none")
	}
	if diags[0].Code != "deprecated-array-syntax" {
		t.Errorf("code = %q, want deprecated-array-syntax", diags[0].Code)
	}
	if diags[0].Severity != SeverityHint {
		t.Errorf("severity = %v, want SeverityHint", diags[0].Severity)
	}
}

func TestArraySyntaxRule_IgnoresStringLiteralContaining_array(t *testing.T) {
	src := `
@description("the array(T) syntax is deprecated")
func (Query) q(args any) (any, error) {
  return concept==v1:foo:bar, nil
}
`
	diags := arraySyntaxRule(src)
	if len(diags) != 0 {
		t.Fatalf("expected no hints (only string literal mentions array()); got %v", diags)
	}
}

func TestArraySyntaxRule_FlagsMigratedSliceShouldNotFire(t *testing.T) {
	src := `
@description("migrated")
@input {
  names  []string
}
func (Prompt) foo(args any) {}
`
	diags := arraySyntaxRule(src)
	if len(diags) != 0 {
		t.Fatalf("migrated source should not fire hint; got %v", diags)
	}
}

// #2610: construct-attached @enabled gets the soft-deprecation hint; a
// stripped construct and prose mentions do not.
func TestRedundantEnabledRule(t *testing.T) {
	withAnnotation := "@enabled\n@description(\"probe\")\nquery Space probeQuery {\n  filter { payload.active == true }\n}\n"
	diags := redundantEnabledRule(withAnnotation)
	if len(diags) != 1 {
		t.Fatalf("want 1 hint on a construct-attached @enabled, got %d", len(diags))
	}
	if diags[0].Code != "redundant-enabled" || diags[0].Severity != SeverityHint {
		t.Errorf("want redundant-enabled Hint, got %s severity %d", diags[0].Code, diags[0].Severity)
	}
	if diags[0].Range.Start.Line != 1 {
		t.Errorf("hint anchored at line %d, want 1", diags[0].Range.Start.Line)
	}
	indented := "\t@enabled\nquery Space probeQuery {\n  filter { payload.active == true }\n}\n"
	ind := redundantEnabledRule(indented)
	if len(ind) != 1 || ind[0].Range.Start.Column != 2 {
		t.Fatalf("indented @enabled must anchor on the token (col 2), got %+v", ind)
	}
	for name, src := range map[string]string{
		"crlf":     "@enabled\r\nquery Space probeQuery {\n}\n",
		"arg-form": "@enabled(true)\nquery Space probeQuery {\n}\n",
		"trailing": "@enabled // temp\nquery Space probeQuery {\n}\n",
	} {
		if got := redundantEnabledRule(src); len(got) != 1 {
			t.Errorf("%s: want 1 hint (gate parity), got %d", name, len(got))
		}
	}

	clean := "@description(\"probe mentions @enabled in prose\")\nquery Space probeQuery {\n  filter { payload.active == true }\n}\n// historical note: this construct once carried @enabled\n"
	if got := redundantEnabledRule(clean); len(got) != 0 {
		t.Fatalf("prose/comment mentions must not hint, got %d", len(got))
	}
}

// #2610 DoD: the stripped embedded tree carries zero redundant-enabled
// hints (the sweep the PR body claims; previously unpinned per review).
func TestRedundantEnabledRule_EmbeddedTreeClean(t *testing.T) {
	root := dslRoot(t)
	var hints []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".memql") {
			return nil
		}
		// _reference/ is deliberately IN scope: its sheets were stripped in
		// this same story (prose calling @enabled a no-op must not model
		// it), and this filesystem walk is the only test that reaches them
		// (the embedded-tree gates cannot -- the embed directive omits
		// underscore paths).
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, _ := filepath.Rel(root, path)
		for _, d := range redundantEnabledRule(string(data)) {
			hints = append(hints, rel+": "+d.Message)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(hints) > 0 {
		t.Errorf("stripped tree yields %d redundant-enabled hint(s):\n  %s", len(hints), strings.Join(hints, "\n  "))
	}
}

// #2613: the default-version literal hints; explicit non-defaults and prose
// mentions do not.
func TestRedundantVersionRule(t *testing.T) {
	if got := redundantVersionRule("@version(\"1.0.0\")\nconcept probe {\n}\n"); len(got) != 1 || got[0].Code != "redundant-version" {
		t.Fatalf("default literal must hint once, got %+v", got)
	}
	// Every shape the dsl/ gate fails must hint (#2657 review parity):
	// trailing comment, inner spacing, CRLF endings.
	for name, src := range map[string]string{
		"trailing-comment": "@version(\"1.0.0\") // keep\nconcept probe {\n}\n",
		"inner-spaces":     "@version( \"1.0.0\" )\nconcept probe {\n}\n",
		"crlf":             "@version(\"1.0.0\")\r\nconcept probe {\r\n}\r\n",
	} {
		if got := redundantVersionRule(src); len(got) != 1 {
			t.Errorf("%s: want one hint, got %+v", name, got)
		}
	}
	for name, src := range map[string]string{
		"non-default": "@version(\"2.5.7\")\nconcept probe {\n}\n",
		"prose":       "// historical: @version(\"1.0.0\") was everywhere\nconcept probe {\n}\n",
	} {
		if got := redundantVersionRule(src); len(got) != 0 {
			t.Errorf("%s: want no hint, got %+v", name, got)
		}
	}
}

func TestDiscardedArgsDescriptionRule(t *testing.T) {
	src := "@description(\"declaration level\")\nquery q {\n  args {\n    x string @required @description(\"dead\")\n  }\n}\nconcept widget {\n  label string @description(\"load-bearing\")\n}\n"
	got := discardedArgsDescriptionRule(src)
	if len(got) != 1 || got[0].Code != "discarded-args-description" || got[0].Range.Start.Line != 4 {
		t.Fatalf("want one hint on line 4, got %+v", got)
	}

	// #2658 review: the opener line is inside the block.
	opener := "query q {\n  args { x string @description(\"dead\") }\n}\n"
	if got := discardedArgsDescriptionRule(opener); len(got) != 1 || got[0].Range.Start.Line != 2 {
		t.Errorf("opener-line annotation: want one hint on line 2, got %+v", got)
	}

	for name, clean := range map[string]string{
		"declaration-level":     "@description(\"keep\")\nquery q {\n  args {\n    x string\n  }\n}\n",
		"concept-field":         "concept widget {\n  label string @description(\"keep\")\n}\n",
		"shape-block":           "mutation m {\n  args {\n    x string\n  }\n  shape {\n    y string @description(\"keep\")\n  }\n}\n",
		"shape-same-line":       "mutation m {\n  args {\n    x string\n  } shape {\n    y string @description(\"keep\")\n  }\n}\n",
		"args-in-comment":       "// args { commentary\nconcept w {\n  label string @description(\"keep\")\n}\n",
		"args-in-string":        "query q {\n  filter { note == \"args {\" }\n}\n@description(\"keep\")\n",
		"args-in-block-comment": "/*\nargs {\n  x string @description(\"commented out\")\n}\n*/\nconcept w {\n  label string\n}\n",
		"longer-annotation":     "query q {\n  args {\n    x string @descriptionX(\"different\")\n  }\n}\n",
	} {
		if got := discardedArgsDescriptionRule(clean); len(got) != 0 {
			t.Errorf("%s: want no hint, got %+v", name, got)
		}
	}
}

// TestActorUndeclaredRule (#2622): the edit-time mirror of the engine's
// actor-binding load rule, positions computed from authored source.
func TestActorUndeclaredRule(t *testing.T) {
	src := "@description(\"owned\")\nquery todo todos {\n  filter todo.ownerUserId == actor.userId\n}\n"
	got := actorUndeclaredRule(src)
	if len(got) != 1 || got[0].Code != "actor-undeclared" || got[0].Severity != SeverityError {
		t.Fatalf("want one actor-undeclared Error, got %+v", got)
	}
	if got[0].Range.Start.Line != 3 || got[0].Range.Start.Column != 30 {
		t.Errorf("anchor = %d:%d, want 3:30", got[0].Range.Start.Line, got[0].Range.Start.Column)
	}

	// The SAME actor.userId text repeats: the second occurrence must
	// anchor on ITS line, not the first (the findInSource trap).
	repeated := "@actor\nquery todo mine {\n  filter todo.ownerUserId == actor.userId\n}\n\nquery todo theirs {\n  filter todo.ownerUserId == actor.userId\n}\n"
	got = actorUndeclaredRule(repeated)
	if len(got) != 1 {
		t.Fatalf("only the undeclared construct flags, got %+v", got)
	}
	if got[0].Range.Start.Line != 7 {
		t.Errorf("second occurrence must anchor on line 7, got line %d", got[0].Range.Start.Line)
	}

	for name, clean := range map[string]string{
		"declared":          "@actor\nquery todo todos {\n  filter todo.ownerUserId == actor.userId\n}\n",
		"declared-unused":   "@actor\nquery todo all {\n  filter todo.done == false\n}\n",
		"no-read":           "query todo all {\n  filter todo.done == false\n}\n",
		"spec-body":         "spec isOwned {\n  when { ownerUserId == actor.userId }\n}\n",
		"shape-kind-marker": "@actor\nshape actorEnvelope {\n  actor.userId\n  actor.role\n}\n",
		"prose-only":        "// gated by actor.rank\nquery todo all {\n  filter todo.done == false\n}\n",
		"event-envelope":    "@trigger(event=\"x.y\")\nautomation onThing {\n  step run {\n    logic handle ( event: event )\n  }\n}\n",
	} {
		if got := actorUndeclaredRule(clean); len(got) != 0 {
			t.Errorf("%s: want no diagnostics, got %+v", name, got)
		}
	}
}

// TestActorUndeclaredRule_NilRegistry (#2622 trap 3): the LSP falls
// back to New(nil) when strict boot trips; Diagnose must not panic and
// the registry-gated semantic rules (this one included) emit nothing.
func TestActorUndeclaredRule_NilRegistry(t *testing.T) {
	s := New(nil)
	got := s.Diagnose("query todo todos {\n  filter todo.ownerUserId == actor.userId\n}\n", "probe.memql")
	for _, d := range got {
		if d.Code == "actor-undeclared" {
			t.Errorf("nil-registry path must not emit the semantic rule, got %+v", d)
		}
	}
}
