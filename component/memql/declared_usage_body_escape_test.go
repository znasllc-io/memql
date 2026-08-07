package memql

import (
	"strings"
	"testing"
)

// declared_usage_body_escape_test.go -- memql#3190, the function-body slicer.
//
// extractFunctionBody decided where a literal ended with a ONE-BYTE LOOKBACK
// (`source[i-1] != '\\'`), which cannot tell an escaped quote from a quote
// that follows a COMPLETED `\\` escape. A body containing a literal that ends
// in a backslash pair therefore never left string state: the `{` / `}` after
// it stopped being counted, the brace depth never returned to zero, and the
// slicer returned "" -- which every caller (declared-usage, actor-binding,
// logic event-field and event-binding validators) treats as "no body", so
// each check it feeds silently passes. A fail-open in four validators at once.

func TestExtractFunctionBody_LiteralEndingInCompletedEscape(t *testing.T) {
	source := `func (Query) q(ctx any) (any, error) {
	return where("path == C:\\"), nil
}
` + "// trailing source outside the body"

	body := extractFunctionBody(source)

	if !strings.Contains(body, "return where(") {
		t.Errorf("body did not contain the function body -- the literal ending in `\\\\` kept the "+
			"scanner in string state, so the closing brace was never counted and the slicer "+
			"returned %q.\nsource:\n%s", body, source)
	}
	if strings.Contains(body, "trailing source") {
		t.Errorf("body over-ran the closing brace:\n%s", body)
	}
}

// A literal containing an ESCAPED QUOTE must still not end the literal -- the
// brace inside it is literal interior, not a body opener.
func TestExtractFunctionBody_EscapedQuoteStillHoldsTheLiteral(t *testing.T) {
	source := `func (Query) q(ctx any) (any, error) {
	return where("a \" } b"), nil
}
`
	body := extractFunctionBody(source)
	if !strings.Contains(body, "return where(") {
		t.Errorf("body lost the return statement: %q", body)
	}
	if !strings.Contains(body, `a \" } b`) {
		t.Errorf("the brace inside the literal ended the body early: %q", body)
	}
}
