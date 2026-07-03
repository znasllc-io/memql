package sense

import (
	"strings"

	"github.com/znasllc-io/memql/component/language/dslspec"
)

// spec.go projects the single source-of-truth DSL spec
// (component/language/dslspec) into the lookup shapes Sense's completion /
// hover surfaces consume. Driving the vocabulary from the spec -- instead of
// the hand-maintained literals that used to live in builtins.go -- means the
// editor surface can no longer drift from the grammar: the #2124 drift test
// pins the spec to the parser + annotation registry, so a construct / keyword /
// field-type added to the grammar flows here automatically.
//
// Scope: the spec covers constructs, keywords, operators, field-types, the
// legal-next rules, and -- since #2155 -- the expression-level builtins
// (ai / coalesce / cond / now / year / contains / ...), which back
// BuiltinFunctions via specBuiltinFunctions(). The per-annotation docs are
// projected directly from the annotation registry via AnnotationsByReceiver /
// AnnotationDocs.
var dslSpec = dslspec.Build()

// specBuiltinFunctions projects dslspec.Builtins into the map shape Sense's
// completion / hover / signature surfaces consume (name -> BuiltinDef). It is
// the spec-driven replacement for the hand-maintained BuiltinFunctions literal
// that used to live in builtins.go and silently drifted from the grammar. The
// #2155 drift test pins dslspec.Builtins' grammar-recognised subset to
// parser.CallableBuiltins, so this map can no longer fall behind the parser.
func specBuiltinFunctions() map[string]BuiltinDef {
	out := make(map[string]BuiltinDef, len(dslSpec.Builtins))
	for _, b := range dslSpec.Builtins {
		params := make([]Parameter, 0, len(b.Params))
		for _, p := range b.Params {
			params = append(params, Parameter{Label: p.Name, Documentation: p.Doc})
		}
		out[b.Name] = BuiltinDef{
			Signature:  b.Signature,
			Doc:        b.Doc,
			Parameters: params,
		}
	}
	return out
}

// specConstructItems returns one completion item per author-facing top-level
// construct keyword (concept / query / mutate / logic / automation / action /
// capability / spec / trait / shape / tool / prompt / provider / builtin /
// policy / seed / use),
// filtered by prefix. This is the spec-driven replacement for the stale
// hand-coded `func / use / concept` list completeTopLevel used to offer.
func specConstructItems(prefix string) []CompletionItem {
	var items []CompletionItem
	for _, c := range dslSpec.Constructs {
		if !strings.HasPrefix(c.Keyword, prefix) {
			continue
		}
		insert := c.Keyword + " "
		items = append(items, CompletionItem{
			Label:         c.Keyword,
			Kind:          "keyword",
			Detail:        string(c.Category) + " construct",
			Documentation: c.Doc,
			InsertText:    insert,
			SortPriority:  2,
		})
	}
	return items
}

// specConceptSignatureKeywords returns the set of construct keywords whose
// signature binds a concept right after the keyword (`mutate <Concept>
// <name>`, `query ...`, `seed ...`, `shape ...`). Derived from the spec's
// ConceptInSignature flag so it can never drift from the grammar -- the #2124
// drift test pins the spec to the parser/rewriter. Used by context.go to
// detect "cursor sits where a concept is expected" and by complete.go to
// suggest concepts (+ imports) there.
func specConceptSignatureKeywords() map[string]bool {
	out := make(map[string]bool)
	for _, c := range dslSpec.Constructs {
		if c.ConceptInSignature {
			out[c.Keyword] = true
		}
	}
	return out
}

// specNextRule returns the legal-next rule for a context label, or nil. Callers
// read SuggestImportWhenMissing off it rather than hardcoding the behaviour.
func specNextRule(context string) *dslspec.NextRule {
	for i := range dslSpec.NextRules {
		if dslSpec.NextRules[i].Context == context {
			return &dslSpec.NextRules[i]
		}
	}
	return nil
}

// specConstructConceptContextLabel maps a construct keyword to the legal-next
// rule Context label the spec uses for "cursor after that keyword expecting a
// concept" (afterMutationKeyword / afterQueryKeyword / afterSeedKeyword /
// afterShapeKeyword). Returns "" for keywords with no such rule.
func specConstructConceptContextLabel(keyword string) string {
	switch keyword {
	case "mutate":
		return "afterMutationKeyword"
	case "query":
		return "afterQueryKeyword"
	case "seed":
		return "afterSeedKeyword"
	case "shape":
		return "afterShapeKeyword"
	default:
		return ""
	}
}

// specFieldTypeNames returns the field-type names valid in a concept / args /
// declarative body field, excluding deprecated spellings (e.g. `array`, which
// the spec flags in favour of `[]T`) so completion never suggests a form the
// authoring-rules diagnostic will immediately warn about.
func specFieldTypeNames() []string {
	out := make([]string, 0, len(dslSpec.FieldTypes))
	for _, ft := range dslSpec.FieldTypes {
		if ft.Deprecated {
			continue
		}
		out = append(out, ft.Name)
	}
	return out
}

// specKeywordNames returns the construct keywords plus the non-construct
// reserved words (control / clause / reserved / import) the spec enumerates.
// De-duplicated and stable. Used to back the package-level Keywords list.
func specKeywordNames() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, c := range dslSpec.Constructs {
		add(c.Keyword)
	}
	for _, kw := range dslSpec.Keywords {
		add(kw.Name)
	}
	return out
}

// constructKeywords is the set of author-facing top-level construct keywords
// (concept / query / mutate / logic / automation / action / capability / spec /
// trait / shape / tool / prompt / provider / builtin / policy / seed / use),
// sourced from the DSL spec. The Sense tokenizer colors these as `keyword`
// semantic tokens: the LOWERCASE construct keywords lex as plain identifiers in
// the core lexer (it only keywords the capitalized receiver forms), so without
// this they render uncolored. Sourcing the set from dslspec means a construct a
// future grammar epic adds inherits coloring automatically -- the #2124 drift
// test pins the spec to the parser, so the set can never fall behind.
var constructKeywords = specConstructKeywordSet()

// specConstructKeywordSet projects dslSpec.Constructs into a membership set of
// their leading keywords. See constructKeywords for how the tokenizer uses it.
func specConstructKeywordSet() map[string]bool {
	out := make(map[string]bool, len(dslSpec.Constructs))
	for _, c := range dslSpec.Constructs {
		out[c.Keyword] = true
	}
	return out
}

// specKeywordDoc returns the spec's doc for a keyword name (construct or
// reserved word), or "" when the spec does not document it. Callers fall back
// to the local KeywordDocs map for control-flow words the spec does not model.
func specKeywordDoc(name string) string {
	for _, c := range dslSpec.Constructs {
		if c.Keyword == name {
			return c.Doc
		}
	}
	for _, kw := range dslSpec.Keywords {
		if kw.Name == name {
			return kw.Doc
		}
	}
	return ""
}
