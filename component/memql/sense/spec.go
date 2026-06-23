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
// Scope: the spec covers constructs, keywords, operators, field-types, and the
// legal-next rules. It does NOT (yet) model the expression-level builtins
// (ai / coalesce / cond / now / ...), which remain in BuiltinFunctions, nor the
// per-annotation docs, which are projected directly from the annotation
// registry via AnnotationsByReceiver / AnnotationDocs.
var dslSpec = dslspec.Build()

// specConstructItems returns one completion item per author-facing top-level
// construct keyword (concept / query / mutation / logic / automation / spec /
// trait / shape / tool / prompt / provider / builtin / policy / seed / use),
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
