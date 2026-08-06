package memql

import (
	"strings"
	"testing"
)

// memql#3120: extractFunctionBody's brace scanner inferred escape state from
// the preceding byte. A literal ending in a completed `\\` escape left it
// inside string state, so every later brace went uncounted -- returning the
// wrong body, or "", which makes each caller SKIP validation entirely
// (validateDeclaredUsage, validateLogicEventBinding, validateActorBinding and
// validateLogicEventFields all fail open on an empty body).
func TestExtractFunctionBodyHandlesCompletedEscapes(t *testing.T) {
	const source = `func (Query) findByPath(ctx any) (any, error) {
	return query("v1:x:y", path == "C:\\")
}
`
	body := extractFunctionBody(source)

	if body == "" {
		t.Fatal("empty body: the scanner never left string state, and every " +
			"caller silently skips validation on an empty body")
	}
	if !strings.Contains(body, `path == "C:\\"`) {
		t.Errorf("body does not contain the filter:\n%s", body)
	}
}

// Control: a brace INSIDE a string literal must still not be counted, or the
// fix would have traded one failure for the other.
func TestExtractFunctionBodyIgnoresBracesInsideLiterals(t *testing.T) {
	const source = `func (Query) braced(ctx any) (any, error) {
	return query("v1:x:y", pattern == "}{")
}
`
	body := extractFunctionBody(source)

	if !strings.Contains(body, `pattern == "}{"`) {
		t.Errorf("a brace inside a literal ended the body early:\n%s", body)
	}
}
