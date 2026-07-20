package dslspec

// nextrules.go encodes the coarse "what is legal to type next" state model
// that drives context-aware completion (#2126 builds the richer AST-aware
// analysis on top of these). Each rule names a cursor context and the
// categories/literals that may legally follow. The category tokens used in
// Expect are: "construct", "concept", "annotation", "fieldType",
// "fieldName", "importPath", "operator", "specRef", "shapeRef", plus any
// concrete literal.
//
// The owner's canonical example -- "I typed `mutate`, so suggest a concept;
// and if no concept is in file scope, offer to `use`/import one" -- is the
// AfterMutationKeyword / AfterQueryKeyword rules with
// SuggestImportWhenMissing set.
func nextRules() []NextRule {
	return []NextRule{
		{
			Context: "topLevel",
			Expect:  []string{"annotation", "construct"},
			Doc:     "At top of file (or between declarations): an @annotation block or a construct keyword (concept, query, mutate, logic, automation, action, capability, spec, trait, shape, tool, prompt, provider, builtin, policy, seed) or a `use` import.",
		},
		{
			Context:                  "afterMutationKeyword",
			Expect:                   []string{"concept"},
			Doc:                      "After `mutate`: the bound concept name (`mutate <Concept> <name>`). Suggest concepts in file scope.",
			SuggestImportWhenMissing: true,
		},
		{
			Context:                  "afterQueryKeyword",
			Expect:                   []string{"concept"},
			Doc:                      "After `query`: the bound concept name (`query <Concept> <name>`). Suggest concepts in file scope.",
			SuggestImportWhenMissing: true,
		},
		{
			Context:                  "afterSeedKeyword",
			Expect:                   []string{"concept"},
			Doc:                      "After `seed`: the bound concept name (`seed <Concept> <name>`).",
			SuggestImportWhenMissing: true,
		},
		{
			Context:                  "afterShapeKeyword",
			Expect:                   []string{"concept", "identifier"},
			Doc:                      "After `shape`: a bound concept for a @row shape (`shape <Concept> <name>`), or directly the shape name for an @actor shape.",
			SuggestImportWhenMissing: true,
		},
		{
			Context: "afterUseKeyword",
			Expect:  []string{"importPath"},
			Doc:     "After `use`: a dotted import path `<domain>.<construct>` followed by a brace list of names, e.g. `use cognition.concepts.{ space }`.",
		},
		{
			Context: "inConceptBody",
			Expect:  []string{"fieldName", "fieldType", "annotation"},
			Doc:     "Inside a concept body: field declarations `<name> <type> [@annotations]` and @relationship links.",
		},
		{
			Context: "inArgsBlock",
			Expect:  []string{"fieldName", "fieldType", "annotation"},
			Doc:     "Inside an args { } block: `<name> <type> [@required] [@enum(...)] [@maxLength(N)] [@pattern(...)]`. (@description here is parsed and DISCARDED -- #2615; per-field docs arrive with /// doc comments, #2601. @default is rejected on an args field -- use coalesce(args.X, ...) or the `args.X ?? fallback` shorthand.)",
		},
		{
			Context: "inWriteBlock",
			Expect:  []string{"fieldName", "clause"},
			Doc:     "Inside an insert { } / update { } block: `<key>: <expr>` write fields, or the sugared form -- `accept { argName, ... }` (each name auto-binds its same-named declared arg) plus `stamp { key: value }` (server-set fields). Loose fields never mix with accept/stamp (the desugar rebuilds the body from the blocks alone); update requires an id.",
		},
		{
			Context: "inFilterClause",
			Expect:  []string{"specRef", "operator", "payloadPath", "intrinsic"},
			Doc:     "Inside a query filter: spec references, trait references, payload.<field> / intrinsic comparisons joined by && / || / ! with Go precedence; membership via `in`; arg-guards via when(args.x){ }.",
		},
		{
			Context: "inShapeBody",
			Expect:  []string{"rowPath", "actorPath", "include"},
			Doc:     "Inside a shape body: row.* / actor.* projection paths and `include <shape>` composition.",
		},
		{
			Context: "inFunctionBody",
			Expect:  []string{"keyword", "builtin", "operator"},
			Doc:     "Inside a logic/automation body: control-flow keywords, builtin calls, and expressions. Query-level directives (sort/paginate/asOf/select/withDepth/shape) are rejected here.",
		},
		{
			Context: "afterAtSign",
			Expect:  []string{"annotation"},
			Doc:     "After `@`: an annotation legal for the construct being declared (filtered by the construct's annotation receiver).",
		},
	}
}
