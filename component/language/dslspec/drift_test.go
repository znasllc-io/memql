package dslspec

// drift_test.go is the #2124 CI drift pin. dslspec is the single
// machine-readable source of truth for the memQL DSL authoring surface
// (constructs / keywords / operators / field-types / legal-next rules)
// plus a strict projection of the annotations registry. Nothing else
// pins that SoT against the LIVE grammar, so Sense could silently fall
// behind the parser. This test is that pin: it FAILS when the live
// grammar (the parser's top-level dispatch + the struct-form rewriter)
// or the annotations registry moves ahead of the spec.
//
// A _test.go file may import component/language/parser and
// component/language/annotations without a production import cycle:
// dslspec's PRODUCTION code imports only annotations (a leaf), and the
// parser does NOT import dslspec. The exported parser vars
// (parser.TopLevelDeclKeywords, parser.StructFormKeywords) are the
// authoritative, introspectable lists the live switch/rewriter
// reference -- this test compares the spec against THEM, not against a
// hand-copied literal.
//
// Each assertion names the exact drifted symbol so a future failure is
// self-explaining ("add X to dslspec constructs()" / "remove Y").

import (
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/parser"
)

// useImportKeyword is the file-top `use ...` import statement. It is an
// author-facing construct in dslspec but is handled by the parser
// BEFORE parseDefinition (so it is not in TopLevelDeclKeywords) and is
// not a struct-form rewrite stage (so it is not in StructFormKeywords).
// The drift test adds it explicitly to the expected union.
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
//     constructs (query / mutation / logic / automation), derived from
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

// TestAnnotationsProjectRegistryFromRegistrySide asserts -- from the
// REGISTRY side (#2124 assertion 2) -- that dslspec's Annotations is an
// exact projection of annotations.ByReceiver + annotations.Docs:
//
//   - every annotation name in the registry appears exactly once in the
//     spec, with its registry doc,
//   - every receiver key in the registry maps (via
//     receiverKeyToConstructKeywords) to at least one REAL construct
//     keyword dslspec declares,
//   - and -- the new-receiver guard -- no receiver key falls through
//     receiverKeyToConstructKeywords' default branch (which would surface
//     the raw key as a fake "construct").
//
// The spec_test.go side already checks the projection from the spec
// side; this checks it from the registry side and additionally fails on
// a NEW unmapped receiver key.
func TestAnnotationsProjectRegistryFromRegistrySide(t *testing.T) {
	specConstructs := specConstructKeywords()

	// New-receiver guard: every registry receiver key must map to
	// construct keywords dslspec actually declares. A new key added to
	// annotations.ByReceiver that receiverKeyToConstructKeywords does not
	// handle falls through to its default branch (returns the raw key),
	// which is NOT a construct keyword -- caught here.
	for receiverKey := range annotations.ByReceiver {
		mapped := receiverKeyToConstructKeywords(receiverKey)
		if len(mapped) == 0 {
			t.Errorf("DRIFT: annotations.ByReceiver receiver key %q maps to no construct keywords", receiverKey)
			continue
		}
		for _, kw := range mapped {
			if !specConstructs[kw] {
				t.Errorf("DRIFT: annotations.ByReceiver has a NEW receiver key %q that "+
					"receiverKeyToConstructKeywords maps to %q, which is not a dslspec construct keyword -- "+
					"add a case for %q in receiverKeyToConstructKeywords "+
					"(component/language/dslspec/spec.go) mapping it to its construct keyword(s)",
					receiverKey, kw, receiverKey)
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
		if len(a.Receivers) == 0 {
			t.Errorf("annotation %q projects with no receivers", n)
		}
		for _, r := range a.Receivers {
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
// dsl/no_retired_operators_test.go, not by the parser. So a
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
