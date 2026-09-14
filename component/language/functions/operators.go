package functions

// operators.go is the one table of the v1 expression operators: how each is
// written, what it does, what it does with an absent operand and how tightly
// it binds. Sense's hover cards, dslspec's operator list (and through it the
// generated TextMate grammar) and the generated docs all read this table, so
// an operator is described once.
//
// Two record decisions shape it:
//
//   - D9, the precedence. Level is the row of the record's table, 1 binding
//     tightest, and TestOperatorLevelsAreTheParsersPrecedence holds every
//     level to parser.V1PrecedenceTable, the table the parser binds by and
//     the language reference publishes.
//   - D8, absence. "Absent" means the key is missing OR its value is JSON
//     null. In `==`, `!=` and `in` an absent value, `nil` and the empty string
//     are one value, "unset" (settled 2026-09-13). Absence restates the
//     record's one table for the operator; both evaluators reproduce that
//     table, and the differential lane holds them to it. The prose here is
//     what an author reads.

// Operator is one operator of the v1 expression grammar.
type Operator struct {
	// Symbol is how the operator is written. The ternary's two halves are
	// written "? :"; "-" appears twice, once unary and once binary.
	Symbol string
	// Name is a stable lowerCamel identifier for the operator. Where the
	// operator is a node kind of its own it is that kind's name ("coalesce",
	// "not", "ternary"); a comparison or an arithmetic operator is named for
	// itself ("notEqual", "add").
	Name string
	// Kind is the node kind the operator's node has, in the vocabulary of
	// component/language/ast (NodeKind) and so of the tier manifest: "==" is a
	// "comparison", "+" is "arithmetic". It is a string so this package stays
	// a leaf; a test in component/language/tiers pins every value to
	// ast.AllNodeKinds().
	Kind string
	// Form shows the operator in use, for the code block a hover card opens
	// with: "a ?? b", "row.?lineage.planId".
	Form string
	// Doc is one or two sentences saying what the operator does.
	Doc string
	// Absence is the record's absence rule for the operator, where the table
	// has one, in one or two sentences; "" where it has none.
	Absence string
	// Level is the precedence level, 1 binding tightest (D9).
	Level int
}

// Operators returns the table in precedence order, tightest first. The result
// is a copy.
func Operators() []Operator {
	return []Operator{
		// ---- Level 1: member access ----
		{
			Symbol: ".", Name: "member", Kind: "member", Form: "row.status", Level: 1,
			Doc:     "Reads a field of an object: `row.status` is the row's status field.",
			Absence: "An absent object or field reads as absent. Where the object itself may be absent, the load requires `.?` instead.",
		},
		{
			Symbol: ".?", Name: "optionalMember", Kind: "optionalMember", Form: "row.?lineage.planId", Level: 1,
			Doc:     "Reads a field of an object that may be absent: `row.?lineage.planId` is the plan id when lineage is present.",
			Absence: "An absent object reads as absent instead of refusing, and the rest of the chain stays absent. The load requires `.?` wherever the object may be absent: an optional object field, an optional arg, an untyped value.",
		},

		// ---- Level 2: unary ----
		{
			Symbol: "!", Name: "not", Kind: "not", Form: "!(x in list)", Level: 2,
			Doc:     "Negates a boolean: `!(row.status in [\"open\", \"held\"])` is true when the status is neither.",
			Absence: "Every predicate answers true or false, absent included, so `!(x == v)` is exactly `x != v`.",
		},
		{
			Symbol: "-", Name: "negate", Kind: "negate", Form: "-x", Level: 2,
			Doc: "Negates a number.",
		},

		// ---- Level 3: multiplicative ----
		{
			Symbol: "*", Name: "multiply", Kind: "arithmetic", Form: "a * b", Level: 3,
			Doc: "Multiplies two numbers.",
		},
		{
			Symbol: "/", Name: "divide", Kind: "arithmetic", Form: "a / b", Level: 3,
			Doc: "Divides a by b; dividing by zero is an error.",
		},
		{
			Symbol: "%", Name: "remainder", Kind: "arithmetic", Form: "a % b", Level: 3,
			Doc: "Returns the remainder of dividing a by b; a zero divisor is an error.",
		},

		// ---- Level 4: additive ----
		{
			Symbol: "+", Name: "add", Kind: "arithmetic", Form: "a + b", Level: 4,
			Doc:     "Adds two numbers, or joins two strings: `\"si-\" + args.id`. A number joined to a string contributes its text.",
			Absence: "In a join, an absent operand contributes the empty string.",
		},
		{
			Symbol: "-", Name: "subtract", Kind: "arithmetic", Form: "a - b", Level: 4,
			Doc: "Subtracts b from a. Write it with spaces, because `a-b` is one hyphenated name.",
		},

		// ---- Level 5: coalesce ----
		{
			Symbol: "??", Name: "coalesce", Kind: "coalesce", Form: "a ?? b", Level: 5,
			Doc:     "Returns a, or b when a is missing: `args.stage ?? \"active\"`. It binds tighter than comparison, so `a ?? \"\" == \"x\"` compares the coalesced value.",
			Absence: "Falls through to b when a is absent or a blank or whitespace-only string; `false`, `0` and an empty list are kept.",
		},

		// ---- Level 6: comparison, membership, prefix ----
		{
			Symbol: "==", Name: "equal", Kind: "comparison", Form: "a == b", Level: 6,
			Doc:     "Is true when two values are equal. Numbers compare numerically, and `1 == \"1\"` is false.",
			Absence: "A missing field, JSON null, `nil` and `\"\"` are one unset value: each equals the others and nothing else.",
		},
		{
			Symbol: "!=", Name: "notEqual", Kind: "comparison", Form: "a != b", Level: 6,
			Doc:     "Is true when two values differ.",
			Absence: "The exact negation of `==`, so an unset field is not equal to `\"x\"`, and `row.f != nil` and `row.f != \"\"` are false for it.",
		},
		{
			Symbol: "<", Name: "less", Kind: "comparison", Form: "a < b", Level: 6,
			Doc:     "Is true when a orders before b. Strings order by byte, so RFC 3339 timestamps order by time.",
			Absence: "Any ordering against an absent field is false.",
		},
		{
			Symbol: "<=", Name: "lessOrEqual", Kind: "comparison", Form: "a <= b", Level: 6,
			Doc:     "Is true when a orders before b or equals it.",
			Absence: "Any ordering against an absent field is false.",
		},
		{
			Symbol: ">", Name: "greater", Kind: "comparison", Form: "a > b", Level: 6,
			Doc:     "Is true when a orders after b.",
			Absence: "Any ordering against an absent field is false.",
		},
		{
			Symbol: ">=", Name: "greaterOrEqual", Kind: "comparison", Form: "a >= b", Level: 6,
			Doc:     "Is true when a orders after b or equals it.",
			Absence: "Any ordering against an absent field is false.",
		},
		{
			Symbol: "in", Name: "in", Kind: "in", Form: "v in list", Level: 6,
			Doc:     "Is true when v equals an element of the list: `row.status in [\"open\", \"held\"]`, `args.tag in row.tags`.",
			Absence: "Membership is `==` against each element, so an unset v is in a list only when the list holds `\"\"` or `nil`. Nothing is in an absent list.",
		},
		{
			Symbol: "startsWith", Name: "startsWith", Kind: "startsWith", Form: "s startsWith p", Level: 6,
			Doc:     "Is true when the string s begins with the prefix p, or with any prefix in a list of them.",
			Absence: "Is false when either side is absent; a blank prefix and an empty list match nothing.",
		},

		// ---- Levels 7 and 8: the connectives ----
		{
			Symbol: "&&", Name: "and", Kind: "and", Form: "a && b", Level: 7,
			Doc: "Is true when both sides are true, and skips the right side when the left is false. Both sides must be boolean: nothing else counts as true.",
		},
		{
			Symbol: "||", Name: "or", Kind: "or", Form: "a || b", Level: 8,
			Doc: "Is true when either side is true, and skips the right side when the left is true. Both sides must be boolean: nothing else counts as true.",
		},

		// ---- Level 9: the conditional value ----
		{
			Symbol: "? :", Name: "ternary", Kind: "ternary", Form: "p ? a : b", Level: 9,
			Doc: "Returns a when the boolean p is true, and b when it is false. It binds loosest of the value operators, so `x == 1 ? a : b` tests `x == 1`.",
		},

		// ---- Level 10: the lambda ----
		{
			Symbol: "=>", Name: "lambda", Kind: "lambda", Form: "row => row.status == \"open\"", Level: 10,
			Doc: "Names the parameter a predicate or a projection reads: `row => ...` over a row, `t => t == \"x\"` over an element. Two parameters are written `(acc, x) => ...`.",
		},
	}
}
