package dslspec

import "testing"

// TestStartsWithIsInTheSpec pins the authoring surface for the `startsWith`
// predicate (memql#4208): the spec is what Sense completion and hover read,
// so an operator the parser accepts but the spec omits is one the editor
// cannot offer or explain.
func TestStartsWithIsInTheSpec(t *testing.T) {
	s := Build()

	var asOperator, asKeyword bool
	for _, op := range s.Operators {
		if op.Symbol == "startsWith" {
			asOperator = true
			if op.Doc == "" {
				t.Error("startsWith operator has no Doc")
			}
		}
	}
	for _, kw := range s.Keywords {
		if kw.Name == "startsWith" {
			asKeyword = true
			if kw.Kind != "control" {
				t.Errorf("startsWith keyword Kind = %q, want control (it sits beside `in` / `when`)", kw.Kind)
			}
		}
	}
	if !asOperator {
		t.Error("startsWith missing from dslspec operators() -- add it in lexicon.go")
	}
	if !asKeyword {
		t.Error("startsWith missing from dslspec keywords() -- add it in lexicon.go")
	}
}
