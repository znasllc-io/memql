package parser

import (
	"strings"
	"testing"
)

// C1 (memql#2041, epic #2031): `mutate` is the descriptive verb that
// replaces `mutation` as the mutation declaration keyword. It is a pure
// surface keyword -- it rewrites and parses identically to `mutation`
// and maps to the same internal ReceiverMutation kind. The dual-accept
// is transitional; C6 sweeps the tree to `mutate` and drops `mutation`.

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

// TestMutateVerb_RewritesIdenticallyToMutation proves the verb alias and
// the legacy noun produce byte-identical procedural IR (modulo the
// keyword itself), so every downstream stage sees the same shape.
func TestMutateVerb_RewritesIdenticallyToMutation(t *testing.T) {
	body := ` space createSpace {
  args {
    spaceId string @required
    name    string @required
  }
  insert {
    id:        args.spaceId
    name:      args.name
    status:    "active"
  }
}`
	mutationOut, err := NormaliseAll("mutation" + body)
	if err != nil {
		t.Fatalf("`mutation` form should rewrite cleanly, got: %v", err)
	}
	mutateOut, err := NormaliseAll("mutate" + body)
	if err != nil {
		t.Fatalf("`mutate` form should rewrite cleanly, got: %v", err)
	}
	if mutationOut != mutateOut {
		t.Fatalf("`mutate` and `mutation` should rewrite identically.\nmutation:\n%s\nmutate:\n%s", mutationOut, mutateOut)
	}
	if !strings.Contains(mutateOut, "func (Mutation) createSpace") {
		t.Fatalf("`mutate` should emit the same `func (Mutation)` procedural form; got %q", mutateOut)
	}
}

// TestMutateVerb_ParsesToReceiverMutation proves `mutate` parses to the
// same AST receiver kind as `mutation`.
func TestMutateVerb_ParsesToReceiverMutation(t *testing.T) {
	mutateSrc := `mutate space createSpace {
  insert {
    id:   args.spaceId
    name: args.name
  }
}`
	mutationSrc := `mutation space createSpace {
  insert {
    id:   args.spaceId
    name: args.name
  }
}`

	mutateFn := parseToFunctionDef(t, mutateSrc)
	mutationFn := parseToFunctionDef(t, mutationSrc)

	if mutateFn.Receiver.Type != ReceiverMutation {
		t.Fatalf("`mutate` should parse to ReceiverMutation, got %v", mutateFn.Receiver.Type)
	}
	if mutateFn.Receiver.Type != mutationFn.Receiver.Type {
		t.Fatalf("`mutate` (%v) and `mutation` (%v) should map to the same receiver kind",
			mutateFn.Receiver.Type, mutationFn.Receiver.Type)
	}
	if mutateFn.Name != mutationFn.Name {
		t.Fatalf("both forms should parse the same construct name; got %q vs %q",
			mutateFn.Name, mutationFn.Name)
	}
}

// TestMutateVerb_SignatureConceptStillBinds proves the rewriter still
// derives the write target from the `mutate <Concept> <name>` signature.
func TestMutateVerb_SignatureConceptStillBinds(t *testing.T) {
	src := `mutate space createSpace {
  insert {
    id:   args.spaceId
    name: args.name
  }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("`mutate` struct form should rewrite, got: %v", err)
	}
	if !strings.Contains(out, "insert(") || !strings.Contains(out, "space") {
		t.Fatalf("emitted insert should target the signature concept `space`; got %q", out)
	}
}
