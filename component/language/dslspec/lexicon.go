package dslspec

import "github.com/znasllc-io/memql/component/language/parser"

// lexicon.go holds the non-construct vocabulary: control-flow / clause /
// reserved keywords, the one-Go-grammar operator set (#971), and the field
// type names. These tables are the SoT for the corresponding sense surfaces
// (which previously hard-coded a stale set that still carried `has` and
// `array`).

// keywords returns reserved words that are not top-level constructs:
// control-flow words used inside logic/automation bodies, the body clause
// keywords (derived from the parser's clause table, bodyClauseKeywords), the
// reserved engine identifiers, and `use`.
func keywords() []Keyword {
	out := controlKeywords()
	out = append(out, bodyClauseKeywords()...)
	return append(out, reservedKeywords()...)
}

// bodyClauseKeywords returns one clause keyword per body clause any
// construct accepts, in the order the constructs list them, DERIVED from
// parser.BodyClauses (memql#5359) -- the hand list this replaced lacked sort,
// paginate, asOf, count, step and precondition. The doc of each is
// clauseDocs'; a clause without one fails the drift test.
func bodyClauseKeywords() []Keyword {
	seen := map[string]bool{}
	var out []Keyword
	for _, kw := range constructKeywords() {
		for _, clause := range parser.BodyClauses(kw) {
			if seen[clause] {
				continue
			}
			seen[clause] = true
			out = append(out, Keyword{Name: clause, Doc: clauseDocs[clause], Kind: "clause"})
		}
	}
	return out
}

// clauseDocs is the one-line doc of each body clause.
var clauseDocs = map[string]string{
	"args":         "Input-schema block: declares caller-passed args read as args.X in the body.",
	"filter":       "Query clause: the boolean row predicate (specs + payload/intrinsic comparisons). A line clause -- `filter <expr>`, no block.",
	"shape":        "Query clause: names the projection shape for the result -- `shape <name>`. (Also the `shape` construct keyword and the `<expr> with shape(...)` expression.)",
	"sort":         "Query clause: order the result -- `sort \"row.createdAt\", \"desc\"`. Payload keys are bare; row intrinsics take the row. namespace.",
	"paginate":     "Query clause: bound the result to a window -- `paginate 25`. A list-returning query carries paginate, sort, count or @unbounded(\"reason\") (memql#1965).",
	"asOf":         "Query clause: read the stream as of a moment -- `asOf latest`, or `asOf args.at ?? latest`. Query-only (core-builtins ADR 2.3).",
	"count":        "Query clause: aggregate the matching set to {count: N} -- a bare `count`. Mutually exclusive with shape, sort and paginate.",
	"insert":       "Mutation block: the row to create. Exactly one insert OR update per mutation.",
	"update":       "Mutation block: partial read-merge-write of an existing row (keyed by id).",
	"accept":       "Write-block sugar: `accept { name, ... }` lists the public fields the mutation accepts -- each auto-binds to its same-named arg (`name` -> `name: args.name`). Every name must be a declared arg. Nested inside insert{}/update{} (or top-level, which means insert). Never mixed with loose fields.",
	"stamp":        "Write-block sugar: `stamp { key: value, ... }` carries the server-set fields beside an accept{} list. Nested inside insert{}/update{} (or top-level with accept, which means insert).",
	"body":         "Logic block: named statements ending in `return <expr>`. An automation has no body block -- its body is step blocks.",
	"step":         "Automation block: `step <name> { <call> }`, one unit of the automation's work; steps run in order, and each result is readable by name.",
	"precondition": "Automation block: `precondition <name> { ... }`, a deterministic check that must hold before the steps run (Epic 4, memql#2139).",
	"params":       "Provider block: model/window/cost parameters.",
	"auth":         "Provider block: vendor auth (e.g. apiKey env(\"...\")).",
}

// controlKeywords are the control-flow words of logic / automation bodies.
func controlKeywords() []Keyword {
	return []Keyword{
		{Name: "if", Doc: "Conditional control flow: if cond { ... } else { ... }. For a conditional VALUE use the cond(...) expression.", Kind: "control"},
		{Name: "else", Doc: "Alternative branch of an if statement.", Kind: "control"},
		{Name: "for", Doc: "Iterate a collection: for item := range collection { ... }.", Kind: "control"},
		{Name: "range", Doc: "Iteration source in a for statement.", Kind: "control"},
		{Name: "return", Doc: "Return the trailing value from a logic body.", Kind: "control"},
		{Name: "when", Doc: "Arg-conditional guard: when(args.x) { <expr> } -- the guarded block (and its connective) is dropped if args.x is absent.", Kind: "control"},
		{Name: "in", Doc: "Membership test: args.x in payload.list, or payload.kind in [\"a\", \"b\"]. The single membership operator (`has` is retired).", Kind: "control"},
		{Name: "startsWith", Doc: "String-prefix test: <field> startsWith \"lit\", [\"a\", \"b\"] (ANY of) or args.x. Filter and spec predicate; an empty list and a blank prefix match nothing (memql#4208).", Kind: "control"},
	}
}

// reservedKeywords are the reserved engine identifiers and `use`.
func reservedKeywords() []Keyword {
	return []Keyword{
		// Reserved engine identifiers (bare top-level names, not args).
		{Name: "now", Doc: "Reserved: RFC3339 timestamp captured at eval start.", Kind: "reserved"},
		{Name: "actor", Doc: "Reserved: the auth envelope. Closed member set (#2623): userId, role, identityId, isClusterOwner, primaryEmail, now, plus the legacy isOwner alias. Reading it requires @actor in the construct preamble (#2621).", Kind: "reserved", Properties: []KeywordProperty{
			{Name: "userId", Doc: "The acting user's id."},
			{Name: "role", Doc: "Cluster role: owner / admin / developer / writer / reader."},
			{Name: "identityId", Doc: "The credential row (token, magic-link, PAT)."},
			{Name: "isClusterOwner", Doc: "Bool short-circuit; bypasses the per-partition ACL."},
			{Name: "primaryEmail", Doc: "The acting user's primary email address."},
			{Name: "now", Doc: "RFC3339 timestamp at evaluation start (the shape-body spelling of the reserved `now`)."},
			{Name: "isOwner", Doc: "Legacy alias of isClusterOwner.", AliasOf: "isClusterOwner"},
		}},
		{Name: "partition", Doc: "Reserved: the active partition for this call.", Kind: "reserved"},
		{Name: "config", Doc: "Reserved: allow-listed config values (component/config/policy_exposable.go).", Kind: "reserved"},
		{Name: "trace", Doc: "Reserved engine identifier.", Kind: "reserved"},
		{Name: "args", Doc: "Reserved namespace for caller-passed inputs (args.X).", Kind: "reserved"},
		{Name: "payload", Doc: "Reserved: bound-concept row payload (payload.X) -- valid in query filter/shape only (SQL pushdown).", Kind: "reserved"},

		// Import.
		{Name: "use", Doc: "File-top import: use <domain>.<construct>.{ names }.", Kind: "import"},
	}
}

// operators returns the one-Go-boolean-grammar operator set (#971). The
// retired `;`-AND / `,`-OR separators and the `has` membership operator are
// intentionally absent -- the drift test asserts they never reappear here.
func operators() []Operator {
	return []Operator{
		{Symbol: "==", Doc: "Equality."},
		{Symbol: "!=", Doc: "Inequality."},
		{Symbol: "<", Doc: "Less than."},
		{Symbol: "<=", Doc: "Less than or equal."},
		{Symbol: ">", Doc: "Greater than."},
		{Symbol: ">=", Doc: "Greater than or equal."},
		{Symbol: "&&", Doc: "Logical AND."},
		{Symbol: "||", Doc: "Logical OR."},
		// `!` is listed so Sense can EXPLAIN the token an author types,
		// not to offer it as usable. It lexes and parses and is then
		// refused by every ASTConverter surface -- filters and specs get
		// the #2542 expression-led scope error, logic bodies and
		// collection lambdas get "NOT/! does not convert". Its only
		// working home is an automation cond-step condition, which the
		// string evaluator in component/automations/evaluator.go handles.
		// The Doc used to read "Logical NOT (highest precedence)", which
		// advertised an operator the loader has never accepted
		// (memql#3630).
		{Symbol: "!", Doc: "Logical NOT -- NOT SUPPORTED in filters, specs, logic bodies or collection lambdas; rejected at load. Write the != comparison form. Works in runtime condition strings only: an automation cond-step condition and a trigger @filter."},
		{Symbol: "in", Doc: "Membership: lhs in rhsCollection."},
		{Symbol: "startsWith", Doc: "String prefix: field startsWith prefix, or ANY of a list of prefixes. Parameterized `^@ ANY(text[])` in SQL; empty list / blank prefix match nothing (memql#4208)."},
		{Symbol: "??", Doc: "Null-coalescing: first non-nil/non-empty operand; a ?? b ?? c folds to coalesce(a, b, c) with the final operand as the ultimate fallback. Binds tighter than comparison, looser than arithmetic (#2611)."},
	}
}

// fieldTypes returns the type names valid in a concept field, args field, or
// declarative-construct body field. `array` is retained as a deprecated
// spelling that the grammar still accepts but flags (migrate to []T).
func fieldTypes() []FieldType {
	return []FieldType{
		{Name: "string", Doc: "UTF-8 text."},
		{Name: "int", Doc: "Integer."},
		{Name: "float", Doc: "Floating-point number."},
		{Name: "bool", Doc: "Boolean."},
		{Name: "datetime", Doc: "RFC3339 timestamp."},
		{Name: "object", Doc: "Nested object / JSON map."},
		{Name: "enum", Doc: "Restricted value set. First-class parameterized form (#2618): `status enum(\"open\", \"closed\")` -- self-contained, same representation as the legacy `string @enum(...)` pair (which keeps parsing)."},
		{Name: "array", Doc: "Deprecated list spelling.", Deprecated: true, ReplacedBy: "[]T"},
	}
}
