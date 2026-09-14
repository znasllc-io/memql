package parser

// v1_positions.go -- where the construct parsers hand an expression to the
// edition-2026 grammar (task memql#5364, Task 3 of the DSL v1 expressions
// plan), and the transition that lets the tree keep loading while it happens.
//
// # The transition
//
// The PUSHDOWN positions -- a query's `filter`, a spec or trait body, an
// automation's `@filter` -- have v1 spellings no legacy spelling can be
// mistaken for (`row => ...`, `= row => ...`), so both are accepted side by
// side, always: the tree migrates construct by construct and every step of it
// loads.
//
// The IN-PROCESS positions -- conditions, forEach sources and filters, switch
// subjects, step-call arguments, logic statements, mutation values -- reuse
// spellings the two grammars read differently (`cond(`, `concat(`, a bare
// `payload.x`), so they change grammar as ONE unit behind
// Options.ExpressionsV1. With it on, each position parses with the v1 parser,
// its existing string field holds the canonical v1 source (ast.FormatExpr),
// and the node sits beside it (StepDef.ConditionExpr, ForEachStepConfig.
// SourceExpr/FilterExpr, SwitchStepConfig.ExpressionExpr, MutationStmt.
// PayloadExpr, or the v1 node itself inside a value map); and the legacy
// pushdown spellings are refused naming memqlmigrate --rewrite=expressions.
// With it off, nothing an existing construct parses to changes.
//
// The engine's internal query form -- the string an SDK sends to Execute --
// keeps the legacy grammar in both modes. The one thing the legacy grammar
// learns here is to read a v1 lambda where it meets an operand, because that
// is how a struct-form query's v1 filter reaches it:
// `concept==<id> && (row => ...)`.

import (
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// tryParseV1LambdaOperand parses an edition-2026 lambda where the legacy
// grammar meets an operand, its body through the v1 parser: `(row => body)`,
// the parenthesised lambda the struct-form rewriter joins a v1 filter with;
// `(a, b) => body`; and `x => body`. ok=false, consuming nothing, when the
// cursor is at none of them.
//
// Purely additive: every one of these shapes failed to parse in the legacy
// grammar, which read the name and then choked on `=>`. A collection method's
// lambda argument never reaches here -- parseMethodArg claims `x => ...` and
// `(a, b) => ...` first, with a legacy body -- so today's logic bodies are
// unchanged.
func (p *Parser) tryParseV1LambdaOperand() (ExpressionNode, bool, error) {
	switch {
	case p.check(TokenParenOpen) && p.peekAhead(1).Type == TokenIdentifier && p.isArrowAt(2):
		open := p.current
		p.advance() // (
		e, err := p.parseV1Lambda()
		if err != nil {
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
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &RefineExpr{Target: target, Lambda: lam}, nil
}

// v1FilterLambdaAhead reports whether the cursor opens a lambda: `x =>`, or a
// parenthesised parameter list and `=>`.
func (p *Parser) v1FilterLambdaAhead() bool {
	return (p.check(TokenIdentifier) && p.isArrowAt(1)) || p.v1LambdaAhead()
}

// parseAttributeArgValue parses one named attribute argument's value.
// @trigger's filter= takes the edition-2026 lambda @filter takes (memql#5364),
// stored as the node; with ExpressionsV1 on its legacy raw-text value is
// refused, as a legacy @filter is. Everything else is today's value grammar.
func (p *Parser) parseAttributeArgValue(attrName, argName string, argTok Token) (any, error) {
	if attrName == AttrTrigger && argName == "filter" {
		if p.v1FilterLambdaAhead() {
			lam, err := p.parseOneParamLambda("@trigger(filter=...)")
			if err != nil {
				return nil, err
			}
			return lam, nil
		}
		if p.opts.ExpressionsV1 {
			return nil, v1Retired(argTok, ruleFilterAnnotation)
		}
	}
	return p.parseValue()
}

// formatV1 is ast.FormatExpr: canonical edition-2026 source.
func formatV1(n ExpressionNode) string { return ast.FormatExpr(n) }

// checkV1QueryFilter refuses, with ExpressionsV1 on, a query whose filter is
// not a lambda. A struct-form query reaches the parser as
// `[directives](concept==<id> [&& (<filter>)])`, so the filter is the right
// operand of the join under the directive wrappers. from is the index of the
// body's first token.
func (p *Parser) checkV1QueryFilter(body ExpressionNode, from int) error {
	if !p.opts.ExpressionsV1 {
		return nil
	}
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
// In-process positions, behind Options.ExpressionsV1.
// ---------------------------------------------------------------------------

// parseStepCondition parses a step's condition, up to the `{` that opens its
// block: with ExpressionsV1 on, a v1 expression -- its canonical source and
// its node -- and otherwise today's canonicalised string and no node.
func (p *Parser) parseStepCondition() (string, ExpressionNode, error) {
	if !p.opts.ExpressionsV1 {
		s, err := p.parseConditionExpression()
		return s, nil, err
	}
	n, err := p.parseV1Expression()
	if err != nil {
		return "", nil, err
	}
	return formatV1(n), n, nil
}

// stepBlockAhead reports whether a step's right-hand side is a step-type
// block -- `query { ... }`, `mutation if c { ... }`, `parallel { ... }`,
// `action { ... }`, or one of the retired inline blocks, which the step-type
// switch refuses by name -- rather than an expression.
func (p *Parser) stepBlockAhead() bool {
	if p.check(TokenKeywordQuery) || p.check(TokenKeywordMutation) {
		return true
	}
	if !p.check(TokenIdentifier) {
		return false
	}
	if next := p.peekAhead(1).Type; next != TokenBraceOpen && next != TokenKeywordIf {
		return false
	}
	switch strings.ToLower(p.current.Literal) {
	case "query", "mutation", "parallel", "action", "shape", "webhook", "event", "publishevent":
		return true
	}
	return false
}

// v1Step is the step a v1 right-hand side makes. A call to a function or a
// construct is a function step, its Args map holding the v1 argument nodes
// under the keys the legacy call step used -- the argument's name, or its
// position ("0", "1", ...) -- and anything else is a query step carrying the
// node, which the runtime evaluates in process.
func v1Step(id string, retry int, n ExpressionNode) *StepDef {
	if call, ok := n.(*ast.CallExpr); ok && call.Receiver == nil {
		return &StepDef{ID: id, Type: StepTypeFunction, RetryCount: retry, Config: &FunctionStepConfig{Name: call.Name, Args: v1CallArgs(call)}}
	}
	return &StepDef{ID: id, Type: StepTypeQuery, RetryCount: retry, Config: &QueryStepConfig{Query: n}}
}

// v1CallArgs is a v1 call's arguments as a step's Args map.
func v1CallArgs(call *ast.CallExpr) map[string]any {
	args := make(map[string]any, len(call.Args)+len(call.Named))
	for i, a := range call.Args {
		args[strconv.Itoa(i)] = a
	}
	for _, a := range call.Named {
		args[a.Name] = a.Value
	}
	return args
}

// parseV1StepCall parses a statement that must be one call -- a conditional
// step's body, a bare call in an if-body -- returning the call.
func (p *Parser) parseV1StepCall(what string) (*ast.CallExpr, error) {
	start := p.current
	n, err := p.parseV1Expression()
	if err != nil {
		return nil, err
	}
	call, ok := n.(*ast.CallExpr)
	if !ok || call.Receiver != nil {
		return nil, v1Errorf(start, "%s must be a function or construct call, got `%s`", what, formatV1(n))
	}
	return call, nil
}

// ifStatementToStepsV1 is ifStatementToSteps over v1 conditions: each step
// under the if carries the if's condition, ANDed with its own, and the else
// branch carries its negation -- as nodes, so the stamped Condition string is
// canonical v1 source rather than the legacy `(a) and (b)` / `not (...)`.
func ifStatementToStepsV1(stmt *IfStmt) []StepDef {
	if stmt == nil {
		return nil
	}
	cond := stmt.Condition
	var out []StepDef
	for _, step := range stmt.ThenSteps {
		out = append(out, stampStepConditionV1(step, cond))
	}
	negated := &ast.UnaryExpr{Op: "!", Operand: cond}
	if stmt.ElseIf != nil {
		for _, step := range ifStatementToStepsV1(stmt.ElseIf) {
			out = append(out, stampStepConditionV1(step, negated))
		}
	} else {
		for _, step := range stmt.ElseSteps {
			out = append(out, stampStepConditionV1(step, negated))
		}
	}
	return out
}

// stampStepConditionV1 ANDs outer onto a step's condition, and onto every step
// inside a for-range the step owns (they do not inherit it otherwise).
func stampStepConditionV1(step StepDef, outer ExpressionNode) StepDef {
	if outer == nil {
		return step
	}
	if step.ConditionExpr == nil {
		step.ConditionExpr = outer
	} else {
		step.ConditionExpr = &ast.BinaryExpr{Op: "&&", Left: outer, Right: step.ConditionExpr}
	}
	step.Condition = formatV1(step.ConditionExpr)
	if cfg, ok := step.Config.(*ForEachStepConfig); ok && cfg != nil {
		for i := range cfg.Do {
			cfg.Do[i] = stampStepConditionV1(cfg.Do[i], outer)
		}
	}
	return step
}

// parseMutationValue parses one insert()/update() argument value: a v1 node
// with ExpressionsV1 on, today's template value otherwise.
func (p *Parser) parseMutationValue() (any, error) {
	if !p.opts.ExpressionsV1 {
		return p.parseValueMaybeCoalesce()
	}
	n, err := p.parseV1Expression()
	if err != nil {
		return nil, err
	}
	return n, nil
}

// parseV1Payload parses an insert()/update() payload -- a map literal -- with
// ExpressionsV1 on: the node, and its canonical source for PayloadRaw.
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

// parseV1ArgsMap parses a step config's `args: { ... }` with ExpressionsV1 on,
// into an Args map of v1 nodes. allowPuns admits a bare name as `name: name`:
// the struct-form rewriter lowers `action f(workdir, ref: ref)` into
// `args: { workdir, ref: ref }`, so an action's args map is a construct call's
// argument list in map clothing, and a construct call's bare name puns.
func (p *Parser) parseV1ArgsMap(allowPuns bool) (map[string]any, error) {
	if !p.check(TokenBraceOpen) {
		return nil, v1Errorf(p.current, "args is a map literal, { name: value, ... }; got %s", v1Describe(p.current))
	}
	e, err := p.parseV1MapWith(allowPuns)
	if err != nil {
		return nil, err
	}
	m := e.n.(*ast.MapExpr)
	args := make(map[string]any, len(m.Entries))
	for _, en := range m.Entries {
		args[en.Key] = en.Value
	}
	return args, nil
}
