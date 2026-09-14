package dslspec

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// statements_test.go -- the lexicon describes the statement language (epic
// memql#5370). Sense reads a keyword's Doc verbatim on hover and in
// completion, so a Doc that still showed a retired body form would teach it,
// and a statement form with no keyword would have nothing to say.

// statementKeyword is the lexicon word each statement form the parser accepts
// is written with; "" for a form written with no keyword of its own (a name
// bound with :=, a call by its construct's kind).
var statementKeyword = map[string]string{
	"assign": "", "call": "",
	"if": "if", "else": "else", "for": "for", "switch": "switch", "parallel": "parallel",
	"publish": "publish", "return": "return",
	"retry": "retry", "wait": "wait", "onError": "on", "onSurface": "on",
}

// retiredBodySpellings are the retired body forms' spellings, which no keyword
// Doc may show except one saying the keyword itself is retired.
var retiredBodySpellings = []string{":= range", "steps.", "forEach", "publishEvent(", "body {", "step "}

func TestKeywordDocsTeachTheStatementForms(t *testing.T) {
	byName := map[string]Keyword{}
	for _, k := range Build().Keywords {
		byName[k.Name] = k
	}
	for _, form := range parser.BodyStatementForms() {
		word, known := statementKeyword[form]
		if !known {
			t.Errorf("statement form %q has no entry here: add its keyword to lexicon.go's keywords() and to statementKeyword", form)
			continue
		}
		if word == "" {
			continue
		}
		k, ok := byName[word]
		switch {
		case !ok:
			t.Errorf("the lexicon has no keyword %q for the statement form %q", word, form)
		case strings.HasPrefix(k.Doc, "Retired"):
			t.Errorf("keyword %q, which writes the statement form %q, is documented as retired: %q", word, form, k.Doc)
		}
	}
	for _, k := range Build().Keywords {
		if strings.HasPrefix(k.Doc, "Retired") {
			continue
		}
		for _, spelling := range retiredBodySpellings {
			if strings.Contains(k.Doc, spelling) {
				t.Errorf("keyword %q's Doc shows the retired body form %q: %q", k.Name, spelling, k.Doc)
			}
		}
	}
}
