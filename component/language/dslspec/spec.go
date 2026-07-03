// Package dslspec is the single machine-readable source of truth for the
// memQL DSL authoring surface: the top-level constructs an author may
// write, the keywords / operators / field-types the grammar accepts, the
// annotations each construct allows, and the "legal-next" rules that drive
// context-aware completion.
//
// # Why this package exists
//
// Before dslspec, memQL Sense (component/memql/sense) carried its
// completion / hover / diagnose tables as hand-maintained Go literals
// (sense/builtins.go's Keywords / ReceiverTypes / FieldTypes, etc.). They
// drifted from the grammar with nothing to catch it -- e.g. they still
// listed the retired `has` operator, the deprecated `array(T)` field
// type, and the receiver-function model, while omitting the live
// struct-form constructs (logic / trait / policy / seed). This package is
// the durable fix: ONE spec that Sense is generated/driven from (#2123),
// that a CI drift test pins against the parser + annotation registry
// (#2124), and that exports as portable JSON for the cockpit and any
// future web editor (#2125).
//
// Source-of-truth boundaries (deliberate, see the A1 design note on #2122)
//
//   - Annotations + their per-receiver legality are DERIVED from
//     component/language/annotations (the existing #991 registry that
//     already backs both the load-time gate and the editor). dslspec does
//     NOT re-list them; Annotations() inverts that registry so the spec
//     cannot disagree with it.
//   - Constructs, keywords, operators, field-types, and legal-next rules
//     have no pre-existing Go registry -- the truth was split across the
//     parser's top-level dispatch (component/language/parser/parser.go) and
//     the struct-form rewriter (parser/rewriter.go). dslspec is the SoT for
//     those; the drift test (#2124) introspects the parser/rewriter and
//     asserts this spec stays in lockstep, rather than this package
//     importing the (heavy, non-introspectable-by-switch) parser.
//
// The package is a near-leaf: it imports only component/language/annotations
// (itself a leaf), so component/memql/sense and the gRPC/SDK export layer
// can both consume it without an import cycle.
package dslspec

import (
	"sort"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// SpecVersion is the schema version of the serialized spec. Bump it on any
// backward-incompatible change to the JSON shape so a consuming editor can
// detect a contract mismatch. The field/value semantics living inside the
// spec (a new construct, a new annotation) do NOT bump this -- only the
// envelope shape does.
const SpecVersion = "1.0.0"

// Category groups a construct by how it reads, so an editor can present
// constructs in sensible sections and the legal-next rules can refer to a
// whole class.
type Category string

const (
	// CategoryFunction: behavioural constructs the rewriter expands to the
	// internal func form (query / mutation / logic / automation).
	CategoryFunction Category = "function"
	// CategorySchema: the concept declaration (the base of the dependency tree).
	CategorySchema Category = "schema"
	// CategoryPredicate: boolean predicates (spec / trait).
	CategoryPredicate Category = "predicate"
	// CategoryDeclarative: schema-only declarations consumed by the engine
	// (shape / tool / prompt / provider / builtin / policy / seed).
	CategoryDeclarative Category = "declarative"
	// CategoryImport: the `use` cross-file import statement.
	CategoryImport Category = "import"
)

// Construct describes one top-level, author-facing DSL construct -- the
// thing a user starts a declaration with (`mutation`, `concept`, `use`, ...).
type Construct struct {
	// Keyword is the leading contextual keyword the author types.
	Keyword string `json:"keyword"`
	// Category buckets the construct (see Category).
	Category Category `json:"category"`
	// Doc is a one-line description for hover/completion.
	Doc string `json:"doc"`
	// AnnotationReceiver is the key into the annotations registry whose
	// allow-list applies to this construct. Empty string is the concept
	// (top-level) receiver. "" with Keyword=="concept" is intentional; a
	// genuinely un-keyed lookup is impossible because Keyword is required.
	AnnotationReceiver string `json:"annotationReceiver"`
	// RegistryBacked is true when AnnotationReceiver resolves in
	// component/language/annotations.ByReceiver. When false, the construct's
	// annotations are not yet enumerated in the shared registry (policy /
	// seed / trait / relationship today) -- a gap the drift test (#2124)
	// tracks and A2 (#2123) is expected to close by extending the registry.
	RegistryBacked bool `json:"registryBacked"`
	// ConceptInSignature is true for constructs whose signature names a
	// bound concept: `query <Concept> <name>`, `mutation <Concept> <name>`,
	// `seed <Concept> <name>`, and the @row form of `shape <Concept> <name>`.
	// Completion uses this to suggest a concept (or an import) right after
	// the keyword.
	ConceptInSignature bool `json:"conceptInSignature"`
	// BodyBlocks lists the named sub-blocks legal inside this construct's
	// body (e.g. args / filter / shape for a query; insert / update for a
	// mutation; params / auth for a provider). Empty for constructs whose
	// body is a bare field/path/expression list.
	BodyBlocks []string `json:"bodyBlocks,omitempty"`
}

// Annotation is one `@name` directive and the set of construct keywords it
// is legal on, projected from the annotations registry.
type Annotation struct {
	Name string `json:"name"`
	Doc  string `json:"doc"`
	// Receivers are the construct keywords this annotation is legal on
	// (e.g. "query", "mutation"), sorted. Derived from annotations.ByReceiver
	// by mapping each receiver key back to its construct keyword(s).
	Receivers []string `json:"receivers"`
}

// FieldType is a scalar/shape type name valid in a concept field, args
// field, or declarative-construct body field.
type FieldType struct {
	Name string `json:"name"`
	Doc  string `json:"doc"`
	// Deprecated marks a type the grammar still accepts but flags; ReplacedBy
	// names the preferred spelling (e.g. array -> []T).
	Deprecated bool   `json:"deprecated,omitempty"`
	ReplacedBy string `json:"replacedBy,omitempty"`
}

// Keyword is a non-construct reserved word (control flow, clause, or a
// reserved engine identifier) the grammar recognises.
type Keyword struct {
	Name string `json:"name"`
	Doc  string `json:"doc"`
	// Kind buckets the keyword: "control" (if/for/return...), "clause"
	// (filter/shape/args... block headers), "reserved" (now/actor/partition...),
	// or "import" (use).
	Kind string `json:"kind"`
}

// Operator is a boolean/comparison/membership operator in the one-Go-grammar
// filter surface (#971).
type Operator struct {
	Symbol string `json:"symbol"`
	Doc    string `json:"doc"`
}

// NextRule encodes a single "what is legal to type next" hint for
// context-aware completion (#2126). It is intentionally coarse -- a label
// for the cursor context plus the expected categories/literals -- so Sense
// can layer richer AST-driven analysis on top without contradicting the spec.
type NextRule struct {
	// Context is a stable label for where the cursor sits (e.g.
	// "topLevel", "afterMutationKeyword", "afterUseKeyword", "inFunctionBody").
	Context string `json:"context"`
	// Expect names what may legally follow: category tokens like
	// "construct", "concept", "annotation", "fieldType", "importPath", or
	// concrete literals.
	Expect []string `json:"expect"`
	// Doc explains the rule for tooling authors / docs generation.
	Doc string `json:"doc"`
	// SuggestImportWhenMissing, when true, tells completion to offer a
	// `use ...` import if the expected concept is not in file scope -- the
	// owner's "no concept defined yet -> suggest importing one" behaviour.
	SuggestImportWhenMissing bool `json:"suggestImportWhenMissing,omitempty"`
}

// Spec is the whole serializable authoring surface.
type Spec struct {
	Version     string       `json:"version"`
	Constructs  []Construct  `json:"constructs"`
	Annotations []Annotation `json:"annotations"`
	Keywords    []Keyword    `json:"keywords"`
	Operators   []Operator   `json:"operators"`
	FieldTypes  []FieldType  `json:"fieldTypes"`
	NextRules   []NextRule   `json:"nextRules"`
	Builtins    []Builtin    `json:"builtins"`
}

// Build assembles the spec from the construct/keyword/operator/field-type
// tables in this package and the derived annotation projection. It is pure
// and cheap; callers may build on demand (the export layer caches one copy).
func Build() *Spec {
	return &Spec{
		Version:     SpecVersion,
		Constructs:  constructs(),
		Annotations: buildAnnotations(),
		Keywords:    keywords(),
		Operators:   operators(),
		FieldTypes:  fieldTypes(),
		NextRules:   nextRules(),
		Builtins:    builtins(),
	}
}

// ConstructByKeyword returns the construct for a leading keyword, or nil.
func (s *Spec) ConstructByKeyword(keyword string) *Construct {
	for i := range s.Constructs {
		if s.Constructs[i].Keyword == keyword {
			return &s.Constructs[i]
		}
	}
	return nil
}

// buildAnnotations inverts annotations.ByReceiver (receiver -> []name) into
// a per-annotation view (name -> doc + []constructKeyword), so the spec
// stays a strict projection of the #991 registry and cannot disagree with
// it. Receiver keys are mapped to construct keywords via
// receiverKeyToConstructKeywords (e.g. the "Spec" receiver backs both the
// `spec` and `trait` keywords).
func buildAnnotations() []Annotation {
	// name -> set of construct keywords.
	receiverFor := map[string]map[string]bool{}
	for receiverKey, names := range annotations.ByReceiver {
		keywords := receiverKeyToConstructKeywords(receiverKey)
		for _, name := range names {
			set := receiverFor[name]
			if set == nil {
				set = map[string]bool{}
				receiverFor[name] = set
			}
			for _, kw := range keywords {
				set[kw] = true
			}
		}
	}

	out := make([]Annotation, 0, len(receiverFor))
	for name, set := range receiverFor {
		receivers := make([]string, 0, len(set))
		for kw := range set {
			receivers = append(receivers, kw)
		}
		sort.Strings(receivers)
		out = append(out, Annotation{
			Name:      name,
			Doc:       annotations.Docs[name],
			Receivers: receivers,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// receiverKeyToConstructKeywords maps an annotations-registry receiver key
// to the author-facing construct keyword(s) it governs. Most are 1:1; the
// empty key is the concept declaration, and the "Spec" receiver governs
// both `spec` and `trait` (trait shares spec's runtime contract -- it is
// parsed by parseSpecDecl with trait=true).
func receiverKeyToConstructKeywords(receiverKey string) []string {
	switch receiverKey {
	case "":
		return []string{"concept"}
	case "Query":
		return []string{"query"}
	case "Mutation":
		return []string{"mutate"}
	case "Logic":
		return []string{"logic"}
	case "Automation":
		return []string{"automation"}
	case "Action":
		return []string{"action"}
	case "Capability":
		return []string{"capability"}
	case "Spec":
		return []string{"spec", "trait"}
	case "Tool":
		return []string{"tool"}
	case "Builtin":
		return []string{"builtin"}
	case "Prompt":
		return []string{"prompt"}
	case "Provider":
		return []string{"provider"}
	case "Shape":
		return []string{"shape"}
	case "Policy":
		return []string{"policy"}
	case "Seed":
		return []string{"seed"}
	default:
		// Unknown receiver key: surface it under its lowercased self so the
		// drift test notices rather than silently dropping it.
		return []string{receiverKey}
	}
}
