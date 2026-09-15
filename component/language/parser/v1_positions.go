package parser

// v1_positions.go -- where the construct parsers hand an expression to the
// edition-2026 grammar (task memql#5364, Task 3 of the DSL v1 expressions
// plan).
//
// # The positions
//
// The PUSHDOWN positions -- a query's `filter`, a spec or trait body, an
// automation's `@filter` -- take a lambda (`row => ...`, `= row => ...`); the
// legacy spelling each replaced is refused naming memqlmigrate
// --rewrite=expressions.
//
// The IN-PROCESS positions parse with the v1 parser: a logic's and an
// automation's statements (the statement parser, v1_body.go), and a
// mutation's values, whose string field holds the canonical v1 source
// (ast.FormatExpr) beside the node (MutationStmt.PayloadExpr, or the v1 node
// itself inside a value map).
//
// The engine's internal query form -- the string an SDK sends to Execute --
// keeps its own grammar. The one thing that grammar learns here is to read a
// v1 lambda where it meets an operand, because that is how a struct-form
// query's v1 filter reaches it: `concept==<id> && (row => ...)`.

import (
	"github.com/znasllc-io/memql/component/language/ast"
)

// tryParseV1LambdaOperand parses an edition-2026 lambda where the legacy
// grammar meets an operand, its body through the v1 parser: `(row => body)`,
// the parenthesised lambda the struct-form rewriter joins a v1 filter with;
// `(a, b) => body`; and `x => body`. ok=false, consuming nothing, when the
// cursor is at none of them.
//
// Purely additive: every one of these shapes failed to parse in the internal
// query form, which read the name and then choked on `=>`. A collection
// method's lambda argument never reaches here -- parseMethodArg claims
// `x => ...` and `(a, b) => ...` first.
func (p *Parser) tryParseV1LambdaOperand() (ExpressionNode, bool, error) {
	switch {
	case p.check(TokenParenOpen) && p.peekAhead(1).Type == TokenIdentifier && p.isArrowAt(2):
		open := p.current
		p.advance() // (
		e, err := p.parseV1Lambda()
		if err != nil {
			return nil, true, err
		}
		if err := p.refuseCommaAfterLambda(); err != nil {
			return nil, true, err
		}
		if !p.check(TokenParenClose) {
			return nil, true, p.v1Expected("`)` to close the lambda opened at " + v1Where(open))
		}
		p.advance()
		return e.n, true, nil
	case p.check(TokenParenOpen) && p.v1LambdaAhead():
		e, err := p.parseV1Lambda()
		if err != nil {
			return nil, true, err
		}
		return e.n, true, nil
	case p.check(TokenIdentifier) && p.isArrowAt(1):
		e, err := p.parseV1Lambda()
		if err != nil {
			return nil, true, err
		}
		return e.n, true, nil
	}
	return nil, false, nil
}

// parseOneParamLambda parses a v1 expression mid-stream and requires it to be
// a lambda of exactly one parameter -- the shape of every predicate position:
// a filter, a spec or trait body, a trigger filter, refine. what names the
// position for the refusal.
func (p *Parser) parseOneParamLambda(what string) (*ast.LambdaExpr, error) {
	start := p.current
	n, err := p.parseV1Expression()
	if err != nil {
		return nil, err
	}
	lam, ok := n.(*ast.LambdaExpr)
	if !ok {
		return nil, v1Errorf(start, "%s takes a lambda of one parameter, as in row => <predicate>; got `%s`", what, ast.FormatExpr(n))
	}
	if len(lam.Params) != 1 {
		return nil, v1Errorf(start, "%s takes a lambda of one parameter (the row), got %d: row => <predicate>", what, len(lam.Params))
	}
	return lam, nil
}

// parseRefineFunction parses the internal `refine(<paginated query>, <lambda>)`
// directive the struct-form rewriter emits for a `refine` clause. The target
// must be the paginate wrapper: refine runs over the page paginate reads, and
// a refine over anything else is a scan of the whole matching set in process.
// The opening `(` is already consumed (parseFunctionCall).
func (p *Parser) parseRefineFunction() (ExpressionNode, error) {
	if p.check(TokenParenClose) {
		return nil, newParseErrorf(&p.current, "refine() requires a paginated query and a lambda: refine(paginate(<query>, n), row => <predicate>)")
	}
	targetTok := p.current
	target, err := p.parseDirectiveTarget()
	if err != nil {
		return nil, err
	}
	if _, ok := target.(*PaginateExpr); !ok {
		return nil, v1Errorf(targetTok, "refine runs over the page paginate reads, so its target is a paginate(...) wrapper; got %T", target)
	}
	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "expected ',' before the refine lambda, got %q", p.current.Literal)
	}
	p.advance()
	lam, err := p.parseOneParamLambda("refine")
	if err != nil {
		return nil, err
	}
	if err := p.refuseCommaAfterLambda(); err != nil {
		return nil, err
	}
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &RefineExpr{Target: target, Lambda: lam}, nil
}

// refuseCommaAfterLambda refuses a `,` directly after the lambda of a
// predicate clause -- a filter, a refine clause, a spec or trait, @filter.
// The lambda is the whole clause, so a comma there can only be the retired
// `,` connective (`filter row => a, b`), and it gets that refusal, naming
// `||` and the migrator, rather than the "expected )" the enclosing construct
// would otherwise report. @trigger(filter=...) is the one place a comma
// legitimately follows the lambda -- it separates the annotation's
// arguments -- and does not call this.
func (p *Parser) refuseCommaAfterLambda() error {
	if p.check(TokenComma) {
		return v1Retired(p.current, ruleCommaConnective)
	}
	return nil
}

// refuseAfterPredicate refuses a token left on the line of a spec's or
// trait's lambda. The lambda is the one predicate clause no bracket or brace
// closes: it is parsed mid-stream and stops at the first token that cannot
// extend it, so a word the grammar does not know (`row => row.a == 1 or
// row.b == 2`) would end the predicate early and everything after it would be
// dropped -- the declaration would load meaning less than its author wrote.
// A declaration that follows starts on a line of its own. what names the
// predicate for the message.
func (p *Parser) refuseAfterPredicate(what string) error {
	if p.check(TokenEOF) || p.pos == 0 || p.current.Line != p.tokens[p.pos-1].Line {
		return nil
	}
	return p.v1Trailing(what)
}

// v1Trailing refuses the token at the cursor, left over after what: a known
// mistake by its fix, an English connective by the operator that replaces
// it, anything else as unexpected.
func (p *Parser) v1Trailing(what string) error {
	if err := p.v1RefuseMistake(); err != nil {
		return err
	}
	if p.check(TokenIdentifier) {
		switch p.current.Literal {
		case "or":
			return v1Errorf(p.current, "`or` is not an operator: write `||`")
		case "and":
			return v1Errorf(p.current, "`and` is not an operator: write `&&`")
		}
	}
	return v1Errorf(p.current, "unexpected %s after %s", v1Describe(p.current), what)
}

// v1FilterLambdaAhead reports whether the cursor opens a lambda: `x =>`, or a
// parenthesised parameter list and `=>`.
func (p *Parser) v1FilterLambdaAhead() bool {
	return (p.check(TokenIdentifier) && p.isArrowAt(1)) || p.v1LambdaAhead()
}

// parseAttributeArgValue parses one named attribute argument's value.
// @trigger's filter= takes the edition-2026 lambda @filter takes (memql#5364),
// stored as the node; any other value is the retired raw-text filter and is
// refused, as a raw-text @filter is. Every other argument takes the attribute
// value grammar.
func (p *Parser) parseAttributeArgValue(attrName, argName string, argTok Token) (any, error) {
	if attrName == AttrTrigger && argName == "filter" {
		if p.v1FilterLambdaAhead() {
			lam, err := p.parseOneParamLambda("@trigger(filter=...)")
			if err != nil {
				return nil, err
			}
			return lam, nil
		}
		return nil, v1Retired(argTok, ruleFilterAnnotation)
	}
	return p.parseValue()
}

// formatV1 is ast.FormatExpr: canonical edition-2026 source.
func formatV1(n ExpressionNode) string { return ast.FormatExpr(n) }

// checkV1QueryFilter refuses a query whose filter is not a lambda. A struct-form query reaches the parser as
// `[directives](concept==<id> [&& (<filter>)])`, so the filter is the right
// operand of the join under the directive wrappers. from is the index of the
// body's first token.
func (p *Parser) checkV1QueryFilter(body ExpressionNode, from int) error {
	base := unwrapQueryDirectives(body)
	and, ok := base.(*LogicalExpr)
	if !ok || and.Op != LogicalAnd {
		return nil
	}
	if cmp, ok := and.Left.(*ComparisonExpr); !ok || cmp.Field.Raw != "concept" {
		return nil
	}
	if _, isLambda := and.Right.(*ast.LambdaExpr); isLambda {
		return nil
	}
	first, last := p.v1FilterExtent(from)
	err := v1Retired(first, ruleFilterWithoutLambda)
	// The refusal covers the whole predicate the rewrite converts, so an
	// editor's squiggle -- and the quick fix keyed on it -- is that clause on
	// its own line(s), not one token of it.
	err.(*RetiredFormError).Parse.setEnd(last)
	return err
}

// v1FilterExtent is the first and last token of the filter a struct-form
// query joins as `concept==<id> && (<filter>)`, searched from the body's
// first token: the refusal of a filter belongs on the author's text, and every
// token around the filter is the rewriter's. The body's first token is the
// fallback for both.
func (p *Parser) v1FilterExtent(from int) (first, last Token) {
	for i := from; i+5 < len(p.tokens); i++ {
		t := p.tokens[i]
		if t.Type == TokenBraceClose {
			break
		}
		if t.Type == TokenIdentifier && t.Literal == "concept" && p.tokens[i+1].Literal == "==" &&
			p.tokens[i+3].Type == TokenAmpAmp && p.tokens[i+4].Type == TokenParenOpen {
			depth := 0
			for j := i + 5; j < len(p.tokens); j++ {
				switch p.tokens[j].Type {
				case TokenParenOpen, TokenBracketOpen, TokenBraceOpen:
					depth++
				case TokenParenClose, TokenBracketClose, TokenBraceClose:
					if depth == 0 {
						return p.tokens[i+5], p.tokens[max(j-1, i+5)]
					}
					depth--
				case TokenEOF:
					return p.tokens[i+5], p.tokens[i+5]
				}
			}
			return p.tokens[i+5], p.tokens[i+5]
		}
	}
	fallback := p.current
	if from < len(p.tokens) {
		fallback = p.tokens[from]
	}
	return fallback, fallback
}

// unwrapQueryDirectives strips the directive wrappers a struct-form query's
// clauses lower to, down to the concept-and-filter base.
func unwrapQueryDirectives(n ExpressionNode) ExpressionNode {
	for {
		switch e := n.(type) {
		case *ShapeExpr:
			n = e.Target
		case *CountExpr:
			n = e.Target
		case *PaginateExpr:
			n = e.Target
		case *SortExpr:
			n = e.Target
		case *TimestampExpr:
			n = e.Target
		case *DepthExpr:
			n = e.Target
		case *SelectExpr:
			n = e.Target
		case *RefineExpr:
			n = e.Target
		default:
			return n
		}
	}
}

// ---------------------------------------------------------------------------
// In-process positions.
// ---------------------------------------------------------------------------

// parseMutationValue parses one insert()/update() argument value, a v1 node.
func (p *Parser) parseMutationValue() (any, error) {
	n, err := p.parseV1Expression()
	if err != nil {
		return nil, err
	}
	return n, nil
}

// parseV1Payload parses an insert()/update() payload -- a map literal -- into
// the node, and its canonical source for PayloadRaw.
func (p *Parser) parseV1Payload() (ExpressionNode, string, error) {
	if !p.check(TokenBraceOpen) {
		return nil, "", v1Errorf(p.current, "a payload is a map literal, { key: value, ... }; got %s", v1Describe(p.current))
	}
	e, err := p.parseV1MapWith(false)
	if err != nil {
		return nil, "", err
	}
	return e.n, formatV1(e.n), nil
}
