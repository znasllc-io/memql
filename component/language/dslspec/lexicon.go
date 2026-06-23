package dslspec

// lexicon.go holds the non-construct vocabulary: control-flow / clause /
// reserved keywords, the one-Go-grammar operator set (#971), and the field
// type names. These tables are the SoT for the corresponding sense surfaces
// (which previously hard-coded a stale set that still carried `has` and
// `array`).

// keywords returns reserved words that are not top-level constructs:
// control-flow words used inside logic/automation bodies, the block-header
// clause keywords, the reserved engine identifiers, and `use`.
func keywords() []Keyword {
	return []Keyword{
		// Control flow (logic / automation bodies).
		{Name: "if", Doc: "Conditional control flow: if cond { ... } else { ... }. For a conditional VALUE use the cond(...) expression.", Kind: "control"},
		{Name: "else", Doc: "Alternative branch of an if statement.", Kind: "control"},
		{Name: "for", Doc: "Iterate a collection: for item := range collection { ... }.", Kind: "control"},
		{Name: "range", Doc: "Iteration source in a for statement.", Kind: "control"},
		{Name: "return", Doc: "Return the trailing value from a logic body.", Kind: "control"},
		{Name: "when", Doc: "Arg-conditional guard: when(args.x) { <expr> } -- the guarded block (and its connective) is dropped if args.x is absent.", Kind: "control"},
		{Name: "in", Doc: "Membership test: args.x in payload.list, or payload.kind in [\"a\", \"b\"]. The single membership operator (`has` is retired).", Kind: "control"},

		// Block-header clauses (struct-form construct bodies).
		{Name: "args", Doc: "Input-schema block: declares caller-passed args read as args.X in the body.", Kind: "clause"},
		{Name: "filter", Doc: "Query clause: the boolean row predicate (specs + payload/intrinsic comparisons).", Kind: "clause"},
		{Name: "shape", Doc: "Query clause: names the projection shape for the result. (Also the `shape` construct keyword and the `<expr> with shape(...)` expression.)", Kind: "clause"},
		{Name: "insert", Doc: "Mutation block: the row to create. Exactly one insert OR update per mutation.", Kind: "clause"},
		{Name: "update", Doc: "Mutation block: partial read-merge-write of an existing row (keyed by id).", Kind: "clause"},
		{Name: "body", Doc: "Logic/automation block: named statements ending in `return <expr>`.", Kind: "clause"},
		{Name: "params", Doc: "Provider block: model/window/cost parameters.", Kind: "clause"},
		{Name: "auth", Doc: "Provider block: vendor auth (e.g. apiKey env(\"...\")).", Kind: "clause"},
		{Name: "include", Doc: "Shape statement: compose another shape's fields (with optional aliasing).", Kind: "clause"},

		// Reserved engine identifiers (bare top-level names, not args).
		{Name: "now", Doc: "Reserved: RFC3339 timestamp captured at eval start.", Kind: "reserved"},
		{Name: "actor", Doc: "Reserved: resolved auth context (userId, role, identityId, isClusterOwner, partitions).", Kind: "reserved"},
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
		{Symbol: "!", Doc: "Logical NOT (highest precedence)."},
		{Symbol: "in", Doc: "Membership: lhs in rhsCollection."},
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
		{Name: "enum", Doc: "Restricted value set (pair with @enum(...))."},
		{Name: "array", Doc: "Deprecated list spelling.", Deprecated: true, ReplacedBy: "[]T"},
	}
}
