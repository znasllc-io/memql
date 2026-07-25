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
	startPos, startCur := p.pos, p.current
	val, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	if !p.check(TokenQuestionQuestion) {
		return val, nil
	}
	// Rewind and re-parse the WHOLE chain through the operand parser the
	// coalesce() CALL form uses, instead of folding the parseValue
	// results (memql#2766 review).
	//
	// parseValue returns a bare Go string for an identifier and
	// valueToExprNode wraps that in a LiteralExpr, so `body ?? ""`
	// compiled to coalesce("body", "") -- the automation then wrote the
	// identifier's own NAME into the row (`summary: "body"`), which is
	// the memql#580 render-the-identifier-as-its-own-name bug class. The
	// coalesce() spelling never had this: its args go through
	// parsePrimary, which keeps a reference a reference.
	//
	// Same rewind-and-reparse the comparison-value position already does
	// for a pending `??` (#2611 review round 2, finding B).
	p.pos, p.current = startPos, startCur
	return p.parseCoalesceArgOperand()
}
