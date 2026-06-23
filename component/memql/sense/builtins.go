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
// can no longer fall behind the grammar (it picks up year / quarter / month /
// dayOfMonth / subtractTimestamps / isAnniversary / isFirstDayOfQuarter /
// memqlVersion that the old hand-coded literal omitted). Completion / hover /
// signature consume the same map shape (name -> BuiltinDef) as before.
var BuiltinFunctions = specBuiltinFunctions()

// AnnotationsByReceiver is the editor projection of the single
// annotation registry (component/language/annotations, #991),
// re-exported under its historic name so the sense complete / diagnose
// / hover code keeps referring to a package-local symbol. For the four
// function constructs (Query / Mutation / Logic / Automation) the same
// registry backs the parser-side load gate (constructAnnotationAllowLists),
// so they cannot drift; TestAnnotationReceiverGateConsistency (#991)
// guards that the derived views still agree.
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

// KeywordDocs maps keywords to documentation strings.
var KeywordDocs = map[string]string{
	"func":     "Declare a function with a receiver type: func (ReceiverType) name(args) { ... }",
	"for":      "Loop over a range: for item := range collection { ... }",
	"range":    "Used with for to iterate: for item := range collection",
	"if":       "Conditional execution: if condition { ... } else { ... }",
	"else":     "Alternative branch in an if statement.",
	"switch":   "Multi-way branch: switch value { case X: ... default: ... }",
	"case":     "A branch in a switch statement.",
	"default":  "Default branch in a switch statement.",
	"continue": "Skip to the next iteration of a for loop.",
	"break":    "Exit the current for loop.",
	"return":   "Return a value from a function.",
	"nil":      "The nil value (absence of a value).",
	"retry":    "Retry an expression N times: retry(3) expression",
	"when":     "Guard clause in automation steps.",
	"as":       "Alias in forEach iteration: for item as alias",
	"where":    "Filter in forEach: for item := range collection where condition",
	"use":      "Import a concept: use v1:domain:concept",
	"concept":  "Define a concept schema: concept Name { ... }",
	"in":       "Membership test: value in collection",
	"has":      "Property existence test: object has \"field\"",
	"not":      "Negation: not in, not has",
}

// FieldTypes lists the field types the editor offers in concept / args /
// declarative bodies, projected from the DSL spec (deprecated spellings like
// `array` are excluded so completion never suggests a form the authoring-rules
// diagnostic immediately flags -- migrate to []T).
var FieldTypes = specFieldTypeNames()
