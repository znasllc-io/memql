package dslspec

import (
	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
)

// lexicon.go holds the non-construct vocabulary: control-flow / clause /
// reserved keywords, the operator set (projected from the operator table since
// memql#5365), and the field type names. These tables are the SoT for the
// corresponding sense surfaces (which previously hard-coded a stale set that
// still carried `has` and `array`).

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
// paginate, asOf, count and precondition. The doc of each is
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
	"filter":       "Query clause: `filter row => <predicate>`, the boolean predicate over the row, pushed down to SQL. A line clause -- it takes the rest of its line and the lines after it that open with an operator; no block.",
	"refine":       "Query clause: `refine row => <predicate>`, a predicate the database cannot run, applied in process to each page `paginate` reads, so a page may come back short. Requires paginate; never with count.",
	"shape":        "Query clause: names the projection shape for the result -- `shape <name>`. (Also the `shape` construct keyword and the `<expr> with shape(...)` expression.)",
	"sort":         "Query clause: order the result -- `sort \"row.createdAt\", \"desc\"`. Payload keys are bare; row intrinsics take the row. namespace.",
	"paginate":     "Query clause: bound the result to a window -- `paginate 25`. A list-returning query carries paginate, sort, count or @unbounded(\"reason\") (memql#1965).",
	"asOf":         "Query clause: read the stream as of a moment -- `asOf latest`, or `asOf args.at ?? latest`. Query-only (core-builtins ADR 2.3).",
	"count":        "Query clause: aggregate the matching set to {count: N} -- a bare `count`. Mutually exclusive with shape, sort and paginate.",
	"insert":       "Mutation block: the row to create. Exactly one insert OR update per mutation.",
	"update":       "Mutation block: partial read-merge-write of an existing row (keyed by id).",
	"accept":       "Write-block sugar: `accept { name, ... }` lists the public fields the mutation accepts -- each auto-binds to its same-named arg (`name` -> `name: args.name`). Every name must be a declared arg. Nested inside insert{}/update{} (or top-level, which means insert). Never mixed with loose fields.",
	"stamp":        "Write-block sugar: `stamp { key: value, ... }` carries the server-set fields beside an accept{} list. Nested inside insert{}/update{} (or top-level with accept, which means insert).",
	"precondition": "Automation block: `precondition <name> { ... }`, a deterministic check that must hold before the statements run (Epic 4, memql#2139).",
	"params":       "Provider block: model/window/cost parameters.",
	"auth":         "Provider block: vendor auth (e.g. apiKey env(\"...\")).",
}

// controlKeywords are the control-flow words of logic / automation bodies.
func controlKeywords() []Keyword {
	return []Keyword{
		// Statements (logic / automation bodies, edition 2026, epic memql#5370).
		// One statement per line, run in the order written; a name is bound once
		// and read by the statements after it. Their trailing clauses (retry,
		// wait, on) are control words too: "clause" is a body clause of the
		// parser's clause tables (bodyClauseKeywords).
		{Name: "if", Doc: "Conditional statement: `if <cond> { ... } else if <cond> { ... } else { ... }`. A name bound in a branch is readable after the chain -- absent if the branch that binds it did not run -- and the branches of one chain may bind the same name. For a conditional VALUE write the expression `p ? a : b`.", Kind: "control"},
		{Name: "else", Doc: "Alternative branch of an if statement, on the closing brace's line: `} else {`.", Kind: "control"},
		{Name: "for", Doc: "Loop statement: `for item in <source> [if <cond>] { ... }` -- the loop variable and every name bound in the body exist in each iteration only. A return inside ends the body the loop is in. Trailing clause: `on error continue`.", Kind: "control"},
		{Name: "switch", Doc: "Switch statement: `switch <value> { case <literal>[, <literal>] { ... } default { ... } }` -- the first case whose label equals the value runs, else the default. Its names share the switch's scope, as an if chain's do.", Kind: "control"},
		{Name: "case", Doc: "A switch branch: `case <literal>[, <literal>] { ... }`. A label is a literal, written once per switch.", Kind: "control"},
		{Name: "default", Doc: "The switch branch that runs when no case matches.", Kind: "control"},
		{Name: "parallel", Doc: "Parallel statement: `parallel { branch <label> { ... } ... } [wait any]` -- the branches run at once, each a list of its own whose names stay inside it; a failed branch stops the others. A branch cannot return. Trailing clause: `on error continue`.", Kind: "control"},
		{Name: "branch", Doc: "One list of a parallel statement: `branch <label> { ... }`.", Kind: "control"},
		{Name: "publish", Doc: "Publish statement, in an automation: `publish \"<topic>\" { key: value, ... }` puts an event on the bus. A logic may not publish (D14): publish from the automation that calls it.", Kind: "control"},
		{Name: "return", Doc: "End the body with a value: `return <expr>`, or `return <call>` for what the call returns. A logic's last statement is its return; an automation's return is its run's outcome.", Kind: "control"},
		{Name: "retry", Doc: "Trailing clause of a construct call: `<call> retry(n)` runs a failed call up to n more times. Written after `on surface(...)` and before `on error continue`.", Kind: "control"},
		{Name: "wait", Doc: "Trailing clause of a parallel: `wait any` ends it when one branch ends. `wait all` is the default and is not written.", Kind: "control"},
		{Name: "on", Doc: "Trailing clauses: `on surface(\"<name>\")` names where an action runs; `on error continue` records a failed call, for or parallel and goes on, its name left absent (`on error stop` is the default and is not written).", Kind: "control"},
		{Name: "in", Doc: "Membership test: `args.tag in row.tags`, or `row.kind in [\"a\", \"b\"]`. The single membership operator (`has` and the `.contains(v)` collection method are retired).", Kind: "control"},
		{Name: "startsWith", Doc: "String-prefix test: `row.name startsWith \"lit\"`, a list of prefixes (ANY of), or an arg. An empty list and a blank prefix match nothing (memql#4208).", Kind: "control"},
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
