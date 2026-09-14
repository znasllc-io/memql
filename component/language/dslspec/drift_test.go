package dslspec

// drift_test.go is the #2124 CI drift pin. dslspec is the single
// machine-readable source of truth for the MemQL DSL authoring surface
// (constructs / keywords / operators / field-types / legal-next rules)
// plus a strict projection of the annotations registry. Nothing else
// pins that SoT against the LIVE grammar, so Sense could silently fall
// behind the parser. This test is that pin: it FAILS when the live
// grammar (the parser's top-level dispatch + the struct-form rewriter)
// or the annotations registry moves ahead of the spec.
//
// dslspec imports component/language/parser and
// component/language/annotations without an import cycle: the parser does
// NOT import dslspec. Since memql#5359 the construct set, the body clauses
// and the clause keywords are DERIVED from the parser's exported tables
// (parser.ConstructKeywords -- itself parser.TopLevelDeclKeywords,
// parser.StructFormKeywords and `use` -- and parser.BodyClauses), so this
// test pins the hand-authored remainder -- constructCatalog, clauseDocs,
// the next-rules -- to those tables, and asserts the derived projections
// against them by name.
//
// Each assertion names the exact drifted symbol so a future failure is
// self-explaining ("add X to dslspec constructs()" / "remove Y").

import (
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
)

// useImportKeyword is the file-top `use ...` import statement. It is an
// author-facing construct in dslspec but is handled by the parser
// BEFORE parseDefinition (so it is not in TopLevelDeclKeywords) and is
// not a struct-form rewrite stage (so it is not in StructFormKeywords).
// The drift test adds it explicitly to the expected union, which it builds
// on its own rather than reading parser.ConstructKeywords, the table
// dslspec's construct set comes from.
const useImportKeyword = "use"

// specConstructKeywords returns the set of construct keywords dslspec
// currently declares.
func specConstructKeywords() map[string]bool {
	out := map[string]bool{}
	for _, c := range constructs() {
		out[c.Keyword] = true
	}
	return out
}

// TestConstructsMatchParserGrammar is the central drift pin (#2124,
// assertion 1). dslspec's construct keyword set must EXACTLY equal the
// union of the live parser surfaces:
//
//   - parser.StructFormKeywords -- the struct-form rewriter's recognised
//     constructs (query / mutate / logic / automation), derived from
//     the rewriter's own structFormSteps chain.
//   - parser.TopLevelDeclKeywords -- the parser's top-level dispatch
//     keywords (concept / shape / provider / builtin / tool / prompt /
//     policy / spec / trait / seed), derived from the parser's own
//     topLevelDeclParsers dispatch table.
//   - the `use` file-top import.
//
// If the grammar gains or loses a construct, one of those exported
// lists changes and this test fails until dslspec's constructs() is
// updated in lockstep. Conversely, a construct added to dslspec with no
// live grammar backing also fails here.
func TestConstructsMatchParserGrammar(t *testing.T) {
	want := map[string]bool{useImportKeyword: true}
	for _, kw := range parser.StructFormKeywords {
		if want[kw] {
			t.Errorf("parser.StructFormKeywords contains %q which is already in the expected union "+
				"(overlap between rewriter and top-level dispatch -- investigate the parser refactor)", kw)
		}
		want[kw] = true
	}
	for _, kw := range parser.TopLevelDeclKeywords {
		if want[kw] {
			t.Errorf("parser.TopLevelDeclKeywords contains %q which is already in the expected union "+
				"(overlap with StructFormKeywords or `use` -- investigate the parser refactor)", kw)
		}
		want[kw] = true
	}

	got := specConstructKeywords()

	for kw := range want {
		if !got[kw] {
			t.Errorf("DRIFT: live grammar recognises construct %q but dslspec constructs() does not list it -- "+
				"add a Construct{Keyword: %q, ...} entry in component/language/dslspec/constructs.go", kw, kw)
		}
	}
	for kw := range got {
		if !want[kw] {
			t.Errorf("DRIFT: dslspec constructs() lists %q but no live grammar surface recognises it "+
				"(not in parser.StructFormKeywords, parser.TopLevelDeclKeywords, or `use`) -- "+
				"remove it from component/language/dslspec/constructs.go or wire it into the parser", kw)
		}
	}
}

// TestConstructCatalogCoversTheParserExactly: the construct SET is derived
// from the parser, so what can drift is the hand-authored catalog beside it.
// Every keyword the parser recognises must have an entry with a doc and a
// category, and the catalog may not describe a keyword the parser has
// dropped.
func TestConstructCatalogCoversTheParserExactly(t *testing.T) {
	parserSet := map[string]bool{useImportKeyword: true}
	for _, kw := range parser.StructFormKeywords {
		parserSet[kw] = true
	}
	for _, kw := range parser.TopLevelDeclKeywords {
		parserSet[kw] = true
	}
	catalog := map[string]Construct{}
	for _, c := range constructCatalog() {
		if _, dup := catalog[c.Keyword]; dup {
			t.Errorf("constructCatalog lists %q twice", c.Keyword)
		}
		catalog[c.Keyword] = c
	}
	for kw := range parserSet {
		c, ok := catalog[kw]
		if !ok || c.Doc == "" || c.Category == "" {
			t.Errorf("DRIFT: the parser recognises construct %q but constructCatalog "+
				"(component/language/dslspec/constructs.go) has no entry with a doc and a category for it", kw)
		}
	}
	for kw := range catalog {
		if !parserSet[kw] {
			t.Errorf("DRIFT: constructCatalog describes %q, which no parser surface recognises -- delete the entry", kw)
		}
	}
}

// TestBodyBlocksCarryTheFactsTheHandListGotWrong (memql#5359): BodyBlocks is
// projected from parser.BodyClauses, and the parser's own tests pin that table
// to the switch each body parser dispatches on and to what the parsers accept
// and refuse (component/language/parser/body_clauses_test.go). What is checked
// HERE is the published spec: the facts the hand list it replaced got wrong,
// derived by hand rather than read back from the table the spec was built
// from.
func TestBodyBlocksCarryTheFactsTheHandListGotWrong(t *testing.T) {
	has := func(kw, clause string) bool {
		for _, b := range Build().ConstructByKeyword(kw).BodyBlocks {
			if b == clause {
				return true
			}
		}
		return false
	}
	if has("automation", "body") {
		t.Error("an automation has no body block -- its body is step blocks (emitAutomation refuses `body { }`)")
	}
	for _, clause := range []string{"args", "step", "precondition"} {
		if !has("automation", clause) {
			t.Errorf("automation BodyBlocks lack %q", clause)
		}
	}
	for _, clause := range []string{"sort", "paginate", "asOf", "count"} {
		if !has("query", clause) {
			t.Errorf("query BodyBlocks lack %q", clause)
		}
	}
	for _, clause := range []string{"accept", "stamp"} {
		if !has("mutate", clause) {
			t.Errorf("mutation BodyBlocks lack %q", clause)
		}
	}
}

// TestClauseKeywordsMatchTheParserClauseTables: the lexicon's clause keywords
// are exactly the clauses some construct body accepts, each with a doc, and
// clauseDocs describes nothing else.
func TestClauseKeywordsMatchTheParserClauseTables(t *testing.T) {
	want := map[string]bool{}
	for _, c := range constructs() {
		for _, clause := range c.BodyBlocks {
			want[clause] = true
		}
	}
	got := map[string]bool{}
	for _, k := range keywords() {
		if k.Kind != "clause" {
			continue
		}
		got[k.Name] = true
		if k.Doc == "" {
			t.Errorf("clause keyword %q has no doc -- add it to clauseDocs (lexicon.go)", k.Name)
		}
	}
	if !sameStringSet(got, want) {
		t.Errorf("DRIFT: clause keywords = %s, the parser's clause tables name %s", sortedKeys(got), sortedKeys(want))
	}
	for clause := range clauseDocs {
		if !want[clause] {
			t.Errorf("clauseDocs documents %q, which no construct body accepts", clause)
		}
	}
}

// TestFieldAnnotationsFollowTheFieldLists: every field receiver the registry
// has is reached by at least one construct, and each construct's
// FieldAnnotations is the field list its body actually has -- facts derived
// by hand: a concept's fields take @pii, a tool's @autoInjected, a prompt's
// @default, a builtin's exactly @description and @required, and every
// construct with an args block its args-field set; a construct with no field
// list has none.
func TestFieldAnnotationsFollowTheFieldLists(t *testing.T) {
	reached := map[annotations.Receiver]bool{}
	for _, c := range constructs() {
		if r := fieldReceiverFor(c); r != "" {
			reached[r] = true
		}
	}
	for _, r := range []annotations.Receiver{annotations.ConceptField, annotations.ArgsField, annotations.ToolField, annotations.PromptField, annotations.BuiltinField} {
		if !reached[r] {
			t.Errorf("field receiver %s is reached by no construct", r)
		}
	}
	spec := Build()
	carries := func(kw, name string) bool {
		c := spec.ConstructByKeyword(kw)
		if c == nil {
			return false
		}
		for _, a := range c.FieldAnnotations {
			if a == name {
				return true
			}
		}
		return false
	}
	for kw, name := range map[string]string{
		"concept": "pii", "tool": "autoInjected", "prompt": "default",
		"query": "maxLength", "mutate": "required", "logic": "enum", "automation": "pattern",
		"action": "minimum", "capability": "maximum",
	} {
		if !carries(kw, name) {
			t.Errorf("%s fields take @%s, but FieldAnnotations lacks it", kw, name)
		}
	}
	if got := strings.Join(spec.ConstructByKeyword("builtin").FieldAnnotations, ","); got != "description,required" {
		t.Errorf("builtin FieldAnnotations = [%s], want [description,required]", got)
	}
	for _, kw := range []string{"shape", "spec", "trait", "policy", "rule", "seed", "provider", "use"} {
		if c := spec.ConstructByKeyword(kw); c != nil && len(c.FieldAnnotations) != 0 {
			t.Errorf("%s has no field list, but FieldAnnotations = %v", kw, c.FieldAnnotations)
		}
	}
}

// TestTopLevelNextRuleNamesEveryConstruct: the top-level rule's keyword list
// is derived from the construct set; the hand list it replaced never offered
// `rule`.
func TestTopLevelNextRuleNamesEveryConstruct(t *testing.T) {
	var doc string
	for _, r := range nextRules() {
		if r.Context == "topLevel" {
			doc = r.Doc
		}
	}
	if doc == "" {
		t.Fatal("no topLevel next-rule")
	}
	for _, c := range constructs() {
		if !strings.Contains(doc, c.Keyword) {
			t.Errorf("the topLevel next-rule does not name construct %q: %s", c.Keyword, doc)
		}
	}
	// The filter rule teaches the edition-2026 form (memql#5364), in which
	// `!` is an ordinary negation and every field is read through the lambda
	// parameter.
	for _, r := range nextRules() {
		if r.Context == "inFilterClause" && !strings.Contains(r.Doc, "filter row =>") {
			t.Errorf("the inFilterClause rule does not teach the lambda form `filter row => <predicate>`: %s", r.Doc)
		}
	}
}

// TestStructFormConstructsAreFunctionCategory asserts the rewriter's
// recognised keywords are exactly the constructs dslspec buckets as
// CategoryFunction. This pins the categorisation: a construct the
// rewriter expands to the internal func form is, by definition, a
// function-category construct.
func TestStructFormConstructsAreFunctionCategory(t *testing.T) {
	rewriterSet := map[string]bool{}
	for _, kw := range parser.StructFormKeywords {
		rewriterSet[kw] = true
	}

	functionCategory := map[string]bool{}
	for _, c := range constructs() {
		if c.Category == CategoryFunction {
			functionCategory[c.Keyword] = true
		}
	}

	for kw := range rewriterSet {
		if !functionCategory[kw] {
			t.Errorf("DRIFT: parser.StructFormKeywords contains %q but dslspec does not bucket it as "+
				"CategoryFunction -- set Category: CategoryFunction on construct %q", kw, kw)
		}
	}
	for kw := range functionCategory {
		if !rewriterSet[kw] {
			t.Errorf("DRIFT: dslspec marks %q CategoryFunction but the struct-form rewriter "+
				"(parser.StructFormKeywords) does not recognise it -- re-check the category or wire the rewriter", kw)
		}
	}
}

// receiverKeyToConstructKeywords maps an annotations-registry receiver key to
// the author-facing construct keyword(s) it governs, derived from
// constructs(): a construct receiver governs the constructs naming it as
// their AnnotationReceiver (the "Spec" receiver backs both `spec` and
// `trait`); a field receiver governs the constructs whose field list it
// checks; ConceptBody governs the concept. A key no construct names, and no
// construct's field list is checked by, maps to nothing. Only this test
// needs the mapping, so it lives here rather than in the spec.
func receiverKeyToConstructKeywords(receiverKey string) []string {
	set := map[string]bool{}
	if receiverKey == string(annotations.ConceptBody) {
		set["concept"] = true
	}
	for _, c := range constructs() {
		if c.AnnotationReceiver == receiverKey && receiverKey != "" {
			set[c.Keyword] = true
		}
		if string(fieldReceiverFor(c)) == receiverKey && receiverKey != "" {
			set[c.Keyword] = true
		}
	}
	return sortedSet(set)
}

// TestAnnotationsProjectRegistryFromRegistrySide asserts -- from the
// REGISTRY side (#2124 assertion 2) -- that dslspec's Annotations is an
// exact projection of annotations.ByReceiver + annotations.Docs:
//
//   - every annotation name in the registry appears exactly once in the
//     spec, with its registry doc,
//   - and -- the new-receiver guard -- every receiver key in the registry
//     maps (receiverKeyToConstructKeywords, above) to at least one REAL
//     construct keyword dslspec declares. The mapping reads constructs(),
//     so a receiver key no construct names and no construct's field list
//     is checked by maps to nothing, and fails here by name.
//
// The spec_test.go side already checks the projection from the spec
// side; this checks it from the registry side and additionally fails on
// a NEW unmapped receiver key.
func TestAnnotationsProjectRegistryFromRegistrySide(t *testing.T) {
	specConstructs := specConstructKeywords()

	// New-receiver guard: every registry receiver key must map to
	// construct keywords dslspec actually declares. A new key added to
	// annotations.ByReceiver that no construct names as its
	// AnnotationReceiver, and that fieldReceiverFor gives to no construct,
	// maps to nothing -- caught here.
	for receiverKey := range annotations.ByReceiver {
		mapped := receiverKeyToConstructKeywords(receiverKey)
		if len(mapped) == 0 {
			t.Errorf("DRIFT: annotations.ByReceiver receiver key %q maps to no construct keywords -- "+
				"name it as a construct's AnnotationReceiver, or give fieldReceiverFor "+
				"(component/language/dslspec/constructs.go) the construct whose fields it checks", receiverKey)
			continue
		}
		for _, kw := range mapped {
			if !specConstructs[kw] {
				t.Errorf("DRIFT: annotations.ByReceiver receiver key %q maps to %q, which is not a dslspec "+
					"construct keyword -- fix the mapping in receiverKeyToConstructKeywords (drift_test.go) "+
					"or the construct catalog (component/language/dslspec/constructs.go)",
					receiverKey, kw)
			}
		}
	}

	// Build the spec's annotation projection and index it.
	specAnn := map[string]Annotation{}
	for _, a := range buildAnnotations() {
		if _, dup := specAnn[a.Name]; dup {
			t.Errorf("annotation %q projected more than once by dslspec", a.Name)
		}
		specAnn[a.Name] = a
	}

	// Every registry annotation name is present exactly once, with its
	// registry doc.
	registryNames := map[string]bool{}
	for _, names := range annotations.ByReceiver {
		for _, n := range names {
			registryNames[n] = true
		}
	}
	for n := range registryNames {
		a, ok := specAnn[n]
		if !ok {
			t.Errorf("DRIFT: registry annotation %q is missing from dslspec's projection "+
				"(check buildAnnotations / receiverKeyToConstructKeywords)", n)
			continue
		}
		if a.Doc != annotations.Docs[n] {
			t.Errorf("DRIFT: annotation %q doc diverges -- spec=%q registry=%q "+
				"(dslspec must project annotations.Docs verbatim)", n, a.Doc, annotations.Docs[n])
		}
		if len(a.Receivers) == 0 && len(a.Fields) == 0 {
			t.Errorf("annotation %q projects with no receivers and no fields", n)
		}
		for _, r := range append(append([]string(nil), a.Receivers...), a.Fields...) {
			if !specConstructs[r] {
				t.Errorf("DRIFT: annotation %q lists receiver %q which is not a dslspec construct keyword", n, r)
			}
		}
	}

	// Inverse: the spec must not invent annotation names the registry
	// does not carry.
	for n := range specAnn {
		if !registryNames[n] {
			t.Errorf("DRIFT: dslspec projects annotation %q which is not in annotations.ByReceiver "+
				"(buildAnnotations should never invent names)", n)
		}
	}
}

// TestRetiredFormsStayGone (#2124 assertion 3) guards that retired
// authoring forms never reappear in the spec: the `has` membership
// operator (retired in favour of `in`, #971) and `array` as a
// NON-deprecated field type (the bare `array(T)` spelling is retired in
// favour of []T).
//
// NOTE on the parser: `has` is still LEXED (TokenKeywordHas) and `array(T)`
// is still PARSED by the grammar as legacy/back-compat -- their
// retirement is enforced at the AUTHORING level by
// test/dslconformance/no_retired_operators_test.go, not by the parser. So a
// "this snippet must fail to parse" assertion would be FALSE here (the
// parser accepts both). The honest, parser-truthful pin is therefore:
// dslspec -- the authoring SoT -- must not surface `has` as an operator
// and must mark `array` Deprecated. That is what Sense reads, and that
// is where the retirement lives.
func TestRetiredFormsStayGone(t *testing.T) {
	s := Build()

	for _, op := range s.Operators {
		if op.Symbol == "has" {
			t.Error("DRIFT: retired `has` membership operator present in dslspec operators() -- " +
				"membership is `in` only (#971); remove it from component/language/dslspec/lexicon.go")
		}
	}

	// `array` must be present only as a deprecated spelling (or absent).
	for _, ft := range s.FieldTypes {
		if ft.Name == "array" && !ft.Deprecated {
			t.Error("DRIFT: `array` field type is not marked Deprecated in dslspec fieldTypes() -- " +
				"the bare array(T) spelling is retired (migrate to []T); set Deprecated: true, ReplacedBy: \"[]T\"")
		}
	}

	// Parser-truthful cross-check: confirm the grammar STILL lexes `has`
	// as a distinct token (legacy reality) -- if a future change drops
	// the token entirely the comment above must be revisited, but more
	// importantly this documents WHY we do not assert a parse error. The
	// lexer must produce exactly one TokenKeywordHas for the `has` word.
	toks, err := parser.NewLexer("has").Tokenize()
	if err != nil {
		t.Fatalf("lexing `has` failed: %v", err)
	}
	sawHasToken := false
	for _, tok := range toks {
		if tok.Type == parser.TokenKeywordHas {
			sawHasToken = true
		}
	}
	if !sawHasToken {
		t.Log("NOTE: parser no longer lexes `has` as TokenKeywordHas -- the dslspec retirement assertion " +
			"above is now also enforceable as a parse error; consider tightening TestRetiredFormsStayGone")
	}
}

// TestConstructDocsRejectRetiredForms (E2 / memql#2373) pins the CONTENT of
// each construct's Doc + BodyBlocks -- not just the keyword set -- against the
// grammar the parser retired in the construct-invocation epic (#2322). The
// keyword-set drift test (TestConstructsMatchParserGrammar) caught a construct
// appearing/disappearing, but NOT a stale Doc that keeps teaching a shape the
// parser now rejects (the `action` intent/params{}/argTemplate{} body, the
// `action("name@1")` invocation, `mutation <Concept>` as a declaration). Sense
// hover/completion reads these Docs verbatim, so a stale Doc silently offers a
// form the parser rejects. This test is the content pin: it FAILS if a retired
// form reappears in any construct Doc/BodyBlocks, so the next grammar epic
// cannot leave dslspec's prose behind.
func TestConstructDocsRejectRetiredForms(t *testing.T) {
	// Retired authoring forms that must never reappear in a construct's Doc or
	// BodyBlocks. Each names the epic that retired it so a failure self-explains.
	retired := []struct{ needle, why string }{
		{"argTemplate", "action's retired argTemplate{} block -- replaced by the typed `capability <verb>(...)` call (construct-invocation ADR, memql#2322)"},
		{"$params", "action's retired $params.X string interpolation (memql#2322)"},
		{`action("`, `retired versioned action invocation action("name@1") -- an action is invoked 'action <name>(args...)' now (memql#2322/#2328)`},
		{"func (", "retired procedural receiver-function form -- the struct form is the only author surface"},
		{"mutation <", "`mutation` is the invocation-step prefix; the declaration keyword is `mutate` (rewriter.go mutationStructHeader, memql#2041)"},
	}
	for _, c := range Build().Constructs {
		hay := c.Doc + "\x00" + strings.Join(c.BodyBlocks, "\x00")
		for _, r := range retired {
			if strings.Contains(hay, r.needle) {
				t.Errorf("DRIFT: construct %q Doc/BodyBlocks contains retired form %q -- %s", c.Keyword, r.needle, r.why)
			}
		}
	}

	// The `action` construct: its body is `args { }` + a single capability
	// call (construct-invocation ADR Decision 3), so BodyBlocks is exactly
	// {args} -- the retired params/argTemplate blocks must be gone -- and the
	// Doc must describe the capability call.
	if a := Build().ConstructByKeyword("action"); a != nil {
		if len(a.BodyBlocks) != 1 || a.BodyBlocks[0] != "args" {
			t.Errorf("DRIFT: action BodyBlocks = %v, want [args] (params/argTemplate retired, memql#2322)", a.BodyBlocks)
		}
		if !strings.Contains(a.Doc, "capability") {
			t.Errorf("DRIFT: action Doc must describe the single `capability <verb>(...)` call; got %q", a.Doc)
		}
	} else {
		t.Error("DRIFT: dslspec is missing the `action` construct")
	}

	// THE DIRECTION REVERSED. memql#2041 made `mutate` the declaration
	// keyword and left `mutation` as the invocation-step prefix; epic
	// memql#5375 collapsed the pair onto `mutation` in both positions,
	// because one construct answering to two words meant a reader grepping
	// for either found part of the tree.
	if Build().ConstructByKeyword("mutate") != nil {
		t.Error("DRIFT: dslspec still lists a `mutate` construct -- the keyword is `mutation` in both the declaration and the call since memql#5375")
	}
	if Build().ConstructByKeyword("mutation") == nil {
		t.Error("DRIFT: dslspec is missing the `mutation` construct (the write-function declaration keyword)")
	}
}

// TestRegistryBackedHonesty (#2124 assertion 4) asserts that any
// construct dslspec marks RegistryBacked=true actually resolves in
// annotations.ByReceiver, and emits a CLEAR signal for the known gaps
// (policy / seed are not yet enumerated in the registry) so closing
// them is tracked.
//
// Design decision (documented, non-failing for the known gaps): the
// known RegistryBacked=false gaps (policy, seed) are reported via
// t.Log -- they are a tracked, intentional state (A2/#2123 is expected
// to close them by extending the registry), so failing CI on them would
// block unrelated work. But this test DOES fail if:
//   - a RegistryBacked=true construct's receiver is absent from the
//     registry (a lie), or
//   - the set of RegistryBacked=false constructs changes from the known
//     {policy, seed, use} gap set (so closing OR widening the gap is
//     forced through this test and the issue tracker).
func TestRegistryBackedHonesty(t *testing.T) {
	// The known, tracked RegistryBacked=false constructs. After #2151 added
	// the Policy + Seed receivers to annotations.ByReceiver, the only
	// remaining unbacked construct is `use` -- the file-top import statement,
	// which carries no annotations at all.
	knownUnbacked := map[string]bool{"use": true}

	gotUnbacked := map[string]bool{}
	for _, c := range constructs() {
		_, inRegistry := annotations.ByReceiver[c.AnnotationReceiver]

		if c.RegistryBacked && !inRegistry {
			t.Errorf("DRIFT: construct %q claims RegistryBacked=true but receiver %q is NOT in "+
				"annotations.ByReceiver -- either wire the receiver into the registry or set "+
				"RegistryBacked=false on the construct in component/language/dslspec/constructs.go",
				c.Keyword, c.AnnotationReceiver)
		}

		if !c.RegistryBacked {
			gotUnbacked[c.Keyword] = true
		}
	}

	// Report the tracked gaps clearly (non-failing) so closing them is visible.
	gaps := []string{}
	for kw := range gotUnbacked {
		if kw != useImportKeyword {
			gaps = append(gaps, kw)
		}
	}
	sort.Strings(gaps)
	if len(gaps) > 0 {
		t.Logf("KNOWN REGISTRY GAPS (RegistryBacked=false, tracked for A2/#2123): %s -- "+
			"these constructs' annotations are not yet enumerated in annotations.ByReceiver", strings.Join(gaps, ", "))
	}

	// Fail if the unbacked set drifts from the known gap set, so closing
	// a gap (or introducing a new one) is forced through this test.
	if !sameStringSet(gotUnbacked, knownUnbacked) {
		t.Errorf("DRIFT: the set of RegistryBacked=false constructs changed.\n  got:  %s\n  want: %s\n"+
			"If you CLOSED a gap (added the receiver to annotations.ByReceiver and flipped RegistryBacked=true), "+
			"update knownUnbacked in this test. If you ADDED a construct, decide its RegistryBacked state and "+
			"update knownUnbacked + the #2124 tracking.",
			sortedKeys(gotUnbacked), sortedKeys(knownUnbacked))
	}
}

// ---------------------------------------------------------------------------
// #2155 / memql#5365: expression-builtins drift pin.
//
// dslspec.Builtins is the SoT Sense's builtin surface is driven from. Since
// memql#5365 its expression builtins (CategoryBuiltinExpr) are PROJECTED from
// the v1 function catalog (component/language/functions) -- the one list of
// functions a v1 expression calls -- so this pin now holds three tables
// together, where it used to hold two:
//
//   - dslspec's expression builtins ARE the catalog's functions, name,
//     signature and doc (TestBuiltinsProjectTheCatalog);
//   - every name the parser special-cases as a builtin (parser.CallableBuiltins,
//     derived from parser/callable.go's callableParsers) is a catalog function
//     or a retired spelling the catalog names a replacement for
//     (functions.RetiredFunctions) -- so a builtin the parser still accepts
//     cannot drop out of the editor without the "write this instead" line
//     (TestParserBuiltinsAreCatalogFunctionsOrRetired);
//   - every catalog function is a name the parser recognises in some category,
//     or one of the configuration lookups resolved from a generic call at run
//     time (TestCatalogFunctionsAreParserRecognised).
//
// The pre-v1 parser still accepts the retired spellings (concat / coalesce /
// cond / first / last) until the tree is migrated, which is why the second
// assertion is "catalog OR retired" rather than "catalog". When the parser
// stops recognising them they leave parser.CallableBuiltins and that branch
// simply stops being taken -- no assertion here has to change.
// ---------------------------------------------------------------------------

// runtimeResolvedFunctions are the catalog functions the parser has no rule
// for: a generic call the mutation-template evaluator resolves by name at run
// time. They are the configuration lookups, and they are the ONLY catalog
// functions allowed to be unknown to the parser -- a new catalog function the
// parser does not recognise fails TestCatalogFunctionsAreParserRecognised until
// it is wired into the grammar or listed here with its reason.
var runtimeResolvedFunctions = map[string]string{
	"systemVar":    "configuration lookup resolved by the mutation-template evaluator",
	"secret":       "configuration lookup resolved by the mutation-template evaluator",
	"systemSecret": "configuration lookup resolved by the mutation-template evaluator",
}

// catalogFunctions returns the catalog's functions (not its methods) keyed by
// their one spelling.
func catalogFunctions() map[string]functions.Function {
	out := map[string]functions.Function{}
	for _, f := range functions.Catalog() {
		if f.Receiver == "" {
			out[f.Name] = f
		}
	}
	return out
}

// lowered returns the keys of m lower-cased: the parser's callable tables are
// keyed by the LOWERCASED dispatch name (shortid), the catalog by the author
// spelling (shortId).
func lowered[V any](m map[string]V) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[strings.ToLower(k)] = true
	}
	return out
}

// builtinAllowList documents every parser-recognised callable name that is
// DELIBERATELY not a dslspec expression builtin (CategoryBuiltinExpr), with the
// reason. Adding a new recognised name to the parser forces a decision: model
// it in the catalog, name it in RetiredFunctions, or add it here with a reason.
//
// Categories (all keyed by the parser's LOWERCASED dispatch name):
//
//   - Accessors (parser.CallableAccessors): item / event / step / input /
//     field / var / actor / error. Context accessors; var and error are ALSO
//     catalog functions, which TestBuiltinAccessorsAreParserAccessors accounts
//     for.
//   - Keyword-functions: case / default -- control-flow-shaped, modelled
//     nowhere as a plain builtin.
//   - Query directives: paginate / sort / select / asof / withdepth / count /
//     shape -- they wrap an inner expression / produce specialised AST.
//   - Relationship wrappers: parentof / childof / aliasof / equals /
//     references / owns / createdby / ids -- produce a RelationshipExpr (and
//     are catalog functions too).
//   - Retired: caller / now / timestamp and the 2026.08 set -- recognised
//     only to emit a migration hint.
func builtinAllowList() map[string]string {
	allow := map[string]string{}
	for _, n := range parser.CallableAccessors {
		allow[n] = "accessor (parser.CallableAccessors)"
	}
	for _, n := range parser.CallableKeywordFuncs {
		allow[n] = "keyword-function (case/default) -- control-flow-shaped, not a plain builtin"
	}
	for _, n := range parser.CallableDirectives {
		allow[n] = "query directive -- wraps an inner expression / specialised AST"
	}
	for _, n := range parser.CallableRelationshipWrappers {
		allow[n] = "relationship wrapper -- produces a RelationshipExpr"
	}
	for _, n := range parser.CallableRetiredNames {
		allow[n] = "retired -- recognised only to emit a migration hint"
	}
	return allow
}

// TestBuiltinsProjectTheCatalog: dslspec's expression builtins are EXACTLY the
// catalog's functions, in both directions, and carry the catalog's signature
// and doc verbatim -- dslspec restates nothing about a function.
func TestBuiltinsProjectTheCatalog(t *testing.T) {
	catalog := catalogFunctions()
	if len(catalog) == 0 {
		t.Fatal("the catalog has no functions: this test examined nothing, which is not a pass")
	}
	spec := map[string]Builtin{}
	for _, b := range builtins() {
		if b.Category != CategoryBuiltinExpr {
			continue
		}
		if _, dup := spec[b.Name]; dup {
			t.Errorf("dslspec lists expression builtin %q twice", b.Name)
		}
		spec[b.Name] = b
	}

	for name, f := range catalog {
		b, ok := spec[name]
		if !ok {
			t.Errorf("DRIFT: catalog function %q has no CategoryBuiltinExpr entry in dslspec.Builtins -- "+
				"the projection in component/language/dslspec/builtins.go dropped it", name)
			continue
		}
		if b.Signature != f.Signature() {
			t.Errorf("DRIFT: %s signature = %q, the catalog's is %q (dslspec must project it verbatim)", name, b.Signature, f.Signature())
		}
		if b.Doc != f.Doc {
			t.Errorf("DRIFT: %s doc diverges from the catalog's (dslspec must project it verbatim)", name)
		}
		if b.Tier != string(f.Tier) {
			t.Errorf("DRIFT: %s tier = %q, the catalog's is %q", name, b.Tier, f.Tier)
		}
		if len(b.Params) != len(f.Params) {
			t.Errorf("DRIFT: %s has %d params, the catalog's has %d", name, len(b.Params), len(f.Params))
		}
	}
	for name := range spec {
		if _, ok := catalog[name]; !ok {
			t.Errorf("DRIFT: dslspec marks %q CategoryBuiltinExpr but the catalog has no such function -- "+
				"an expression builtin is a catalog entry (component/language/functions) or it is not one", name)
		}
	}
}

// TestParserBuiltinsAreCatalogFunctionsOrRetired: every name the parser
// special-cases as a builtin is a catalog function or a retired spelling with a
// named replacement. The retired set is what makes a spelling like concat(...)
// legitimately absent from the editor while the parser still accepts it: the
// hover says what to write instead, rather than the name silently vanishing.
func TestParserBuiltinsAreCatalogFunctionsOrRetired(t *testing.T) {
	catalog := lowered(catalogFunctions())
	retired := lowered(functions.RetiredFunctions())
	if len(parser.CallableBuiltins) == 0 {
		t.Fatal("parser.CallableBuiltins is empty: this test examined nothing, which is not a pass")
	}
	for _, n := range parser.CallableBuiltins {
		if !catalog[n] && !retired[n] {
			t.Errorf("DRIFT: parser.CallableBuiltins recognises %q, which is neither a catalog function nor a "+
				"retired spelling with a replacement -- add it to component/language/functions "+
				"(an entry in catalog.go, or a RetiredFunctions line naming what to write instead)", n)
		}
	}
}

// TestCatalogFunctionsAreParserRecognised is the reverse direction: the
// catalog invents no function the grammar cannot parse. A catalog function is a
// parser builtin, accessor or relationship wrapper, or one of the documented
// run-time lookups.
func TestCatalogFunctionsAreParserRecognised(t *testing.T) {
	recognised := map[string]bool{}
	for _, set := range [][]string{parser.CallableBuiltins, parser.CallableAccessors, parser.CallableRelationshipWrappers} {
		for _, n := range set {
			recognised[n] = true
		}
	}
	for name := range catalogFunctions() {
		if recognised[strings.ToLower(name)] {
			continue
		}
		if _, ok := runtimeResolvedFunctions[name]; ok {
			continue
		}
		t.Errorf("DRIFT: catalog function %q is not recognised by the parser (not a builtin, accessor or "+
			"relationship wrapper) and is not a documented run-time lookup -- wire it into "+
			"component/language/parser/callable.go or add it to runtimeResolvedFunctions with its reason", name)
	}
	// The exception list may not outlive its reason: every name on it must still
	// be a catalog function the parser does not recognise.
	catalog := catalogFunctions()
	for name := range runtimeResolvedFunctions {
		if _, ok := catalog[name]; !ok {
			t.Errorf("runtimeResolvedFunctions names %q, which is not a catalog function", name)
		}
		if recognised[strings.ToLower(name)] {
			t.Errorf("runtimeResolvedFunctions names %q, which the parser now recognises -- drop it from the list", name)
		}
	}
}

// TestParserRecognisedCallablesAreAllClassified is the cross-check over the
// parser's FULL recognised callable set (builtins + accessors + keyword-funcs +
// directives + wrappers + retired): every name is allow-listed with a reason,
// a dslspec expression builtin, or a retired spelling. A NEW recognised name
// cannot slip past all three.
func TestParserRecognisedCallablesAreAllClassified(t *testing.T) {
	allow := builtinAllowList()
	specExpr := map[string]bool{}
	for _, b := range builtins() {
		if b.Category == CategoryBuiltinExpr {
			specExpr[strings.ToLower(b.Name)] = true
		}
	}
	retired := lowered(functions.RetiredFunctions())
	recognised := map[string]bool{}
	for _, n := range parser.CallableBuiltins {
		recognised[n] = true
	}
	for n := range allow {
		recognised[n] = true
	}
	for n := range recognised {
		if allow[n] != "" || specExpr[n] || retired[n] {
			continue
		}
		t.Errorf("DRIFT: parser recognises callable %q which is neither a dslspec expression builtin, a "+
			"retired spelling, nor in builtinAllowList() -- decide its category", n)
	}
}

// TestBuiltinAccessorsAreParserAccessors asserts every parser accessor (plus the
// documented `index` exception) is modelled in dslspec -- as a
// CategoryBuiltinAccessor entry, or as the catalog function it also is (var and
// error: a v1 expression calls them as functions) -- and that dslspec's
// accessors are all parser accessors. `index` is special-cased in
// parseFunctionCall (the no-arg form is the loop accessor, index(arr,i) reads an
// array element) so it is NOT in callableParsers / CallableAccessors, but Sense
// still offers it as a call -- hence the explicit exception.
func TestBuiltinAccessorsAreParserAccessors(t *testing.T) {
	const indexException = "index"

	parserAcc := map[string]bool{indexException: true}
	for _, n := range parser.CallableAccessors {
		parserAcc[n] = true
	}

	specAcc := map[string]bool{}
	for _, b := range builtins() {
		if b.Category == CategoryBuiltinAccessor {
			specAcc[strings.ToLower(b.Name)] = true
		}
	}
	catalog := lowered(catalogFunctions())

	for n := range parserAcc {
		if !specAcc[n] && !catalog[n] {
			t.Errorf("DRIFT: parser recognises accessor %q (or the index exception) but dslspec models it "+
				"neither as a CategoryBuiltinAccessor entry nor as a catalog function -- add it in "+
				"component/language/dslspec/builtins.go", n)
		}
		// One entry per name: an accessor the catalog also lists is the
		// catalog's, and a second accessor entry would describe it twice.
		if specAcc[n] && catalog[n] {
			t.Errorf("%q is both a CategoryBuiltinAccessor entry and a catalog function -- keep the catalog's", n)
		}
	}
	for n := range specAcc {
		if !parserAcc[n] {
			t.Errorf("DRIFT: dslspec.Builtins marks %q CategoryBuiltinAccessor but the parser does not "+
				"recognise it as an accessor (not in parser.CallableAccessors, and not the index "+
				"exception) -- re-check the category or wire callable.go", n)
		}
	}
}

// TestOperatorsProjectTheTable: dslspec's operators are the operator table
// (functions.Operators) row for row, so Sense, the grammar generator and the
// JSON export read the same prose.
func TestOperatorsProjectTheTable(t *testing.T) {
	table := functions.Operators()
	got := operators()
	if len(got) != len(table) {
		t.Fatalf("dslspec has %d operators, the table has %d", len(got), len(table))
	}
	for i, op := range table {
		want := Operator{
			Symbol: op.Symbol, Name: op.Name, Kind: op.Kind, Form: op.Form,
			Doc: op.Doc, Absence: op.Absence, Level: op.Level,
		}
		if got[i] != want {
			t.Errorf("operator %d = %+v, the table's is %+v", i, got[i], want)
		}
	}
}

// TestBuiltinRegistryNamesNotParserSpecialCased asserts dslspec's
// CategoryBuiltinRegistry entries (ai / node / similar / ...) are NOT
// special-cased by the parser -- they are resolved at runtime from the
// integration / builtin registry and must fall through parseFunctionCall to a
// generic FunctionCallExpr. If one ever gains a dedicated parser rule it should
// be re-categorised, which this test forces.
func TestBuiltinRegistryNamesNotParserSpecialCased(t *testing.T) {
	parserNames := map[string]bool{}
	for _, n := range parser.CallableBuiltins {
		parserNames[n] = true
	}
	for _, n := range parser.CallableAccessors {
		parserNames[n] = true
	}
	for _, n := range parser.CallableKeywordFuncs {
		parserNames[n] = true
	}
	for _, n := range parser.CallableDirectives {
		parserNames[n] = true
	}

	for _, b := range builtins() {
		if b.Category != CategoryBuiltinRegistry {
			continue
		}
		if parserNames[strings.ToLower(b.Name)] {
			t.Errorf("DRIFT: dslspec.Builtins marks %q CategoryBuiltinRegistry but the parser DOES "+
				"special-case it -- re-categorise it (CategoryBuiltinExpr / CategoryBuiltinAccessor)", b.Name)
		}
	}
}

// TestBuiltinsCoverGapFilledNames documents the specific staleness #2155
// closed: the old hand-coded sense literal omitted these parser-known builtins.
// This asserts dslspec now models every one (belt-and-suspenders over the
// name-set equality test, with a self-explaining name list).
func TestBuiltinsCoverGapFilledNames(t *testing.T) {
	// The other #2155 gap-filled names (year / quarter / month / dayOfMonth /
	// subtractTimestamps / isAnniversary / isFirstDayOfQuarter / memqlVersion)
	// were hard-retired under the 2026.08 epoch (#2620 ruling / #2707), so
	// only contains remains modellable.
	gapFilled := []string{
		"contains",
	}
	present := map[string]bool{}
	for _, b := range builtins() {
		present[b.Name] = true
	}
	for _, n := range gapFilled {
		if !present[n] {
			t.Errorf("DRIFT: gap-filled builtin %q (parser-known, omitted by the old sense literal) "+
				"is missing from dslspec.Builtins", n)
		}
	}
}

func sameStringSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
