package dslspec

import (
	"sort"

	"github.com/znasllc-io/memql/component/language/functions"
)

// builtins.go is the single source of truth for the builtins Sense and the JSON
// export describe: the expression functions an author calls (lower / hash /
// childOf / ...), the context accessors (item / event / step / ...), and the
// runtime-registered builtins resolved from the integration/builtin registry
// (ai / node / similar / embed / ...).
//
// Before #2155 this metadata lived only in a hand-maintained map in
// component/memql/sense/builtins.go (BuiltinFunctions), the one Sense surface
// with no drift guard -- and it had already gone stale against the grammar.
// This table is the durable fix: Sense is DRIVEN from dslspec.Builtins
// (sense/builtins.go projects it), and the drift test (drift_test.go) pins it.
//
// Since memql#5365 the expression functions are not written here at all: they
// are PROJECTED from the v1 function catalog (component/language/functions),
// the one list of functions a v1 expression calls, with the catalog's
// signature, doc, tier and parameters. A spelling the catalog retires (concat /
// coalesce / cond / first / last) is therefore no longer an editor builtin; the
// catalog's RetiredFunctions names what to write instead, and the drift test
// holds every name the parser still accepts to "catalog function or retired".
//
// Categorisation (BuiltinCategory) is load-bearing -- the drift test treats the
// three categories differently:
//
//   - CategoryBuiltinExpr: a catalog function (component/language/functions),
//     projected verbatim. The drift test pins the projection to the catalog, and
//     the catalog to the parser's callable tables.
//   - CategoryBuiltinAccessor: a context accessor the parser recognises
//     (parser.CallableAccessors) that is not a catalog function -- event /
//     field / actor. var and error are accessors to the parser as well, but a
//     v1 expression calls them as the catalog functions they are, so the
//     catalog's entry is their only one.
//   - CategoryBuiltinRegistry: a builtin resolved at runtime from the
//     integration / builtin registry, NOT special-cased by the parser (it falls
//     through parseFunctionCall to a generic FunctionCallExpr): ai / node /
//     children / parent / payload / similar / embed. The drift test asserts the
//     parser has no rule for them.

// BuiltinCategory buckets a builtin by how the grammar treats its name, so the
// drift test can pin the parser-recognised expression builtins exactly while
// allowing the accessors and runtime-registry builtins as documented surface.
type BuiltinCategory string

const (
	// CategoryBuiltinExpr is a function of the v1 function catalog
	// (component/language/functions), projected from it.
	CategoryBuiltinExpr BuiltinCategory = "expr"
	// CategoryBuiltinAccessor is a parser-recognised context accessor
	// (parser.CallableAccessors) that is not a catalog function.
	CategoryBuiltinAccessor BuiltinCategory = "accessor"
	// CategoryBuiltinRegistry is a runtime-registry builtin not special-cased
	// by the parser grammar.
	CategoryBuiltinRegistry BuiltinCategory = "registry"
)

// BuiltinDependency is the second, orthogonal axis on a builtin (the first is
// BuiltinCategory): whether the name is ambient language (free to call) or an
// imported dependency that a body must declare via a `use` import. This is the
// machine-readable form of the purity line decided in the core-builtins ADR
// (docs/internal/design/core-builtins-and-collections-adr.md, epic #2298): a
// call that always returns the same value given identical explicit arguments,
// observing nothing outside them, is ambient; a call that reads the clock /
// environment / randomness is imported from `core` so the `use` block reveals
// every source of nondeterminism.
//
// This axis is set in Story 2 (#2300) as the definitive classification; the
// loader-level enforcement (a load error when an imported builtin is used
// without its `use core.{ ... }` import) and the `use core` resolution land in
// Story 3 (#2301). Sense uses it to offer an imported builtin in completion
// only when the file has the import.
type BuiltinDependency string

const (
	// DependencyAmbient is the zero value: the name is ambient language and
	// needs no import. Every pure operator, every reserved engine identifier
	// (bare now / actor / partition / config), and -- on this axis -- the
	// runtime-registry builtins (whose own resolution is mediated by the
	// integration registry, a separate mechanism) are ambient.
	DependencyAmbient BuiltinDependency = ""
	// DependencyCore marks a nondeterministic language-level primitive that
	// resolves only when imported via a core import. Membership is currently
	// EMPTY: the clock primitive collapsed to the ambient reserved `now`
	// (timestamp() is an alias, not a distinct read -- owner ruling 2026-06-29,
	// recorded in the core-builtins ADR), and globalVariable / env were already
	// a query / provider-scoped. DependencyCore is the forward-looking home for
	// a genuinely-new nondeterministic primitive (uuid / random) if one is
	// introduced.
	DependencyCore BuiltinDependency = "core"
)

// BuiltinParam is one positional/variadic/optional parameter of a builtin, for
// signature help and hover.
type BuiltinParam struct {
	// Name is the parameter label shown in signature help.
	Name string `json:"name"`
	// Doc explains the parameter.
	Doc string `json:"doc"`
	// Type is the catalog type word of a catalog function's parameter
	// ("string", "lambda", ...); empty for the hand-authored entries.
	Type string `json:"type,omitempty"`
	// Optional marks a parameter a call may leave out (a traversal's leading
	// `as` label).
	Optional bool `json:"optional,omitempty"`
}

// Builtin is one expression-level builtin / accessor / registry function in the
// authoring surface.
type Builtin struct {
	// Name is the author-facing spelling (shortId / canonicalId / asOf are
	// mixed-case in source even though the parser lower-cases for dispatch).
	Name string `json:"name"`
	// Category buckets the builtin (see BuiltinCategory).
	Category BuiltinCategory `json:"category"`
	// Signature is the one-line call signature for hover/completion detail,
	// e.g. `lower(value string) string`.
	Signature string `json:"signature"`
	// Doc is the prose documentation for hover.
	Doc string `json:"doc"`
	// Params lists the parameters for signature help. Empty for the nullary
	// accessors (event() / actor()).
	Params []BuiltinParam `json:"params,omitempty"`
	// Dependency is the ambient-vs-imported classification (see
	// BuiltinDependency). Zero value (DependencyAmbient) means ambient -- the
	// common case; only the nondeterministic core primitives set it.
	Dependency BuiltinDependency `json:"dependency,omitempty"`
	// Tier is a catalog function's tier: "P" when it pushes down to SQL, "M"
	// when it runs in process. Empty for the accessors and registry builtins.
	Tier string `json:"tier,omitempty"`
}

// CoreBuiltinNames is the sorted set of builtin names classified
// DependencyCore -- the membership of the forward-looking `core` import
// namespace. It is currently EMPTY (see DependencyCore): the clock primitive
// collapsed to the ambient reserved `now` rather than an imported timestamp().
// The accessor stays available for a genuinely-new nondeterministic primitive
// (uuid / random); when one lands it is flagged DependencyCore here and the
// intrinsic (loader-level) `core` namespace is wired to resolve it.
func CoreBuiltinNames() []string {
	out := make([]string, 0)
	for _, b := range builtins() {
		if b.Dependency == DependencyCore {
			out = append(out, b.Name)
		}
	}
	sort.Strings(out)
	return out
}

// builtins returns the full editor-callable builtin table: the catalog's
// functions, projected (CategoryBuiltinExpr), then the hand-authored accessors
// and registry builtins the catalog does not describe.
func builtins() []Builtin {
	return append(catalogBuiltins(), handAuthoredBuiltins()...)
}

// catalogBuiltins projects every catalog FUNCTION (not method -- a method is
// called on a value, and the builtin table is keyed by bare name) into a
// CategoryBuiltinExpr entry. Nothing is restated: the name, signature, doc,
// tier and parameters are the catalog's.
func catalogBuiltins() []Builtin {
	var out []Builtin
	for _, f := range functions.Catalog() {
		if f.Receiver != "" {
			continue
		}
		params := make([]BuiltinParam, 0, len(f.Params))
		for _, p := range f.Params {
			params = append(params, BuiltinParam{Name: p.Name, Type: p.Type, Optional: p.Optional})
		}
		out = append(out, Builtin{
			Name:      f.Name,
			Category:  CategoryBuiltinExpr,
			Signature: f.Signature(),
			Doc:       f.Doc,
			Params:    params,
			Tier:      string(f.Tier),
		})
	}
	return out
}

// handAuthoredBuiltins are the builtins the catalog does not describe: the
// parser's context accessors and the runtime-registry builtins.
func handAuthoredBuiltins() []Builtin {
	return []Builtin{
		// ============================================================
		// Context accessors (parser.CallableAccessors).
		// actor / partition / config / trace are ALSO reserved keywords
		// (keywords()); they are listed here too because they are callable
		// like functions and Sense offers them in call completion.
		//
		// `now` is NOT here: the clock is the bare reserved identifier `now`
		// (a keyword, see keywords()), and the now() / timestamp() call-forms
		// are retired (epic #2298 / #2301 -- CallableRetired in the parser).
		// ============================================================
		{
			Name:      "event",
			Category:  CategoryBuiltinAccessor,
			Signature: `event()`,
			Doc:       "Access the trigger event data in an automation.",
		},
		{
			Name:      "field",
			Category:  CategoryBuiltinAccessor,
			Signature: `field(name string)`,
			Doc:       "Reference a field accessor in the current evaluation context.",
			Params:    []BuiltinParam{{Name: "name", Doc: "Field name to access."}},
		},
		{
			Name:      "actor",
			Category:  CategoryBuiltinAccessor,
			Signature: `actor()`,
			Doc:       "Access the resolved auth context (userId, role, identityId, isClusterOwner, partitions).",
		},

		// ============================================================
		// Runtime-registry builtins (NOT special-cased by the parser; resolved
		// from the integration / builtin registry at runtime). Not pinned to
		// the parser by the drift test.
		// ============================================================
		{
			// THE PROVIDER ARGUMENT IS GONE, and this entry is why the removal
			// had to be deliberate: for as long as `ai()` had no executor, this
			// published signature was what authors and models were told the
			// call is -- a third argument naming a provider included.
			//
			// A level plus the routing rules decide which model serves a call
			// (epic memql#5127). A provider named at a call site is a release
			// every time the fleet changes, and a routing decision written
			// where no rule can see it and no decision record explains it. The
			// pin that survives is @defaultProvider ON THE PROMPT, read at load
			// and refused when it names a policy -- and TestNoPaidDefault
			// refuses it for a federated provider, which is every concrete
			// provider record this repository ships.
			//
			// `ai` is a DSL-declared BUILTIN (dsl/agents/builtins.memql), so it
			// is written with its kind and its arguments by name:
			// `builtin ai(templateId: "docSummary", data: { content: ... })`.
			// It is not a catalog function and must not become one -- see
			// component/language/tiers/ai_builtin_position_test.go for what a
			// catalogued `ai` would thereby be Admitted to do.
			Name:      "ai",
			Category:  CategoryBuiltinRegistry,
			Signature: `ai(templateId string, data object)`,
			Doc: "Call a named prompt with a data object and return {prompt, reply}. Written as a builtin call, " +
				"`builtin ai(templateId: ..., data: ...)`, in a logic or automation body. The prompt's @level and the " +
				"routing rules choose the model, so the call never names one.",
			Params: []BuiltinParam{
				{Name: "templateId", Doc: "Name of the prompt to call."},
				{Name: "data", Doc: "Object whose fields the prompt's body declares; `{}` when it declares none."},
			},
		},
		{
			Name:      "node",
			Category:  CategoryBuiltinRegistry,
			Signature: `node(field string)`,
			Doc:       "Access a field from the current node in a shape template.",
			Params:    []BuiltinParam{{Name: "field", Doc: "Dot-separated field path (e.g., \"payload.name\")."}},
		},
		{
			Name:      "children",
			Category:  CategoryBuiltinRegistry,
			Signature: `children(concept string)`,
			Doc:       "Retrieve child nodes of the current node for a given concept.",
			Params:    []BuiltinParam{{Name: "concept", Doc: "Child concept name (e.g., \"v1:cognition:participant\")."}},
		},
		{
			Name:      "parent",
			Category:  CategoryBuiltinRegistry,
			Signature: `parent(concept string)`,
			Doc:       "Retrieve the parent node for a given concept.",
			Params:    []BuiltinParam{{Name: "concept", Doc: "Parent concept name."}},
		},
		{
			Name:      "payload",
			Category:  CategoryBuiltinRegistry,
			Signature: `payload(field string)`,
			Doc:       "Access a field from the event payload.",
			Params:    []BuiltinParam{{Name: "field", Doc: "Dot-separated field path."}},
		},
		{
			Name:      "similar",
			Category:  CategoryBuiltinRegistry,
			Signature: `similar(text string, concept string, field string, topK? int, minScore? float)`,
			Doc:       "Find semantically similar nodes using vector search (pgvector).",
			Params: []BuiltinParam{
				{Name: "text", Doc: "Query text to find similar content for."},
				{Name: "concept", Doc: "Concept to search within."},
				{Name: "field", Doc: "Vector field name (e.g., \"content\")."},
				{Name: "topK", Doc: "Maximum number of results (default: 10)."},
				{Name: "minScore", Doc: "Minimum similarity score 0-1 (default: 0.5)."},
			},
		},
		{
			Name:      "embed",
			Category:  CategoryBuiltinRegistry,
			Signature: `embed(text string, model? string)`,
			Doc:       "Generate an embedding vector for text using the configured provider.",
			Params: []BuiltinParam{
				{Name: "text", Doc: "Text to embed."},
				{Name: "model", Doc: "Optional model override (default: text-embedding-3-small)."},
			},
		},
	}
}
