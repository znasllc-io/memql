package sense

import "github.com/znasllc-io/memql/component/language/annotations"

// BuiltinDef describes a built-in function's signature for completion and hover.
type BuiltinDef struct {
	Signature  string
	Doc        string
	Parameters []Parameter
}

// ReceiverTypes lists all valid receiver types for func declarations.
var ReceiverTypes = []string{
	"Query", "Mutation", "Automation", "Spec", "Tool", "Builtin", "Prompt", "Provider", "Shape",
}

// Keywords lists the MemQL keywords the editor recognises, projected from the
// DSL spec (component/language/dslspec) -- the author-facing construct keywords
// plus the reserved control/clause/import words. Sourcing it from the spec
// drops the stale entries the old literal carried (the retired `has` membership
// operator; `func`, the internal rewriter target rather than an author word)
// and picks up the struct-form constructs it omitted (logic / trait / policy /
// seed). The lexer-driven highlighter in tokenize.go keys off parser token
// types, not this list, so highlighting is unaffected.
var Keywords = specKeywordNames()

// BuiltinFunctions maps builtin function names to their signatures and
// documentation, DRIVEN from the single source of truth dslspec.Builtins
// (#2155) rather than a hand-maintained literal. The expression-builtin subset
// is pinned to parser.CallableBuiltins by the dslspec drift test, so this map
// can no longer fall behind the grammar -- which also means the eight
// builtins hard-retired under 2026.08 (#2620 ruling / #2707: year / quarter
// / month / dayOfMonth / subtractTimestamps / isAnniversary /
// isFirstDayOfQuarter / memqlVersion) dropped out of completion / hover the
// moment their dslspec rows were deleted. Completion / hover / signature
// consume the same map shape (name -> BuiltinDef) as before.
var BuiltinFunctions = specBuiltinFunctions()

// AnnotationsByReceiver is the editor projection of the single
// annotation registry (component/language/annotations, #991),
// re-exported under its historic name so the sense complete / diagnose
// / hover code keeps referring to a package-local symbol. The same
// registry is the parser's one annotation gate for every receiver
// (memql#5359), so the editor and the gate cannot drift;
// component/memql's TestEveryReceiverGateReadsTheRegistry drives the gates
// themselves.
var AnnotationsByReceiver = annotations.ByReceiver

// AnnotationDocs maps annotation names to documentation strings. Note that a
// few names are deliberately per-construct overloaded (resolved by which
// construct they sit on): `@type` is a provider vendor type AND a concept row
// kind ("object" / "collection" / "reference"); `@default` is a provider
// default-flag AND an args/concept field default. The doc below states the
// common meaning.
// AnnotationDocs is the per-annotation hover/completion doc map, re-
// exported from the single annotation registry
// (component/language/annotations, #991) under its historic name.
var AnnotationDocs = annotations.Docs

// KeywordDocs maps keywords to their hover / completion documentation: the
// DSL spec's lexicon (dslspec) for every word it documents -- the one source
// of the statement language's docs (epic memql#5370), so hover never shows a
// second, older description of a word -- and the entries below for the few
// words the spec does not model. The retired forms stay purged so hover never
// teaches grammar the parser rejects (E2 / memql#2373): the procedural
// `func (Receiver)` receiver form, the `has` membership operator (retired for
// `in`, #971), the `use v1:domain:concept` import, and the retired body forms
// (`for x := range`, the forEach step's `as` and `where`, `continue` and
// `break`, which no body has).
var KeywordDocs = keywordDocs()

func keywordDocs() map[string]string {
	out := map[string]string{
		"nil":     "The nil value (absence of a value).",
		"concept": "Define a concept schema: concept Name { ... }",
		"not":     "Retired in edition 2026: `x not in list` is written `!(x in list)`. memqlmigrate --rewrite=expressions rewrites it.",
		"as":      "Retired in edition 2026 with the forEach step: a loop names its variable in `for <x> in <source>`. memqlmigrate --rewrite=bodies rewrites it.",
		"where":   "Retired in edition 2026 with the forEach step: a loop filters with `for <x> in <source> if <cond>`. memqlmigrate --rewrite=bodies rewrites it.",
	}
	for _, kw := range dslSpec.Keywords {
		out[kw.Name] = kw.Doc
	}
	return out
}

// FieldTypes lists the field types the editor offers in concept / args /
// declarative bodies, projected from the DSL spec (deprecated spellings like
// `array` are excluded so completion never suggests a form the authoring-rules
// diagnostic immediately flags -- migrate to []T).
var FieldTypes = specFieldTypeNames()
