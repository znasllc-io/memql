package parser

import (
	"strings"
	"testing"
)

// TestAttributeAcceptsKeywordArgumentNames pins a grammar widening made for
// memql#3652: a lexer-promoted keyword is a valid ARGUMENT NAME inside an
// annotation.
//
// The lexer promotes `as` to TokenKeywordAs because the language uses it in
// `forEach ... as x` and in `use ... as` import aliasing. That promotion leaked
// into annotation arguments, where there is no such ambiguity -- an argument
// name sits after `(` or `,` and is followed by `=`, a position no control-flow
// keyword can occupy.
//
// The same allowance already exists one position over, for annotation NAMES:
// isKeywordTokenForAttribute lets @default / @return / @case parse, and already
// lists TokenKeywordAs. This extends the identical rule to argument names, so
// @relationship(as="assignedTo") parses.
//
// This is a widening: it accepts input that was previously rejected and rejects
// nothing that previously parsed.
func TestAttributeAcceptsKeywordArgumentNames(t *testing.T) {
	// One case per keyword that isKeywordTokenForAttribute already blesses at
	// the annotation-name position, so the two positions cannot drift apart.
	for _, argName := range []string{"as", "in", "where", "when", "default", "for", "if", "case"} {
		t.Run(argName, func(t *testing.T) {
			src := `
@description("Concept exercising a keyword argument name.")
concept widget {
  agentId  string  @description("FK.")

  @relationship(type="references", ` + argName + `="someValue", field="agentId", target="v1:agents:agent", direction="outgoing")
}
`
			tokens, err := NewLexer(src).Tokenize()
			if err != nil {
				t.Fatalf("Tokenize: %v", err)
			}

			if _, err := NewParser(tokens).Parse(); err != nil &&
				strings.Contains(err.Error(), "expected argument name in attribute") {
				t.Errorf("keyword %q was rejected as an argument name: %v", argName, err)
			}
		})
	}
}
