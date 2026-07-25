package parser

// parseValueMaybeCoalesce parses a value slot and folds any following
// `??` chain into a single flat CoalesceExpr (memql#2772).
//
// #2611 gave the operator its own cascade level, but several VALUE slots
// do not route through the cascade -- they call parseValue() directly and
// hand the result to a caller that then expects `,` or `)`. A trailing
// `??` therefore surfaced as `expected ')', got "??"` in:
//
//   - named and positional args of a construct call
//     (`query q( planId: x ?? "" )`)
//   - the `id=` / `createdAt=` / `parent=` / `aliasOf=` slots of the
//     normalised `insert(...)` / `update(...)` write form, which is what
//     an authored `insert { id: ... }` field becomes
//
// The fold is n-ary and flat -- `a ?? b ?? c` is ONE CoalesceExpr with
// three arms, identical to what `coalesce(a, b, c)` produces -- which is
// what keeps #2611's central claim true: every downstream consumer of the
// node is untouched by construction.
//
// A slot with no `??` returns exactly what parseValue produced, so this
// is a drop-in for parseValue at any value position.
func (p *Parser) parseValueMaybeCoalesce() (any, error) {
	val, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	if !p.check(TokenQuestionQuestion) {
		return val, nil
	}
	args := []ExpressionNode{p.valueToExprNode(val)}
	for p.check(TokenQuestionQuestion) {
		p.advance()
		rhs, rerr := p.parseValue()
		if rerr != nil {
			return nil, rerr
		}
		args = append(args, p.valueToExprNode(rhs))
	}
	return &CoalesceExpr{Args: args}, nil
}
