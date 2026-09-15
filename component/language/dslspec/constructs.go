package dslspec

import (
	"sort"

	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/parser"
)

// constructs returns the author-facing top-level construct table -- the SoT
// for "what can a .memql declaration start with".
//
// DERIVED from the parser wherever the parser can say it (memql#5359):
//   - the SET of constructs is parser.ConstructKeywords, the one table of
//     words that open a top-level statement -- the struct-form rewriter's
//     family (query / mutation / logic / automation), the parser's top-level
//     dispatch, and the `use` import -- which the parser's refusal of any
//     other word and the load gate construct_unknown read too (memql#5356);
//   - each construct's BodyBlocks are parser.BodyClauses -- the clauses the
//     rewriter and the construct parsers accept, pinned to them by the
//     parser's own tests;
//   - FieldAnnotations and RegistryBacked are the annotation registry's.
//
// Hand-authored, in constructCatalog, is only what the parser cannot say:
// each keyword's category, doc, annotation receiver and whether its
// signature binds a concept. A keyword the parser gains with no catalog
// entry, or an entry the parser no longer recognises, fails the drift test.
func constructs() []Construct {
	catalog := map[string]Construct{}
	for _, c := range constructCatalog() {
		catalog[c.Keyword] = c
	}
	keywords := constructKeywords()
	out := make([]Construct, 0, len(keywords))
	for _, kw := range keywords {
		c := catalog[kw]
		c.Keyword = kw
		c.BodyBlocks = parser.BodyClauses(kw)
		_, c.RegistryBacked = annotations.ByReceiver[c.AnnotationReceiver]
		if r := fieldReceiverFor(c); r != "" {
			c.FieldAnnotations = annotations.ByReceiver[string(r)]
		}
		out = append(out, c)
	}
	return out
}

// useKeyword is the file-top import statement: a word that opens a top-level
// statement (it is in parser.ConstructKeywords) but declares nothing, so the
// declaration keywords leave it out (declarationKeywords).
const useKeyword = "use"

// constructKeywords is the parser's construct set (parser.ConstructKeywords),
// in the order constructCatalog lists them; a keyword the catalog does not
// know is appended in sorted order (and fails the drift test for its missing
// entry).
func constructKeywords() []string {
	set := map[string]bool{}
	for _, kw := range parser.ConstructKeywords() {
		set[kw] = true
	}
	var out []string
	for _, c := range constructCatalog() {
		if set[c.Keyword] {
			out = append(out, c.Keyword)
			delete(set, c.Keyword)
		}
	}
	rest := make([]string, 0, len(set))
	for kw := range set {
		rest = append(rest, kw)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// fieldReceiverFor names the registry receiver that checks the construct's
// fields: the concept's fields, the field list that is a tool / prompt /
// builtin body, or -- for every construct with an `args` block -- the args
// field. Empty for a construct with no field list.
func fieldReceiverFor(c Construct) annotations.Receiver {
	switch c.Keyword {
	case "concept":
		return annotations.ConceptField
	case "tool":
		return annotations.ToolField
	case "prompt":
		return annotations.PromptField
	case "builtin":
		return annotations.BuiltinField
	}
	for _, b := range c.BodyBlocks {
		if b == "args" {
			return annotations.ArgsField
		}
	}
	return ""
}

// BodyFieldReceiver names the registry receiver of the fields a construct's
// body IS -- a concept's, or the field list that is a tool / prompt / builtin
// body -- and "" for a construct whose body is clauses (an args block's
// fields sit one block down, on annotations.ArgsField). The editor reads it to
// offer, at an `@` after a field's type, what that field takes.
func BodyFieldReceiver(keyword string) annotations.Receiver {
	for _, c := range constructs() {
		if c.Keyword == keyword {
			if r := fieldReceiverFor(c); r != annotations.ArgsField {
				return r
			}
		}
	}
	return ""
}

// constructCatalog is the hand-authored part of the construct table, in
// display order: the category, doc, annotation receiver and signature shape
// of each construct keyword. BodyBlocks, FieldAnnotations and RegistryBacked
// are filled by constructs() from the parser and the registry.
func constructCatalog() []Construct {
	return []Construct{
		{
			Keyword:            "concept",
			Category:           CategorySchema,
			Doc:                "Define a node schema (the base of the dependency tree). Body is a field list; cross-concept links via @relationship.",
			AnnotationReceiver: string(annotations.Concept),
			ConceptInSignature: false,
		},
		{
			Keyword:            "query",
			Category:           CategoryFunction,
			Doc:                "Read function: stitch a bound concept + filter (specs) + projection (shape) + args into a typed read. Struct form `query <Concept> <name>`.",
			AnnotationReceiver: "Query",
			ConceptInSignature: true,
		},
		{
			Keyword:            "mutation",
			Category:           CategoryFunction,
			Doc:                "Write function on a bound concept: declared `mutation <Concept> <name>` with exactly one insert{} OR update{} block, and called `mutation <name>(...)` -- the one word declares and calls (D13).",
			AnnotationReceiver: "Mutation",
			ConceptInSignature: true,
		},
		{
			Keyword:            "logic",
			Category:           CategoryFunction,
			Doc:                "A procedure that decides. `args { }` declares its inputs; its statements follow in the order they run (`x := <kind> name(...)`, if, for, switch, parallel) and end with `return <expr>`. It calls queries, mutations, logic and builtins; publishing, calling an automation and dispatching an action are an automation's (D14).",
			AnnotationReceiver: "Logic",
			ConceptInSignature: false,
		},
		{
			Keyword:            "automation",
			Category:           CategoryFunction,
			Doc:                "Event- or schedule-triggered side-effect (via @trigger). Its statements run in the order written and call every construct kind; the triggering event's payload is bound into its `args { }` block.",
			AnnotationReceiver: "Automation",
			ConceptInSignature: false,
		},
		{
			Keyword:            "action",
			Category:           CategoryDeclarative,
			Doc:                "Authored external-side-effect primitive (behavioral-constructs ADR §2.3, construct-invocation ADR Decision 3): performs exactly ONE external capability (shell.* / fs.* / http.* / integration.* / mcp.*) on a surface and never touches the graph. Body is an `args { }` schema plus a SINGLE `capability <verb>(...)` call -- no body{}, no return; the @sideEffect class lives on the capability, not the action. Invoked from an automation as `action <name>(args...)`; replays token-free (fingerprint-verified) on identical input.",
			AnnotationReceiver: "Action",
			ConceptInSignature: false,
		},
		{
			Keyword:            "capability",
			Category:           CategoryDeclarative,
			Doc:                "Surface-backed external capability verb (construct-invocation ADR Decision 4): declared like a typed, side-effect-classified builtin with NO body. Namespaced/dotted name (fs.* / shell.* / http.* / integration.* / mcp.*). @sideEffect(\"read\"|\"write\"|\"exec\") -- the UNSPOOFABLE risk class -- lives HERE, not on the action that invokes it (ADR §7). Imported at the verb level (`use capabilities.<ns>.{ verb }`) and called via `capability verb(args)`. Body: an optional args{} input schema.",
			AnnotationReceiver: "Capability",
			ConceptInSignature: false,
		},
		{
			Keyword:            "spec",
			Category:           CategoryPredicate,
			Doc:                "Atomic boolean predicate over one bound concept or shape: `spec <bound> <name> = row => <predicate>`, applied as `name(row)`. Over an @actor shape the parameter is `actor` and the predicate evaluates in process; over a row it pushes down to SQL.",
			AnnotationReceiver: "Spec",
			ConceptInSignature: false,
		},
		{
			Keyword:            "trait",
			Category:           CategoryPredicate,
			Doc:                "Concept-agnostic boolean predicate (same runtime contract as spec): `trait <name> = row => <predicate>`, applied as `name(row)` to a row of any concept.",
			AnnotationReceiver: "Spec",
			ConceptInSignature: false,
		},
		{
			Keyword:            "shape",
			Category:           CategoryDeclarative,
			Doc:                "Reusable field projection. @row projects a concept payload/intrinsics (signature `shape <Concept> <name>`); @actor projects the auth envelope; both = mixed. Body is a path list; there is no composition verb.",
			AnnotationReceiver: "Shape",
			ConceptInSignature: true,
		},
		{
			Keyword:            "tool",
			Category:           CategoryDeclarative,
			Doc:                "AI-callable tool definition. Body is the input-schema field list; @handler wires it to a query/function.",
			AnnotationReceiver: "Tool",
			ConceptInSignature: false,
		},
		{
			Keyword:            "prompt",
			Category:           CategoryDeclarative,
			Doc:                "AI prompt template with an input schema and a default provider. Body is a bare input-schema field list; @templateFile points at the .tmpl.",
			AnnotationReceiver: "Prompt",
			ConceptInSignature: false,
		},
		{
			Keyword:            "provider",
			Category:           CategoryDeclarative,
			Doc:                "AI provider configuration (vendor + model + auth). @base providers carry auth+type; children @extends a base. Body: params{} / auth{}.",
			AnnotationReceiver: "Provider",
			ConceptInSignature: false,
		},
		{
			Keyword:            "builtin",
			Category:           CategoryDeclarative,
			Doc:                "Go-backed executor exposed as a DSL function. Body is the input schema; @executor names the integration.X.Y implementation.",
			AnnotationReceiver: "Builtin",
			ConceptInSignature: false,
		},
		{
			Keyword:            "policy",
			Category:           CategoryDeclarative,
			Doc:                "AI provider-selection record (empty body): an ordered chain of @primary / @fallback entries -- a provider name, a fleet: / app: / federation: selector, or policy:<name> -- consumed by the AI Router. (Caller-context checks use specs, not policies.)",
			AnnotationReceiver: "Policy",
			ConceptInSignature: false,
		},
		{
			Keyword:            "rule",
			Category:           CategoryDeclarative,
			Doc:                "Routing rule (empty body): maps a call's declared metadata to a policy. @when(...) states the closed condition set, @policy names the chain, @level overrides the call's level, @precedence orders the set (highest first) and @onUnavailable says whether an exhausted chain degrades or parks.",
			AnnotationReceiver: "Rule",
			ConceptInSignature: false,
		},
		{
			Keyword:            "seed",
			Category:           CategoryDeclarative,
			Doc:                "Seed an initial row for a bound concept. Struct form `seed <Concept> <name>`.",
			AnnotationReceiver: "Seed",
			ConceptInSignature: true,
		},
		{
			Keyword:            "use",
			Category:           CategoryImport,
			Doc:                "File-top cross-file import: `use <domain>.<construct>.{ a, b }` pulls named constructs (concepts/shapes/specs/...) into local scope.",
			AnnotationReceiver: "",
			ConceptInSignature: false,
		},
	}
}
