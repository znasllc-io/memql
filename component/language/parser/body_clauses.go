package parser

// body_clauses.go -- the clauses each construct's body accepts (memql#5359).
//
// The struct-form rewriter (parseStructQueryBody, parseStructMutationBody),
// the statement parser (parseV1Definition: the blocks a logic's or an
// automation's statements follow) and the construct parsers
// (parseActionDecl, parseCapabilityDecl, parseProviderDecl) each accept a
// fixed set of body clauses. That set used to be restated by hand in dslspec, which is how the
// editor came to offer `body { }` inside an automation (it has none) and
// never offered `sort`, `paginate`, `asOf` or `count` on a query. It is
// stated once here instead: dslspec projects it as Construct.BodyBlocks, and
// body_clauses_test.go pins it to the switch each body parser dispatches on
// (TestBodyClausesMatchTheParserDispatch) and to what the parsers accept and
// refuse (TestBodyClausesMatchWhatTheParsersAccept,
// TestUnlistedClausesAreRefused) -- a restated list with a pin, rather than a
// list the parsers read, because the rewriter's `case` arms are themselves
// the structural arm of the grammar-surface digest and must stay literal.

// bodyClauseTable lists, per construct keyword, the clauses its body
// accepts, in authoring order. A construct whose body is a bare list -- a
// concept's fields, a shape's paths, a tool / prompt / builtin field list, a
// seed's assignments, the empty body of a policy or rule, a spec's return --
// has no entry.
var bodyClauseTable = map[string][]string{
	"query":      {"args", "filter", "refine", "shape", "sort", "paginate", "asOf", "count"},
	"mutation":   {"args", "insert", "update", "accept", "stamp"},
	"logic":      {"args"},
	"automation": {"args", "precondition"},
	"action":     {"args"},
	"capability": {"args"},
	"provider":   {"params", "auth"},
}

// lineClauses are the body clauses that take the rest of their line
// (`filter <expr>`, `paginate 25`, `count`) rather than opening a block.
var lineClauses = map[string]bool{
	"filter": true, "refine": true, "shape": true, "sort": true, "paginate": true, "asOf": true, "count": true,
}

// namedBlocks are the body clauses whose block carries a name
// (`precondition <name> { ... }`) -- the name is part of the clause, and a
// nameless block is refused.
var namedBlocks = map[string]bool{
	"precondition": true,
}

// BodyClauses returns the clauses the construct keyword's body accepts, in
// authoring order, or nil when the body is a bare list. The result is a
// fresh slice the caller may keep.
func BodyClauses(keyword string) []string {
	return append([]string(nil), bodyClauseTable[keyword]...)
}

// IsLineClause reports whether a body clause takes the rest of its line
// (`filter <expr>`) rather than opening a `<clause> { ... }` block.
func IsLineClause(clause string) bool {
	return lineClauses[clause]
}

// IsNamedBlock reports whether a body clause's block carries a name:
// `precondition <name> { ... }` rather than `args { ... }`.
func IsNamedBlock(clause string) bool {
	return namedBlocks[clause]
}
