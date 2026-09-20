package dslspec

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/functions"
)

// The vocabulary is the second artifact a model is given (memql#5388, D24):
// every construct, annotation, builtin and function with a description of three
// or four sentences. The grammar says what may be WRITTEN; the vocabulary says
// what each thing MEANS, which is the half a grammar cannot carry.
func TestVocabularyCoversEveryNamedThing(t *testing.T) {
	v := Vocabulary()
	byName := map[string]VocabularyEntry{}
	for _, e := range v {
		key := e.Kind + ":" + e.Name
		if _, dup := byName[key]; dup {
			t.Errorf("the vocabulary lists %s twice; a model reading it twice learns nothing the second time, "+
				"and a page generated from it renders two entries under one heading", key)
		}
		byName[key] = e
	}
	for _, c := range Build().Constructs {
		if _, ok := byName["construct:"+c.Keyword]; !ok {
			t.Errorf("the vocabulary omits the construct %q", c.Keyword)
		}
	}
	for _, p := range annotations.Placements() {
		if _, ok := byName["annotation:@"+p.Name]; !ok {
			t.Errorf("the vocabulary omits the annotation @%s", p.Name)
		}
	}
	// Keyed by Key(), not Name: a method's one spelling is receiver.name
	// ("list.any"), and two receivers may both answer to `count`.
	for _, f := range functions.Catalog() {
		if _, ok := byName["function:"+f.Key()]; !ok {
			t.Errorf("the vocabulary omits the function %q", f.Key())
		}
	}
	for _, op := range functions.Operators() {
		if _, ok := byName["operator:"+op.Name]; !ok {
			t.Errorf("the vocabulary omits the operator %q (%s)", op.Name, op.Symbol)
		}
	}
	for _, k := range Build().Keywords {
		if _, ok := byName["keyword:"+k.Name]; !ok {
			t.Errorf("the vocabulary omits the keyword %q", k.Name)
		}
	}
	for _, b := range Build().Builtins {
		if b.Category == CategoryBuiltinExpr {
			continue // a catalog function, listed under function:.
		}
		if _, ok := byName["builtin:"+b.Name]; !ok {
			t.Errorf("the vocabulary omits the builtin %q", b.Name)
		}
	}
}

// D24 says "a description of three or four sentences". A one-word description
// is the shape that makes a vocabulary useless to a model: it repeats the name.
func TestVocabularyDescriptionsAreSentences(t *testing.T) {
	for _, e := range Vocabulary() {
		d := strings.TrimSpace(e.Description)
		if d == "" {
			t.Errorf("%s %q has no description", e.Kind, e.Name)
			continue
		}
		if len(strings.Fields(d)) < 6 {
			t.Errorf("%s %q's description is %d words (%q); D24 asks for three or four sentences, and a "+
				"description shorter than the name it explains teaches a model nothing",
				e.Kind, e.Name, len(strings.Fields(d)), d)
		}
	}
}

// Every entry must say how it is WRITTEN as well as what it means. A
// vocabulary of bare names is a list a model cannot act on: it knows `@cache`
// exists and not that it takes a number.
func TestVocabularyEntriesCarryASignature(t *testing.T) {
	for _, e := range Vocabulary() {
		if strings.TrimSpace(e.Signature) == "" {
			t.Errorf("%s %q carries no signature, so the vocabulary says what it means and not how to write it", e.Kind, e.Name)
		}
	}
}

func TestVocabularyNamesNoRetiredThing(t *testing.T) {
	entries := Vocabulary()
	if len(entries) == 0 {
		t.Fatal("Vocabulary() is empty, so every check below passes over nothing")
	}
	// A name retired EVERYWHERE and accepted by no receiver may never appear;
	// a name retired on one receiver and live on another (@internal, retired
	// on a query and live on a concept field) legitimately does.
	accepted := map[string]bool{}
	for _, p := range annotations.Placements() {
		accepted[p.Name] = true
	}
	for _, r := range annotations.Retirements() {
		if r.Prefix || accepted[r.Name] {
			continue
		}
		for _, e := range entries {
			if e.Kind == "annotation" && e.Name == "@"+r.Name {
				t.Errorf("the vocabulary advertises the retired annotation @%s (%s)", r.Name, r.Hint)
			}
		}
	}
	for name, replacement := range functions.RetiredFunctions() {
		for _, e := range entries {
			if e.Kind == "function" && e.Name == name {
				t.Errorf("the vocabulary advertises the retired function %q; write %q instead", name, replacement)
			}
		}
	}
	for key, replacement := range functions.RetiredMethods() {
		for _, e := range entries {
			if e.Kind == "function" && e.Name == key {
				t.Errorf("the vocabulary advertises the retired method %q; write %q instead", key, replacement)
			}
		}
	}
}

// The vocabulary and the grammar describe one language, so a NAME the grammar
// offers must be a name the vocabulary explains. The two are rendered from the
// same tables, which is exactly why this is worth checking: a renderer that
// reads one table for the grammar and a different one for the vocabulary
// produces two pages that disagree, and nothing else would notice.
func TestVocabularyExplainsWhatTheGrammarOffers(t *testing.T) {
	bnf := BNF()
	explained := map[string]bool{}
	for _, e := range Vocabulary() {
		explained[e.Kind+":"+e.Name] = true
	}
	for _, c := range Build().Constructs {
		if !strings.Contains(bnf, "<"+c.Keyword+">") {
			continue
		}
		if !explained["construct:"+c.Keyword] {
			t.Errorf("the grammar offers <%s> and the vocabulary does not explain it", c.Keyword)
		}
	}
	for _, f := range functions.Catalog() {
		if !explained["function:"+f.Key()] {
			t.Errorf("the grammar offers the call %q and the vocabulary does not explain it", f.Key())
		}
	}
}

// TestVocabularyPageWrapsTheEntries holds the committed page's frame, the way
// TestGrammarPageWrapsTheBNF does for the grammar.
func TestVocabularyPageWrapsTheEntries(t *testing.T) {
	page := RenderVocabulary()
	for _, key := range []string{"title:", "audience:", "status:", "area:", "sinceVersion:", "owner:"} {
		if !strings.Contains(page, key) {
			t.Errorf("the vocabulary page has no %s front-matter key; docs_front_matter_test.go refuses it", key)
		}
	}
	if !strings.Contains(page, "make docs-grammar") {
		t.Error("the vocabulary page does not name `make docs-grammar`, so a reader who finds it stale has nothing to run")
	}
	if strings.Contains(page, "```memql\n") {
		t.Error("the vocabulary page opens a ```memql fence: its signatures are forms, not loadable examples, and " +
			"a fence there would be handed to the parser by the docs-example gate")
	}
	for _, e := range Vocabulary() {
		if !strings.Contains(page, e.Name) {
			t.Errorf("the vocabulary page does not name the %s %q it lists", e.Kind, e.Name)
		}
	}
}
