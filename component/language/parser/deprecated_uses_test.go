package parser

// deprecated_uses_test.go -- finding the uses of a deprecated form, and the
// parser's refusal of one whose window is spent (memql#5390).
//
// The refusal is tested by moving the RELEASE, not by flipping a flag: the
// decision is deprecation.Form.RefusesAt(release), so a test names 0.25.0 and
// sees exactly what a 0.25.0 cluster will see, today, on the shipped table.

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/deprecation"
)

func TestScanDeprecatedUsesFindsTheArrayType(t *testing.T) {
	got := ScanDeprecatedUses("concept ticket {\n  tags array(string)\n  note string\n}")
	want := []DeprecatedUse{{Rule: "deprecated_array_type", Line: 2, Column: 8, Text: "array(string)"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanDeprecatedUses =\n  %+v\nwant\n  %+v", got, want)
	}
}

// Strings and comments are not source: the lexer never hands their text back as
// tokens, so a scan over tokens ignores them by construction.
func TestScanDeprecatedUsesIgnoresStringsAndComments(t *testing.T) {
	for name, src := range map[string]string{
		"string literal": "@description(\"array(x)\")\nconcept ticket {\n  note string\n}",
		"line comment":   "concept ticket {\n  // array(x)\n  note string\n}",
		"doc comment":    "concept ticket {\n  /// Was array(x) once.\n  note string\n}",
		"block comment":  "concept ticket {\n  /* tags array(x) */\n  note string\n}",
		"the new form":   "concept ticket {\n  tags []string\n}",
		"bare array":     "concept ticket {\n  tags array\n}",
	} {
		if got := ScanDeprecatedUses(src); len(got) != 0 {
			t.Errorf("%s: want no use, got %+v", name, got)
		}
	}
}

// Every position the parser reads a type in is a position the form can be
// written in: a field, an element of `[]` or of a map, and the element of
// another `array(...)`. Each use is its own, with its text as written.
func TestScanDeprecatedUsesFindsEveryTypePosition(t *testing.T) {
	src := "concept ticket {\n" +
		"  a array( string )\n" +
		"  b []array(int)\n" +
		"  c map[string]array(v1:shop:product)\n" +
		"  d array(array(string))\n" +
		"}\n" +
		"builtin probe {\n" +
		"  e array(object)\n" +
		"}\n"
	want := []DeprecatedUse{
		{Rule: deprecation.ArrayType, Line: 2, Column: 5, Text: "array( string )"},
		{Rule: deprecation.ArrayType, Line: 3, Column: 7, Text: "array(int)"},
		{Rule: deprecation.ArrayType, Line: 4, Column: 16, Text: "array(v1:shop:product)"},
		{Rule: deprecation.ArrayType, Line: 5, Column: 5, Text: "array(array(string))"},
		{Rule: deprecation.ArrayType, Line: 5, Column: 11, Text: "array(string)"},
		{Rule: deprecation.ArrayType, Line: 8, Column: 5, Text: "array(object)"},
	}
	if got := ScanDeprecatedUses(src); !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanDeprecatedUses =\n  %+v\nwant\n  %+v", got, want)
	}
}

// `array` is not a function anywhere in the language, so a call spelled with it
// in an expression is not the type form, and neither is an unclosed one.
func TestScanDeprecatedUsesIgnoresWhatIsNotATypePosition(t *testing.T) {
	for name, src := range map[string]string{
		"a call argument": "logic probe {\n  x := list(array(1))\n  return x\n}",
		"an assignment":   "logic probe {\n  x := array(1)\n  return x\n}",
		"unclosed":        "concept ticket {\n  tags array(string\n",
	} {
		if got := ScanDeprecatedUses(src); len(got) != 0 {
			t.Errorf("%s: want no use, got %+v", name, got)
		}
	}
}

// A Sense column counts runes (memql#2788), and so does a use's.
func TestScanDeprecatedUsesCountsColumnsInRunes(t *testing.T) {
	got := ScanDeprecatedUses("concept ticket {\n  naïve array(string)\n}")
	if len(got) != 1 || got[0].Line != 2 || got[0].Column != 9 {
		t.Fatalf("want one use at 2:9 (rune column; a byte column would be 10), got %+v", got)
	}
	if line, col := got[0].End(); line != 2 || col != 22 {
		t.Errorf("End() = %d:%d, want 2:22", line, col)
	}
}

func TestADeprecatedUseEndsAfterItsText(t *testing.T) {
	u := DeprecatedUse{Rule: deprecation.ArrayType, Line: 3, Column: 5, Text: "array(\n    string\n  )"}
	if line, col := u.End(); line != 5 || col != 4 {
		t.Fatalf("End() = %d:%d, want 5:4", line, col)
	}
}

// A use's replacement is the form's replacement with the use's own parts in it:
// what the editor's quick fix writes over the spelling.
func TestADeprecatedUseNamesItsReplacementAsWritten(t *testing.T) {
	for text, want := range map[string]string{
		"array(string)":            "[]string",
		"array( v1:shop:product )": "[]v1:shop:product",
		"array(array(int))":        "[]array(int)",
		"array(map[string]int)":    "[]map[string]int",
	} {
		got, ok := DeprecatedUse{Rule: deprecation.ArrayType, Line: 1, Column: 1, Text: text}.Replacement()
		if !ok || got != want {
			t.Errorf("Replacement of %q = %q, %v; want %q", text, got, ok, want)
		}
	}
	for _, u := range []DeprecatedUse{
		{Rule: deprecation.ArrayType, Text: "array()"},
		{Rule: deprecation.ArrayType, Text: "list(string)"},
		{Rule: "deprecated_no_such_form", Text: "array(string)"},
	} {
		if got, ok := u.Replacement(); ok {
			t.Errorf("Replacement of %+v = %q; want none", u, got)
		}
	}
}

// A source the lexer refuses has no uses: nothing in it loads, and its lexer
// error is what refuses it.
func TestScanDeprecatedUsesOfAnUnlexableSourceIsEmpty(t *testing.T) {
	if got := ScanDeprecatedUses("concept ticket {\n  tags array(string)\n  note \"unterminated\n}"); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

// Every rule the scanner reports is a registered form, or a use would name a
// form nothing can look up.
func TestEveryRuleTheScannerReportsIsRegistered(t *testing.T) {
	for _, u := range ScanDeprecatedUses("concept ticket {\n  tags array(string)\n}") {
		if _, ok := deprecation.Lookup(u.Rule); !ok {
			t.Errorf("the scanner reports rule %q, which no form registers", u.Rule)
		}
	}
}

const deprecatedArrayConcept = "concept ticket {\n  tags array(string)\n}"

// Inside its window the old spelling parses exactly as `[]T` does -- the whole
// promise of a window is that the form keeps working while it warns.
func TestADeprecatedArrayTypeParsesInsideItsWindow(t *testing.T) {
	restore := deprecation.SetCurrent("0.23.0")
	defer restore()

	file, err := ParseFile(deprecatedArrayConcept)
	if err != nil {
		t.Fatalf("a form inside its window must parse: %v", err)
	}
	slice, err := ParseFile("concept ticket {\n  tags []string\n}")
	if err != nil {
		t.Fatal(err)
	}
	old := file.Definitions[0].(*ConceptDecl).Properties[0].Type
	now := slice.Definitions[0].(*ConceptDecl).Properties[0].Type
	if !reflect.DeepEqual(old, now) {
		t.Fatalf("array(string) parsed as %+v, []string as %+v", old, now)
	}
}

// Once the window is spent the spelling is refused the way a retired form is: a
// *RetiredFormError carrying the form's rule, whose text is the form's refusal,
// positioned on the spelling. Nothing about the table changed between this test
// and the one above -- only the release deciding.
func TestAnExpiredArrayTypeIsRefusedNamingItsReplacement(t *testing.T) {
	restore := deprecation.SetCurrent("0.25.0")
	defer restore()
	f, ok := deprecation.Lookup(deprecation.ArrayType)
	if !ok {
		t.Fatalf("the %s form is not registered", deprecation.ArrayType)
	}

	_, err := ParseFile(deprecatedArrayConcept)
	var refused *RetiredFormError
	if !errors.As(err, &refused) {
		t.Fatalf("want a *RetiredFormError, got %v", err)
	}
	if refused.RuleCode() != deprecation.ArrayType {
		t.Errorf("RuleCode() = %q, want %q", refused.RuleCode(), deprecation.ArrayType)
	}
	if !strings.Contains(err.Error(), f.Refusal()) {
		t.Errorf("the refusal %q does not carry the form's text %q", err.Error(), f.Refusal())
	}
	for _, want := range []string{f.Replacement, f.Migrator} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%s", want, err.Error())
		}
	}
	if line, col := refused.Parse.Position(); line != 2 || col != 8 {
		t.Errorf("the refusal is at %d:%d, want 2:8", line, col)
	}
	if line, col := refused.Parse.EndPosition(); line != 2 || col != 21 {
		t.Errorf("the refusal ends at %d:%d, want 2:21 (after the closing paren)", line, col)
	}

	// The refusal a load reports for a use is the same refusal, word for word.
	uses := ScanDeprecatedUses(deprecatedArrayConcept)
	if len(uses) != 1 {
		t.Fatalf("uses = %+v", uses)
	}
	if got := DeprecatedUseRefusal(uses[0], f); got.Error() != refused.Error() || got.RuleCode() != refused.RuleCode() {
		t.Errorf("DeprecatedUseRefusal = %q (%s), the parser's = %q (%s)", got.Error(), got.RuleCode(), refused.Error(), refused.RuleCode())
	}
}

// Bare `array` is not the deprecated spelling, and an expired `array(T)` does
// not take it down with it.
func TestAnExpiredArrayTypeLeavesBareArrayAlone(t *testing.T) {
	restore := deprecation.SetCurrent("0.25.0")
	defer restore()

	if _, err := ParseFile("concept ticket {\n  tags array\n}"); err != nil {
		t.Fatalf("bare array is not the deprecated form: %v", err)
	}
}

// A build that names no release -- every test binary, every dev build, the
// language server -- decides at "", which is unreadable, so the form keeps
// parsing. window.go's fail-open posture, reaching the parser.
func TestAnUnstampedBuildKeepsParsingTheDeprecatedForm(t *testing.T) {
	restore := deprecation.SetCurrent("")
	defer restore()

	if _, err := ParseFile(deprecatedArrayConcept); err != nil {
		t.Fatalf("a build that names no release must not refuse a deprecated form: %v", err)
	}
}
