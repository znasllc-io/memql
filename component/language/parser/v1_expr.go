package parser

// v1_expr.go -- the edition-2026 expression grammar (epic memql#5363, task
// memql#5364; D1, D8, D9, D10 and D24 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// A second, separate recursive descent over the same token stream, producing
// the v1 node set of component/language/ast/v1.go. The legacy expression
// grammar (parseExpression .. parsePrimary in parser.go) is untouched and stays
// the parser of the engine's internal query form -- the string an SDK sends to
// Execute and the wrapper the struct-form rewriter generates -- because that
// string is a wire contract, and a grammar change there is a client break.
//
// # One function per level, tightest first
//
//	postfix   .f  .?f  f(...)  .m(...)                           left
//	unary     !  -                                                right
//	          *  /  %                                             left
//	          +  -                                                left
//	          ??                                                  left (folds n-ary)
//	          ==  !=  <  <=  >  >=  in  startsWith                none
//	          &&                                                  left
//	          ||                                                  left
//	          c ? a : b                                           right
//	lambda    x => body   (x, y) => body                          right
//
// V1PrecedenceTable returns the same table as data; the tests generate cases
// from it and fail when this file and the table disagree.
//
// # Decisions a reader would otherwise have to rediscover
//
//   - A lambda is recognised where a PRIMARY is, by lookahead (`x =>`,
//     `(x, y) =>`, `() =>`), and its body is the loosest expression, so it
//     extends as far right as it can: `a && x => x || b` is `a && (x => (x ||
//     b))`. A lambda is therefore legal as an operand, and the printer writes
//     the parentheses that make that reading visible.
//   - Comparisons do not associate. `a < b < c` is refused rather than read as
//     `(a < b) < c`, which compares a boolean with c and is never what was
//     meant.
//   - `??` binds tighter than comparison (D9): `args.stage ?? "" == "x"`
//     groups the way it reads.
//   - The lexer glues a '-' to a following digit (`-5` is one number token),
//     because it cannot know position. A negative literal is kept as a
//     LiteralExpr -- a filter's `row.score > -5` must lower as a constant,
//     and negation is not a pushdown node -- and `- 5` (the operator, then a
//     number) folds into the same literal, so every spelling of minus-five is
//     one node and the printer's `-5` re-reads as what it printed. Only when a
//     postfix follows (`-5.x`) is the glued sign a unary minus over the member,
//     because postfix binds tighter than unary.
//   - The lexer fuses a dotted path into one identifier (`row.items.any`) and
//     a `.field` tail after `)` into an identifier starting with `.`. Both are
//     split here into IdentExpr plus MemberExpr, and a `(` after the last
//     segment makes it a method call on the rest.
//   - A `:` or a `-` inside such a name is refused: in expression position
//     `v1:crm:lead` is a string that lost its quotes and `total-used` is a
//     subtraction that lost its spaces, and reading either as a name is the
//     silent misreading memql#3624 documented.
//   - Four reused node types (LiteralExpr, NilExpr, LambdaExpr, TernaryExpr)
//     carry no Span field, so every level returns a v1Expr: the node and its
//     extent. A parent's span therefore still starts and ends where its
//     children do.
//   - Recursion is bounded (v1MaxDepth): ten thousand `(` is a parse error,
//     not a goroutine stack overflow the process cannot survive.

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/core/baseparser"
)

// PrecedenceLevel is one row of the edition-2026 precedence table (D9). Level
// 1 binds tightest. Operators are written the way the language reference
// prints them; Assoc is "left", "right" or "none".
type PrecedenceLevel struct {
	Level     int
	Operators []string
	Assoc     string
}

// v1Precedence is the table. The parser does not interpret it -- one function
// per level encodes it -- so TestV1PrecedenceTableMatchesTheParser derives
// cases from it, and TestV1PrecedenceTableIsPublished holds the language
// reference to it.
var v1Precedence = []PrecedenceLevel{
	{Level: 1, Operators: []string{".f", ".?f", "f(...)", ".m(...)"}, Assoc: "left"},
	{Level: 2, Operators: []string{"!", "-"}, Assoc: "right"},
	{Level: 3, Operators: []string{"*", "/", "%"}, Assoc: "left"},
	{Level: 4, Operators: []string{"+", "-"}, Assoc: "left"},
	{Level: 5, Operators: []string{"??"}, Assoc: "left"},
	{Level: 6, Operators: []string{"==", "!=", "<", "<=", ">", ">=", "in", "startsWith"}, Assoc: "none"},
	{Level: 7, Operators: []string{"&&"}, Assoc: "left"},
	{Level: 8, Operators: []string{"||"}, Assoc: "left"},
	{Level: 9, Operators: []string{"c ? a : b"}, Assoc: "right"},
	{Level: 10, Operators: []string{"x => body", "(x, y) => body"}, Assoc: "right"},
}

// V1PrecedenceTable returns the edition-2026 precedence table, tightest level
// first. The slices are the caller's.
func V1PrecedenceTable() []PrecedenceLevel {
	out := make([]PrecedenceLevel, len(v1Precedence))
	for i, lv := range v1Precedence {
		out[i] = PrecedenceLevel{Level: lv.Level, Operators: append([]string(nil), lv.Operators...), Assoc: lv.Assoc}
	}
	return out
}

// v1MaxDepth bounds how deeply an expression may nest: brackets, call
// arguments, lambda bodies, conditional branches and prefix operators each
// count one. Real expressions nest a handful deep; the bound exists so that
// hostile or generated input costs a refusal, never the process.
const v1MaxDepth = 256

// ParseV1Expression parses src as exactly one edition-2026 expression. Errors
// carry a line and column; a retired spelling is a *RetiredFormError naming
// its replacement and the migrator that writes it.
func ParseV1Expression(src string) (ast.ExpressionNode, error) {
	p, err := newV1Parser(src)
	if err != nil {
		return nil, err
	}
	e, err := p.parseV1Ternary()
	if err != nil {
		return nil, err
	}
	if err := p.v1ExpectEnd(); err != nil {
		return nil, err
	}
	return e.n, nil
}

// ParseV1Lambda parses src as exactly one lambda, `x => body` or
// `(x, y) => body`. A parenthesised lambda is refused: a clause that holds a
// lambda has one spelling, and the gates that read clauses with this function
// should not have to strip parentheses to find it.
func ParseV1Lambda(src string) (*ast.LambdaExpr, error) {
	p, err := newV1Parser(src)
	if err != nil {
		return nil, err
	}
	first := p.current
	e, err := p.parseV1Ternary()
	if err != nil {
		return nil, err
	}
	if err := p.v1ExpectEnd(); err != nil {
		return nil, err
	}
	if lam, ok := e.n.(*ast.LambdaExpr); ok {
		return lam, nil
	}
	if _, ok := ast.Unparen(e.n).(*ast.LambdaExpr); ok {
		return nil, v1Errorf(first, "expected a lambda, got one in parentheses: write it without them, as in %s", ast.FormatExpr(ast.Unparen(e.n)))
	}
	return nil, v1Errorf(first, "expected a lambda (x => body, or (x, y) => body), got `%s`", ast.FormatExpr(e.n))
}

func newV1Parser(src string) (*Parser, error) {
	tokens, err := NewLexer(src).Tokenize()
	if err != nil {
		return nil, fmt.Errorf("tokenize: %w", err)
	}
	return NewParser(tokens), nil
}

// parseV1Expression is the entry the construct parsers call mid-stream: it
// parses one expression and stops at the first token that cannot extend it --
// a `{` or `}` after a complete operand, the `)` or `]` of an enclosing
// bracket, a `,` outside any bracket this expression opened, a statement
// keyword, the next statement's first name, end of input. The stopping token
// is not consumed; the caller judges it. A token that can never continue an
// expression anywhere (`=`, `has`, `;`, `?.` ...) is refused here instead,
// because only here can the message name the fix.
func (p *Parser) parseV1Expression() (ast.ExpressionNode, error) {
	e, err := p.parseV1Ternary()
	if err != nil {
		return nil, err
	}
	if err := p.v1RefuseMistake(); err != nil {
		return nil, err
	}
	return e.n, nil
}

// v1Expr is a parsed node and its source extent (see the file comment for why
// the extent travels beside the node).
type v1Expr struct {
	n  ast.ExpressionNode
	sp ast.Span
}

// v1Take returns the token at the cursor and moves past it -- but never past
// end of input, so an error positioned at p.current afterwards still has a
// line and a column (Parser.advance past the end yields a zero token).
func (p *Parser) v1Take() Token {
	tok := p.current
	if tok.Type != TokenEOF {
		p.advance()
	}
	return tok
}

func (p *Parser) v1TooDeep() error {
	return v1Errorf(p.current, "the expression nests too deeply (more than %d levels): name its parts as separate statements or constructs", v1MaxDepth)
}

// v1ExpectOperand refuses a missing operand after the token `after`.
func (p *Parser) v1ExpectOperand(after Token) error {
	if v1CanStartOperand(p.current) {
		return nil
	}
	return v1Errorf(p.current, "expected an expression after `%s`, got %s", after.Literal, v1Describe(p.current))
}

// v1CanStartOperand reports whether tok can begin an operand. The retired
// prefixes count as operand starts on purpose: parseV1Primary refuses each
// with its replacement, which says more than "expected an expression".
func v1CanStartOperand(tok Token) bool {
	switch tok.Type {
	case TokenNumber, TokenString, TokenIdentifier, TokenKeywordNil,
		TokenParenOpen, TokenBracketOpen, TokenBraceOpen, TokenBang,
		TokenQuestionDot, TokenKeywordWhen, TokenKeywordNot, TokenKeywordHas:
		return true
	case TokenOperator:
		return tok.Literal == "-" || tok.Literal == "$"
	}
	return false
}

// ---------------------------------------------------------------------------
// The levels, loosest first.
// ---------------------------------------------------------------------------

// parseV1Ternary parses level 9, `c ? a : b`, right-associative, and is the
// entry every nested full expression goes through -- which is why the depth
// bound is counted here.
func (p *Parser) parseV1Ternary() (v1Expr, error) {
	p.v1Depth++
	defer func() { p.v1Depth-- }()
	if p.v1Depth > v1MaxDepth {
		return v1Expr{}, p.v1TooDeep()
	}
	cond, err := p.parseV1Or()
	if err != nil || !p.check(TokenQuestion) {
		return cond, err
	}
	q := p.v1Take()
	if err := p.v1ExpectOperand(q); err != nil {
		return v1Expr{}, err
	}
	then, err := p.parseV1Ternary()
	if err != nil {
		return v1Expr{}, err
	}
	if !p.check(TokenColon) {
		return v1Expr{}, p.v1Expected(fmt.Sprintf("`:` in the conditional whose `?` is at %s (p ? a : b)", v1Where(q)))
	}
	colon := p.v1Take()
	if err := p.v1ExpectOperand(colon); err != nil {
		return v1Expr{}, err
	}
	els, err := p.parseV1Ternary()
	if err != nil {
		return v1Expr{}, err
	}
	sp := joinV1Span(cond.sp, els.sp)
	return v1Expr{n: &ast.TernaryExpr{Condition: cond.n, Then: then.n, Else: els.n}, sp: sp}, nil
}

func (p *Parser) parseV1Or() (v1Expr, error) {
	return p.parseV1LeftAssoc(p.parseV1And, v1TokenOp(TokenPipePipe, "||"))
}

func (p *Parser) parseV1And() (v1Expr, error) {
	return p.parseV1LeftAssoc(p.parseV1Compare, v1TokenOp(TokenAmpAmp, "&&"))
}

// parseV1Compare parses level 6. It is non-associative: one comparison, and a
// second comparison operator after it is refused.
func (p *Parser) parseV1Compare() (v1Expr, error) {
	left, err := p.parseV1Coalesce()
	if err != nil {
		return v1Expr{}, err
	}
	op, ok := v1ComparisonOp(p.current)
	if !ok {
		return left, nil
	}
	opTok := p.v1Take()
	if err := p.v1ExpectOperand(opTok); err != nil {
		return v1Expr{}, err
	}
	right, err := p.parseV1Coalesce()
	if err != nil {
		return v1Expr{}, err
	}
	if next, chained := v1ComparisonOp(p.current); chained {
		return v1Expr{}, v1Errorf(p.current, "comparisons do not chain: parenthesise one side, as in (a %s b) %s c", op, next)
	}
	return v1Binary(op, left, right), nil
}

// parseV1Coalesce parses level 5. `a ?? b ?? c` folds left into nested
// BinaryExpr nodes, `(a ?? b) ?? c`, which the evaluators read n-ary.
func (p *Parser) parseV1Coalesce() (v1Expr, error) {
	return p.parseV1LeftAssoc(p.parseV1Additive, v1TokenOp(TokenQuestionQuestion, "??"))
}

func (p *Parser) parseV1Additive() (v1Expr, error) {
	return p.parseV1LeftAssoc(p.parseV1Multiplicative, v1OperatorIn("+", "-"))
}

func (p *Parser) parseV1Multiplicative() (v1Expr, error) {
	return p.parseV1LeftAssoc(p.parseV1Unary, v1OperatorIn("*", "/", "%"))
}

// parseV1LeftAssoc parses one left-associative binary level: operands from
// next, joined by any operator op recognises.
func (p *Parser) parseV1LeftAssoc(next func() (v1Expr, error), op func(Token) (string, bool)) (v1Expr, error) {
	left, err := next()
	if err != nil {
		return v1Expr{}, err
	}
	for {
		name, ok := op(p.current)
		if !ok {
			return left, nil
		}
		opTok := p.v1Take()
		if err := p.v1ExpectOperand(opTok); err != nil {
			return v1Expr{}, err
		}
		right, err := next()
		if err != nil {
			return v1Expr{}, err
		}
		left = v1Binary(name, left, right)
	}
}

// parseV1Unary parses level 2: `!` and `-` as prefixes, right-associative.
func (p *Parser) parseV1Unary() (v1Expr, error) {
	tok := p.current
	isMinus := tok.Type == TokenOperator && tok.Literal == "-"
	if tok.Type == TokenBang || isMinus {
		p.v1Depth++
		defer func() { p.v1Depth-- }()
		if p.v1Depth > v1MaxDepth {
			return v1Expr{}, p.v1TooDeep()
		}
		p.v1Take()
		if err := p.v1ExpectOperand(tok); err != nil {
			return v1Expr{}, err
		}
		operand, err := p.parseV1Unary()
		if err != nil {
			return v1Expr{}, err
		}
		sp := joinV1Span(v1TokenSpan(tok), operand.sp)
		if isMinus {
			// `- 5` is the literal -5, exactly as the glued `-5` is.
			if lit, ok := v1NegateLiteral(operand.n); ok {
				return v1Expr{n: lit, sp: sp}, nil
			}
		}
		return v1Expr{n: &ast.UnaryExpr{Op: tok.Literal, Operand: operand.n, Span: sp}, sp: sp}, nil
	}
	if tok.Type == TokenNumber && strings.HasPrefix(tok.Literal, "-") && v1StartsPostfix(p.peekAhead(1)) {
		return p.parseV1UngluedNegative()
	}
	return p.parseV1Postfix()
}

// parseV1UngluedNegative reads a glued negative number that a postfix follows
// (`-5.x`): postfix binds tighter than unary minus, so the sign applies to the
// whole member chain, and the lexer's `-5` is taken apart to say so.
func (p *Parser) parseV1UngluedNegative() (v1Expr, error) {
	tok := p.v1Take()
	magnitude, err := v1Number(tok, strings.TrimPrefix(tok.Literal, "-"))
	if err != nil {
		return v1Expr{}, err
	}
	litSp := v1TokenSpan(tok)
	litSp.Col++ // past the sign
	operand, err := p.parseV1PostfixFrom(v1Expr{n: &ast.LiteralExpr{Value: magnitude}, sp: litSp})
	if err != nil {
		return v1Expr{}, err
	}
	sp := joinV1Span(v1TokenSpan(tok), operand.sp)
	return v1Expr{n: &ast.UnaryExpr{Op: "-", Operand: operand.n, Span: sp}, sp: sp}, nil
}

// v1NegateLiteral folds a unary minus over a non-negative number literal into
// the negative literal. A literal that is already negative is left under its
// minus (`--5` stays a negation of -5), so the fold never changes a value's
// sign twice.
func v1NegateLiteral(n ast.ExpressionNode) (ast.ExpressionNode, bool) {
	lit, ok := n.(*ast.LiteralExpr)
	if !ok {
		return nil, false
	}
	switch v := lit.Value.(type) {
	case int64:
		if v >= 0 {
			return &ast.LiteralExpr{Value: -v}, true
		}
	case float64:
		if v >= 0 {
			return &ast.LiteralExpr{Value: -v}, true
		}
	}
	return nil, false
}

// parseV1Postfix parses level 1: a primary and then any run of `.f`, `.?f`
// and `.m(...)`.
func (p *Parser) parseV1Postfix() (v1Expr, error) {
	base, err := p.parseV1Primary()
	if err != nil {
		return v1Expr{}, err
	}
	if _, lambda := base.n.(*ast.LambdaExpr); lambda {
		// A lambda's body already took every postfix it could reach.
		return base, nil
	}
	return p.parseV1PostfixFrom(base)
}

func (p *Parser) parseV1PostfixFrom(base v1Expr) (v1Expr, error) {
	for {
		tok := p.current
		switch {
		case tok.Type == TokenIdentifier && strings.HasPrefix(tok.Literal, "."):
			// The `.field` tail the lexer fused after `)`, `]`, `}`, a
			// literal or whitespace: `f(x).a.b` scans as `f`, `(`, `x`,
			// `)`, `.a.b`.
			segs, err := v1PathSegments(tok)
			if err != nil {
				return v1Expr{}, err
			}
			p.v1Take()
			if base, err = p.v1MemberChain(base, segs, false); err != nil {
				return v1Expr{}, err
			}
		case tok.Type == TokenDotQuestion:
			p.v1Take()
			name := p.current
			if name.Type == TokenNumber {
				return v1Expr{}, v1Errorf(name, "`.?` reads a named field; an element is read with `.`, as in args.items.0")
			}
			if !v1IsNameToken(name) || strings.HasPrefix(name.Literal, ".") {
				return v1Expr{}, v1Errorf(name, "expected a field name after `.?`, got %s", v1Describe(name))
			}
			// The name may itself be a fused path, `.?lineage.planId`: only
			// its FIRST segment is optional.
			segs, err := v1PathSegments(name)
			if err != nil {
				return v1Expr{}, err
			}
			p.v1Take()
			if base, err = p.v1MemberChain(base, segs, true); err != nil {
				return v1Expr{}, err
			}
		default:
			return base, nil
		}
	}
}

// v1MemberChain applies segs to base as members; a `(` after the last segment
// makes it a method call instead. optionalFirst marks the first member `.?`.
func (p *Parser) v1MemberChain(base v1Expr, segs []v1Seg, optionalFirst bool) (v1Expr, error) {
	for i, seg := range segs {
		optional := optionalFirst && i == 0
		if i == len(segs)-1 && p.check(TokenParenOpen) {
			if optional {
				return v1Expr{}, v1Errorf(seg.token(), "`.?` reads a field; a method is called with `.`, as in x.%s(...)", seg.text)
			}
			return p.parseV1MethodCall(base, seg)
		}
		sp := joinV1Span(base.sp, seg.sp)
		base = v1Expr{n: &ast.MemberExpr{Object: base.n, Field: seg.text, Optional: optional, Span: sp}, sp: sp}
	}
	return base, nil
}

func (p *Parser) parseV1MethodCall(recv v1Expr, seg v1Seg) (v1Expr, error) {
	if !v1IsCallableName(seg.text) {
		return v1Expr{}, v1Errorf(seg.token(), "`%s` is not a method name: a method name starts with a letter", seg.text)
	}
	if seg.text == "contains" {
		return v1Expr{}, v1Retired(seg.token(), ruleContainsMethod)
	}
	args, named, closeTok, err := p.parseV1CallArgs(seg.text, "", false)
	if err != nil {
		return v1Expr{}, err
	}
	sp := joinV1Span(recv.sp, v1TokenSpan(closeTok))
	return v1Expr{n: &ast.CallExpr{Receiver: recv.n, Name: seg.text, Args: args, Named: named, Span: sp}, sp: sp}, nil
}

// parseV1Primary parses a literal, a name (and what a name can begin: a call,
// a construct call, a member chain), a lambda, a group, a list or a map.
func (p *Parser) parseV1Primary() (v1Expr, error) {
	tok := p.current
	switch tok.Type {
	case TokenNumber:
		v, err := v1Number(tok, tok.Literal)
		if err != nil {
			return v1Expr{}, err
		}
		p.v1Take()
		return v1Expr{n: &ast.LiteralExpr{Value: v}, sp: v1TokenSpan(tok)}, nil
	case TokenString:
		p.v1Take()
		return v1Expr{n: &ast.LiteralExpr{Value: tok.Literal}, sp: v1TokenSpan(tok)}, nil
	case TokenKeywordNil:
		p.v1Take()
		return v1Expr{n: &ast.NilExpr{}, sp: v1TokenSpan(tok)}, nil
	case TokenIdentifier:
		if v1IsArrow(p.peekAhead(1)) {
			return p.parseV1Lambda()
		}
		return p.parseV1Name()
	case TokenParenOpen:
		if p.v1LambdaAhead() {
			return p.parseV1Lambda()
		}
		return p.parseV1Paren()
	case TokenBracketOpen:
		return p.parseV1List()
	case TokenBraceOpen:
		return p.parseV1Map()
	case TokenQuestionDot:
		return v1Expr{}, v1Retired(tok, ruleConditionalPrefix)
	case TokenKeywordWhen:
		return v1Expr{}, v1Retired(tok, ruleWhenGuard)
	case TokenKeywordNot:
		return v1Expr{}, v1Retired(tok, ruleNotCall)
	case TokenKeywordHas:
		return v1Expr{}, v1Retired(tok, ruleHas)
	case TokenOperator:
		if tok.Literal == "$" {
			return v1Expr{}, v1Retired(tok, ruleDollarArgs)
		}
	}
	return v1Expr{}, v1Errorf(tok, "expected an expression, got %s", v1Describe(tok))
}

// parseV1Name parses what an identifier token begins. The token may be a fused
// path; its root is a literal (`true`, `false`, `nil`), a name, or -- when a
// second name follows on the same line -- a construct-call prefix.
func (p *Parser) parseV1Name() (v1Expr, error) {
	tok := p.current
	if strings.HasPrefix(tok.Literal, ".") {
		return v1Expr{}, v1Errorf(tok, "a member needs an object: `%s` reads a field of nothing; write it on a name, as in row%s", tok.Literal, tok.Literal)
	}
	segs, err := v1PathSegments(tok)
	if err != nil {
		return v1Expr{}, err
	}
	root := segs[0]
	if root.text == "null" {
		return v1Expr{}, v1Retired(tok, ruleNull)
	}
	if len(segs) == 1 {
		next := p.peekAhead(1)
		// `<word> <name>` on one line. The line condition keeps a name that
		// ends one statement from reading as the prefix of the next one,
		// which starts on its own line.
		wordThenName := next.Type == TokenIdentifier && next.Line == tok.Line && !strings.HasPrefix(next.Literal, ".")
		switch {
		case wordThenName && isInvocationKindKeyword(root.text):
			return p.parseV1ConstructCall()
		case wordThenName && root.text == "spec":
			return v1Expr{}, v1Retired(tok, ruleSpecReference)
		case wordThenName && root.text == "trait":
			return v1Expr{}, v1Retired(tok, ruleTraitReference)
		case wordThenName && p.peekAhead(2).Type == TokenParenOpen:
			// memql#2358: `mutate createNode(...)` -- the retired declaration
			// verb where `mutation` belongs -- would otherwise read as a
			// name and a separate call, dropping the call's kind silently.
			return v1Expr{}, v1Errorf(tok, "%q is not a construct-invocation kind, so the call %s(...) would be silently dropped -- a kind-prefixed call must lead with one of %s%s",
				root.text, next.Literal, renderKeywordList(invocationKindKeywordList()), didYouMean(root.text, kindSuggestionCandidates()))
		case next.Type == TokenParenOpen && root.text != "true" && root.text != "false":
			return p.parseV1FunctionCall()
		}
	}
	p.v1Take()
	var base v1Expr
	switch root.text {
	case "true", "false":
		base = v1Expr{n: &ast.LiteralExpr{Value: root.text == "true"}, sp: root.sp}
	case "nil":
		// Only a fused path (`nil.x`) reaches here; a bare `nil` is a keyword.
		base = v1Expr{n: &ast.NilExpr{}, sp: root.sp}
	default:
		base = v1Expr{n: &ast.IdentExpr{Name: root.text, Span: root.sp}, sp: root.sp}
	}
	return p.v1MemberChain(base, segs[1:], false)
}

// parseV1FunctionCall parses `name(args)`: a function, a predicate applied to
// its receiver (`isActiveRecord(row)`), or a builtin. An unknown name is not an
// error here; the engine resolves names, against the position.
//
// Three names the legacy callable dispatch refused keep their refusals, with
// the legacy messages: the builtins retired under the 2026.08 epoch (#2707)
// and caller(), which edition 2026 did not bring back, and `asOf` outside a
// query (core-builtins ADR §2.3). Without them an in-process position -- a
// logic statement, an if-condition, a step argument -- would read each as a
// call to a function nothing defines, and the migration hint would be gone.
func (p *Parser) parseV1FunctionCall() (v1Expr, error) {
	nameTok := p.v1Take()
	name := nameTok.Literal
	if !v1IsCallableName(name) {
		return v1Expr{}, v1Errorf(nameTok, "`%s` is not a function name: a function name starts with a letter", name)
	}
	lower := strings.ToLower(name)
	if rule, retired := v1RetiredCalls[lower]; retired {
		return v1Expr{}, v1Retired(nameTok, rule)
	}
	// `mutation(...)`, `query(...)`: a construct kind is not a function. A
	// construct is called by name, as a statement of its own.
	if bodyCallKinds[name] {
		return v1Expr{}, bodyRefuse(nameTok, codeBodyCallKindMissing,
			"`%s(...)` is not a call: a %s is declared elsewhere and called by name, as a statement of its own -- `x := %s <name>(<named args>)`", name, name, name)
	}
	if hint, retired := retiredExprBuiltins[lower]; retired {
		return v1Expr{}, v1Errorf(nameTok, "%s", retiredExprBuiltinMessage(name, hint))
	}
	if lower == "caller" {
		return v1Expr{}, v1Errorf(nameTok, "%s", baseparser.ErrCallerRetired.Error())
	}
	if lower == "asof" && p.asOfOutsideQuery() {
		return v1Expr{}, v1Errorf(nameTok, "%s", asOfQueryOnlyMessage(p.currentFuncType))
	}
	args, named, closeTok, err := p.parseV1CallArgs(name, "", true)
	if err != nil {
		return v1Expr{}, err
	}
	// `contains` is two things by shape: with a lambda (and an optional
	// leading label) it is the graph traversal, which stays; with two plain
	// arguments it is the retired substring test.
	if lower == "contains" && len(args) == 2 && !v1IsLambda(args[1]) {
		return v1Expr{}, v1Retired(nameTok, ruleContainsCall)
	}
	sp := joinV1Span(v1TokenSpan(nameTok), v1TokenSpan(closeTok))
	return v1Expr{n: &ast.CallExpr{Name: name, Args: args, Named: named, Span: sp}, sp: sp}, nil
}

// parseV1ConstructCall parses `<kind> <name>(args)`, kind one of the
// invocation kinds (query, mutation, logic, builtin, automation, action,
// capability).
func (p *Parser) parseV1ConstructCall() (v1Expr, error) {
	kindTok := p.v1Take()
	nameTok := p.current
	if strings.ContainsAny(nameTok.Literal, ".:-") {
		return v1Expr{}, v1Errorf(nameTok, "a construct name is a simple identifier, got `%s`: %s <name>(k: v, ...)", nameTok.Literal, kindTok.Literal)
	}
	if p.peekAhead(1).Type != TokenParenOpen {
		return v1Expr{}, v1Errorf(nameTok, "a construct call needs its argument list: %s %s(k: v, ...), or %s %s() with none", kindTok.Literal, nameTok.Literal, kindTok.Literal, nameTok.Literal)
	}
	p.v1Take()
	args, named, closeTok, err := p.parseV1CallArgs(nameTok.Literal, kindTok.Literal, false)
	if err != nil {
		return v1Expr{}, err
	}
	sp := joinV1Span(v1TokenSpan(kindTok), v1TokenSpan(closeTok))
	return v1Expr{n: &ast.CallExpr{Kind: kindTok.Literal, Name: nameTok.Literal, Args: args, Named: named, Span: sp}, sp: sp}, nil
}

// parseV1CallArgs parses a parenthesised argument list with the cursor on its
// `(`, for the call named callee. kind is the construct-call prefix ("" for a
// bare or a method call) and bare marks a bare `name(...)` call. It returns the
// closing `)` so the caller can end the call's span on it.
//
// A construct call's arguments are NAMED, and the result has no positional
// ones: a bare name is a named argument that puns (`logic f(event, mode: "x")`
// is `logic f(event: event, mode: "x")`, memql#2365), so it is recorded as
// exactly that, in source order, and a consumer never has to know a pun was
// written. Any other positional argument on a construct call is refused as the
// legacy grammar refused it (memql#2395).
//
// A bare or method call's arguments are all positional or all named: a mix is
// refused, because nothing in `f(a, k: 1)` says whether `a` fills the first
// slot or means `a: a`. A named argument's key may contain '-'
// (`dry-run: args.dryRun`): a key is never an expression.
func (p *Parser) parseV1CallArgs(callee, kind string, bare bool) ([]ast.ExpressionNode, []ast.NamedArg, Token, error) {
	open := p.v1Take()
	type argument struct {
		named *ast.NamedArg      // set for `name: value`
		value ast.ExpressionNode // set for a positional argument
		start Token
	}
	var list []argument
	seen := map[string]any{}
	for !p.check(TokenParenClose) {
		if p.check(TokenEOF) {
			return nil, nil, Token{}, p.v1Expected(fmt.Sprintf("`)` to close the argument list of %s(...) opened at %s", callee, v1Where(open)))
		}
		tok := p.current
		switch {
		case v1IsNameToken(tok) && p.peekAhead(1).Type == TokenColon:
			if strings.ContainsAny(tok.Literal, ".:") {
				return nil, nil, Token{}, v1Errorf(tok, "an argument name is one name, got `%s`", tok.Literal)
			}
			if err := checkCallArgName(&tok, callee, tok.Literal, seen); err != nil {
				return nil, nil, Token{}, err
			}
			seen[tok.Literal] = true
			p.v1Take()
			colon := p.v1Take()
			if err := p.v1ExpectOperand(colon); err != nil {
				return nil, nil, Token{}, err
			}
			val, err := p.parseV1Ternary()
			if err != nil {
				return nil, nil, Token{}, err
			}
			list = append(list, argument{named: &ast.NamedArg{Name: tok.Literal, Value: val.n}, start: tok})
		case tok.Type == TokenString && p.peekAhead(1).Type == TokenColon:
			return nil, nil, Token{}, v1Errorf(tok, "an argument's name is not quoted: write %s: ... in %s(...)", tok.Literal, callee)
		default:
			val, err := p.parseV1Ternary()
			if err != nil {
				return nil, nil, Token{}, err
			}
			if id, ok := val.n.(*ast.IdentExpr); ok && p.check(TokenOperator) && p.current.Literal == "=" {
				return nil, nil, Token{}, v1Errorf(p.current, "a named argument is written `%s: ...`, not `%s = ...`", id.Name, id.Name)
			}
			list = append(list, argument{value: val.n, start: tok})
		}
		if p.check(TokenComma) {
			p.v1Take()
			continue
		}
		if !p.check(TokenParenClose) {
			return nil, nil, Token{}, p.v1Expected(fmt.Sprintf("`,` or `)` in the argument list of %s(...)", callee))
		}
	}
	closeTok := p.v1Take()

	// The retired object-literal wrapper (memql#2335): named arguments go in
	// the parentheses, not in one map passed positionally. A method's
	// argument is data, so only bare and construct calls are held to it.
	if (bare || kind != "") && len(list) == 1 && list[0].named == nil {
		if _, isMap := list[0].value.(*ast.MapExpr); isMap {
			return nil, nil, Token{}, v1Errorf(list[0].start,
				"object-literal call args are removed; pass named args directly: %s(k: v, ...), empty = %s() (was: %s({...}))", callee, callee, callee)
		}
	}

	var (
		args  []ast.ExpressionNode
		named []ast.NamedArg
	)
	if kind != "" {
		for _, a := range list {
			if a.named != nil {
				named = append(named, *a.named)
				continue
			}
			id, pun := a.value.(*ast.IdentExpr)
			if !pun {
				return nil, nil, Token{}, v1Errorf(a.start,
					"positional args are removed on construct calls; name the argument: %s %s(k: v, ...) -- a bare name puns to its own name (%s(x) == %s(x: x))",
					kind, callee, callee, callee)
			}
			start := a.start
			if err := checkCallArgName(&start, callee, id.Name, seen); err != nil {
				return nil, nil, Token{}, err
			}
			seen[id.Name] = true
			named = append(named, ast.NamedArg{Name: id.Name, Value: id})
		}
		return nil, named, closeTok, nil
	}

	var firstPositional *Token
	for i, a := range list {
		if a.named != nil {
			named = append(named, *a.named)
			continue
		}
		if firstPositional == nil {
			firstPositional = &list[i].start
		}
		args = append(args, a.value)
	}
	if len(args) > 0 && len(named) > 0 {
		msg := fmt.Sprintf("a call's arguments are all positional or all named: name every argument of %s(...)", callee)
		for _, a := range args {
			if id, ok := a.(*ast.IdentExpr); ok {
				msg += fmt.Sprintf(" (write `%s: %s`)", id.Name, id.Name)
				break
			}
		}
		return nil, nil, Token{}, v1Errorf(*firstPositional, "%s", msg)
	}
	return args, named, closeTok, nil
}

// v1LambdaAhead reports whether the cursor is at a parenthesised lambda
// parameter list -- `()`, `(x)`, `(x, y)` or `(x, y,)`, then `=>` -- without
// consuming anything. Parameters are matched as identifier TOKENS; whether
// each is a simple name is checked after, so `(a.b) => 1` is refused as a bad
// parameter rather than parsed as a group and refused as something vaguer.
func (p *Parser) v1LambdaAhead() bool {
	if !p.check(TokenParenOpen) {
		return false
	}
	i := 1
	if p.peekAhead(i).Type == TokenParenClose {
		return v1IsArrow(p.peekAhead(i + 1))
	}
	for {
		if p.peekAhead(i).Type != TokenIdentifier {
			return false
		}
		i++
		if p.peekAhead(i).Type != TokenComma {
			break
		}
		i++
		if p.peekAhead(i).Type == TokenParenClose {
			break
		}
	}
	return p.peekAhead(i).Type == TokenParenClose && v1IsArrow(p.peekAhead(i+1))
}

// parseV1Lambda parses `x => body` or a parenthesised parameter list and its
// body. The body is the loosest expression, so it extends as far right as it
// can. Only a call on a shape v1LambdaAhead (or the `x =>` lookahead) accepted
// reaches here.
func (p *Parser) parseV1Lambda() (v1Expr, error) {
	first := p.current
	var params []Token
	if p.check(TokenIdentifier) {
		params = append(params, p.v1Take())
	} else {
		p.v1Take() // (
		for p.check(TokenIdentifier) {
			params = append(params, p.v1Take())
			if p.check(TokenComma) {
				p.v1Take()
			}
		}
		p.v1Take() // )
	}
	arrow := p.v1Take()
	names := make([]string, len(params))
	seen := map[string]bool{}
	for i, tok := range params {
		if err := v1CheckParam(tok); err != nil {
			return v1Expr{}, err
		}
		if seen[tok.Literal] {
			return v1Expr{}, v1Errorf(tok, "lambda parameter `%s` appears twice", tok.Literal)
		}
		seen[tok.Literal] = true
		names[i] = tok.Literal
	}
	if err := p.v1ExpectOperand(arrow); err != nil {
		return v1Expr{}, err
	}
	body, err := p.parseV1Ternary()
	if err != nil {
		return v1Expr{}, err
	}
	return v1Expr{n: &ast.LambdaExpr{Params: names, Body: body.n}, sp: joinV1Span(v1TokenSpan(first), body.sp)}, nil
}

// v1CheckParam refuses a parameter that is not a simple name. A reserved root
// (`actor`, `args`) is NOT refused here: the parameter of a spec over an
// @actor shape is spelled `actor` and is the actor envelope, so which names a
// position allows is decided where the lambda is bound.
func v1CheckParam(tok Token) error {
	if strings.ContainsAny(tok.Literal, ".:-") {
		return v1Errorf(tok, "a lambda parameter is a simple name, got `%s`: x => body, or (x, y) => body", tok.Literal)
	}
	switch tok.Literal {
	case "true", "false", "null":
		return v1Errorf(tok, "`%s` is a value, not a parameter name", tok.Literal)
	}
	return nil
}

// parseV1Paren parses `( expr )`. The group is kept as a ParenExpr: the
// printer reproduces it, and the pushdown tier tells a parenthesised nested
// conditional from a bare one.
func (p *Parser) parseV1Paren() (v1Expr, error) {
	open := p.v1Take()
	if p.check(TokenParenClose) {
		return v1Expr{}, v1Errorf(p.current, "empty parentheses: a group holds one expression, and a lambda with no parameters is written () => body")
	}
	if err := p.v1ExpectOperand(open); err != nil {
		return v1Expr{}, err
	}
	inner, err := p.parseV1Ternary()
	if err != nil {
		return v1Expr{}, err
	}
	switch {
	case p.check(TokenParenClose):
		closeTok := p.v1Take()
		sp := joinV1Span(v1TokenSpan(open), v1TokenSpan(closeTok))
		return v1Expr{n: &ast.ParenExpr{Inner: inner.n, Span: sp}, sp: sp}, nil
	case p.check(TokenComma):
		// A comma inside a group separates nothing: it is the retired `,`.
		return v1Expr{}, v1Retired(p.current, ruleCommaConnective)
	}
	return v1Expr{}, p.v1Expected(fmt.Sprintf("`)` to close the `(` at %s", v1Where(open)))
}

// parseV1List parses `[a, b, ...]`, a trailing comma allowed.
func (p *Parser) parseV1List() (v1Expr, error) {
	open := p.v1Take()
	var elems []ast.ExpressionNode
	for !p.check(TokenBracketClose) {
		if p.check(TokenEOF) {
			return v1Expr{}, p.v1Expected(fmt.Sprintf("`]` to close the list opened at %s", v1Where(open)))
		}
		el, err := p.parseV1Ternary()
		if err != nil {
			return v1Expr{}, err
		}
		elems = append(elems, el.n)
		if p.check(TokenComma) {
			p.v1Take()
			continue
		}
		if !p.check(TokenBracketClose) {
			return v1Expr{}, p.v1Expected(fmt.Sprintf("`,` or `]` in the list opened at %s", v1Where(open)))
		}
	}
	closeTok := p.v1Take()
	sp := joinV1Span(v1TokenSpan(open), v1TokenSpan(closeTok))
	return v1Expr{n: &ast.ListExpr{Elems: elems, Span: sp}, sp: sp}, nil
}

// parseV1Map parses `{key: value, ...}`, a trailing comma allowed. Keys are
// unquoted names (authoring rule 18) and may contain '-' (a key is never an
// expression); a key written twice is refused, because a map collapses it
// last-wins and the first value would vanish with no signal.
func (p *Parser) parseV1Map() (v1Expr, error) {
	return p.parseV1MapWith(false)
}

// parseV1MapWith is parseV1Map, admitting with allowPuns a bare name as the
// entry `name: name` -- only where a map is a construct call's argument list
// in other clothing (a step config's args, parseV1ArgsMap). A map literal an
// author writes as a value never puns.
func (p *Parser) parseV1MapWith(allowPuns bool) (v1Expr, error) {
	open := p.v1Take()
	var entries []ast.MapEntry
	seen := map[string]bool{}
	for !p.check(TokenBraceClose) {
		keyTok := p.current
		switch {
		case keyTok.Type == TokenEOF:
			return v1Expr{}, p.v1Expected(fmt.Sprintf("`}` to close the map opened at %s", v1Where(open)))
		case keyTok.Type == TokenString:
			return v1Expr{}, v1Errorf(keyTok, "map keys are unquoted names (authoring rule 18): write %s: ..., not %s: ...", keyTok.Literal, ast.QuoteString(keyTok.Literal))
		case !v1IsNameToken(keyTok):
			return v1Expr{}, v1Errorf(keyTok, "expected a map key (an unquoted name), got %s", v1Describe(keyTok))
		case strings.Contains(keyTok.Literal, ":"):
			return v1Expr{}, v1ColonError(keyTok)
		}
		if next := p.peekAhead(1); keyTok.Type == TokenIdentifier && (next.Type == TokenComma || next.Type == TokenBraceClose) {
			if err := v1KeylessEntry(keyTok, allowPuns); err != nil {
				return v1Expr{}, err
			}
		}
		if strings.Contains(keyTok.Literal, ".") {
			// Followed by `:`, so the author wrote a dotted KEY, and nesting is
			// the fix; a dotted VALUE with no key was refused just above.
			return v1Expr{}, v1Errorf(keyTok, "a map key is one name, got `%s`: nest a map for a path", keyTok.Literal)
		}
		key := keyTok.Literal
		if seen[key] {
			return v1Expr{}, v1Errorf(keyTok, "duplicate key `%s` in a map literal: the first value would be discarded with no signal; write each key once", key)
		}
		seen[key] = true
		p.v1Take()
		if !p.check(TokenColon) {
			if allowPuns && keyTok.Type == TokenIdentifier && !strings.Contains(key, "-") && (p.check(TokenComma) || p.check(TokenBraceClose)) {
				entries = append(entries, ast.MapEntry{Key: key, Value: &ast.IdentExpr{Name: key, Span: v1TokenSpan(keyTok)}})
				if p.check(TokenComma) {
					p.v1Take()
				}
				continue
			}
			if p.check(TokenComma) || p.check(TokenBraceClose) {
				// Only a keyword gets here: an identifier with no `key:` was
				// refused as a key-less entry above.
				return v1Expr{}, v1Errorf(keyTok, "a map entry is written key: value (write %s: %s)", key, key)
			}
			if p.check(TokenOperator) && p.current.Literal == "=" {
				return v1Expr{}, v1Errorf(p.current, "a map entry is written key: value, not key = value")
			}
			return v1Expr{}, p.v1Expected(fmt.Sprintf("`:` after the map key `%s`", key))
		}
		colon := p.v1Take()
		if err := p.v1ExpectOperand(colon); err != nil {
			return v1Expr{}, err
		}
		val, err := p.parseV1Ternary()
		if err != nil {
			return v1Expr{}, err
		}
		entries = append(entries, ast.MapEntry{Key: key, Value: val.n})
		if p.check(TokenComma) {
			p.v1Take()
			continue
		}
		if !p.check(TokenBraceClose) {
			return v1Expr{}, p.v1Expected(fmt.Sprintf("`,` or `}` in the map opened at %s", v1Where(open)))
		}
	}
	closeTok := p.v1Take()
	sp := joinV1Span(v1TokenSpan(open), v1TokenSpan(closeTok))
	return v1Expr{n: &ast.MapExpr{Entries: entries, Span: sp}, sp: sp}, nil
}

// ---------------------------------------------------------------------------
// Tokens, names and spans.
// ---------------------------------------------------------------------------

// v1Seg is one segment of a fused dotted identifier, with its own extent: the
// token is on one line (an identifier cannot contain a newline), so each
// segment's columns follow from the rune lengths before it -- in the lexed
// text and, when the identifier carries one, in the author's source.
type v1Seg struct {
	text string
	tok  Token
	sp   ast.Span
}

func (s v1Seg) token() Token { return s.tok }

// v1PathSegments splits an identifier token -- possibly fused, possibly the
// `.a.b` tail the lexer emits after `)` -- into its segments, refusing a
// glued ':' or '-' in any of them.
func v1PathSegments(tok Token) ([]v1Seg, error) {
	lit := tok.Literal
	if strings.Contains(lit, ":") {
		return nil, v1ColonError(tok)
	}
	if strings.Contains(lit, "-") {
		return nil, v1KebabError(tok)
	}
	off := 0 // runes from the token's start to the segment's
	if strings.HasPrefix(lit, ".") {
		lit = lit[1:]
		off = 1
	}
	parts := strings.Split(lit, ".")
	segs := make([]v1Seg, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, v1Errorf(tok, "`%s` has an empty member name", tok.Literal)
		}
		n := utf8.RuneCountInString(part)
		st := Token{Type: TokenIdentifier, Literal: part, Pos: tok.Pos + off, Line: tok.Line, Column: tok.Column + off,
			EndPos: tok.Pos + off + n, EndLine: tok.Line, EndCol: tok.Column + off + n}
		if tok.AuthoredLine > 0 {
			st.AuthoredLine, st.AuthoredCol = tok.AuthoredLine, tok.AuthoredCol+off
			st.AuthoredEndLine, st.AuthoredEndCol = tok.AuthoredLine, tok.AuthoredCol+off+n
		}
		segs = append(segs, v1Seg{text: part, tok: st, sp: v1TokenSpan(st)})
		off += n + 1
	}
	return segs, nil
}

// v1IsNameToken reports whether tok can be a field name, a map key or a named
// argument: an identifier, or a keyword, since a payload field may well be
// called `default` or `in` and a keyword never begins an expression there.
func v1IsNameToken(tok Token) bool {
	switch tok.Type {
	case TokenIdentifier,
		TokenKeywordQuery, TokenKeywordMutation, TokenKeywordAutomation,
		TokenKeywordSpec, TokenKeywordTool, TokenKeywordBuiltin,
		TokenKeywordFunc, TokenKeywordFor, TokenKeywordRange, TokenKeywordIf,
		TokenKeywordElse, TokenKeywordSwitch, TokenKeywordCase, TokenKeywordDefault,
		TokenKeywordContinue, TokenKeywordBreak, TokenKeywordReturn, TokenKeywordNil,
		TokenKeywordRetry, TokenKeywordWhen, TokenKeywordAs, TokenKeywordWhere,
		TokenKeywordUse, TokenKeywordImport, TokenKeywordConcept, TokenKeywordIn,
		TokenKeywordHas, TokenKeywordNot, TokenKeywordStartsWith:
		return true
	}
	return false
}

// v1IsCallableName reports whether a function or method name starts the way
// a name does. A fused path's numeric segment (`items.0`) is a member, never
// a method.
func v1IsCallableName(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsLetter(r) || r == '_'
}

func v1IsArrow(tok Token) bool { return tok.Type == TokenOperator && tok.Literal == "=>" }

func v1IsLambda(n ast.ExpressionNode) bool {
	_, ok := ast.Unparen(n).(*ast.LambdaExpr)
	return ok
}

// v1StartsPostfix reports whether tok continues an operand as a postfix.
func v1StartsPostfix(tok Token) bool {
	return tok.Type == TokenDotQuestion || (tok.Type == TokenIdentifier && strings.HasPrefix(tok.Literal, "."))
}

// v1ComparisonOp returns the level-6 operator tok spells, if it spells one.
func v1ComparisonOp(tok Token) (string, bool) {
	switch tok.Type {
	case TokenKeywordIn:
		return "in", true
	case TokenKeywordStartsWith:
		return "startsWith", true
	case TokenOperator:
		switch tok.Literal {
		case "==", "!=", "<", "<=", ">", ">=":
			return tok.Literal, true
		}
	}
	return "", false
}

func v1TokenOp(typ TokenType, name string) func(Token) (string, bool) {
	return func(tok Token) (string, bool) { return name, tok.Type == typ }
}

func v1OperatorIn(ops ...string) func(Token) (string, bool) {
	return func(tok Token) (string, bool) {
		if tok.Type != TokenOperator {
			return "", false
		}
		for _, op := range ops {
			if tok.Literal == op {
				return op, true
			}
		}
		return "", false
	}
}

// v1Number reads a number token: an int64 when it has no fraction or
// exponent, otherwise a float64 -- the rule the legacy parser and the attribute
// parser share (baseparser.ParseNumericLiteral), so a literal has one type
// whichever grammar read it.
func v1Number(tok Token, lit string) (any, error) {
	v, err := baseparser.ParseNumericLiteral(lit)
	if err != nil {
		return nil, v1Errorf(tok, "invalid number `%s`", tok.Literal)
	}
	return v, nil
}

func v1Binary(op string, l, r v1Expr) v1Expr {
	sp := joinV1Span(l.sp, r.sp)
	return v1Expr{n: &ast.BinaryExpr{Op: op, Left: l.n, Right: r.n, Span: sp}, sp: sp}
}

// v1TokenSpan is tok's extent as a node Span. A Span is where the node sits in
// the author's source -- a runtime error quotes it -- so a token of a marked
// lowering contributes its authored extent (position_markers.go).
func v1TokenSpan(tok Token) ast.Span {
	line, col := tok.At()
	endLine, endCol := tok.EndAt()
	return ast.Span{Line: line, Col: col, EndLine: endLine, EndCol: endCol}
}

func joinV1Span(from, to ast.Span) ast.Span {
	return ast.Span{Line: from.Line, Col: from.Col, EndLine: to.EndLine, EndCol: to.EndCol}
}
