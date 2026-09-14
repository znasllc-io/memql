package parser

// v1_body.go -- the edition-2026 statement body of `logic` and `automation`
// (epic memql#5370, task memql#5371; D12-D15 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// A logic and an automation share one body: statements in source order, read
// here natively rather than through the struct-form text rewriter. The
// statement set is closed (ast/body.go); expressions inside statements are
// epic memql#5363's grammar, parsed mid-stream by parseV1Expression.
//
//	x := <kind> f(k: v, ...) [on surface("s")] [retry(n)] [on error continue]
//	x := <expression>
//	<kind> f(...)            [trailing clauses as above]
//	if c { } else if d { } else { }
//	for x in <source> [if <cond>] { } [on error continue]
//	switch <subject> { case "a", "b" { } default { } }
//	parallel { branch a { } branch b { } } [wait any] [on error continue]
//	publish "<topic>" { k: v }
//	return [<expression> | <kind> f(...)]
//
// # Decisions a reader would otherwise have to rediscover
//
//   - One statement per line. The lexer drops newlines, so a statement ends
//     where parseV1Expression (or the statement's own closing token) stops,
//     and the next statement must start on a later line or be the block's
//     `}`. An expression continues across lines only where the grammar needs
//     more -- inside brackets, after an operator -- because no statement
//     begins with an operator.
//   - A construct call is a statement of its own: the whole right-hand side of
//     `:=`, the whole value of `return`, or a bare call. Nested inside an
//     expression it is refused, because a side effect that is not a statement
//     is not journaled, previewed or retried.
//   - Every argument of a call is named. The pun (`logic f(event)`) is refused
//     here, before the shared construct-call parser -- which records a pun as a
//     named argument so the legacy tree loads -- can accept it.
//   - Trailing clauses are written on the statement's last line, once each, in
//     one order: `on surface(...)`, `retry(n)`, `on error continue`. A default
//     is never written (`on error stop`, `wait all`): D24's one form per
//     operation.
//   - Until the tree is migrated, a construct in a retired form must keep
//     loading, so the struct-form rewriter still expands a logic that opens
//     `body {`, an automation holding a `step` block, and the terse header
//     (rewriter.go's native-block sentinel), and every other `logic` /
//     `automation` reaches this file. The flip deletes the rewriter's half.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// isV1BodyKeyword reports whether word declares a statement body.
func isV1BodyKeyword(word string) bool { return word == "logic" || word == "automation" }

// bodyCallKinds are the construct kinds a statement calls.
var bodyCallKinds = map[string]bool{
	"query": true, "mutation": true, "logic": true, "builtin": true, "automation": true, "action": true,
}

// lastTok is the most recently consumed token.
func (p *Parser) lastTok() Token {
	i := p.pos - 1
	if i >= len(p.tokens) {
		i = len(p.tokens) - 1
	}
	if i < 0 {
		return Token{}
	}
	return p.tokens[i]
}

// endLine is the line a token ends on.
func endLine(t Token) int {
	if t.EndLine > 0 {
		return t.EndLine
	}
	return t.Line
}

// spanFrom is the span from start to the last consumed token.
func (p *Parser) spanFrom(start Token) ast.Span {
	return joinV1Span(v1TokenSpan(start), v1TokenSpan(p.lastTok()))
}

func isSimpleName(tok Token) bool {
	return tok.Type == TokenIdentifier && !strings.ContainsAny(tok.Literal, ".:-")
}

// parseV1LogicOrAutomation parses `logic NAME { args {...} statements }` and
// `automation NAME { args {...} precondition NAME {...} statements }`. The
// result is a *FunctionDef whose Body is an *AutomationDef carrying the
// statement Body; attributes attach afterwards, in parseDefinition, exactly as
// for every other definition. attrToks are the `@` tokens of attrs, for the
// position of a refused trigger spelling.
func (p *Parser) parseV1LogicOrAutomation(attrs []*Attribute, attrToks []Token) (*FunctionDef, error) {
	kind, name := p.current.Literal, p.peekAhead(1).Literal
	def, err := p.parseV1Definition(attrs, attrToks)
	if err != nil {
		// Every refusal names its construct (D24). The wrap keeps the chain:
		// errors.As still finds the *BodyRefusal and the positioned
		// *ParseError beneath it.
		return nil, fmt.Errorf("%s %s: %w", kind, name, err)
	}
	return def, nil
}

func (p *Parser) parseV1Definition(attrs []*Attribute, attrToks []Token) (*FunctionDef, error) {
	kindTok := p.v1Take()
	kind := kindTok.Literal
	nameTok := p.current
	if !isSimpleName(nameTok) {
		return nil, v1Errorf(nameTok, "expected the %s's name, a simple identifier, after `%s`, got %s", kind, kind, v1Describe(nameTok))
	}
	p.v1Take()
	name := nameTok.Literal
	construct := kind + " " + name

	if kind == "automation" && p.check(TokenAt) {
		return nil, bodyRetired(kindTok, codeBodyTerseRetired,
			fmt.Sprintf("the terse `automation %s @trigger(...) => logic L` form", name),
			fmt.Sprintf("write @trigger(...) above `automation %s { logic L(event: event) }`", name))
	}
	if !p.check(TokenBraceOpen) {
		return nil, p.v1Expected(fmt.Sprintf("`{` to open the body of %s", construct))
	}
	p.v1Take()
	if p.pendingArgs != nil {
		p.pendingArgs = nil
		return nil, v1Errorf(nameTok, "the args block of %s goes inside its braces, before its statements: %s { args { ... } ... }", construct, construct)
	}

	var schema *ArgsSchema
	if p.check(TokenIdentifier) && p.current.Literal == "args" && p.peekAhead(1).Type == TokenBraceOpen {
		argsLine := p.current.Line
		s, err := p.parseFileTopArgsBlock()
		if err != nil {
			return nil, err
		}
		p.addTransparentSpan(argsLine, p.current.Line)
		schema = s
	}
	if kind == "automation" {
		// precondition blocks are extracted from source by the automations
		// loader, unchanged; the statement parser only steps over them.
		for p.check(TokenIdentifier) && p.current.Literal == "precondition" {
			if err := p.skipV1PreconditionBlock(); err != nil {
				return nil, err
			}
		}
		if err := checkV1TriggerAttributes(attrs, attrToks); err != nil {
			return nil, err
		}
	}

	stmts, err := p.parseV1Statements(kind, construct)
	if err != nil {
		return nil, err
	}
	closeTok := p.v1Take()
	if kind == "automation" && len(stmts) == 0 {
		return nil, bodyRefuse(closeTok, codeBodyEmpty, "%s has no statement: an automation has at least one", construct)
	}

	body := &ast.Body{Statements: stmts, Span: joinV1Span(v1TokenSpan(kindTok), v1TokenSpan(closeTok))}
	auto := &AutomationDef{Name: name, Steps: []StepDef{}, Enabled: true, ExpressionsV1: true, Body: body}
	def := &FunctionDef{
		Name:          name,
		Args:          []FunctionArg{{Name: "_", Type: "any"}},
		Body:          auto,
		Enabled:       true,
		ArgsSchema:    schema,
		ExpressionsV1: true,
	}
	if kind == "logic" {
		def.Receiver = &FunctionReceiver{Type: ReceiverLogic}
		def.Type = FunctionTypeLogic
		def.Returns = []string{"any", "error"}
	} else {
		def.Receiver = &FunctionReceiver{Type: ReceiverAutomation}
		def.Type = FunctionTypeAutomation
	}
	return def, nil
}

// checkV1TriggerAttributes refuses the two trigger spellings D15 retires.
func checkV1TriggerAttributes(attrs []*Attribute, toks []Token) error {
	for i, a := range attrs {
		var tok Token
		if i < len(toks) {
			tok = toks[i]
		}
		switch a.Name {
		case AttrSchedule:
			return bodyRetired(tok, codeTriggerScheduleRetired, "`@schedule(cron=...)`", "write `@trigger(schedule=...)`")
		case AttrTrigger:
			if _, ok := a.Args["partition"]; ok {
				return bodyRetired(tok, codeTriggerPartitionRetired, "`partition=` on @trigger", "delete it")
			}
		}
	}
	return nil
}

// skipV1PreconditionBlock steps over `precondition NAME { ... }`.
func (p *Parser) skipV1PreconditionBlock() error {
	start := p.v1Take()
	if !isSimpleName(p.current) || p.peekAhead(1).Type != TokenBraceOpen {
		return v1Errorf(p.current, "expected `precondition <name> { ... }`, got %s", v1Describe(p.current))
	}
	p.v1Take()
	depth := 0
	for {
		switch p.current.Type {
		case TokenEOF:
			return v1Errorf(start, "the precondition block opened on line %d is not closed", start.Line)
		case TokenBraceOpen:
			depth++
		case TokenBraceClose:
			depth--
			if depth == 0 {
				p.v1Take()
				return nil
			}
		}
		p.v1Take()
	}
}

// parseV1Statements reads statements until the `}` that closes the enclosing
// block, which it leaves unconsumed. kind decides nothing here -- the
// construct rules are load checks (compiler.CheckBody) -- and construct names
// the enclosing construct in messages.
func (p *Parser) parseV1Statements(kind, construct string) ([]ast.BodyStatement, error) {
	var out []ast.BodyStatement
	prevEnd := 0
	for !p.check(TokenBraceClose) {
		tok := p.current
		if tok.Type == TokenEOF {
			return nil, v1Errorf(tok, "expected `}` to close the body of %s", construct)
		}
		if prevEnd > 0 && tok.Line <= prevEnd {
			if err := p.v1RefuseMistake(); err != nil {
				return nil, err
			}
			if !startsV1Statement(tok) {
				return nil, v1Errorf(tok, "unexpected %s after the statement", v1Describe(tok))
			}
			return nil, bodyRefuse(tok, codeBodyOneStatementPerLine, "one statement per line: %s starts a second statement on line %d", v1Describe(tok), tok.Line)
		}
		s, err := p.parseV1Statement(kind, construct)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		prevEnd = endLine(p.lastTok())
	}
	return out, nil
}

// startsV1Statement reports whether a token can begin a statement.
func startsV1Statement(tok Token) bool {
	switch tok.Type {
	case TokenIdentifier, TokenKeywordIf, TokenKeywordFor, TokenKeywordSwitch, TokenKeywordReturn, TokenKeywordRetry, TokenKeywordElse:
		return true
	}
	return false
}

// parseV1Statement parses the statement at the cursor.
func (p *Parser) parseV1Statement(kind, construct string) (ast.BodyStatement, error) {
	tok := p.current
	switch tok.Type {
	case TokenKeywordIf:
		return p.parseV1If(kind, construct)
	case TokenKeywordFor:
		return p.parseV1For(kind, construct)
	case TokenKeywordSwitch:
		return p.parseV1Switch(kind, construct)
	case TokenKeywordReturn:
		return p.parseV1Return()
	case TokenKeywordRetry:
		return nil, bodyRefuse(tok, codeBodyRetryPlacement,
			"`retry(n)` follows the construct call it retries, on the same line: `<name> := <kind> <callee>(...) retry(n)`")
	case TokenKeywordElse:
		return nil, bodyRefuse(tok, codeBodyElsePlacement, "`else` follows the closing brace on the same line: `} else {`")
	case TokenKeywordCase, TokenKeywordDefault:
		return nil, v1Errorf(tok, "`%s` belongs inside a switch: switch <value> { case ... { } default { } }", tok.Literal)
	case TokenIdentifier:
		return p.parseV1WordStatement(kind, construct)
	}
	if err := p.v1RefuseMistake(); err != nil {
		return nil, err
	}
	return nil, v1Errorf(tok, "expected a statement, got %s", v1Describe(tok))
}

// atV1Call reports whether the cursor is at `<word> <name>(` on one line --
// the shape of a construct call -- and returns the word.
func (p *Parser) atV1Call() (string, bool) {
	tok, next := p.current, p.peekAhead(1)
	if tok.Type != TokenIdentifier || next.Type != TokenIdentifier || next.Line != tok.Line || p.peekAhead(2).Type != TokenParenOpen {
		return "", false
	}
	return tok.Literal, true
}

// refuseV1CallWord refuses a call whose word is not a body call kind.
func (p *Parser) refuseV1CallWord(word string) error {
	tok := p.current
	if word == "capability" {
		return bodyRefuse(tok, codeBodyCallKindMissing,
			"a capability is called from an action's body: a logic or an automation calls the action, `action <name>(...)`")
	}
	return bodyRefuse(tok, codeBodyCallKindMissing,
		"`%s` is not a construct kind a statement calls: write `query`, `mutation`, `logic`, `builtin`, `automation` or `action` before %s(...)",
		word, p.peekAhead(1).Literal)
}

// parseV1WordStatement parses a statement that opens with an identifier.
func (p *Parser) parseV1WordStatement(kind, construct string) (ast.BodyStatement, error) {
	tok := p.current
	word := tok.Literal
	next := p.peekAhead(1)
	sameLine := next.Line == tok.Line
	switch {
	case word == "step" && next.Type == TokenIdentifier && p.peekAhead(2).Type == TokenBraceOpen:
		return nil, bodyRetired(tok, codeBodyStepRetired, fmt.Sprintf("`step %s { ... }`", next.Literal),
			fmt.Sprintf("write the step's call as a statement, `%s := <call>`", next.Literal))
	case word == "body" && next.Type == TokenBraceOpen:
		if kind == "logic" {
			return nil, bodyRetired(tok, codeBodyBlockRetired, "`body { }`", "a logic's statements follow its args block directly")
		}
		return nil, bodyRetired(tok, codeBodyBlockRetired, "`body { }`", "an automation's statements follow its args block directly")
	case word == "forEach":
		return nil, bodyRetired(tok, codeBodyForEachRetired, "`forEach`", "write `for <x> in <source> if <cond> { }`")
	case word == "publishEvent" && next.Type == TokenParenOpen:
		return nil, bodyRetired(tok, codeBodyPublishEventRetired, "`publishEvent(...)`", "write `publish \"<topic>\" { ... }` in an automation")
	case word == "parallel" && next.Type == TokenBraceOpen:
		return p.parseV1Parallel(kind, construct)
	case word == "publish" && next.Type == TokenString:
		return p.parseV1Publish()
	case word == "on" && next.Type == TokenIdentifier && (next.Literal == "error" || next.Literal == "surface"):
		if next.Literal == "error" {
			return nil, bodyRefuse(tok, codeBodyOnErrorPlacement, "`on error continue` follows its statement on the same line")
		}
		return nil, bodyRefuse(tok, codeBodySurfacePlacement, "`on surface(...)` follows its `action` call on the same line")
	case word == "args" && next.Type == TokenBraceOpen:
		return nil, v1Errorf(tok, "the args block of %s comes first, before any statement", construct)
	case word == "precondition":
		return nil, v1Errorf(tok, "a precondition block comes before the first statement of %s", construct)
	case next.Type == TokenDefine:
		return p.parseV1Assign()
	}
	if w, ok := p.atV1Call(); ok {
		if !bodyCallKinds[w] {
			return nil, p.refuseV1CallWord(w)
		}
		call, err := p.parseV1BodyCall()
		if err != nil {
			return nil, err
		}
		mods, err := p.parseV1Clauses(call, allowRetry|allowOnError)
		if err != nil {
			return nil, err
		}
		return &ast.CallStatement{Call: call, Mods: mods, Span: p.spanFrom(tok)}, nil
	}
	switch {
	case next.Type == TokenParenOpen && sameLine:
		return nil, bodyRefuse(tok, codeBodyCallKindMissing,
			"`%s(...)` names no construct kind: write `query`, `mutation`, `logic`, `builtin`, `automation` or `action` before it (%s adds it)", word, bodyMigrator)
	case next.Type == TokenBraceOpen && sameLine:
		return nil, bodyRefuse(tok, codeBodyCallKindMissing,
			"`%s { ... }` names no construct kind: write `<kind> %s(<named args>)` (%s rewrites it)", word, word, bodyMigrator)
	}
	return nil, v1Errorf(tok, "expected a statement, got %s: a statement binds a name (x := ...), calls a construct (query x(...)), or is if, for, switch, parallel, publish or return", v1Describe(tok))
}

// parseV1Assign parses `name := <call>` or `name := <expression>`.
func (p *Parser) parseV1Assign() (ast.BodyStatement, error) {
	nameTok := p.v1Take()
	if !isSimpleName(nameTok) {
		return nil, v1Errorf(nameTok, "a statement's name is a simple identifier, got `%s`", nameTok.Literal)
	}
	defTok := p.v1Take()
	rhs := p.current
	if rhs.Line != defTok.Line || rhs.Type == TokenEOF || rhs.Type == TokenBraceClose {
		return nil, v1Errorf(rhs, "the value of `%s :=` starts on the same line", nameTok.Literal)
	}
	stmt := &ast.AssignStatement{Name: nameTok.Literal, NameSpan: v1TokenSpan(nameTok)}
	switch {
	case rhs.Type == TokenKeywordIf:
		return nil, bodyRetired(rhs, codeBodyConditionalAssignRetired,
			fmt.Sprintf("`%s := if <cond> { <call> }`", nameTok.Literal),
			fmt.Sprintf("write `if <cond> { %s := <call> }` -- a name bound in an if branch is readable after it", nameTok.Literal))
	case rhs.Type == TokenKeywordRetry:
		return nil, bodyRetired(rhs, codeBodyRetryPlacement,
			fmt.Sprintf("`%s := retry(n) <call>`", nameTok.Literal),
			fmt.Sprintf("write the clause after the call, `%s := <call> retry(n)`", nameTok.Literal))
	}
	if w, ok := p.atV1Call(); ok {
		if !bodyCallKinds[w] {
			return nil, p.refuseV1CallWord(w)
		}
		call, err := p.parseV1BodyCall()
		if err != nil {
			return nil, err
		}
		mods, err := p.parseV1Clauses(call, allowRetry|allowOnError)
		if err != nil {
			return nil, err
		}
		stmt.Call, stmt.Mods = call, mods
		stmt.Span = p.spanFrom(nameTok)
		return stmt, nil
	}
	value, err := p.parseV1BodyExpression()
	if err != nil {
		return nil, err
	}
	if p.check(TokenBraceOpen) && p.current.Line == endLine(p.lastTok()) {
		if id, ok := value.(*ast.IdentExpr); ok {
			return nil, bodyRefuse(rhs, codeBodyCallKindMissing,
				"`%s { ... }` names no construct kind: write `%s := <kind> %s(<named args>)` (%s rewrites it)", id.Name, nameTok.Literal, id.Name, bodyMigrator)
		}
		return nil, v1Errorf(p.current, "unexpected `{` after the value of `%s`", nameTok.Literal)
	}
	if _, err := p.parseV1Clauses(nil, 0); err != nil {
		return nil, err
	}
	stmt.Value = value
	stmt.Span = p.spanFrom(nameTok)
	return stmt, nil
}

// parseV1BodyExpression parses one expression in a body position and holds it
// to the body's rules: no construct call nested inside, no `steps.` root, no
// publishEvent call.
func (p *Parser) parseV1BodyExpression() (ast.ExpressionNode, error) {
	e, err := p.parseV1Expression()
	if err != nil {
		return nil, err
	}
	if err := checkV1BodyExpression(e); err != nil {
		return nil, err
	}
	return e, nil
}

// tokenAt is a position-only token for a node's span.
func tokenAt(sp ast.Span) Token { return Token{Line: sp.Line, Column: sp.Col} }

func checkV1BodyExpression(e ast.ExpressionNode) error {
	var err error
	ast.WalkV1(e, func(n ast.ExpressionNode) bool {
		if err != nil {
			return false
		}
		switch v := n.(type) {
		case *ast.IdentExpr:
			if v.Name == "steps" {
				err = bodyRetired(tokenAt(v.Span), codeBodyStepsReferenceRetired, "`steps.<id>...`", "a statement's name is its value, so write `<id>`")
			}
		case *ast.CallExpr:
			switch {
			case v.Kind != "":
				err = bodyRefuse(tokenAt(v.Span), codeBodyCallInExpression,
					"a construct call is a statement of its own: bind it, `<n> := %s %s(...)`, and read <n> here", v.Kind, v.Name)
			case v.Receiver == nil && v.Name == "publishEvent":
				err = bodyRetired(tokenAt(v.Span), codeBodyPublishEventRetired, "`publishEvent(...)`", "write `publish \"<topic>\" { ... }` in an automation")
			}
		}
		return true
	})
	return err
}

// parseV1BodyCall parses `<kind> <name>(<named args>)` at the cursor. Every
// argument is named: a bare name (the retired pun) or any other positional
// argument is refused here, before the shared construct-call parser -- which
// records a pun as a named argument so the legacy tree keeps loading -- can
// accept it.
func (p *Parser) parseV1BodyCall() (*ast.ConstructCall, error) {
	kindTok, nameTok := p.current, p.peekAhead(1)
	if err := p.refuseV1PositionalArgs(kindTok.Literal, nameTok.Literal, p.pos+2); err != nil {
		return nil, err
	}
	e, err := p.parseV1ConstructCall()
	if err != nil {
		return nil, err
	}
	ce, ok := e.n.(*ast.CallExpr)
	if !ok || ce.Kind == "" {
		return nil, v1Errorf(kindTok, "expected a construct call: %s %s(k: v, ...)", kindTok.Literal, nameTok.Literal)
	}
	for _, a := range ce.Named {
		if err := checkV1BodyExpression(a.Value); err != nil {
			return nil, err
		}
	}
	return &ast.ConstructCall{Kind: ce.Kind, Name: ce.Name, Args: ce.Named, Span: ce.Span}, nil
}

// refuseV1PositionalArgs scans the argument list whose `(` is token open and
// refuses the first argument that is not `name: value`. It reads tokens only;
// the construct-call parser parses the list afterwards.
func (p *Parser) refuseV1PositionalArgs(kind, callee string, open int) error {
	depth := 0
	argStart := -1
	check := func(start, end int) error {
		if start < 0 || start >= end {
			return nil
		}
		first := p.tokens[start]
		if first.Type == TokenIdentifier && strings.Contains(first.Literal, ":") {
			return nil // a glued `k:v` or a canonical id: the expression parser names it
		}
		if (first.Type == TokenIdentifier || first.Type == TokenString) && start+1 < end && p.tokens[start+1].Type == TokenColon {
			return nil
		}
		if first.Type == TokenBraceOpen {
			return nil // the object-literal wrapper: the construct-call parser refuses it by name
		}
		if isSimpleName(first) && start+1 == end {
			return bodyRefuse(first, codeBodyPositionalArgument,
				"`%s %s(%s)` passes %s without a name: write `%s: <value>`; the bare-argument pun is retired (%s rewrites it)",
				kind, callee, first.Literal, first.Literal, first.Literal, bodyMigrator)
		}
		return bodyRefuse(first, codeBodyPositionalArgument,
			"`%s %s(...)` passes an argument without a name: every argument of a construct call is `name: value`", kind, callee)
	}
	for i := open; i < len(p.tokens); i++ {
		switch p.tokens[i].Type {
		case TokenEOF:
			return nil
		case TokenParenOpen, TokenBracketOpen, TokenBraceOpen:
			depth++
			if depth == 1 {
				argStart = i + 1
				continue
			}
		case TokenParenClose, TokenBracketClose, TokenBraceClose:
			depth--
			if depth == 0 {
				return check(argStart, i)
			}
		case TokenComma:
			if depth == 1 {
				if err := check(argStart, i); err != nil {
					return err
				}
				argStart = i + 1
			}
		}
	}
	return nil
}

// clauseAllow is which trailing clauses a statement admits.
type clauseAllow int

const (
	allowRetry clauseAllow = 1 << iota
	allowOnError
)

// The canonical clause order, by index.
const (
	clauseSurface = iota
	clauseRetry
	clauseOnError
)

// parseV1Clauses reads the trailing clauses on the statement's last line.
// call is the statement's construct call, nil when the statement is not one.
// A token on that line that is not a clause is left for the caller.
func (p *Parser) parseV1Clauses(call *ast.ConstructCall, allow clauseAllow) (ast.StatementMods, error) {
	var mods ast.StatementMods
	line := endLine(p.lastTok())
	last := -1
	for p.current.Line == line && p.current.Type != TokenEOF {
		tok := p.current
		next := p.peekAhead(1)
		isOn := tok.Type == TokenIdentifier && tok.Literal == "on" && next.Type == TokenIdentifier && next.Line == line
		var idx int
		switch {
		case isOn && next.Literal == "surface":
			idx = clauseSurface
			if call == nil || call.Kind != "action" {
				return mods, bodyRefuse(tok, codeBodySurfacePlacement, "`on surface(...)` applies to an `action` call")
			}
			if err := p.orderedClause(tok, idx, last); err != nil {
				return mods, err
			}
			s, err := p.parseV1Surface()
			if err != nil {
				return mods, err
			}
			call.Surface = s
		case tok.Type == TokenKeywordRetry:
			idx = clauseRetry
			if allow&allowRetry == 0 {
				return mods, bodyRefuse(tok, codeBodyRetryPlacement, "`retry(n)` applies to a construct call statement")
			}
			if err := p.orderedClause(tok, idx, last); err != nil {
				return mods, err
			}
			n, err := p.parseV1Retry()
			if err != nil {
				return mods, err
			}
			mods.Retry = n
		case isOn && next.Literal == "error":
			idx = clauseOnError
			if allow&allowOnError == 0 {
				return mods, bodyRefuse(tok, codeBodyOnErrorPlacement, "`on error continue` applies to a call, a `for` or a `parallel` statement")
			}
			if err := p.orderedClause(tok, idx, last); err != nil {
				return mods, err
			}
			p.v1Take()
			p.v1Take()
			v := p.current
			switch {
			case v.Type == TokenKeywordContinue && v.Line == line:
				p.v1Take()
				mods.OnError = "continue"
			case v.Type == TokenIdentifier && v.Literal == "stop" && v.Line == line:
				return mods, bodyRefuse(v, codeBodyDefaultClause, "`on error stop` is the default: delete it")
			default:
				return mods, v1Errorf(v, "`on error` takes `continue`, got %s", v1Describe(v))
			}
		default:
			return mods, nil
		}
		last = idx
	}
	return mods, nil
}

// orderedClause refuses a clause written out of order or twice.
func (p *Parser) orderedClause(tok Token, idx, last int) error {
	if idx > last {
		return nil
	}
	return bodyRefuse(tok, codeBodyClauseOrder,
		"trailing clauses are written once each, in the order `on surface(...)`, `retry(n)`, `on error continue`")
}

// parseV1Surface parses `on surface("<name>")`.
func (p *Parser) parseV1Surface() (string, error) {
	p.v1Take() // on
	p.v1Take() // surface
	if !p.check(TokenParenOpen) {
		return "", p.v1Expected("`(` after `on surface`")
	}
	p.v1Take()
	s := p.current
	if s.Type != TokenString || s.Literal == "" {
		return "", v1Errorf(s, "`on surface(...)` takes the surface's name as a string, got %s", v1Describe(s))
	}
	p.v1Take()
	if !p.check(TokenParenClose) {
		return "", p.v1Expected("`)` to close `on surface(...)`")
	}
	p.v1Take()
	return s.Literal, nil
}

// parseV1Retry parses `retry(n)`, n a whole number of at least 1.
func (p *Parser) parseV1Retry() (int, error) {
	p.v1Take() // retry
	if !p.check(TokenParenOpen) {
		return 0, p.v1Expected("`(` after `retry`: retry(n)")
	}
	p.v1Take()
	nt := p.current
	n, err := strconv.Atoi(nt.Literal)
	if nt.Type != TokenNumber || err != nil || n < 1 {
		return 0, v1Errorf(nt, "`retry(n)` takes a whole number of at least 1, got %s", v1Describe(nt))
	}
	p.v1Take()
	if !p.check(TokenParenClose) {
		return 0, p.v1Expected("`)` to close retry(n)")
	}
	p.v1Take()
	return n, nil
}

// parseV1Block parses `{ statements }`.
func (p *Parser) parseV1Block(kind, construct, what string) ([]ast.BodyStatement, error) {
	if !p.check(TokenBraceOpen) {
		return nil, p.v1Expected(fmt.Sprintf("`{` to open the %s block", what))
	}
	p.v1Take()
	stmts, err := p.parseV1Statements(kind, construct)
	if err != nil {
		return nil, err
	}
	p.v1Take() // `}`: parseV1Statements returns only there
	return stmts, nil
}

// parseV1If parses `if c { } else if d { } else { }`. `else` sits on the
// line of the brace it follows.
func (p *Parser) parseV1If(kind, construct string) (ast.BodyStatement, error) {
	ifTok := p.v1Take()
	stmt := &ast.IfStatement{}
	branchStart := ifTok
	for {
		cond, err := p.parseV1BodyExpression()
		if err != nil {
			return nil, err
		}
		body, err := p.parseV1Block(kind, construct, "if")
		if err != nil {
			return nil, err
		}
		stmt.Branches = append(stmt.Branches, ast.IfBranch{Cond: cond, Body: body, Span: p.spanFrom(branchStart)})
		closeLine := endLine(p.lastTok())
		if !p.check(TokenKeywordElse) {
			break
		}
		if p.current.Line != closeLine {
			return nil, bodyRefuse(p.current, codeBodyElsePlacement, "`else` follows the closing brace on the same line: `} else {`")
		}
		elseTok := p.v1Take()
		if p.check(TokenKeywordIf) {
			branchStart = elseTok
			p.v1Take()
			continue
		}
		body, err = p.parseV1Block(kind, construct, "else")
		if err != nil {
			return nil, err
		}
		stmt.Branches = append(stmt.Branches, ast.IfBranch{Body: body, Span: p.spanFrom(elseTok)})
		break
	}
	if _, err := p.parseV1Clauses(nil, 0); err != nil {
		return nil, err
	}
	stmt.Span = p.spanFrom(ifTok)
	return stmt, nil
}

// parseV1For parses `for x in <source> [if <cond>] { } [on error continue]`.
func (p *Parser) parseV1For(kind, construct string) (ast.BodyStatement, error) {
	forTok := p.v1Take()
	varTok := p.current
	if next := p.peekAhead(1); varTok.Type == TokenIdentifier && (next.Type == TokenDefine || next.Type == TokenComma) {
		return nil, bodyRetired(forTok, codeBodyForRangeRetired, "`for x := range <source>`", "write `for x in <source>`")
	}
	if !isSimpleName(varTok) {
		return nil, v1Errorf(varTok, "expected the loop variable's name after `for`, got %s", v1Describe(varTok))
	}
	p.v1Take()
	if !p.check(TokenKeywordIn) {
		return nil, p.v1Expected("`in` after the loop variable: for <x> in <source> { }")
	}
	p.v1Take()
	src, err := p.parseV1BodyExpression()
	if err != nil {
		return nil, err
	}
	var filter ast.ExpressionNode
	if p.check(TokenKeywordIf) {
		p.v1Take()
		if filter, err = p.parseV1BodyExpression(); err != nil {
			return nil, err
		}
	}
	body, err := p.parseV1Block(kind, construct, "for")
	if err != nil {
		return nil, err
	}
	mods, err := p.parseV1Clauses(nil, allowOnError)
	if err != nil {
		return nil, err
	}
	return &ast.ForStatement{
		Var: varTok.Literal, VarSpan: v1TokenSpan(varTok), Source: src, Filter: filter,
		Body: body, Mods: mods, Span: p.spanFrom(forTok),
	}, nil
}

// parseV1Switch parses `switch <subject> { case "a", "b" { } default { } }`.
func (p *Parser) parseV1Switch(kind, construct string) (ast.BodyStatement, error) {
	swTok := p.v1Take()
	subject, err := p.parseV1BodyExpression()
	if err != nil {
		return nil, err
	}
	if !p.check(TokenBraceOpen) {
		return nil, p.v1Expected("`{` to open the switch")
	}
	p.v1Take()
	stmt := &ast.SwitchStatement{Subject: subject}
	seen := map[string]bool{}
	hasDefault := false
	for !p.check(TokenBraceClose) {
		armTok := p.current
		switch armTok.Type {
		case TokenKeywordCase:
			p.v1Take()
			var labels []ast.ExpressionNode
			for {
				lt := p.current
				l, err := p.parseV1BodyExpression()
				if err != nil {
					return nil, err
				}
				key := ast.FormatExpr(l)
				if !isV1LiteralLabel(l) || seen[key] {
					return nil, bodyRefuse(lt, codeBodyCaseLabel, "a case label is a literal written once: `%s`", key)
				}
				seen[key] = true
				labels = append(labels, l)
				if !p.check(TokenComma) {
					break
				}
				p.v1Take()
			}
			body, err := p.parseV1Block(kind, construct, "case")
			if err != nil {
				return nil, err
			}
			stmt.Cases = append(stmt.Cases, ast.CaseArm{Labels: labels, Body: body, Span: p.spanFrom(armTok)})
		case TokenKeywordDefault:
			if hasDefault {
				return nil, bodyRefuse(armTok, codeBodyCaseLabel, "a switch has one `default`")
			}
			hasDefault = true
			p.v1Take()
			body, err := p.parseV1Block(kind, construct, "default")
			if err != nil {
				return nil, err
			}
			stmt.Cases = append(stmt.Cases, ast.CaseArm{Default: true, Body: body, Span: p.spanFrom(armTok)})
		case TokenEOF:
			return nil, v1Errorf(armTok, "the switch opened on line %d is not closed", swTok.Line)
		default:
			return nil, p.v1Expected("`case` or `default` in the switch")
		}
	}
	p.v1Take()
	if len(stmt.Cases) == 0 {
		return nil, v1Errorf(swTok, "a switch has at least one case")
	}
	if _, err := p.parseV1Clauses(nil, 0); err != nil {
		return nil, err
	}
	stmt.Span = p.spanFrom(swTok)
	return stmt, nil
}

// isV1LiteralLabel reports whether a case label is a literal: a string, a
// number, a boolean or nil, as written (no parentheses).
func isV1LiteralLabel(e ast.ExpressionNode) bool {
	switch e.(type) {
	case *ast.LiteralExpr, *ast.NilExpr:
		return true
	}
	return false
}

// parseV1Parallel parses `parallel { branch a { } ... } [wait any] [on error continue]`.
func (p *Parser) parseV1Parallel(kind, construct string) (ast.BodyStatement, error) {
	parTok := p.v1Take()
	p.v1Take() // {
	stmt := &ast.ParallelStatement{Wait: "all"}
	labels := map[string]bool{}
	for !p.check(TokenBraceClose) {
		bt := p.current
		if bt.Type == TokenEOF {
			return nil, v1Errorf(bt, "the parallel opened on line %d is not closed", parTok.Line)
		}
		if bt.Type != TokenIdentifier || bt.Literal != "branch" {
			return nil, p.v1Expected("`branch <label> { }` in the parallel")
		}
		p.v1Take()
		lt := p.current
		if !isSimpleName(lt) {
			return nil, v1Errorf(lt, "a branch's label is a simple identifier, got %s", v1Describe(lt))
		}
		if labels[lt.Literal] {
			return nil, v1Errorf(lt, "the branch label `%s` is used twice in this parallel", lt.Literal)
		}
		labels[lt.Literal] = true
		p.v1Take()
		body, err := p.parseV1Block(kind, construct, "branch")
		if err != nil {
			return nil, err
		}
		stmt.Branches = append(stmt.Branches, ast.ParallelBranch{Label: lt.Literal, Body: body, Span: p.spanFrom(bt)})
	}
	p.v1Take()
	if len(stmt.Branches) == 0 {
		return nil, v1Errorf(parTok, "a parallel has at least one branch")
	}
	line := endLine(p.lastTok())
	if w := p.current; w.Line == line && w.Type == TokenIdentifier && w.Literal == "wait" {
		p.v1Take()
		v := p.current
		switch {
		case v.Line == line && v.Type == TokenIdentifier && v.Literal == "any":
			p.v1Take()
			stmt.Wait = "any"
		case v.Line == line && v.Type == TokenIdentifier && v.Literal == "all":
			return nil, bodyRefuse(v, codeBodyDefaultClause, "`wait all` is the default: delete it")
		default:
			return nil, bodyRefuse(v, codeBodyWaitValue, "`wait` takes `any` (`wait all` is the default and is not written), got %s", v1Describe(v))
		}
	}
	mods, err := p.parseV1Clauses(nil, allowOnError)
	if err != nil {
		return nil, err
	}
	if w := p.current; w.Line == line && w.Type == TokenIdentifier && w.Literal == "wait" {
		return nil, bodyRefuse(w, codeBodyClauseOrder, "`wait any` comes before `on error continue`")
	}
	stmt.Mods = mods
	stmt.Span = p.spanFrom(parTok)
	return stmt, nil
}

// parseV1Publish parses `publish "<topic>" { <payload> }`.
func (p *Parser) parseV1Publish() (ast.BodyStatement, error) {
	pubTok := p.v1Take()
	topicTok := p.v1Take()
	if topicTok.Literal == "" {
		return nil, v1Errorf(topicTok, "a publish topic is a non-empty string")
	}
	if !p.check(TokenBraceOpen) || p.current.Line != topicTok.Line {
		return nil, v1Errorf(p.current, "publish takes its payload map after the topic: publish %s { k: v }", ast.QuoteString(topicTok.Literal))
	}
	payload, err := p.parseV1BodyExpression()
	if err != nil {
		return nil, err
	}
	m, ok := payload.(*ast.MapExpr)
	if !ok {
		return nil, v1Errorf(topicTok, "the payload of publish %s is a map literal, { k: v }", ast.QuoteString(topicTok.Literal))
	}
	if _, err := p.parseV1Clauses(nil, 0); err != nil {
		return nil, err
	}
	return &ast.PublishStatement{Topic: topicTok.Literal, Payload: m, Span: p.spanFrom(pubTok)}, nil
}

// parseV1Return parses `return`, `return <expression>` and `return <call>`.
// A bare return is one whose next token is on a later line or is the `}`.
func (p *Parser) parseV1Return() (ast.BodyStatement, error) {
	retTok := p.v1Take()
	stmt := &ast.ReturnStatement{}
	if p.check(TokenBraceClose) || p.check(TokenEOF) || p.current.Line != retTok.Line {
		stmt.Span = v1TokenSpan(retTok)
		return stmt, nil
	}
	if w, ok := p.atV1Call(); ok {
		if !bodyCallKinds[w] {
			return nil, p.refuseV1CallWord(w)
		}
		call, err := p.parseV1BodyCall()
		if err != nil {
			return nil, err
		}
		mods, err := p.parseV1Clauses(call, allowRetry)
		if err != nil {
			return nil, err
		}
		stmt.Call, stmt.Mods = call, mods
	} else {
		v, err := p.parseV1BodyExpression()
		if err != nil {
			return nil, err
		}
		if p.check(TokenComma) && p.current.Line == endLine(p.lastTok()) {
			return nil, v1Errorf(p.current, "a return has one value: delete `, ...` after it")
		}
		if _, err := p.parseV1Clauses(nil, 0); err != nil {
			return nil, err
		}
		stmt.Value = v
	}
	stmt.Span = p.spanFrom(retTok)
	return stmt, nil
}
