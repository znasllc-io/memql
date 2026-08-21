package parser

import (
	"reflect"
	"strings"
	"testing"
)

// startswith_test.go -- the `<field> startsWith <prefix>` comparison
// (memql#4208).
//
// It is the prefix sibling of `in`: identifier-led, a keyword operator, and a
// right-hand side that is a string literal, a list of string literals, or an
// `args.<field>` reference (a string or a list at call time). The engine
// compiles it to a parameterized `^@ ANY(text[])` and evaluates it in-process
// with strings.HasPrefix; see component/memql/executor_filter.go.
//
// The negative cases are the memql#2383 rule: a grammar addition lands with
// the malformed forms proven to fail loud. The ones that matter most are the
// ones that would otherwise be SILENT -- a bare identifier on the right reads
// as the literal text of that identifier under parseValue, so `a startsWith b`
// would compile to a predicate that never matches and nothing would say so.

// parseFilterExprStrict parses a filter expression and requires the parser to
// have consumed every token: a leftover `startsWith` after an early-returning
// `args.x` would otherwise read as a successful parse of the prefix alone.
func parseFilterExprStrict(t *testing.T, src string) (ExpressionNode, error) {
	t.Helper()
	tokens, err := NewLexer(src).Tokenize()
	if err != nil {
		return nil, err
	}
	p := NewParser(tokens)
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if !p.check(TokenEOF) {
		return nil, newParseErrorf(&p.current, "unexpected token %q after expression", p.current.Literal)
	}
	return expr, nil
}

func TestStartsWith_LexesAsKeyword(t *testing.T) {
	tokens, err := NewLexer(`codeReference startsWith "integration."`).Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	var found bool
	for _, tok := range tokens {
		if tok.Type == TokenKeywordStartsWith {
			found = true
			if tok.Literal != "startsWith" {
				t.Errorf("keyword literal = %q, want startsWith", tok.Literal)
			}
		}
	}
	if !found {
		t.Fatalf("startsWith did not lex as TokenKeywordStartsWith: %+v", tokens)
	}
	if TokenKeywordStartsWith.String() != "startsWith" {
		t.Errorf("TokenKeywordStartsWith.String() = %q", TokenKeywordStartsWith.String())
	}
}

func TestStartsWith_StringLiteral(t *testing.T) {
	got := parseFilterExpr(t, `codeReference startsWith "integration."`)
	want := &ComparisonExpr{
		Field:    FieldReference{Raw: "codeReference", Parts: []string{"codeReference"}},
		Operator: OpStartsWith,
		Value:    "integration.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestStartsWith_ListLiteral(t *testing.T) {
	got := parseFilterExpr(t, `codeReference startsWith ["integration.email.", "integration.shopify."]`)
	cmp, ok := got.(*ComparisonExpr)
	if !ok || cmp.Operator != OpStartsWith {
		t.Fatalf("expected an OpStartsWith comparison, got %#v", got)
	}
	want := []any{"integration.email.", "integration.shopify."}
	if !reflect.DeepEqual(cmp.Value, want) {
		t.Fatalf("list value = %#v, want %#v", cmp.Value, want)
	}
}

func TestStartsWith_ArgRef(t *testing.T) {
	got := parseFilterExpr(t, `codeReference startsWith args.prefixes`)
	cmp, ok := got.(*ComparisonExpr)
	if !ok || cmp.Operator != OpStartsWith {
		t.Fatalf("expected an OpStartsWith comparison, got %#v", got)
	}
	ref, ok := cmp.Value.(*ArgRefExpr)
	if !ok || ref.Path != "prefixes" {
		t.Fatalf("value = %#v, want *ArgRefExpr{Path: prefixes}", cmp.Value)
	}
}

// A dotted payload path on the left is a field like any other.
func TestStartsWith_DottedFieldPath(t *testing.T) {
	got := parseFilterExpr(t, `source.codeReference startsWith "integration."`)
	cmp, ok := got.(*ComparisonExpr)
	if !ok || cmp.Operator != OpStartsWith {
		t.Fatalf("expected an OpStartsWith comparison, got %#v", got)
	}
	if !reflect.DeepEqual(cmp.Field.Parts, []string{"source", "codeReference"}) {
		t.Fatalf("field parts = %#v", cmp.Field.Parts)
	}
}

// The predicate composes with the rest of the filter grammar: a `when()`
// guard OR-ed with it inside a parenthesised group, under an AND. This is the
// exact shape dsl/observability/queries.memql's codeMetricsInWindow uses.
func TestStartsWith_ComposesWithWhenGuardAndOr(t *testing.T) {
	src := `bucket==args.bucket && (when(args.codeReference) { codeReference==args.codeReference } || codeReference startsWith args.prefixes)`
	got, err := parseFilterExprStrict(t, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	and, ok := got.(*LogicalExpr)
	if !ok || and.Op != LogicalAnd {
		t.Fatalf("expected a top-level AND, got %#v", got)
	}
	or, ok := and.Right.(*LogicalExpr)
	if !ok || or.Op != LogicalOr {
		t.Fatalf("expected an OR on the right of the AND, got %#v", and.Right)
	}
	if _, ok := or.Left.(*ConditionalFilterExpr); !ok {
		t.Fatalf("expected the when() guard on the left of the OR, got %#v", or.Left)
	}
	cmp, ok := or.Right.(*ComparisonExpr)
	if !ok || cmp.Operator != OpStartsWith || cmp.Field.Raw != "codeReference" {
		t.Fatalf("expected codeReference startsWith on the right of the OR, got %#v", or.Right)
	}
}

// The struct-form rewriter passes a filter line through to the grammar; the
// predicate must survive that rewrite inside a real query construct.
func TestStartsWith_StructQueryFilter(t *testing.T) {
	src := `query codeMetric byPrefix {
  args {
    prefixes []string!
  }
  filter codeReference startsWith args.prefixes
}`
	file, err := rewriteAndParse(t, src)
	if err != nil {
		t.Fatalf("rewrite+parse: %v", err)
	}
	var found *ComparisonExpr
	walkExpressions(file, func(n ExpressionNode) {
		if cmp, ok := n.(*ComparisonExpr); ok && cmp.Operator == OpStartsWith {
			found = cmp
		}
	})
	if found == nil {
		t.Fatalf("no OpStartsWith comparison in the parsed query")
	}
	if ref, ok := found.Value.(*ArgRefExpr); !ok || ref.Path != "prefixes" {
		t.Fatalf("value = %#v, want the prefixes arg reference", found.Value)
	}
}

// A spec body reads its bound fields bare; the predicate is a boolean over a
// string field, so it belongs there too.
func TestStartsWith_SpecBody(t *testing.T) {
	decl, err := ParseSpecDecl(`@enabled
@description("Matches integration-owned code references.")
spec codeMetric isIntegrationMetric {
  return codeReference startsWith "integration."
}`)
	if err != nil {
		t.Fatalf("ParseSpecDecl: %v", err)
	}
	cmp, ok := decl.Body.(*ComparisonExpr)
	if !ok || cmp.Operator != OpStartsWith || cmp.Value != "integration." {
		t.Fatalf("spec body = %#v, want codeReference startsWith \"integration.\"", decl.Body)
	}
}

func TestStartsWith_Negative(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // substring the error must carry
	}{
		{"not startsWith is not a form", `codeReference not startsWith "a"`, "not startsWith"},
		{"missing right-hand side", `codeReference startsWith`, "startsWith requires"},
		{"numeric prefix", `codeReference startsWith 5`, "startsWith requires"},
		{"boolean prefix", `codeReference startsWith true`, "startsWith requires"},
		// The silent one: parseValue returns a bare identifier as its own text,
		// so without a dedicated check this would compile to `startsWith 'other'`.
		{"bare identifier prefix", `codeReference startsWith other`, "startsWith requires"},
		{"actor reference prefix", `codeReference startsWith actor.userId`, "startsWith requires"},
		{"list with a non-string element", `codeReference startsWith ["a.", 5]`, "string literals"},
		{"nested list element", `codeReference startsWith [["a."]]`, "string literals"},
		{"args reference on the left", `args.prefix startsWith "a"`, "row field on the left"},
		{"nil prefix", `codeReference startsWith nil`, "startsWith requires"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFilterExprStrict(t, tc.src)
			if err == nil {
				t.Fatalf("%q: expected a parse error, got nil (silently accepted)", tc.src)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%q: error %q should mention %q", tc.src, err.Error(), tc.want)
			}
		})
	}
}

// walkExpressions visits every expression node reachable from a parsed file's
// query statements, including the guarded and logical sub-trees.
func walkExpressions(file *File, visit func(ExpressionNode)) {
	var walk func(n ExpressionNode)
	walk = func(n ExpressionNode) {
		if n == nil {
			return
		}
		visit(n)
		switch v := n.(type) {
		case *LogicalExpr:
			walk(v.Left)
			walk(v.Right)
		case *ConditionalFilterExpr:
			walk(v.Filter)
		case *NotExpr:
			walk(v.Target)
		case *RelationshipExpr:
			walk(v.Target)
		case *SortExpr:
			walk(v.Target)
		case *PaginateExpr:
			walk(v.Target)
		case *ShapeExpr:
			walk(v.Target)
		case *TimestampExpr:
			walk(v.Target)
		}
	}
	var walkBody func(body Node)
	walkBody = func(body Node) {
		switch b := body.(type) {
		case nil:
		case *ReturnStmt:
			for _, r := range b.Results {
				walk(r)
			}
		case *QueryStmt:
			walk(b.Expression)
		case ExpressionNode:
			walk(b)
		}
	}
	for _, def := range file.Definitions {
		if fn, ok := def.(*FunctionDef); ok {
			walkBody(fn.Body)
		}
	}
}
