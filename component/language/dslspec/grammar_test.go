package dslspec

import (
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
)

// The BNF is GENERATED from the same tables the parser reads (memql#5388, D24).
// Its purpose is to be given to a model as grammar-in-prompt and to drive
// syntax-only constrained decoding, and both uses fail the same way if it
// drifts: the model emits a form the parser refuses, confidently.

// bnfProductions reads the generated grammar into name -> right-hand side.
// A production is `<name> ::= <rhs>`; a continuation line opens with the
// alternation bar under the same name. Comments `(* ... *)` and blank lines
// are skipped, so the tests below assert over the grammar rather than over the
// page's prose.
func bnfProductions(t *testing.T, bnf string) map[string]string {
	t.Helper()
	head := regexp.MustCompile(`^<([a-zA-Z0-9-]+)>\s*::=\s*(.*)$`)
	out := map[string]string{}
	name := ""
	for _, line := range strings.Split(bnf, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "(*") {
			name = ""
			continue
		}
		if m := head.FindStringSubmatch(trimmed); m != nil {
			name = m[1]
			out[name] += " " + m[2]
			continue
		}
		if name != "" && strings.HasPrefix(trimmed, "|") {
			out[name] += " " + trimmed
		}
	}
	if len(out) == 0 {
		t.Fatal("bnfProductions read no productions at all: either BNF() stopped emitting them or " +
			"the `<name> ::= ...` shape changed under every test in this file")
	}
	return out
}

func TestBNFNamesEveryConstruct(t *testing.T) {
	bnf := BNF()
	if strings.TrimSpace(bnf) == "" {
		t.Fatal("BNF() is empty")
	}
	productions := bnfProductions(t, bnf)
	for _, c := range Build().Constructs {
		// Every construct the spec advertises must have a production. A
		// construct missing from the grammar is a form the model will never
		// emit and a reader will conclude does not exist.
		if _, ok := productions[c.Keyword]; !ok {
			t.Errorf("no production for construct %q; the grammar advertises a language that is missing it", c.Keyword)
		}
	}
	decl, ok := productions["declaration"]
	if !ok {
		t.Fatal("the grammar has no <declaration> production, so nothing says which constructs open a file")
	}
	for _, c := range Build().Constructs {
		if c.Keyword == "use" {
			continue // the file-top import statement declares nothing.
		}
		if !strings.Contains(decl, "<"+c.Keyword+">") {
			t.Errorf("<declaration> does not offer <%s>, so the grammar cannot derive a file containing one", c.Keyword)
		}
	}
}

func TestBNFNamesTheLanguageItDescribes(t *testing.T) {
	bnf := BNF()
	if !strings.Contains(bnf, parser.Edition) {
		t.Errorf("the BNF does not name its edition (%s); a grammar with no version is a grammar a reader cannot check against their cluster", parser.Edition)
	}
	if !strings.Contains(bnf, parser.GrammarVersion) {
		t.Errorf("the BNF does not name its grammar version (%s)", parser.GrammarVersion)
	}
}

// TestBNFCarriesNoRetiredForm is the plan's gate, scoped to where it is
// decidable.
//
// parser.V1RetiredForms() carries a SPELLING ("cond(p, a, b)", "has", "filter
// <predicate>") rather than a production name, and a bare token search over the
// whole grammar would be wrong in both directions: `filter`, `spec`, `trait`,
// `now`, `count` and `contains` are all LIVE in some other role, and each is
// retired only in one position. So the check is scoped to the two productions
// that LIST NAMES -- the callable set and the operator set -- which are the
// only places the grammar can tell a model to emit a retired call or a retired
// operator. The retired-annotation half is TestBNFCarriesNoRetiredAnnotation
// below, over annotations.Retirements().
func TestBNFCarriesNoRetiredForm(t *testing.T) {
	productions := bnfProductions(t, BNF())
	// WHERE a retired word could be offered depends on the SHAPE of the
	// spelling, and conflating the two is how this gate first read `now` --
	// the live reserved root -- as the retired `now()` call.
	//
	//   - a CALL spelling ("cond(p, a, b)", "now()") can only be offered by
	//     the function name list. The method list is deliberately out: the
	//     retirement of `count(x)`, `first(x)` and `last(x)` is TO the methods
	//     `x.count()`, `x.first()` and `x.last()`, so the method name is the
	//     replacement rather than the retirement.
	//   - a BARE word ("has", "null") can be offered as a literal or a
	//     reserved root as well as as a function.
	calls := productions["function-name"]
	words := calls + productions["literal"] + productions["reserved-root"]
	if strings.TrimSpace(calls) == "" || strings.TrimSpace(words) == "" {
		t.Fatal("the grammar lists no callable, literal or reserved names, so this gate would pass over an empty set")
	}
	checked, argumentShaped := 0, 0
	for _, form := range parser.V1RetiredForms() {
		name := retiredFormToken(form)
		if name == "" {
			continue
		}
		if _, live := functions.Lookup(name); live {
			// The NAME is live and the retirement is ARGUMENT-shaped:
			// `contains(<lambda>)` is the graph traversal and keeps the name,
			// while `contains(s, sub)` retired to `s.includes(sub)`. A name
			// list cannot express that, and the catalog is the authority on
			// which spellings survive -- which is what
			// TestBNFCarriesNoRetiredFunction checks, over
			// functions.RetiredFunctions().
			argumentShaped++
			continue
		}
		checked++
		named := words
		if strings.HasSuffix(form.Spelling, ")") {
			named = calls
		}
		if strings.Contains(named, `"`+name+`"`) {
			t.Errorf("the grammar offers the retired form %q (spelled %q, replaced by %q) as a callable or an operator; "+
				"a retired form in a shipped grammar is worse than an absent one -- it TELLS a model to emit "+
				"something the parser refuses", name, form.Spelling, form.Replacement)
		}
	}
	if checked == 0 {
		t.Fatal("retiredFormToken named nothing in the whole retirement table: the adapter stopped reading it, " +
			"and this gate passes over nothing")
	}
	t.Logf("retired forms checked against the grammar's name lists: %d (%d skipped as argument-shaped)", checked, argumentShaped)
}

// retiredFormToken is the adapter the plan's Step 2 names. A RetiredForm
// carries no production name, so this reads the leading bare word of a
// spelling that is a CALL or an operator word -- "cond(p, a, b)" -> "cond",
// "has" -> "has" -- and "" for a spelling whose retirement is positional
// ("filter <predicate>", "spec <name>"), punctuation (";" as a connective) or
// a placeholder ("$args.x"). A positional retirement cannot be judged from a
// name list, which is what the scoping comment above says.
func retiredFormToken(f parser.RetiredForm) string {
	s := f.Spelling
	// A positional retirement names a construct or clause keyword followed by
	// a placeholder; the word is live in that position's own production.
	if strings.Contains(s, "<") || strings.HasPrefix(s, "@") || strings.HasPrefix(s, "$") ||
		strings.HasPrefix(s, "{") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "?") {
		return ""
	}
	word := s
	if i := strings.IndexAny(word, "( "); i >= 0 {
		word = word[:i]
	}
	for _, r := range word {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') {
			return ""
		}
	}
	return word
}

// TestBNFAnnotationNamesAreTheRegistrys holds every annotation production to
// the registry receiver it was rendered from, in BOTH directions. The renderer
// reads annotations.ByReceiver, so this looks tautological -- and it is not:
// it is the check that catches a renderer which prints a receiver's names
// under the WRONG production, which is the shape a model then writes
// `@templateFile` on a query with.
func TestBNFAnnotationNamesAreTheRegistrys(t *testing.T) {
	productions := bnfProductions(t, BNF())
	check := func(production string, receiver annotations.Receiver) {
		t.Helper()
		rhs, ok := productions[production]
		if !ok {
			t.Errorf("the grammar has no <%s> production, so nothing says which annotations %s accepts",
				production, receiver)
			return
		}
		listed := map[string]bool{}
		for _, m := range regexp.MustCompile(`"@([A-Za-z][A-Za-z0-9_]*)"`).FindAllStringSubmatch(rhs, -1) {
			listed[m[1]] = true
			if _, accepted := annotations.Lookup(receiver, m[1]); !accepted {
				t.Errorf("<%s> offers @%s, which the registry REFUSES on %s", production, m[1], receiver)
			}
		}
		for _, name := range annotations.ByReceiver[string(receiver)] {
			if !listed[name] {
				t.Errorf("<%s> omits @%s, which the registry accepts on %s", production, name, receiver)
			}
		}
	}
	checked := 0
	for _, c := range sortedConstructs(Build()) {
		if len(annotations.ByReceiver[c.AnnotationReceiver]) == 0 {
			continue
		}
		check(c.Keyword+"-annotation", annotations.Receiver(c.AnnotationReceiver))
		checked++
	}
	check("concept-body-annotation", annotations.ConceptBody)
	for _, r := range fieldReceiverNames() {
		check(r.production, annotations.Receiver(r.receiver))
		checked++
	}
	if checked == 0 {
		t.Fatal("no annotation production was checked at all, so this gate passes over nothing")
	}
	t.Logf("annotation productions held to the registry: %d receivers", checked+1)
}

// TestBNFCarriesNoRetiredAnnotation is the half retiredFormToken cannot reach:
// the annotation name lists. It is narrower than it first looks, and the
// narrowing is the registry's own rule: retiredEverywhere means "retired on
// every receiver that does not accept it", so @internal is retired on a query
// and LIVE on a concept field. Only a name no receiver accepts may never
// appear -- and that is the case the grammar could get wrong, by keeping a
// production a receiver dropped.
func TestBNFCarriesNoRetiredAnnotation(t *testing.T) {
	productions := bnfProductions(t, BNF())
	var names strings.Builder
	found := 0
	for name, rhs := range productions {
		if strings.HasSuffix(name, "-annotation") {
			found++
			names.WriteString(rhs)
		}
	}
	if found == 0 {
		t.Fatal("the grammar lists no annotation names at all, so this gate would pass over an empty set")
	}
	accepted := map[string]bool{}
	for _, p := range annotations.Placements() {
		accepted[p.Name] = true
	}
	checked := 0
	for _, r := range annotations.Retirements() {
		if r.Prefix || accepted[r.Name] {
			continue // a family prefix, or a name some receiver still accepts.
		}
		checked++
		if strings.Contains(names.String(), `"@`+r.Name+`"`) {
			t.Errorf("the grammar offers @%s, which no receiver accepts (%s)", r.Name, r.Hint)
		}
	}
	if checked == 0 {
		t.Fatal("every retirement names an annotation some receiver still accepts, so this gate checked nothing; " +
			"either Retirements() stopped reading its tables or Placements() started listing every retired name")
	}
	t.Logf("annotations retired on every receiver, checked absent from the grammar: %d", checked)
}

// TestBNFCarriesNoRetiredFunction closes the same hole from the catalog's
// side: functions.RetiredFunctions and RetiredMethods are the tables Sense and
// the docs read for "what to write instead", and every name in them must be
// absent from the grammar's callable list.
func TestBNFCarriesNoRetiredFunction(t *testing.T) {
	productions := bnfProductions(t, BNF())
	fns, methods := productions["function-name"], productions["method-name"]
	if strings.TrimSpace(fns) == "" || strings.TrimSpace(methods) == "" {
		t.Fatal("the grammar lists no function or method names, so this gate would pass over an empty set")
	}
	for name := range functions.RetiredFunctions() {
		if strings.Contains(fns, `"`+name+`"`) {
			t.Errorf("the grammar offers the retired function %q; write %q instead", name, functions.RetiredFunctions()[name])
		}
	}
	for key, replacement := range functions.RetiredMethods() {
		name := key[strings.Index(key, ".")+1:]
		if strings.Contains(methods, `"`+name+`"`) {
			t.Errorf("the grammar offers the retired method %q; write %q instead", key, replacement)
		}
	}
}

// TestBNFExpressionLadderIsThePrecedenceTable holds the generated expression
// ladder to the operator table's Level column. The ladder is what a
// constrained decoder walks, so a ladder that disagrees with the parser's
// precedence emits text that parses to a DIFFERENT tree than the model meant
// -- the one failure mode a syntax-only grammar can still produce.
func TestBNFExpressionLadderIsThePrecedenceTable(t *testing.T) {
	productions := bnfProductions(t, BNF())
	// A rung may name the production it is (the lambda rung names <lambda>),
	// so the search text is the rung plus every production it directly
	// references. One level of indirection, which is all the ladder uses.
	reach := func(name string) string {
		rhs, ok := productions[name]
		if !ok {
			return ""
		}
		text := rhs
		for _, m := range regexp.MustCompile(`<([a-zA-Z0-9-]+)>`).FindAllStringSubmatch(rhs, -1) {
			if m[1] != name {
				text += " " + productions[m[1]]
			}
		}
		return text
	}
	for _, op := range functions.Operators() {
		name := expressionLevelName(op.Level)
		rhs, ok := reach(name), productions[expressionLevelName(op.Level)] != ""
		if !ok {
			t.Errorf("operator %q (%s) sits at precedence level %d, which the grammar has no <%s> production for",
				op.Symbol, op.Name, op.Level, name)
			continue
		}
		if !strings.Contains(rhs, `"`+op.Symbol+`"`) && !strings.Contains(rhs, `"`+strings.TrimSpace(op.Symbol)+`"`) {
			// "? :" is written as its two halves.
			halves := strings.Fields(op.Symbol)
			all := true
			for _, h := range halves {
				if !strings.Contains(rhs, `"`+h+`"`) {
					all = false
				}
			}
			if !all {
				t.Errorf("<%s> does not offer the operator %q, which the operator table puts at level %d: %s",
					name, op.Symbol, op.Level, rhs)
			}
		}
	}
}

// TestGrammarPageWrapsTheBNF holds the committed page's frame: it must carry
// the front matter docs/ requires, the regeneration instruction, and the BNF
// itself. The root package's TestGrammarPageIsGenerated holds the committed
// FILE to this renderer; this holds the renderer to its own contract.
func TestGrammarPageWrapsTheBNF(t *testing.T) {
	page := RenderGrammar()
	for _, key := range []string{"title:", "audience:", "status:", "area:", "sinceVersion:", "owner:"} {
		if !strings.Contains(page, key) {
			t.Errorf("the grammar page has no %s front-matter key; docs_front_matter_test.go refuses it", key)
		}
	}
	if !strings.Contains(page, "make docs-grammar") {
		t.Error("the grammar page does not name `make docs-grammar`, so a reader who finds it stale has nothing to run")
	}
	if !strings.Contains(page, BNF()) {
		t.Error("the grammar page does not contain the BNF it exists to publish")
	}
	if strings.Contains(page, "```memql\n") {
		t.Error("the grammar page opens a ```memql fence: the page is a grammar, not an example, and a " +
			"fence there would be handed to the parser by the docs-example gate")
	}
}

// TestClauseKeywordsCarryAGrammar is the sibling of the drift test's
// "a clause keyword has no doc" check, for the fact the grammar reads
// (memql#5388). A clause the parser gains with no clauseGrammar entry would
// reach the page as an empty production -- a clause the model is told exists
// and given no way to write.
func TestClauseKeywordsCarryAGrammar(t *testing.T) {
	checked := 0
	for _, k := range Build().Keywords {
		if k.Kind != "clause" {
			continue
		}
		checked++
		if strings.TrimSpace(k.Grammar) == "" {
			t.Errorf("clause keyword %q has no Grammar -- add it to clauseGrammar (lexicon.go); without one the "+
				"generated grammar emits an empty production, which tells a model the clause exists and not how to write it", k.Name)
			continue
		}
		if !strings.Contains(k.Grammar, `"`+k.Name+`"`) {
			t.Errorf("clause keyword %q's Grammar does not open on the clause word: %q", k.Name, k.Grammar)
		}
	}
	if checked == 0 {
		t.Fatal("no clause keyword was checked at all: bodyClauseKeywords stopped producing them, and this gate passes over nothing")
	}
}

// TestStatementKeywordsCarryAGrammar is the same rule for the statement
// language. Heads is what puts a keyword in <statement>; a keyword that heads
// a production and spells none would be offered as an alternative with an
// empty right-hand side.
func TestStatementKeywordsCarryAGrammar(t *testing.T) {
	heads := 0
	for _, k := range Build().Keywords {
		if k.Heads == "" {
			if strings.TrimSpace(k.Grammar) != "" && k.Kind == "control" {
				t.Errorf("control keyword %q carries a Grammar but no Heads, so nothing references its production", k.Name)
			}
			continue
		}
		heads++
		if strings.TrimSpace(k.Grammar) == "" {
			t.Errorf("keyword %q heads a %s production and spells none", k.Name, k.Heads)
		}
		if k.Heads != "statement" && k.Heads != "trailing" {
			t.Errorf("keyword %q's Heads is %q, which no production in the generated grammar collects", k.Name, k.Heads)
		}
	}
	if heads == 0 {
		t.Fatal("no keyword heads a statement production, so <statement> would offer nothing but a binding")
	}
}

// TestSignatureAgreesWithConceptInSignature holds the construct table's two
// signature facts together. ConceptInSignature is the EDITOR's flag ("offer a
// concept right after the keyword"); Signature is the grammar's spelling. They
// answer different questions and are written in different places on the same
// line, which is exactly the pair that drifts.
func TestSignatureAgreesWithConceptInSignature(t *testing.T) {
	for _, c := range Build().Constructs {
		if strings.TrimSpace(c.Signature) == "" {
			t.Errorf("construct %q has no Signature, so the generated grammar cannot spell its declaration", c.Keyword)
			continue
		}
		names := strings.Contains(c.Signature, "<concept-name>")
		if names != c.ConceptInSignature {
			t.Errorf("construct %q: Signature %q %s a concept, but ConceptInSignature is %v",
				c.Keyword, c.Signature, map[bool]string{true: "names", false: "does not name"}[names], c.ConceptInSignature)
		}
		if c.BodyForm == "" {
			t.Errorf("construct %q has no BodyForm, so the generated grammar cannot spell its body", c.Keyword)
		}
	}
}
