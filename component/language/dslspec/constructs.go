package dslspec

// constructs returns the author-facing top-level construct table -- the SoT
// for "what can a .memql declaration start with". It corrects the staleness
// that accumulated in sense/builtins.go (which still modelled the retired
// receiver-function surface and omitted logic/trait/policy/seed).
//
// Authority cross-reference (asserted by the #2124 drift test):
//   - query / mutation / logic / automation are recognised by the struct-form
//     rewriter (component/language/parser/rewriter.go ~line 355) before the
//     raw parser sees them as the internal func form.
//   - concept / shape / provider / builtin / tool / prompt / policy / spec /
//     trait / seed are dispatched directly by the parser's top-level switch
//     (component/language/parser/parser.go ~line 442).
//   - use is the file-top import statement.
//
// RegistryBacked reflects whether component/language/annotations.ByReceiver
// enumerates the construct's annotations today. Every annotation-bearing
// construct is now registry-backed (policy + seed were added to the registry
// in #2151); only `use` stays RegistryBacked=false, because it is the file-top
// import statement and carries no annotations at all.
func constructs() []Construct {
	return []Construct{
		{
			Keyword:            "concept",
			Category:           CategorySchema,
			Doc:                "Define a node schema (the base of the dependency tree). Body is a field list; cross-concept links via @relationship.",
			AnnotationReceiver: "",
			RegistryBacked:     true,
			ConceptInSignature: false,
		},
		{
			Keyword:            "query",
			Category:           CategoryFunction,
			Doc:                "Read function: stitch a bound concept + filter (specs) + projection (shape) + args into a typed read. Struct form `query <Concept> <name>`.",
			AnnotationReceiver: "Query",
			RegistryBacked:     true,
			ConceptInSignature: true,
			BodyBlocks:         []string{"args", "filter", "shape"},
		},
		{
			Keyword:            "mutation",
			Category:           CategoryFunction,
			Doc:                "Write function on a bound concept: exactly one insert{} OR update{} block. Struct form `mutation <Concept> <name>`.",
			AnnotationReceiver: "Mutation",
			RegistryBacked:     true,
			ConceptInSignature: true,
			BodyBlocks:         []string{"args", "insert", "update"},
		},
		{
			Keyword:            "logic",
			Category:           CategoryFunction,
			Doc:                "Imperative procedure called from an automation step. `args { }` declares inputs; `body { }` is named statements ending in `return <expr>`.",
			AnnotationReceiver: "Logic",
			RegistryBacked:     true,
			ConceptInSignature: false,
			BodyBlocks:         []string{"args", "body"},
		},
		{
			Keyword:            "automation",
			Category:           CategoryFunction,
			Doc:                "Event- or schedule-triggered side-effect (via @trigger). Consumes the layers above it; the triggering event is bound as `args`.",
			AnnotationReceiver: "Automation",
			RegistryBacked:     true,
			ConceptInSignature: false,
			BodyBlocks:         []string{"args", "body"},
		},
		{
			Keyword:            "action",
			Category:           CategoryDeclarative,
			Doc:                "Authored external-side-effect primitive (behavioral-constructs ADR §2.3): performs exactly ONE external capability (shell.* / fs.* / http.* / integration.* / mcp.*) on a surface and never touches the graph. Declarative body: capability + intent + params{} + argTemplate{}. Invoked from an automation `action(\"name@1\")` step; replays token-free (fingerprint-verified) on identical input.",
			AnnotationReceiver: "Action",
			RegistryBacked:     true,
			ConceptInSignature: false,
			BodyBlocks:         []string{"params", "argTemplate"},
		},
		{
			Keyword:            "capability",
			Category:           CategoryDeclarative,
			Doc:                "Surface-backed external capability verb (construct-invocation ADR Decision 4): declared like a typed, side-effect-classified builtin with NO body. Namespaced/dotted name (fs.* / shell.* / http.* / integration.* / mcp.*). @sideEffect(\"read\"|\"write\"|\"exec\") -- the UNSPOOFABLE risk class -- lives HERE, not on the action that invokes it (ADR §7). Imported at the verb level (`use capabilities.<ns>.{ verb }`) and called via `capability verb(args)`. Body: an optional args{} input schema.",
			AnnotationReceiver: "Capability",
			RegistryBacked:     true,
			ConceptInSignature: false,
			BodyBlocks:         []string{"args"},
		},
		{
			Keyword:            "spec",
			Category:           CategoryPredicate,
			Doc:                "Atomic boolean predicate. Row-specs (payload.X / intrinsics) compile to SQL; context-specs (actor.X) evaluate in-process. Mixing both is rejected.",
			AnnotationReceiver: "Spec",
			RegistryBacked:     true,
			ConceptInSignature: false,
		},
		{
			Keyword:            "trait",
			Category:           CategoryPredicate,
			Doc:                "Concept-agnostic boolean predicate scaffold (same runtime contract as spec). Used as a cross-concept reusable predicate.",
			AnnotationReceiver: "Spec",
			RegistryBacked:     true,
			ConceptInSignature: false,
		},
		{
			Keyword:            "shape",
			Category:           CategoryDeclarative,
			Doc:                "Reusable field projection. @row projects a concept payload/intrinsics (signature `shape <Concept> <name>`); @actor projects the auth envelope; both = mixed. Body is a path list (+ include).",
			AnnotationReceiver: "Shape",
			RegistryBacked:     true,
			ConceptInSignature: true,
		},
		{
			Keyword:            "tool",
			Category:           CategoryDeclarative,
			Doc:                "AI-callable tool definition. Body is the input-schema field list; @handler wires it to a query/function.",
			AnnotationReceiver: "Tool",
			RegistryBacked:     true,
			ConceptInSignature: false,
		},
		{
			Keyword:            "prompt",
			Category:           CategoryDeclarative,
			Doc:                "AI prompt template with an input schema and a default provider. Body is a bare input-schema field list; @templateFile points at the .tmpl.",
			AnnotationReceiver: "Prompt",
			RegistryBacked:     true,
			ConceptInSignature: false,
		},
		{
			Keyword:            "provider",
			Category:           CategoryDeclarative,
			Doc:                "AI provider configuration (vendor + model + auth). @base providers carry auth+type; children @extends a base. Body: params{} / auth{}.",
			AnnotationReceiver: "Provider",
			RegistryBacked:     true,
			ConceptInSignature: false,
			BodyBlocks:         []string{"params", "auth"},
		},
		{
			Keyword:            "builtin",
			Category:           CategoryDeclarative,
			Doc:                "Go-backed executor exposed as a DSL function. Body is the input schema; @executor names the integration.X.Y implementation.",
			AnnotationReceiver: "Builtin",
			RegistryBacked:     true,
			ConceptInSignature: false,
		},
		{
			Keyword:            "policy",
			Category:           CategoryDeclarative,
			Doc:                "AI provider-selection record (empty body): @primary / @fallback / @maxLatencyMs / @preferredRole, consumed by the AI Router. (Caller-context checks use specs, not policies.)",
			AnnotationReceiver: "Policy",
			RegistryBacked:     true,
			ConceptInSignature: false,
		},
		{
			Keyword:            "seed",
			Category:           CategoryDeclarative,
			Doc:                "Seed an initial row for a bound concept. Struct form `seed <Concept> <name>`.",
			AnnotationReceiver: "Seed",
			RegistryBacked:     true,
			ConceptInSignature: true,
		},
		{
			Keyword:            "use",
			Category:           CategoryImport,
			Doc:                "File-top cross-file import: `use <domain>.<construct>.{ a, b }` pulls named constructs (concepts/shapes/specs/...) into local scope.",
			AnnotationReceiver: "",
			RegistryBacked:     false,
			ConceptInSignature: false,
		},
	}
}
