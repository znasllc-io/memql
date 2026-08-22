package compiler

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// TestExpressionToString_StartsWith pins the serializer's spelling of the
// `startsWith` comparison (memql#4208): a keyword operator needs the spaces
// `in` gets, or the re-parse reads `codeReferencestartsWith"x"` as one
// identifier and a string.
func TestExpressionToString_StartsWith(t *testing.T) {
	c := New(Config{})
	src := `codeReference startsWith "integration."`
	expr, err := parser.ParseExpression(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	got := c.expressionToString(expr)
	if got != src {
		t.Errorf("expressionToString(%q) = %q", src, got)
	}
	if strings.Contains(got, "<<unsupported") {
		t.Fatalf("serialized to an unsupported-expression placeholder: %q", got)
	}
	if _, err := parser.ParseExpression(got); err != nil {
		t.Fatalf("emitted form %q does not re-parse: %v", got, err)
	}
	if jsonForm := c.expressionToJSONExpr(expr); jsonForm != src {
		t.Errorf("expressionToJSONExpr(%q) = %q", src, jsonForm)
	}
}
