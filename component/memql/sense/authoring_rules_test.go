package sense

import (
	"strings"
	"testing"

	"github.com/visionarys-io/memql/component/language/parser"
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
