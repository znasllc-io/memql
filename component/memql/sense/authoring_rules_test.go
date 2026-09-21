package sense

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/deprecation"
	"github.com/znasllc-io/memql/component/language/parser"

	"github.com/znasllc-io/memql/core/repowalk"
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

// A use of a deprecated form still inside its window is a Warning
// (memql#5390): the form's rule is the code, its warning the message -- word
// for word what a load says -- and the range is the spelling as written.
func TestDeprecatedFormsRule_WarnsOnTheArrayType(t *testing.T) {
	src := "/// A ticket.\nconcept ticket {\n  /// Names.\n  names  array(string)\n}\n"
	diags := deprecatedFormsRule(src)
	if len(diags) != 1 {
		t.Fatalf("want one warning for array(string), got %+v", diags)
	}
	form, _ := deprecation.Lookup(deprecation.ArrayType)
	d := diags[0]
	if d.Code != "deprecated_array_type" || d.Severity != SeverityWarning || d.Message != form.Warning() {
		t.Errorf("got code %q severity %v message %q; want %q, SeverityWarning, %q", d.Code, d.Severity, d.Message, "deprecated_array_type", form.Warning())
	}
	if want := (Range{Start: Position{Line: 4, Column: 10}, End: Position{Line: 4, Column: 23}}); d.Range != want {
		t.Errorf("range = %+v, want %+v (the spelling as written)", d.Range, want)
	}
}

func TestDeprecatedFormsRule_IgnoresStringLiteralContainingArray(t *testing.T) {
	src := "/// The array(T) syntax is deprecated.\n@description(\"the array(T) syntax is deprecated\")\nconcept ticket {\n  // tags array(string)\n  note string\n}\n"
	if diags := deprecatedFormsRule(src); len(diags) != 0 {
		t.Fatalf("expected no warning (only a string and comments mention array()); got %v", diags)
	}
}

func TestDeprecatedFormsRule_MigratedSliceDoesNotFire(t *testing.T) {
	src := "/// A ticket.\nconcept ticket {\n  /// Names.\n  names  []string\n}\n"
	if diags := deprecatedFormsRule(src); len(diags) != 0 {
		t.Fatalf("migrated source should not warn; got %v", diags)
	}
}

// Past its window the form is the parser's to refuse, as a retired form is:
// the rule says nothing, and Diagnose reports the refusal as an Error carrying
// the rule and naming the replacement. Nothing about the table changes between
// this test and the one above -- only the release deciding.
func TestDeprecatedFormsRule_ExpiredFormIsTheParsersRefusal(t *testing.T) {
	restore := deprecation.SetCurrent("0.25.0")
	defer restore()
	form, _ := deprecation.Lookup(deprecation.ArrayType)

	src := "/// A ticket.\nconcept ticket {\n  /// Names.\n  names  array(string)\n}\n"
	if diags := deprecatedFormsRule(src); len(diags) != 0 {
		t.Fatalf("a form past its window is not a warning; got %+v", diags)
	}
	var refused []Diagnostic
	for _, d := range New(nil).Diagnose(src, "") {
		if d.Code == deprecation.ArrayType {
			refused = append(refused, d)
		}
	}
	if len(refused) != 1 || refused[0].Severity != SeverityError || !strings.Contains(refused[0].Message, form.Refusal()) {
		t.Fatalf("want one Error carrying the refusal, got %+v", refused)
	}
	if want := (Range{Start: Position{Line: 4, Column: 10}, End: Position{Line: 4, Column: 23}}); refused[0].Range != want {
		t.Errorf("range = %+v, want %+v", refused[0].Range, want)
	}
}

// The warning needs no vocabulary: a registry-less service -- a workspace whose
// build failed -- still says where the file spells a deprecated form.
func TestDiagnose_DeprecatedFormWarnsWithoutAVocabulary(t *testing.T) {
	src := "/// A ticket.\nconcept ticket {\n  /// Names.\n  names  array(string)\n}\n"
	var got []Diagnostic
	for _, d := range New(nil).Diagnose(src, "") {
		if d.Code == deprecation.ArrayType {
			got = append(got, d)
		}
	}
	if len(got) != 1 || got[0].Severity != SeverityWarning {
		t.Fatalf("want one deprecated_array_type Warning from a registry-less Diagnose, got %+v", got)
	}
}

// Construct-attached @enabled is an ERROR since epic memql#5375 retired the
// annotation (it was a #2610 soft-deprecation hint while it still loaded); a
// stripped construct and prose mentions still do not fire.
func TestRedundantEnabledRule(t *testing.T) {
	withAnnotation := "@enabled\n@description(\"probe\")\nquery Space probeQuery {\n  filter { payload.active == true }\n}\n"
	diags := redundantEnabledRule(withAnnotation)
	if len(diags) != 1 {
		t.Fatalf("want 1 diagnostic on a construct-attached @enabled, got %d", len(diags))
	}
	if diags[0].Code != "retired-enabled" || diags[0].Severity != SeverityError {
		t.Errorf("want retired-enabled Error, got %s severity %d", diags[0].Code, diags[0].Severity)
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
			t.Errorf("%s: want 1 diagnostic (gate parity), got %d", name, len(got))
		}
	}

	clean := "@description(\"probe mentions @enabled in prose\")\nquery Space probeQuery {\n  filter { payload.active == true }\n}\n// historical note: this construct once carried "
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
		if info.IsDir() && repowalk.SkipDir(info.Name()) {
			return filepath.SkipDir
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

// memql#3336: the args-field @description Hint is gone -- the parser rejects
// the annotation, so the editor gets the LOAD ERROR itself (Diagnose Phase 2)
// instead of a hint claiming it was "parsed and discarded". This pins that the
// replacement path actually reaches the editor, with the actionable message.
func TestArgsFieldDescriptionSurfacesAsLoadError(t *testing.T) {
	src := `use cognition.concepts.{ participant }
use cognition.shapes.{ participantFull }
@description("declaration level -- load-bearing, must not flag")
query participant spaceParticipants {
  args {
    spaceId string @required @description("dead")
  }
  filter row => row.spaceId == args.spaceId
  shape  participantFull
}
`
	svc := New(nil)
	diags := svc.Diagnose(src, "queries.memql")
	var got *Diagnostic
	for i := range diags {
		if diags[i].Severity == SeverityError && strings.Contains(diags[i].Message, "@description") {
			got = &diags[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("want an Error naming @description, got %+v", diags)
	}
	if got.Range.Start.Line != 6 {
		t.Errorf("anchor line = %d, want 6 (the offending args field)", got.Range.Start.Line)
	}
	if !strings.Contains(got.Message, "///") {
		t.Errorf("message must point at the /// doc comment; got %q", got.Message)
	}

	// The declaration-level @description on the same construct is
	// load-bearing: with the args-field annotation removed the file is clean.
	clean := strings.Replace(src, ` @description("dead")`, "", 1)
	for _, d := range svc.Diagnose(clean, "queries.memql") {
		if d.Severity == SeverityError {
			t.Errorf("declaration-level @description must not flag; got %+v", d)
		}
	}
}

// TestActorUndeclaredRule (#2622): the edit-time mirror of the engine's
// actor-binding load rule, positions computed from authored source.
func TestActorUndeclaredRule(t *testing.T) {
	src := "@description(\"owned\")\nquery todo todos {\n  filter row => row.ownerUserId == actor.userId\n}\n"
	got := actorUndeclaredRule(src)
	if len(got) != 1 || got[0].Code != "actor-undeclared" || got[0].Severity != SeverityError {
		t.Fatalf("want one actor-undeclared Error, got %+v", got)
	}
	// Column 36 is the `actor` of `actor.userId`, after the lambda header.
	if got[0].Range.Start.Line != 3 || got[0].Range.Start.Column != 36 {
		t.Errorf("anchor = %d:%d, want 3:36", got[0].Range.Start.Line, got[0].Range.Start.Column)
	}

	// The SAME actor.userId text repeats: the second occurrence must
	// anchor on ITS line, not the first (the findInSource trap).
	repeated := "@actor\nquery todo mine {\n  filter row => row.ownerUserId == actor.userId\n}\n\nquery todo theirs {\n  filter row => row.ownerUserId == actor.userId\n}\n"
	got = actorUndeclaredRule(repeated)
	if len(got) != 1 {
		t.Fatalf("only the undeclared construct flags, got %+v", got)
	}
	if got[0].Range.Start.Line != 7 {
		t.Errorf("second occurrence must anchor on line 7, got line %d", got[0].Range.Start.Line)
	}

	for name, clean := range map[string]string{
		"declared":          "@actor\nquery todo todos {\n  filter row => row.ownerUserId == actor.userId\n}\n",
		"declared-unused":   "@actor\nquery todo all {\n  filter row => row.done == false\n}\n",
		"no-read":           "query todo all {\n  filter row => row.done == false\n}\n",
		"spec-body":         "spec actorEnvelope isOwner = actor => actor.role == \"owner\"\n",
		"shape-kind-marker": "@actor\nshape actorEnvelope {\n  actor.userId\n  actor.role\n}\n",
		"prose-only":        "// gated by actor.rank\nquery todo all {\n  filter row => row.done == false\n}\n",
		"event-envelope":    "@trigger(event=\"x.y\")\nautomation onThing {\n  run := logic handle(event: event)\n}\n",
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
	got := s.Diagnose("query todo todos {\n  filter row => row.ownerUserId == actor.userId\n}\n", "probe.memql")
	for _, d := range got {
		if d.Code == "actor-undeclared" {
			t.Errorf("nil-registry path must not emit the semantic rule, got %+v", d)
		}
	}
}
