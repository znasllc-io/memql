package parser

import "strings"

// collection_method.go parses the Story 4 (#2302 / ADR §2.2) LINQ-style
// collection-method surface with arrow lambdas:
//
//	args.members.where(m => m.role == "admin" && m.active).count()
//
// The lexer fuses the leading dotted path (`args.members.where`) into a
// single identifier token, so the first call lands in parseFunctionCall
// with the whole path as `name`; chained `.count()` is picked up by
// consumePostCallDotAccess. Each produces a MethodCallExpr; lambda
// arguments parse to LambdaExpr. Scope enforcement (rejecting the surface
// in specs / query filters) happens downstream in the engine converter.

// collectionMethods is the closed set of method names recognised as
// collection operators (ADR §2.2). Kept in sync with the engine-side
// collectionMethodNames set.
var collectionMethods = map[string]bool{
	"where": true, "select": true, "first": true, "last": true,
	"single": true, "any": true, "all": true, "count": true,
	"sum": true, "min": true, "max": true, "avg": true,
	"orderBy": true, "orderByDesc": true, "take": true, "skip": true,
	"distinct": true, "groupBy": true, "reduce": true, "empty": true,
	"contains": true,
}

// isCollectionMethodPath reports whether a fused dotted identifier names a
// collection-method call: it must contain a receiver path before a final
// `.method` segment whose method is a known collection operator.
func isCollectionMethodPath(name string) (recvPath, method string, ok bool) {
	dot := strings.LastIndex(name, ".")
	if dot <= 0 || dot == len(name)-1 {
		return "", "", false
	}
	method = name[dot+1:]
	if !collectionMethods[method] {
		return "", "", false
	}
	return name[:dot], method, true
}

// dottedReceiverExpr turns a dotted receiver path into the expression node
// that parseIdentifierExpression would have produced for it. `args.X` /
// `ctx.X` map to an ArgRefExpr; bare `now` to the clock; anything else to
// a SpecReferenceExpr (a field / spec reference resolved downstream).
func dottedReceiverExpr(path string) ExpressionNode {
	switch {
	case strings.HasPrefix(path, "args."):
		return &ArgRefExpr{Path: strings.TrimPrefix(path, "args.")}
	case strings.HasPrefix(path, "ctx."):
		sub := strings.TrimPrefix(path, "ctx.")
		switch sub {
		case "input", "output", "actor", "partition", "now", "config", "error", "trace":
			return &SpecReferenceExpr{Name: path}
		default:
			return &ArgRefExpr{Path: sub}
		}
	case path == "now":
		return &TimestampExprFunc{}
	default:
		return &SpecReferenceExpr{Name: path}
	}
}

// parseCollectionMethodCall parses a collection-method call whose receiver
// path and method were fused into `name`. The opening `(` has already been
// consumed by parseFunctionCall. It parses the argument list (each arg may
// be a lambda) and then consumes any trailing chained `.method(...)`.
func (p *Parser) parseCollectionMethodCall(recvPath, method string) (ExpressionNode, error) {
	receiver := dottedReceiverExpr(recvPath)
	args, err := p.parseMethodArgList()
	if err != nil {
		return nil, err
	}
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	call := &MethodCallExpr{Receiver: receiver, Method: method, Args: args}
	return p.consumePostCallDotAccess(call)
}

// parseMethodArgList parses the comma-separated arguments of a collection
// method call. Each argument is either an arrow lambda or a plain
// expression. The terminating `)` is left for the caller to consume.
//
// While parsing args the legacy `,`-as-OR separator is suppressed so a
// comma reliably terminates one argument (see Parser.suppressCommaOr).
func (p *Parser) parseMethodArgList() ([]ExpressionNode, error) {
	prev := p.suppressCommaOr
	p.suppressCommaOr = true
	defer func() { p.suppressCommaOr = prev }()

	var args []ExpressionNode
	for !p.check(TokenParenClose) && !p.check(TokenEOF) {
		arg, err := p.parseMethodArg()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		if p.check(TokenComma) {
			p.advance()
			continue
		}
		break
	}
	return args, nil
}

// parseMethodArg parses a single collection-method argument, detecting an
// arrow lambda (`x => <body>` or `(a, b) => <body>`) versus a plain
// expression.
func (p *Parser) parseMethodArg() (ExpressionNode, error) {
	// Single-parameter lambda: IDENT => body
	if p.check(TokenIdentifier) && p.isArrowAt(1) {
		param := p.current.Literal
		p.advance() // consume param
		p.advance() // consume '=>'
		body, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		return &LambdaExpr{Params: []string{param}, Body: body}, nil
	}
	// Parenthesised parameter list lambda: (a, b, ...) => body
	if params, ok := p.tryParseLambdaParamList(); ok {
		body, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		return &LambdaExpr{Params: params, Body: body}, nil
	}
	// Plain value/expression argument (e.g. take(5), contains("x"),
	// reduce seed).
	return p.parseExpression()
}

// isArrowAt reports whether the token n positions ahead of the cursor is
// the `=>` arrow operator.
func (p *Parser) isArrowAt(n int) bool {
	t := p.peekAhead(n)
	return t.Type == TokenOperator && t.Literal == "=>"
}

// tryParseLambdaParamList detects and consumes a `(a, b, ...) =>` parameter
// list. It returns the parameter names and true when the cursor is at a
// param-list lambda; otherwise it consumes nothing and returns false.
func (p *Parser) tryParseLambdaParamList() ([]string, bool) {
	if !p.check(TokenParenOpen) {
		return nil, false
	}
	// Look ahead for `( IDENT (, IDENT)* ) =>` without consuming.
	i := 1
	var params []string
	for {
		t := p.peekAhead(i)
		if t.Type != TokenIdentifier {
			return nil, false
		}
		params = append(params, t.Literal)
		i++
		if p.peekAhead(i).Type == TokenComma {
			i++
			continue
		}
		break
	}
	if p.peekAhead(i).Type != TokenParenClose {
		return nil, false
	}
	i++
	if !(p.peekAhead(i).Type == TokenOperator && p.peekAhead(i).Literal == "=>") {
		return nil, false
	}
	// Commit: consume `( params ) =>`.
	for j := 0; j < i+1; j++ {
		p.advance()
	}
	return params, true
}
