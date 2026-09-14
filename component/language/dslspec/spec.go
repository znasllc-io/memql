// Package dslspec is the single machine-readable source of truth for the
// MemQL DSL authoring surface: the top-level constructs an author may
// write, the keywords / operators / field-types the grammar accepts, the
// annotations each construct allows, and the "legal-next" rules that drive
// context-aware completion.
//
// # Why this package exists
//
// Before dslspec, MemQL Sense (component/memql/sense) carried its
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
//     component/language/annotations, the registry every parser checks
//     annotations against (memql#5359). dslspec does NOT re-list them;
//     Annotations() inverts that registry so the spec cannot disagree with
//     it.
//   - The construct set, each construct's body clauses and the clause
//     keywords are DERIVED from the parser (parser.StructFormKeywords,
//     parser.TopLevelDeclKeywords, parser.BodyClauses; memql#5359).
//     Operators, field types, legal-next rules and each construct's doc
//     have no Go registry and are authored here; the drift test (#2124)
//     holds them against the parser.
//   - The spec names the language it describes: Edition and GrammarVersion
//     come from the parser (memql#5362), so a client reading the export
//     knows which grammar it was built from.
//
// The package imports component/language/annotations (a leaf) and
// component/language/parser, which does not import dslspec, so
// component/memql/sense and the gRPC/SDK export layer can both consume it
// without an import cycle.
package dslspec

import (
	"sort"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/parser"
)

// SpecVersion is the schema version of the serialized spec, so a consuming
// editor can detect a contract mismatch. The MAJOR moves on a
// backward-incompatible change to the JSON envelope; the MINOR on an additive
// one (1.1.0: `edition` and `grammarVersion`, memql#5362). The field/value
// semantics living inside the spec (a new construct, a new annotation) do NOT
// move it -- only the envelope shape does.
const SpecVersion = "1.1.0"

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
	// allow-list applies to this construct's leading annotations ("Concept"
	// for the concept -- it was "" before memql#5359). Empty for `use`, which
	// carries no annotations.
	AnnotationReceiver string `json:"annotationReceiver"`
	// RegistryBacked is true when AnnotationReceiver resolves in
	// component/language/annotations.ByReceiver. #2151 closed the
	// policy / seed / trait gaps this comment used to name; today only
	// `use` is unbacked (it is the file-top import statement, not an
	// annotatable construct), pinned by the drift test's knownUnbacked
	// set. An unbacked construct falls back to the union of every
	// receiver's annotations rather than offering nothing (#2627).
	RegistryBacked bool `json:"registryBacked"`
	// ConceptInSignature is true for constructs whose signature names a
	// bound concept: `query <Concept> <name>`, `mutation <Concept> <name>`,
	// `seed <Concept> <name>`, and the @row form of `shape <Concept> <name>`.
	// Completion uses this to suggest a concept (or an import) right after
	// the keyword.
	ConceptInSignature bool `json:"conceptInSignature"`
	// BodyBlocks lists the clauses legal inside this construct's body, in
	// authoring order -- blocks (`args { }`, `insert { }`, `step x { }`) and
	// line clauses (`filter <expr>`, `paginate 25`) alike, which
	// parser.IsLineClause tells apart. Derived from parser.BodyClauses
	// (memql#5359). Empty for constructs whose body is a bare
	// field/path/expression list.
	BodyBlocks []string `json:"bodyBlocks,omitempty"`
	// FieldAnnotations lists the annotations legal on this construct's
	// fields -- a concept's fields, the fields of an args block, or the field
	// list that is a tool / prompt / builtin body -- projected from the
	// registry's field receiver for the construct (memql#5359). Empty for a
	// construct with no field list.
	FieldAnnotations []string `json:"fieldAnnotations,omitempty"`
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
	// Fields are the construct keywords whose FIELDS accept this annotation
	// (e.g. "concept" for @pii, "query" for an args field's @required),
	// sorted. Derived from the registry's field receivers (memql#5359).
	Fields []string `json:"fields,omitempty"`
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
	// Properties is the structured member table for Kind=="reserved"
	// identifiers that expose a closed dotted-path set (#2623: actor is
	// the first). Empty for every other keyword; additive in the JSON
	// envelope, so no SpecVersion bump.
	Properties []KeywordProperty `json:"properties,omitempty"`
}

// KeywordProperty is one member of a reserved identifier's closed
// dotted-path set (e.g. actor.userId). AliasOf marks a legacy alias of
// a canonical member. The engine-side source of truth is
// auth.ActorEnvelopeFields; a two-directional drift test pins the two
// tables together.
type KeywordProperty struct {
	Name    string `json:"name"`
	Doc     string `json:"doc"`
	AliasOf string `json:"aliasOf,omitempty"`
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
	Version string `json:"version"`
	// Edition and GrammarVersion name the language this spec describes
	// (parser.Edition, parser.GrammarVersion) -- the same two facts
	// ServerHello states on connect (memql#5362, D25), so a client that is
	// not the VS Code extension can compare its own grammar with a cluster's
	// from the DslSpec export alone.
	Edition        string `json:"edition"`
	GrammarVersion string `json:"grammarVersion"`

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
		Version:        SpecVersion,
		Edition:        parser.Edition,
		GrammarVersion: parser.GrammarVersion,

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

// buildAnnotations inverts the registry (receiver -> []name) into a
// per-annotation view (name -> doc + the constructs it is legal on + the
// constructs whose fields accept it), so the spec stays a strict projection
// of component/language/annotations and cannot disagree with it. The receiver
// of each construct is the construct's own AnnotationReceiver and field
// receiver (constructs.go), so the mapping has one source.
func buildAnnotations() []Annotation {
	leading := map[string]map[string]bool{} // name -> construct keywords
	fields := map[string]map[string]bool{}  // name -> constructs whose fields take it
	add := func(into map[string]map[string]bool, name, keyword string) {
		if into[name] == nil {
			into[name] = map[string]bool{}
		}
		into[name][keyword] = true
	}
	for _, c := range constructs() {
		for _, name := range annotations.ByReceiver[c.AnnotationReceiver] {
			add(leading, name, c.Keyword)
		}
		for _, name := range c.FieldAnnotations {
			add(fields, name, c.Keyword)
		}
	}
	// @relationship is written inside the concept's body, not before it.
	for _, name := range annotations.ByReceiver[string(annotations.ConceptBody)] {
		add(leading, name, "concept")
	}

	names := map[string]bool{}
	for n := range leading {
		names[n] = true
	}
	for n := range fields {
		names[n] = true
	}
	out := make([]Annotation, 0, len(names))
	for name := range names {
		out = append(out, Annotation{
			Name:      name,
			Doc:       annotations.Docs[name],
			Receivers: sortedSet(leading[name]),
			Fields:    sortedSet(fields[name]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sortedSet returns the set's members sorted; an empty set is an empty (not
// nil) slice, so a JSON consumer reads `"receivers": []`.
func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
