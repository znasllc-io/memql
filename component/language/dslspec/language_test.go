package dslspec

import (
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// TestSpecNamesTheLanguageItDescribes: the exported spec says which edition
// and which grammar it is (memql#5362, D25), so a client that is not the VS
// Code extension can answer "does my editor speak this cluster's language"
// from the DslSpec export alone.
func TestSpecNamesTheLanguageItDescribes(t *testing.T) {
	s := Build()
	if s.Edition != parser.Edition {
		t.Errorf("Spec.Edition = %q, want parser.Edition %q", s.Edition, parser.Edition)
	}
	if s.GrammarVersion != parser.GrammarVersion {
		t.Errorf("Spec.GrammarVersion = %q, want parser.GrammarVersion %q", s.GrammarVersion, parser.GrammarVersion)
	}

	raw, err := s.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if envelope["edition"] != parser.Edition {
		t.Errorf(`the JSON envelope's "edition" is %v, want %q`, envelope["edition"], parser.Edition)
	}
	if envelope["grammarVersion"] != parser.GrammarVersion {
		t.Errorf(`the JSON envelope's "grammarVersion" is %v, want %q`, envelope["grammarVersion"], parser.GrammarVersion)
	}
}

// TestSpecVersionRecordsTheAdditiveEnvelope: the envelope gained keys, which
// is an additive change to its shape -- a minor move of SpecVersion, so a
// consumer checking the major it was built against keeps working.
//
// 1.1.0 added `edition` and `grammarVersion` (memql#5362). 1.2.0 added the
// three keys the generated grammar derives its productions from (memql#5388):
// `constructs[].signature`, `constructs[].bodyForm` and `keywords[].grammar`
// with its `keywords[].heads`.
func TestSpecVersionRecordsTheAdditiveEnvelope(t *testing.T) {
	if SpecVersion != "1.2.0" {
		t.Errorf("SpecVersion = %q, want 1.2.0: the envelope gained signature, bodyForm, grammar and heads", SpecVersion)
	}
}
