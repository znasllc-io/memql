package parser

// options.go -- how a file is parsed (memql#5364).
//
// The edition-2026 expression grammar arrives in two halves. The pushdown
// positions (a query filter, a spec or trait body, an automation @filter) have
// v1 spellings that no legacy spelling can be mistaken for -- a lambda header,
// `= row =>` -- so both are accepted side by side and need no switch. The
// in-process positions (conditions, step arguments, logic statements, mutation
// values) reuse spellings both grammars read differently (`cond(`, `concat(`,
// a bare `payload.x`), so they change grammar as ONE unit, with the tree's
// migration. ExpressionsV1 is that unit's switch, and DefaultOptions is the one
// place the engine's default is set: the flip is a one-line change.

// Options selects how a file is parsed. The zero value is today's grammar.
type Options struct {
	// ExpressionsV1 parses the in-process expression positions with the
	// edition-2026 grammar and refuses the legacy predicate forms (a filter
	// with no lambda header, a spec or trait `{ return ... }` body, a
	// raw-text @filter), naming memqlmigrate --rewrite=expressions. Every
	// definition parsed with it on carries FunctionDef.ExpressionsV1 (and
	// AutomationDef.ExpressionsV1), which is what its consumers key on.
	ExpressionsV1 bool
}

// DefaultOptions is what ParseFile parses with.
var DefaultOptions = Options{ExpressionsV1: true}

// ParseFileWithOptions is ParseFile under explicit options.
func ParseFileWithOptions(source string, o Options) (*File, error) {
	lexer := NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, err
	}
	parser := NewParser(tokens)
	parser.opts = o
	parser.SetSource(source)
	parser.SetDocComments(lexer.DocComments())
	return parser.parseFile()
}

// markExpressionsV1 stamps a definition parsed with ExpressionsV1 on.
func markExpressionsV1(def Node) {
	fn, ok := def.(*FunctionDef)
	if !ok {
		return
	}
	fn.ExpressionsV1 = true
	if auto, ok := fn.Body.(*AutomationDef); ok {
		auto.ExpressionsV1 = true
	}
}
