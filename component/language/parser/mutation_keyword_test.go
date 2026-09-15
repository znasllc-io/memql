package parser

import (
	"errors"
	"strings"
	"testing"
)

// A mutation is declared with the word a call spells, `mutation` (D13, epic
// memql#5370). The verb `mutate` that C1 (memql#2041) introduced as the
// declaration keyword is retired: a statement it opens is refused, naming
// the replacement and the migrator. These tests lock that end state.

// parseToFunctionDef runs the full author-source pipeline (NormaliseAll
// rewrite -> tokenize -> parse) and returns the single FunctionDef.
func parseToFunctionDef(t *testing.T, src string) *FunctionDef {
	t.Helper()
	rewritten, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll error: %v", err)
	}
	lexer := NewLexer(rewritten)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}
	node, err := NewParser(tokens).Parse()
	if err != nil {
		t.Fatalf("Parse error: %v\nrewritten source:\n%s", err, rewritten)
	}
	file, ok := node.(*File)
	if !ok {
		t.Fatalf("expected *File, got %T", node)
	}
	for _, def := range file.Definitions {
		if fn, ok := def.(*FunctionDef); ok {
			return fn
		}
	}
	t.Fatalf("no FunctionDef in parsed file")
	return nil
}

// mutationBody is a mutation's text after its keyword.
const mutationBody = ` space createSpace {
  insert {
    id:   args.spaceId
    name: args.name
  }
}`

// TestMutationKeyword_RewritesToProceduralForm proves `mutation` rewrites
// cleanly to the internal `func (Mutation)` procedural form, deriving the
// write target from the `mutation <Concept> <name>` signature.
func TestMutationKeyword_RewritesToProceduralForm(t *testing.T) {
	out, err := NormaliseMutationSource("mutation" + mutationBody)
	if err != nil {
		t.Fatalf("`mutation` form should rewrite cleanly, got: %v", err)
	}
	if !strings.Contains(out, "func (Mutation) createSpace") {
		t.Fatalf("`mutation` should emit the `func (Mutation)` procedural form; got %q", out)
	}
	if !strings.Contains(out, "insert(") || !strings.Contains(out, "space") {
		t.Fatalf("emitted insert should target the signature concept `space`; got %q", out)
	}
}

// TestMutationKeyword_ParsesToReceiverMutation proves `mutation` parses to
// the ReceiverMutation AST kind.
func TestMutationKeyword_ParsesToReceiverMutation(t *testing.T) {
	fn := parseToFunctionDef(t, "mutation"+mutationBody)
	if fn.Receiver.Type != ReceiverMutation {
		t.Fatalf("`mutation` should parse to ReceiverMutation, got %v", fn.Receiver.Type)
	}
	if fn.Name != "createSpace" {
		t.Fatalf("expected construct name createSpace, got %q", fn.Name)
	}
}

// TestMutateKeywordRetired proves the retired verb is no struct mutation --
// the rewriter leaves it alone -- and that a file declaring with it is
// refused where it is written, by both the parser and the load gate, with
// the replacement and the migrator named.
func TestMutateKeywordRetired(t *testing.T) {
	// memqlmigrate:keep -- the retired keyword is the case.
	src := "use cognition.concepts.{ space }\n\nmutate" + mutationBody
	if LooksLikeStructMutation(src) {
		t.Fatal("`mutate` must no longer be recognised as a struct mutation")
	}
	const want = "mutate is retired in edition 2026: a mutation is declared mutation <Concept> <name> { ... } (memqlmigrate --rewrite=bodies rewrites it) [construct_unknown]"
	_, err := rewriteAndParse(t, src)
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Line != 3 || pe.Column != 1 || !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("want the refusal at 3:1 ending %q, got %v", want, err)
	}
	found := FindUnknownConstructKeywords(src)
	if len(found) != 1 || found[0].Line != 3 || found[0].Keyword != "mutate" || found[0].Message != want {
		t.Fatalf("the load gate must refuse the same line with the same message, got %+v", found)
	}
}
