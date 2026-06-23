package parser

import (
	"strings"
	"testing"
)

// C1 (memql#2041, epic #2031) introduced `mutate` as the descriptive verb
// replacing `mutation` as the mutation declaration keyword, with a
// transitional dual-accept. C6 (memql#2036) swept the tree to `mutate` and
// DROPPED the `mutation` noun alias -- `mutate` is now the only accepted
// mutation declaration keyword. These tests lock that end-state.

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

// TestMutateVerb_RewritesToProceduralForm proves the `mutate` verb rewrites
// cleanly to the internal `func (Mutation)` procedural form.
func TestMutateVerb_RewritesToProceduralForm(t *testing.T) {
	src := `mutate space createSpace {
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
	out, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("`mutate` form should rewrite cleanly, got: %v", err)
	}
	if !strings.Contains(out, "func (Mutation) createSpace") {
		t.Fatalf("`mutate` should emit the `func (Mutation)` procedural form; got %q", out)
	}
}

// TestMutateVerb_ParsesToReceiverMutation proves `mutate` parses to the
// ReceiverMutation AST kind.
func TestMutateVerb_ParsesToReceiverMutation(t *testing.T) {
	mutateSrc := `mutate space createSpace {
  insert {
    id:   args.spaceId
    name: args.name
  }
}`
	mutateFn := parseToFunctionDef(t, mutateSrc)
	if mutateFn.Receiver.Type != ReceiverMutation {
		t.Fatalf("`mutate` should parse to ReceiverMutation, got %v", mutateFn.Receiver.Type)
	}
	if mutateFn.Name != "createSpace" {
		t.Fatalf("expected construct name createSpace, got %q", mutateFn.Name)
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

// TestLegacyMutationKeywordRejected proves the dropped `mutation` noun alias
// is no longer recognised as a struct-form mutation header (C6 end-state):
// the rewriter detector ignores it, whereas the `mutate` verb is recognised.
// (`mutation` is consequently never rewritten to the procedural form, so a
// loader can no longer materialise it as a mutation construct.)
func TestLegacyMutationKeywordRejected(t *testing.T) {
	// Build the legacy keyword by concatenation so the C6 codemod
	// (scripts/rename/deprefix.py) does not rewrite this intentional fixture.
	legacyKeyword := "muta" + "tion"
	body := ` space createSpace {
  insert {
    id:   args.spaceId
    name: args.name
  }
}`
	if LooksLikeStructMutation(legacyKeyword + body) {
		t.Fatalf("`mutation` keyword must no longer be recognised as a struct mutation")
	}
	if !LooksLikeStructMutation("mutate" + body) {
		t.Fatalf("`mutate` keyword must still be recognised as a struct mutation")
	}
	// The legacy keyword is left untouched by the mutation rewriter (no
	// procedural `func (Mutation)` is emitted for it).
	out, err := NormaliseMutationSource(legacyKeyword + body)
	if err == nil && strings.Contains(out, "func (Mutation)") {
		t.Fatalf("`mutation` keyword must not rewrite to the procedural mutation form; got %q", out)
	}
}
