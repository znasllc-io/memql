package dslspec

import "github.com/znasllc-io/memql/component/language/functions"

// lexicon.go holds the non-construct vocabulary: control-flow / clause /
// reserved keywords, the operator set (projected from the operator table since
// memql#5365), and the field type names. These tables are the SoT for the
// corresponding sense surfaces (which previously hard-coded a stale set that
// still carried `has` and `array`).

// keywords returns reserved words that are not top-level constructs:
// control-flow words used inside logic/automation bodies, the block-header
// clause keywords, the reserved engine identifiers, and `use`.
func keywords() []Keyword {
	return []Keyword{
		// Control flow (logic / automation bodies).
		{Name: "if", Doc: "Conditional control flow: if cond { ... } else { ... }. For a conditional VALUE write the expression `p ? a : b`.", Kind: "control"},
		{Name: "else", Doc: "Alternative branch of an if statement.", Kind: "control"},
		{Name: "for", Doc: "Iterate a collection: for item := range collection { ... }.", Kind: "control"},
		{Name: "range", Doc: "Iteration source in a for statement.", Kind: "control"},
		{Name: "return", Doc: "Return the trailing value from a logic body.", Kind: "control"},
		{Name: "when", Doc: "Retired in edition 2026: the arg-conditional guard `when(args.x) { <predicate> }` is written `args.x == nil || <predicate>`, which lowers the same way. memqlmigrate --rewrite=expressions rewrites it.", Kind: "control"},
		{Name: "in", Doc: "Membership test: `args.tag in row.tags`, or `row.kind in [\"a\", \"b\"]`. The single membership operator (`has` and the `.contains(v)` collection method are retired).", Kind: "control"},
		{Name: "startsWith", Doc: "String-prefix test: `row.name startsWith \"lit\"`, a list of prefixes (ANY of), or an arg. An empty list and a blank prefix match nothing (memql#4208).", Kind: "control"},

		// Block-header clauses (struct-form construct bodies).
		{Name: "args", Doc: "Input-schema block: declares caller-passed args read as args.X in the body.", Kind: "clause"},
		{Name: "filter", Doc: "Query clause: `filter row => <predicate>`, the boolean predicate over the row, pushed down to SQL.", Kind: "clause"},
		{Name: "shape", Doc: "Query clause: names the projection shape for the result. (Also the `shape` construct keyword and the `<expr> with shape(...)` expression.)", Kind: "clause"},
		{Name: "insert", Doc: "Mutation block: the row to create. Exactly one insert OR update per mutation.", Kind: "clause"},
		{Name: "update", Doc: "Mutation block: partial read-merge-write of an existing row (keyed by id).", Kind: "clause"},
		{Name: "accept", Doc: "Write-block sugar: `accept { name, ... }` lists the public fields the mutation accepts -- each auto-binds to its same-named arg (`name` -> `name: args.name`). Every name must be a declared arg. Nested inside insert{}/update{} (or top-level, which means insert). Never mixed with loose fields.", Kind: "clause"},
		{Name: "stamp", Doc: "Write-block sugar: `stamp { key: value, ... }` carries the server-set fields beside an accept{} list. Nested inside insert{}/update{} (or top-level with accept, which means insert).", Kind: "clause"},
		{Name: "body", Doc: "Logic/automation block: named statements ending in `return <expr>`.", Kind: "clause"},
		{Name: "params", Doc: "Provider block: model/window/cost parameters.", Kind: "clause"},
		{Name: "auth", Doc: "Provider block: vendor auth (e.g. apiKey env(\"...\")).", Kind: "clause"},

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

// operators returns the v1 expression operators, projected row for row from
// the one operator table (component/language/functions, Operators) -- symbol,
// prose, absence rule and precedence -- so no operator is described in two
// places (memql#5365). The retired `;`-AND / `,`-OR separators and the `has`
// membership operator are not in that table; the drift test asserts they never
// reappear here.
//
// `!` is a working operator everywhere in v1: the evaluators give it the
// record's two-valued meaning (every predicate answers true or false, absent
// included). Its old Doc warned that it was refused at load (memql#3630), which
// was true of the pre-v1 converters and is exactly what this epic changes.
func operators() []Operator {
	table := functions.Operators()
	out := make([]Operator, 0, len(table))
	for _, op := range table {
		out = append(out, Operator{
			Symbol: op.Symbol, Doc: op.Doc, Name: op.Name, Kind: op.Kind,
			Form: op.Form, Absence: op.Absence, Level: op.Level,
		})
	}
	return out
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
