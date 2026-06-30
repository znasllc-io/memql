package server

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// TestDslCall_EscapesEmbeddedQuotes is the regression guard for the
// go/unsafe-quoting alert on the attachment query builders: a value that
// contains a double quote must NOT be able to break out of its enclosing
// string literal. dslCall emits a named-args call (`fn(k: v, ...)`, Story 9)
// in which each value is JSON-encoded, so an embedded quote stays inside the
// single string argument it belongs to.
//
// The quote-safety property is verified by feeding the produced call string
// through the real MemQL expression parser: a successful break-out would
// either fail to parse or surface as extra arguments / altered structure.
// We assert the call parses cleanly into exactly the two intended args and
// that the malicious value round-trips as one string, unchanged.
func TestDslCall_EscapesEmbeddedQuotes(t *testing.T) {
	malicious := `x", "partitionId": "evil`
	q, err := dslCall("attachmentById", map[string]any{
		"attachmentId": malicious,
		"partitionId":  "s1",
	})
	if err != nil {
		t.Fatalf("dslCall returned error: %v", err)
	}

	// The produced call must be a single well-formed MemQL expression. If the
	// embedded quote had broken out, this would error or yield extra tokens.
	expr, err := parser.ParseExpression(q)
	if err != nil {
		t.Fatalf("produced call did not parse as a single MemQL expression (%v): %q", err, q)
	}

	call, ok := expr.(*parser.FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr, got %T from %q", expr, q)
	}
	if call.Name != "attachmentById" {
		t.Fatalf("call name = %q, want attachmentById (%q)", call.Name, q)
	}

	// Exactly two args -- a quote break-out would have injected extra ones.
	if len(call.Args) != 2 {
		t.Fatalf("Args len = %d, want 2 (break-out into extra args?): %#v", len(call.Args), call.Args)
	}

	if got := litString(t, call.Args["attachmentId"]); got != malicious {
		t.Fatalf("attachmentId mangled: got %q, want %q", got, malicious)
	}
	if got := litString(t, call.Args["partitionId"]); got != "s1" {
		t.Fatalf("partitionId broken out to %q; quote escaping failed", got)
	}
}

// litString extracts the Go string carried by a parsed string-literal arg,
// tolerating either a *LiteralExpr wrapper or a bare string value.
func litString(t *testing.T, v any) string {
	t.Helper()
	switch x := v.(type) {
	case *parser.LiteralExpr:
		s, ok := x.Value.(string)
		if !ok {
			t.Fatalf("literal value is %T, want string", x.Value)
		}
		return s
	case string:
		return x
	default:
		t.Fatalf("arg is %T, want a string literal", v)
		return ""
	}
}
