package memql

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// QueryPlan represents the parsed structure of a MemQL expression.
type QueryPlan struct {
	Root      ExpressionNode
	Mutations []MutationNode
	// MutationCall is a top-level call to a mutation function (func (Mutation) ...).
	// When set, Root will be nil and Mutations will be empty; Execute will evaluate the
	// function template and run exactly one insert.
	MutationCall *FunctionCallExpression
	// LogicCall is a top-level call to a multi-step Logic function
	// (func (Logic) ... whose body has intermediate `name := <call>`
	// steps before `_return`). When set, Root is nil and Execute
	// dispatches through the wired LogicRunner so step results bind
	// for later steps + the `_return` expression. Single-statement
	// Logic bodies don't set this -- their fn.Expr is evaluated
	// directly via the normal query expression path.
	LogicCall         *FunctionCallExpression
	Filters           []FilterNode
	Relationships     []RelationshipNode
	Timestamp         *time.Time
	UseLatest         bool
	Limit             *int
	Offset            *int
	Depth             *int
	Sort              []SortField
	CacheHints        map[string]int64
	Fields            []FieldReference
	ConceptFields     map[string][]FieldReference
	Metadata          metadataSelection
	PayloadSelect     bool
	ShapeTemplate     shapeTemplate
	ShapeTemplateName string // Named shape reference; resolved at execution time
	IncludeBundle     bool   // when true, include bundle in shape response
	InlineSpecs       map[string]*Spec
}

// RelationshipNode identifies relationship traversals declared within a query.
type RelationshipNode struct {
	Alias      string
	Definition RelationshipDefinition
	Filters    []FilterNode
	Depth      int
}

// Parse converts a MemQL query string into a QueryPlan.
func (e *MemQLEngine) Parse(query string) (*QueryPlan, error) {
	if !e.canResolve() {
		return nil, ErrEngineNotInitialized
	}

	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, ErrEmptyQuery
	}

	if isInsertFunction(trimmed) {
		mutation, err := parseInsertFunction(trimmed)
		if err != nil {
			return nil, err
		}
		return &QueryPlan{
			Root:          nil,
			Mutations:     []MutationNode{*mutation},
			Filters:       nil,
			Relationships: nil,
			Timestamp:     nil,
			UseLatest:     false,
		}, nil
	}

	tokens, err := tokenize(trimmed)
	if err != nil {
		return nil, err
	}

	p := newParser(tokens, e.functions)
	root, err := p.parse()
	if err != nil {
		return nil, err
	}

	inlineSpecs, err := inlineDefinitionsToSpecs(p.inlineSpecDefinitions())
	if err != nil {
		return nil, err
	}

	if root == nil {
		return nil, ErrEmptyQuery
	}

	var (
		timestamp *time.Time
		useLatest bool
	)

	if p.match(tokAt) {
		if p.peek().typ == tokEOF {
			return nil, p.errorf(p.previous(), "timestamp suffix requires a value")
		}

		switch next := p.peek(); next.typ {
		case tokString:
			p.next()
			parsed, err := time.Parse(time.RFC3339, next.literal)
			if err != nil {
				return nil, p.errorf(next, "invalid timestamp %q: %v", next.literal, err)
			}
			timestamp = &parsed
		case tokIdentifier:
			p.next()
			if strings.EqualFold(next.literal, "latest") {
				useLatest = true
				timestamp = nil
			} else {
				return nil, p.errorf(next, "unexpected timestamp keyword %q", next.literal)
			}
		default:
			return nil, p.errorf(next, "unexpected token %q after '@'", next.literal)
		}
	}

	if p.peek().typ != tokEOF {
		return nil, p.errorf(p.peek(), "unexpected token %q after expression", p.peek().literal)
	}

	plan := &QueryPlan{
		Root:          root,
		Mutations:     nil,
		Filters:       nil,
		Relationships: nil,
		Timestamp:     timestamp,
		UseLatest:     useLatest,
		CacheHints:    nil,
		ConceptFields: make(map[string][]FieldReference),
		Metadata: metadataSelection{
			IncludeAll: true,
			Fields:     make(map[string]struct{}),
		},
		InlineSpecs: nil,
	}

	normalizedRoot, err := applyDirectiveWrappers(plan)
	if err != nil {
		return nil, err
	}
	plan.Root = normalizedRoot
	if err := e.resolvePlanSpecs(plan, inlineSpecs); err != nil {
		return nil, err
	}
	// Resolve function calls after spec resolution
	if err := resolvePlanFunctions(plan, e.functions, e.specs); err != nil {
		return nil, err
	}
	// Apply directive wrappers again after function resolution
	// Functions may expand to expressions containing directives (shape, sort, paginate, etc.)
	// that need to be extracted into the plan
	normalizedRoot, err = applyDirectiveWrappers(plan)
	if err != nil {
		return nil, err
	}
	plan.Root = normalizedRoot
	populateCacheHints(plan)
	populateConceptFields(plan)
	if err := validateSIContext(plan.Root); err != nil {
		return nil, err
	}

	return plan, nil
}

func applyDirectiveWrappers(plan *QueryPlan) (ExpressionNode, error) {
	expr := plan.Root
	for {
		switch node := expr.(type) {
		case *SortExpression:
			if len(node.Fields) == 0 {
				return nil, fmt.Errorf("sort() requires at least one field")
			}
			if len(plan.Sort) > 0 {
				return nil, fmt.Errorf("multiple sort() directives are not supported")
			}
			fields := make([]SortField, len(node.Fields))
			copy(fields, node.Fields)
			plan.Sort = fields
			expr = node.Target
		case *SelectExpression:
			if len(node.Fields) == 0 {
				return nil, fmt.Errorf("select() requires at least one field")
			}
			if len(plan.Fields) > 0 {
				return nil, fmt.Errorf("multiple select() directives are not supported")
			}
			payloadRefs, metadataSel, err := splitSelectFields(node.Fields)
			if err != nil {
				return nil, err
			}
			plan.Fields = payloadRefs
			plan.Metadata = metadataSel
			plan.PayloadSelect = true
			expr = node.Target
		case *PaginateExpression:
			if plan.Limit != nil || plan.Offset != nil {
				return nil, fmt.Errorf("multiple paginate() directives are not supported")
			}
			plan.Limit = node.Limit
			plan.Offset = node.Offset
			expr = node.Target
		case *TimestampExpression:
			if plan.Timestamp != nil || plan.UseLatest {
				return nil, fmt.Errorf("multiple asOf() directives are not supported")
			}
			plan.Timestamp = node.Timestamp
			plan.UseLatest = node.UseLatest
			expr = node.Target
		case *DepthExpression:
			if plan.Depth != nil {
				return nil, fmt.Errorf("multiple withDepth() directives are not supported")
			}
			depth := node.Depth
			plan.Depth = &depth
			expr = node.Target
		case *ShapeExpression:
			if plan.ShapeTemplate != nil || plan.ShapeTemplateName != "" {
				return nil, fmt.Errorf("multiple shape() directives are not supported")
			}
			plan.ShapeTemplate = node.Template
			plan.ShapeTemplateName = node.TemplateName
			plan.IncludeBundle = node.IncludeBundle
			expr = node.Target
		default:
			if err := ensureNoDirectiveNodes(expr); err != nil {
				return nil, err
			}
			return expr, nil
		}
	}
}

func ensureNoDirectiveNodes(expr ExpressionNode) error {
	if expr == nil {
		return nil
	}
	switch node := expr.(type) {
	case *SortExpression, *SelectExpression, *PaginateExpression, *TimestampExpression, *DepthExpression, *ShapeExpression:
		return fmt.Errorf("directive functions (e.g., paginate()) must be the outermost wrapper around the query expression")
	case *LogicalExpression:
		if err := ensureNoDirectiveNodes(node.Left); err != nil {
			return err
		}
		return ensureNoDirectiveNodes(node.Right)
	case *RelationshipExpression:
		return ensureNoDirectiveNodes(node.Target)
	default:
		return nil
	}
}

func isInsertFunction(query string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(query))
	return strings.HasPrefix(trimmed, "insert(")
}

// parser implements a recursive-descent MemQL parser.
type parser struct {
	tokens          []token
	pos             int
	inlineSpecs     []inlineSpecDefinition
	builtinByLookup map[string]*Function
}

type inlineSpecDefinition struct {
	Name string
	Expr ExpressionNode
}

func newParser(tokens []token, functions *FunctionRegistry) *parser {
	result := &parser{
		tokens:          tokens,
		inlineSpecs:     make([]inlineSpecDefinition, 0),
		builtinByLookup: make(map[string]*Function),
	}
	if functions == nil {
		return result
	}
	for _, fn := range functions.Snapshot() {
		if fn == nil || !fn.IsBuiltin() {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(fn.Name))
		if name != "" {
			result.builtinByLookup[name] = fn
		}
		for _, alias := range fn.BuiltinAliases {
			alias = strings.ToLower(strings.TrimSpace(alias))
			if alias != "" {
				result.builtinByLookup[alias] = fn
			}
		}
	}
	return result
}

func (p *parser) parse() (ExpressionNode, error) {
	if err := p.parseInlineSpecDefinitions(); err != nil {
		return nil, err
	}
	expr, err := p.parseOr(false)
	if err != nil {
		return nil, err
	}
	return expr, nil
}

func (p *parser) inlineSpecDefinitions() []inlineSpecDefinition {
	if p == nil || len(p.inlineSpecs) == 0 {
		return nil
	}
	defs := make([]inlineSpecDefinition, len(p.inlineSpecs))
	copy(defs, p.inlineSpecs)
	return defs
}

func (p *parser) parseInlineSpecDefinitions() error {
	for {
		if p.peek().typ != tokIdentifier || p.peekNext().typ != tokDefine {
			break
		}
		def, err := p.consumeInlineSpecDefinition()
		if err != nil {
			return err
		}
		p.inlineSpecs = append(p.inlineSpecs, def)
	}
	return nil
}

func (p *parser) consumeInlineSpecDefinition() (inlineSpecDefinition, error) {
	nameTok := p.next()
	if _, err := p.expect(tokDefine, "expected ':=' after spec name"); err != nil {
		return inlineSpecDefinition{}, err
	}
	expr, err := p.parseOr(false)
	if err != nil {
		return inlineSpecDefinition{}, err
	}
	return inlineSpecDefinition{
		Name: strings.TrimSpace(nameTok.literal),
		Expr: expr,
	}, nil
}

func (p *parser) parseOr(stopOnComma bool) (ExpressionNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}

	for {
		if stopOnComma && p.peek().typ == tokComma {
			break
		}
		if !p.match(tokComma) {
			break
		}
		if p.peek().typ == tokEOF {
			return nil, p.errorf(p.previous(), "unexpected end of query after ','")
		}
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &LogicalExpression{
			Op:    LogicalOr,
			Left:  left,
			Right: right,
		}
	}

	return left, nil
}

func (p *parser) parseAnd() (ExpressionNode, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}

	for p.match(tokSemicolon) || p.match(tokAmpAmp) {
		if p.peek().typ == tokEOF {
			return nil, p.errorf(p.previous(), "unexpected end of query after ';'")
		}
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		left = &LogicalExpression{
			Op:    LogicalAnd,
			Left:  left,
			Right: right,
		}
	}

	return left, nil
}

func (p *parser) parsePrimary() (ExpressionNode, error) {
	switch p.peek().typ {
	case tokParenOpen:
		p.next()
		expr, err := p.parseOr(false)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokParenClose, "expected ')' to close expression"); err != nil {
			return nil, err
		}
		return expr, nil
	case tokIdentifier:
		identifier := p.peek()
		if next := p.peekNext(); next.typ == tokParenOpen {
			switch {
			case strings.EqualFold(identifier.literal, "sort"):
				return p.parseSort()
			case strings.EqualFold(identifier.literal, "paginate"):
				return p.parsePaginate()
			case strings.EqualFold(identifier.literal, "select"):
				return p.parseSelect()
			case strings.EqualFold(identifier.literal, "asof"):
				return p.parseAsOf()
			case strings.EqualFold(identifier.literal, "withdepth"):
				return p.parseWithDepth()
			case strings.EqualFold(identifier.literal, "shape"):
				return p.parseShape(false)
			case strings.EqualFold(identifier.literal, "shapewithbundle"):
				return p.parseShape(true)
			case strings.EqualFold(identifier.literal, "si"):
				return p.parseSIExpression()
			default:
				if fn, ok := toRelationshipFunction(identifier.literal); ok {
					return p.parseRelationship(fn)
				}
				if builtin, ok := p.lookupBuiltin(identifier.literal); ok {
					return p.parseBuiltinFunctionCall(identifier.literal, builtin)
				}
				// Check if this is a user-defined function call (functionName())
				// User functions are camelCase identifiers with empty parentheses
				return p.parseFunctionCall()
			}
		}
		return p.parseComparison()
	case tokQuestionDot:
		// Handle ?.filter conditional syntax (optional chaining style)
		return p.parseConditionalFilter()
	default:
		return nil, p.errorf(p.peek(), "unexpected token %q", p.peek().literal)
	}
}

// parseConditionalFilter parses ?.field==ctx.name syntax for optional filters.
// The filter is only applied if the referenced argument field exists.
// Uses ?. (optional chaining style) to distinguish from ? (ternary operator).
func (p *parser) parseConditionalFilter() (ExpressionNode, error) {
	questionDotTok := p.next() // consume '?.'

	// Parse the underlying comparison
	if p.peek().typ != tokIdentifier {
		return nil, p.errorf(questionDotTok, "expected field identifier after '?.'")
	}

	comparison, err := p.parseComparison()
	if err != nil {
		return nil, err
	}

	// Extract the ArgReference from the comparison to determine the condition
	comp, ok := comparison.(*ComparisonExpression)
	if !ok {
		return nil, p.errorf(questionDotTok, "?. can only be used with comparison expressions")
	}

	// The comparison value must be an ArgReference for conditional filters
	argRef, ok := comp.Value.(*ArgReference)
	if !ok {
		return nil, p.errorf(questionDotTok, "conditional filter (?.) requires arg() reference as value")
	}

	return &ConditionalFilterExpression{
		ArgPath: argRef.Path,
		Filter:  comparison,
	}, nil
}

func (p *parser) parseRelationship(fn RelationshipFunction) (ExpressionNode, error) {
	fnTok := p.next()

	if _, err := p.expect(tokParenOpen, "expected '(' after relationship function"); err != nil {
		return nil, err
	}

	if p.peek().typ == tokParenClose {
		return nil, p.errorf(fnTok, "relationship function %q requires an expression argument", fnTok.literal)
	}

	target, err := p.parseOr(false)
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(tokParenClose, "expected ')' to close relationship function"); err != nil {
		return nil, err
	}

	return &RelationshipExpression{
		Function: fn,
		Target:   target,
	}, nil
}

// parseFunctionCall parses a user-defined function call: functionName({args})
// User functions must be camelCase and require a single JSON object argument.
func (p *parser) parseFunctionCall() (ExpressionNode, error) {
	fnTok := p.next() // consume the identifier

	if _, err := p.expect(tokParenOpen, "expected '(' after function name"); err != nil {
		return nil, err
	}

	// Validate function name is camelCase
	name := fnTok.literal
	if len(name) == 0 || !unicode.IsLower(rune(name[0])) {
		return nil, p.errorf(fnTok, "function name %q must start with a lowercase letter", name)
	}

	// Parse the required JSON object argument
	var args map[string]any
	if p.peek().typ == tokBraceOpen {
		parsedArgs, err := p.parseFunctionArgs()
		if err != nil {
			return nil, err
		}
		args = parsedArgs
	} else if p.peek().typ == tokParenClose {
		// Empty parentheses () - use empty object
		args = make(map[string]any)
	} else {
		return nil, p.errorf(p.peek(), "function %q requires a JSON object argument, got %q", name, p.peek().literal)
	}

	if _, err := p.expect(tokParenClose, "expected ')' to close function call"); err != nil {
		return nil, err
	}

	return &FunctionCallExpression{
		Name: name,
		Args: args,
	}, nil
}

// parseFunctionArgs parses a function call's object-literal argument. Keys
// are bare identifiers in the canonical form (e.g., `{spaceId: "..."}`);
// quoted string keys are accepted for tool-serialized calls that arrive as
// JSON. Values follow the standard MemQL value grammar (quoted strings,
// numbers, booleans, null, nested objects, arrays).
func (p *parser) parseFunctionArgs() (map[string]any, error) {
	if _, err := p.expect(tokBraceOpen, "expected '{' to start function arguments"); err != nil {
		return nil, err
	}

	args := make(map[string]any)

	// Handle empty object
	if p.match(tokBraceClose) {
		return args, nil
	}

	for {
		// Accept either bare identifier keys (canonical) or quoted string
		// keys (tolerated so JSON-serialized tool calls still parse).
		tok := p.peek()
		var key string
		switch tok.typ {
		case tokIdentifier:
			p.next()
			key = tok.literal
		case tokString:
			p.next()
			key = tok.literal
		default:
			return nil, p.errorf(tok, "expected argument key (identifier or quoted string), got %q", tok.literal)
		}

		if _, err := p.expect(tokColon, "expected ':' after argument key"); err != nil {
			return nil, err
		}

		// Parse value
		value, err := p.parseFunctionArgValue()
		if err != nil {
			return nil, err
		}
		args[key] = value

		// Check for more fields or end of object
		if p.match(tokBraceClose) {
			break
		}
		if _, err := p.expect(tokComma, "expected ',' between arguments"); err != nil {
			return nil, err
		}
	}

	return args, nil
}

// parseFunctionArgValue parses a value in a function argument object.
func (p *parser) parseFunctionArgValue() (any, error) {
	tok := p.peek()
	switch tok.typ {
	case tokString:
		p.next()
		return tok.literal, nil
	case tokNumber:
		p.next()
		if strings.ContainsAny(tok.literal, ".eE") {
			val, err := strconv.ParseFloat(tok.literal, 64)
			if err != nil {
				return nil, p.errorf(tok, "invalid number literal %q", tok.literal)
			}
			return val, nil
		}
		val, err := strconv.ParseInt(tok.literal, 10, 64)
		if err != nil {
			return nil, p.errorf(tok, "invalid integer literal %q", tok.literal)
		}
		return val, nil
	case tokIdentifier:
		p.next()
		switch tok.literal {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return nil, p.errorf(tok, "unexpected identifier %q in function argument", tok.literal)
		}
	case tokBraceOpen:
		return p.parseFunctionArgs()
	case tokBracketOpen:
		return p.parseFunctionArgArray()
	default:
		return nil, p.errorf(tok, "unexpected token %q in function argument", tok.literal)
	}
}

// parseFunctionArgArray parses an array value in a function argument.
func (p *parser) parseFunctionArgArray() ([]any, error) {
	if _, err := p.expect(tokBracketOpen, "expected '[' to start array"); err != nil {
		return nil, err
	}

	items := make([]any, 0)

	// Handle empty array
	if p.match(tokBracketClose) {
		return items, nil
	}

	for {
		value, err := p.parseFunctionArgValue()
		if err != nil {
			return nil, err
		}
		items = append(items, value)

		if p.match(tokBracketClose) {
			break
		}
		if _, err := p.expect(tokComma, "expected ',' between array elements"); err != nil {
			return nil, err
		}
	}

	return items, nil
}

func (p *parser) parseSIExpression() (ExpressionNode, error) {
	p.next()

	if _, err := p.expect(tokParenOpen, "expected '(' after si"); err != nil {
		return nil, err
	}

	idTok, err := p.expect(tokString, "si(): first argument must be a string literal (prompt template ID)")
	if err != nil {
		return nil, err
	}
	templateId := strings.TrimSpace(idTok.literal)
	if templateId == "" {
		return nil, p.errorf(idTok, "si(): prompt template ID must not be empty")
	}

	invocation := &SIInvocation{
		TemplateId: templateId,
	}

	for p.match(tokComma) {
		next := p.peek()
		switch next.typ {
		case tokString:
			if invocation.ProviderOverride != nil {
				return nil, p.errorf(next, "si() provider override specified multiple times")
			}
			p.next()
			invocation.ProviderOverride = optionalString(next.literal)
		case tokNumber:
			if invocation.CacheSeconds != nil {
				return nil, p.errorf(next, "si() cache ttl specified multiple times")
			}
			p.next()
			ttl, err := p.parseNonNegativeInt(next, "si() cache ttl")
			if err != nil {
				return nil, err
			}
			invocation.CacheSeconds = &ttl
		case tokBraceOpen, tokBracketOpen:
			return nil, p.errorf(next, "si() data arguments are only supported inside projection contexts such as shape()")
		default:
			return nil, p.errorf(next, "si() provider override must be a string literal or integer ttl in this context")
		}
	}

	if _, err := p.expect(tokParenClose, "expected ')' to close si() call"); err != nil {
		return nil, err
	}

	return &SIExpression{Invocation: invocation}, nil
}

func (p *parser) parseDocsFunction() (ExpressionNode, error) {
	fnTok := p.next()
	if _, err := p.expect(tokParenOpen, "expected '(' after memqlDocs"); err != nil {
		return nil, err
	}
	if _, err := p.expect(tokParenClose, "memqlDocs() does not accept arguments"); err != nil {
		return nil, p.errorf(fnTok, "memqlDocs() does not accept arguments")
	}
	return &BuiltinFunctionExpression{
		Name:     "memqlDocs",
		Executor: BuiltinExecutorMemqlDocs,
	}, nil
}

func (p *parser) parseConceptsFunction() (ExpressionNode, error) {
	fnTok := p.next()
	if _, err := p.expect(tokParenOpen, "expected '(' after concepts"); err != nil {
		return nil, err
	}

	var args map[string]any

	// Check for optional pattern argument
	if p.peek().typ == tokString {
		patternTok := p.next()
		pattern := patternTok.literal
		if pattern == "" {
			return nil, p.errorf(patternTok, "concepts() pattern cannot be empty")
		}
		args = map[string]any{"pattern": pattern}
	}

	if _, err := p.expect(tokParenClose, "expected ')' after concepts arguments"); err != nil {
		return nil, p.errorf(fnTok, "concepts() accepts an optional string pattern argument")
	}

	return &BuiltinFunctionExpression{
		Name:     "concepts",
		Executor: BuiltinExecutorConcepts,
		Args:     args,
	}, nil
}

// parseValidateFunction parses validate({concept: "...", payload: {...}})
// Returns a BuiltinFunctionExpression with the parsed arguments.
func (p *parser) parseValidateFunction() (ExpressionNode, error) {
	fnTok := p.next() // consume "validate"
	if _, err := p.expect(tokParenOpen, "expected '(' after validate"); err != nil {
		return nil, err
	}

	// Expect a JSON object as the argument
	if p.peek().typ != tokBraceOpen {
		return nil, p.errorf(fnTok, "validate() requires a JSON object argument with 'concept' and 'payload' fields")
	}

	args, err := p.parseFunctionArgs()
	if err != nil {
		return nil, p.errorf(fnTok, "validate() argument: %v", err)
	}

	// Validate required fields
	concept, ok := args["concept"]
	if !ok {
		return nil, p.errorf(fnTok, "validate() requires 'concept' field in argument")
	}
	if _, ok := concept.(string); !ok {
		return nil, p.errorf(fnTok, "validate() 'concept' field must be a string")
	}

	_, hasPayload := args["payload"]
	if !hasPayload {
		return nil, p.errorf(fnTok, "validate() requires 'payload' field in argument")
	}

	if _, err := p.expect(tokParenClose, "expected ')' after validate arguments"); err != nil {
		return nil, err
	}

	return &BuiltinFunctionExpression{
		Name:     "validate",
		Executor: BuiltinExecutorValidate,
		Args:     args,
	}, nil
}

// parseFunctionsBuiltin parses functions() - no arguments
func (p *parser) parseFunctionsBuiltin() (ExpressionNode, error) {
	fnTok := p.next() // consume "functions"
	if _, err := p.expect(tokParenOpen, "expected '(' after functions"); err != nil {
		return nil, err
	}
	if _, err := p.expect(tokParenClose, "functions() does not accept arguments"); err != nil {
		return nil, p.errorf(fnTok, "functions() does not accept arguments")
	}
	return &BuiltinFunctionExpression{
		Name:     "functions",
		Executor: BuiltinExecutorFunctions,
	}, nil
}

// parseToolsBuiltin parses tools() - no arguments
func (p *parser) parseToolsBuiltin() (ExpressionNode, error) {
	fnTok := p.next() // consume "tools"
	if _, err := p.expect(tokParenOpen, "expected '(' after tools"); err != nil {
		return nil, err
	}
	if _, err := p.expect(tokParenClose, "tools() does not accept arguments"); err != nil {
		return nil, p.errorf(fnTok, "tools() does not accept arguments")
	}
	return &BuiltinFunctionExpression{
		Name:     "tools",
		Executor: BuiltinExecutorTools,
	}, nil
}

// parseHelpBuiltin parses help({"name": "..."}) - requires name argument
func (p *parser) parseHelpBuiltin() (ExpressionNode, error) {
	fnTok := p.next() // consume "help"
	if _, err := p.expect(tokParenOpen, "expected '(' after help"); err != nil {
		return nil, err
	}

	// Expect a JSON object or string as the argument
	var args map[string]any
	switch p.peek().typ {
	case tokBraceOpen:
		var err error
		args, err = p.parseFunctionArgs()
		if err != nil {
			return nil, p.errorf(fnTok, "help() argument: %v", err)
		}
	case tokString:
		// Allow shorthand: help("functionName")
		nameTok := p.next()
		args = map[string]any{"name": nameTok.literal}
	default:
		return nil, p.errorf(fnTok, "help() requires a name argument: help(\"name\") or help({\"name\": \"...\"})")
	}

	// Validate required name field
	name, ok := args["name"]
	if !ok {
		return nil, p.errorf(fnTok, "help() requires 'name' field in argument")
	}
	if _, ok := name.(string); !ok {
		return nil, p.errorf(fnTok, "help() 'name' field must be a string")
	}

	if _, err := p.expect(tokParenClose, "expected ')' after help arguments"); err != nil {
		return nil, err
	}

	return &BuiltinFunctionExpression{
		Name:     "help",
		Executor: BuiltinExecutorHelp,
		Args:     args,
	}, nil
}

// parseShapeTemplatesBuiltin parses shapeTemplates() or shapeTemplates("conceptName")
func (p *parser) parseShapeTemplatesBuiltin() (ExpressionNode, error) {
	fnTok := p.next() // consume "shapeTemplates"
	if _, err := p.expect(tokParenOpen, "expected '(' after shapeTemplates"); err != nil {
		return nil, err
	}

	var args map[string]any
	// Optional string argument for concept filter
	if p.peek().typ == tokString {
		conceptTok := p.next()
		args = map[string]any{"concept": conceptTok.literal}
	} else if p.peek().typ == tokBraceOpen {
		var err error
		args, err = p.parseFunctionArgs()
		if err != nil {
			return nil, p.errorf(fnTok, "shapeTemplates() argument: %v", err)
		}
	}

	if _, err := p.expect(tokParenClose, "expected ')' after shapeTemplates arguments"); err != nil {
		return nil, err
	}

	return &BuiltinFunctionExpression{
		Name:     "shapeTemplates",
		Executor: BuiltinExecutorShapeTemplates,
		Args:     args,
	}, nil
}

// parseShapeHelpBuiltin parses shapeHelp("shapeName") or shapeHelp({"name": "..."})
func (p *parser) parseShapeHelpBuiltin() (ExpressionNode, error) {
	fnTok := p.next() // consume "shapeHelp"
	if _, err := p.expect(tokParenOpen, "expected '(' after shapeHelp"); err != nil {
		return nil, err
	}

	var args map[string]any
	switch p.peek().typ {
	case tokBraceOpen:
		var err error
		args, err = p.parseFunctionArgs()
		if err != nil {
			return nil, p.errorf(fnTok, "shapeHelp() argument: %v", err)
		}
	case tokString:
		// Allow shorthand: shapeHelp("shapeName")
		nameTok := p.next()
		args = map[string]any{"name": nameTok.literal}
	default:
		return nil, p.errorf(fnTok, "shapeHelp() requires a name argument: shapeHelp(\"name\") or shapeHelp({\"name\": \"...\"})")
	}

	// Validate required name field
	name, ok := args["name"]
	if !ok {
		return nil, p.errorf(fnTok, "shapeHelp() requires 'name' field in argument")
	}
	if _, ok := name.(string); !ok {
		return nil, p.errorf(fnTok, "shapeHelp() 'name' field must be a string")
	}

	if _, err := p.expect(tokParenClose, "expected ')' after shapeHelp arguments"); err != nil {
		return nil, err
	}

	return &BuiltinFunctionExpression{
		Name:     "shapeHelp",
		Executor: BuiltinExecutorShapeHelp,
		Args:     args,
	}, nil
}

// parseContentIdBuiltin parses contentId({"concept": "...", "payload": {...}})
func (p *parser) parseContentIdBuiltin() (ExpressionNode, error) {
	fnTok := p.next() // consume "contentId"
	if _, err := p.expect(tokParenOpen, "expected '(' after contentId"); err != nil {
		return nil, err
	}

	// Expect a JSON object as the argument
	if p.peek().typ != tokBraceOpen {
		return nil, p.errorf(fnTok, "contentId() requires a JSON object argument")
	}

	args, err := p.parseFunctionArgs()
	if err != nil {
		return nil, p.errorf(fnTok, "contentId() argument: %v", err)
	}

	// No validation at parse time - let executor return structured errors
	if _, err := p.expect(tokParenClose, "expected ')' after contentId arguments"); err != nil {
		return nil, err
	}

	return &BuiltinFunctionExpression{
		Name:     "contentId",
		Executor: BuiltinExecutorContentId,
		Args:     args,
	}, nil
}

// parsePreviewInsertBuiltin parses previewInsert({"concept": "...", "payload": {...}})
func (p *parser) parsePreviewInsertBuiltin() (ExpressionNode, error) {
	fnTok := p.next() // consume "previewInsert"
	if _, err := p.expect(tokParenOpen, "expected '(' after previewInsert"); err != nil {
		return nil, err
	}

	// Expect a JSON object as the argument
	if p.peek().typ != tokBraceOpen {
		return nil, p.errorf(fnTok, "previewInsert() requires a JSON object argument")
	}

	args, err := p.parseFunctionArgs()
	if err != nil {
		return nil, p.errorf(fnTok, "previewInsert() argument: %v", err)
	}

	// No validation at parse time - let executor return structured errors
	if _, err := p.expect(tokParenClose, "expected ')' after previewInsert arguments"); err != nil {
		return nil, err
	}

	return &BuiltinFunctionExpression{
		Name:     "previewInsert",
		Executor: BuiltinExecutorPreviewInsert,
		Args:     args,
	}, nil
}

// parseServiceVersionBuiltin parses memqlVersion() / serviceVersion() - no arguments.
func (p *parser) parseServiceVersionBuiltin() (ExpressionNode, error) {
	fnTok := p.next()
	if _, err := p.expect(tokParenOpen, "expected '(' after version function"); err != nil {
		return nil, err
	}
	if _, err := p.expect(tokParenClose, "version function does not accept arguments"); err != nil {
		return nil, p.errorf(fnTok, "version function does not accept arguments")
	}
	return &BuiltinFunctionExpression{
		Name:     "memqlVersion",
		Executor: BuiltinExecutorServiceVersion,
	}, nil
}

func (p *parser) parseSort() (ExpressionNode, error) {
	fnTok := p.next()

	if _, err := p.expect(tokParenOpen, "expected '(' after sort"); err != nil {
		return nil, err
	}

	if p.peek().typ == tokParenClose {
		return nil, p.errorf(fnTok, "sort() requires an expression argument")
	}

	target, err := p.parseOr(true)
	if err != nil {
		return nil, err
	}

	fields, err := p.parseSortFields()
	if err != nil {
		return nil, err
	}

	if len(fields) == 0 {
		return nil, p.errorf(fnTok, "sort() requires at least one field")
	}

	if _, err := p.expect(tokParenClose, "expected ')' to close sort()"); err != nil {
		return nil, err
	}

	return &SortExpression{
		Target: target,
		Fields: fields,
	}, nil
}

func (p *parser) parseSortFields() ([]SortField, error) {
	fields := make([]SortField, 0, 1)

	for {
		if len(fields) == 0 {
			if _, err := p.expect(tokComma, "expected ',' before first sort field"); err != nil {
				return nil, err
			}
		} else {
			if _, err := p.expect(tokComma, "expected ',' before additional sort field"); err != nil {
				return nil, err
			}
		}

		fieldTok, err := p.expect(tokString, "sort field must be a string literal (e.g. \"createdAt\" or \"payload.title\")")
		if err != nil {
			return nil, err
		}

		field := strings.TrimSpace(fieldTok.literal)
		if field == "" {
			return nil, p.errorf(fieldTok, "sort field must not be empty")
		}

		direction := SortDirectionDesc
		if p.peek().typ == tokComma {
			if next := p.peekNext(); next.typ == tokString && isSortDirectionLiteral(next.literal) {
				p.next() // consume comma
				p.next() // consume direction literal
				direction = parseSortDirection(next.literal)
			}
		}

		fields = append(fields, SortField{Field: field, Direction: direction})

		if p.peek().typ == tokParenClose {
			break
		}
	}

	return fields, nil
}

func (p *parser) parseSelect() (ExpressionNode, error) {
	fnTok := p.next()

	if _, err := p.expect(tokParenOpen, "expected '(' after select"); err != nil {
		return nil, err
	}

	if p.peek().typ == tokParenClose {
		return nil, p.errorf(fnTok, "select() requires an expression argument")
	}

	target, err := p.parseOr(true)
	if err != nil {
		return nil, err
	}

	if p.peek().typ == tokParenClose {
		return nil, p.errorf(p.peek(), "select() requires at least one field")
	}

	fields, err := p.parseFieldReferenceList("select", true)
	if err != nil {
		return nil, err
	}

	if len(fields) == 0 {
		return nil, p.errorf(fnTok, "select() requires at least one field")
	}

	if _, err := p.expect(tokParenClose, "expected ')' to close select()"); err != nil {
		return nil, err
	}

	return &SelectExpression{
		Target: target,
		Fields: fields,
	}, nil
}

func isSortDirectionLiteral(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(SortDirectionAsc), string(SortDirectionDesc):
		return true
	default:
		return false
	}
}

func parseSortDirection(value string) SortDirection {
	if strings.EqualFold(strings.TrimSpace(value), string(SortDirectionAsc)) {
		return SortDirectionAsc
	}
	return SortDirectionDesc
}

func (p *parser) parsePaginate() (ExpressionNode, error) {
	fnTok := p.next()

	if _, err := p.expect(tokParenOpen, "expected '(' after paginate"); err != nil {
		return nil, err
	}

	if p.peek().typ == tokParenClose {
		return nil, p.errorf(fnTok, "paginate() requires an expression argument")
	}

	target, err := p.parseOr(true)
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(tokComma, "expected ',' before paginate limit"); err != nil {
		return nil, err
	}

	limitTok, err := p.expect(tokNumber, "paginate limit must be an integer literal")
	if err != nil {
		return nil, err
	}

	limit, err := p.parsePositiveInt(limitTok, "paginate limit")
	if err != nil {
		return nil, err
	}

	var offsetPtr *int
	if p.peek().typ == tokComma {
		p.next()
		offsetTok, err := p.expect(tokNumber, "paginate offset must be an integer literal")
		if err != nil {
			return nil, err
		}
		offset, err := p.parseNonNegativeInt(offsetTok, "paginate offset")
		if err != nil {
			return nil, err
		}
		offsetPtr = &offset
	}

	if _, err := p.expect(tokParenClose, "expected ')' to close paginate()"); err != nil {
		return nil, err
	}

	return &PaginateExpression{
		Target: target,
		Limit:  &limit,
		Offset: offsetPtr,
	}, nil
}

func (p *parser) parseAsOf() (ExpressionNode, error) {
	fnTok := p.next()

	if _, err := p.expect(tokParenOpen, "expected '(' after asOf"); err != nil {
		return nil, err
	}

	if p.peek().typ == tokParenClose {
		return nil, p.errorf(fnTok, "asOf() requires an expression argument")
	}

	target, err := p.parseOr(true)
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(tokComma, "expected ',' before asOf timestamp"); err != nil {
		return nil, err
	}

	var (
		timestamp *time.Time
		useLatest bool
	)

	switch tok := p.peek(); tok.typ {
	case tokString:
		p.next()
		value := strings.TrimSpace(tok.literal)
		if value == "" {
			return nil, p.errorf(tok, "asOf timestamp must not be empty")
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, p.errorf(tok, "invalid RFC3339 timestamp %q", value)
		}
		timestamp = &parsed
	case tokIdentifier:
		p.next()
		if strings.EqualFold(strings.TrimSpace(tok.literal), "latest") {
			useLatest = true
		} else {
			return nil, p.errorf(tok, "asOf second argument must be an RFC3339 string or latest")
		}
	default:
		return nil, p.errorf(tok, "asOf second argument must be an RFC3339 string or latest")
	}

	if _, err := p.expect(tokParenClose, "expected ')' to close asOf()"); err != nil {
		return nil, err
	}

	return &TimestampExpression{
		Target:    target,
		Timestamp: timestamp,
		UseLatest: useLatest,
	}, nil
}

func (p *parser) parseWithDepth() (ExpressionNode, error) {
	fnTok := p.next()

	if _, err := p.expect(tokParenOpen, "expected '(' after withDepth"); err != nil {
		return nil, err
	}

	if p.peek().typ == tokParenClose {
		return nil, p.errorf(fnTok, "withDepth() requires an expression argument")
	}

	target, err := p.parseOr(true)
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(tokComma, "expected ',' before withDepth value"); err != nil {
		return nil, err
	}

	depthTok, err := p.expect(tokNumber, "withDepth value must be an integer literal")
	if err != nil {
		return nil, err
	}

	depth, err := p.parsePositiveInt(depthTok, "withDepth value")
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(tokParenClose, "expected ')' to close withDepth()"); err != nil {
		return nil, err
	}

	return &DepthExpression{
		Target: target,
		Depth:  depth,
	}, nil
}

func (p *parser) parseShape(includeBundle bool) (ExpressionNode, error) {
	fnTok := p.next()
	fnName := fnTok.literal

	if _, err := p.expect(tokParenOpen, fmt.Sprintf("expected '(' after %s", fnName)); err != nil {
		return nil, err
	}

	if p.peek().typ == tokParenClose {
		return nil, p.errorf(fnTok, "%s() requires an expression argument", fnName)
	}

	target, err := p.parseOr(true)
	if err != nil {
		return nil, err
	}

	var tmpl shapeTemplate
	if p.match(tokComma) {
		if p.peek().typ == tokParenClose {
			return nil, p.errorf(fnTok, "%s() template cannot be empty", fnName)
		}
		templateValue, err := p.parseShapeTemplateValue()
		if err != nil {
			return nil, err
		}
		tmpl = templateValue
	}

	// Both shape() and shapeWithBundle() require a template
	if tmpl == nil {
		return nil, p.errorf(fnTok, "%s() requires a template argument", fnName)
	}

	if _, err := p.expect(tokParenClose, fmt.Sprintf("expected ')' to close %s()", fnName)); err != nil {
		return nil, err
	}

	return &ShapeExpression{
		Target:        target,
		Template:      tmpl,
		IncludeBundle: includeBundle,
	}, nil
}

func (p *parser) parseShapeTemplateValue() (shapeTemplate, error) {
	switch tok := p.peek(); tok.typ {
	case tokBraceOpen:
		return p.parseShapeObject()
	case tokBracketOpen:
		return p.parseShapeArray()
	case tokString:
		p.next()
		return &shapeLiteral{Value: tok.literal}, nil
	case tokNumber:
		p.next()
		if strings.ContainsAny(tok.literal, ".eE") {
			val, err := strconv.ParseFloat(tok.literal, 64)
			if err != nil {
				return nil, p.errorf(tok, "invalid number literal %q", tok.literal)
			}
			return &shapeLiteral{Value: val}, nil
		}
		val, err := strconv.ParseInt(tok.literal, 10, 64)
		if err != nil {
			return nil, p.errorf(tok, "invalid integer literal %q", tok.literal)
		}
		return &shapeLiteral{Value: val}, nil
	case tokIdentifier:
		return p.parseShapeIdentifierValue()
	default:
		return nil, p.errorf(tok, "unexpected token %q in shape template", tok.literal)
	}
}

func (p *parser) parseShapeObject() (shapeTemplate, error) {
	p.next() // consume '{'
	fields := make(map[string]shapeTemplate)

	if p.match(tokBraceClose) {
		return &shapeObject{Fields: fields}, nil
	}

	for {
		keyTok, err := p.expect(tokString, "shape template object keys must be strings")
		if err != nil {
			return nil, err
		}
		if err := p.expectShapeColon("expected ':' after object key"); err != nil {
			return nil, err
		}
		value, err := p.parseShapeTemplateValue()
		if err != nil {
			return nil, err
		}
		fields[keyTok.literal] = value
		if p.match(tokBraceClose) {
			break
		}
		if _, err := p.expect(tokComma, "expected ',' between object fields"); err != nil {
			return nil, err
		}
	}

	return &shapeObject{Fields: fields}, nil
}

func (p *parser) parseShapeArray() (shapeTemplate, error) {
	p.next() // consume '['
	items := make([]shapeTemplate, 0)

	if p.match(tokBracketClose) {
		return &shapeArray{Items: items}, nil
	}

	for {
		value, err := p.parseShapeTemplateValue()
		if err != nil {
			return nil, err
		}
		items = append(items, value)
		if p.match(tokBracketClose) {
			break
		}
		if _, err := p.expect(tokComma, "expected ',' between array elements"); err != nil {
			return nil, err
		}
	}

	return &shapeArray{Items: items}, nil
}

func (p *parser) expectShapeColon(message string) error {
	tok := p.next()
	if tok.typ == tokColon {
		return nil
	}
	if tok.typ == tokIdentifier && tok.literal == ":" {
		return nil
	}
	return p.errorf(tok, "%s", message)
}

func (p *parser) parseShapeIdentifierValue() (shapeTemplate, error) {
	tok := p.peek()
	lower := strings.ToLower(tok.literal)

	switch lower {
	case "true":
		p.next()
		return &shapeLiteral{Value: true}, nil
	case "false":
		p.next()
		return &shapeLiteral{Value: false}, nil
	case "null":
		p.next()
		return &shapeLiteral{Value: nil}, nil
	default:
		if next := p.peekNext(); next.typ == tokParenOpen {
			return p.parseShapeFunction()
		}
		return nil, p.errorf(tok, "unexpected identifier %q in shape template", tok.literal)
	}
}

func (p *parser) parseShapeFunction() (shapeTemplate, error) {
	fnTok := p.next()
	if _, err := p.expect(tokParenOpen, "expected '(' after shape function"); err != nil {
		return nil, err
	}
	name := strings.ToLower(fnTok.literal)

	switch name {
	case "node":
		fields := make([]FieldReference, 0)
		if p.match(tokParenClose) {
			return &shapeNodeFunc{Fields: fields}, nil
		}
		for {
			argTok, err := p.expect(tokString, "node() arguments must be string literals")
			if err != nil {
				return nil, err
			}
			ref, parseErr := parseFieldReferenceLiteral(argTok.literal)
			if parseErr != nil {
				return nil, parseErr
			}
			fields = append(fields, ref)
			if p.match(tokParenClose) {
				break
			}
			if _, err := p.expect(tokComma, "expected ',' between node() fields"); err != nil {
				return nil, err
			}
		}
		return &shapeNodeFunc{Fields: fields}, nil
	case "children", "aliases", "contains", "owns", "createdby", "interactions":
		if p.peek().typ == tokParenClose {
			return nil, p.errorf(fnTok, "%s() requires a template argument", fnTok.literal)
		}
		childTemplate, err := p.parseShapeTemplateValue()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokParenClose, fmt.Sprintf("expected ')' to close %s()", fnTok.literal)); err != nil {
			return nil, err
		}
		return &shapeRelationFunc{
			Relation: name,
			Template: childTemplate,
		}, nil
	case "si":
		value, err := p.parseShapeSIValue(fnTok)
		if err != nil {
			return nil, err
		}
		return value, nil
	case "match":
		value, err := p.parseShapeMatchExpr(fnTok)
		if err != nil {
			return nil, err
		}
		return value, nil
	case "json":
		if p.match(tokParenClose) {
			return nil, p.errorf(fnTok, "json() requires an argument")
		}
		inner, err := p.parseShapeTemplateValue()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokParenClose, "expected ')' to close json()"); err != nil {
			return nil, err
		}
		return &shapeJSONFunc{Inner: inner}, nil
	default:
		return nil, p.errorf(fnTok, "unknown shape function %q", fnTok.literal)
	}
}

func (p *parser) parseShapeSIValue(fnTok token) (shapeTemplate, error) {
	idTok, err := p.expect(tokString, "si() requires a prompt template ID string")
	if err != nil {
		return nil, err
	}
	templateId := strings.TrimSpace(idTok.literal)
	if templateId == "" {
		return nil, p.errorf(idTok, "si() prompt template ID must not be empty")
	}

	invocation := &SIInvocation{
		TemplateId: templateId,
	}

	var dataTemplate shapeTemplate
	for p.match(tokComma) {
		next := p.peek()
		switch next.typ {
		case tokBraceOpen:
			if dataTemplate != nil {
				return nil, p.errorf(next, "si() data argument specified multiple times")
			}
			value, err := p.parseShapeTemplateValue()
			if err != nil {
				return nil, err
			}
			if _, ok := value.(*shapeObject); !ok {
				return nil, p.errorf(fnTok, "si() data argument must be an object literal")
			}
			dataTemplate = value
		case tokString:
			if invocation.ProviderOverride != nil {
				return nil, p.errorf(next, "si() provider override specified multiple times")
			}
			p.next()
			invocation.ProviderOverride = optionalString(next.literal)
		case tokNumber:
			if invocation.CacheSeconds != nil {
				return nil, p.errorf(next, "si() cache ttl specified multiple times")
			}
			p.next()
			ttl, err := p.parseNonNegativeInt(next, "si() cache ttl")
			if err != nil {
				return nil, err
			}
			invocation.CacheSeconds = &ttl
		default:
			return nil, p.errorf(next, "si() arguments must be object literal, provider string, or integer ttl")
		}
	}

	if _, err := p.expect(tokParenClose, fmt.Sprintf("expected ')' to close %s()", fnTok.literal)); err != nil {
		return nil, err
	}

	return &shapeSIValue{
		Invocation: invocation,
		Data:       dataTemplate,
	}, nil
}

// parseShapeMatchExpr parses a match() expression with case() and default() branches.
// Syntax: match(case(condition, value), case(condition2, value2), default(fallback))
func (p *parser) parseShapeMatchExpr(fnTok token) (shapeTemplate, error) {
	match := &shapeMatchExpr{
		Cases: make([]*shapeCaseBranch, 0),
	}

	if p.peek().typ == tokParenClose {
		return nil, p.errorf(fnTok, "match() requires at least one case() or default()")
	}

	for {
		next := p.peek()
		if next.typ == tokParenClose {
			break
		}

		if next.typ != tokIdentifier {
			return nil, p.errorf(next, "match() expects case() or default(), got %q", next.literal)
		}

		branchName := strings.ToLower(next.literal)
		switch branchName {
		case "case":
			branch, err := p.parseShapeCaseBranch()
			if err != nil {
				return nil, err
			}
			match.Cases = append(match.Cases, branch)
		case "default":
			if match.Default != nil {
				return nil, p.errorf(next, "match() can only have one default()")
			}
			defaultValue, err := p.parseShapeDefaultBranch()
			if err != nil {
				return nil, err
			}
			match.Default = defaultValue
		default:
			return nil, p.errorf(next, "match() expects case() or default(), got %q()", branchName)
		}

		// Check for comma or closing paren
		if p.peek().typ == tokComma {
			p.next() // consume comma
			continue
		}
		if p.peek().typ == tokParenClose {
			break
		}
		return nil, p.errorf(p.peek(), "expected ',' or ')' in match(), got %q", p.peek().literal)
	}

	if len(match.Cases) == 0 && match.Default == nil {
		return nil, p.errorf(fnTok, "match() requires at least one case() or default()")
	}

	if _, err := p.expect(tokParenClose, "expected ')' to close match()"); err != nil {
		return nil, err
	}

	return match, nil
}

// parseShapeCaseBranch parses case(condition, value).
func (p *parser) parseShapeCaseBranch() (*shapeCaseBranch, error) {
	fnTok := p.next() // consume "case"
	if _, err := p.expect(tokParenOpen, "expected '(' after case"); err != nil {
		return nil, err
	}

	// Parse condition - can be spec name (identifier) or inline comparison (node(...) == value)
	condition, err := p.parseShapeCondition()
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(tokComma, "expected ',' after case condition"); err != nil {
		return nil, err
	}

	// Parse value template
	value, err := p.parseShapeTemplateValue()
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(tokParenClose, fmt.Sprintf("expected ')' to close %s()", fnTok.literal)); err != nil {
		return nil, err
	}

	return &shapeCaseBranch{
		Condition: condition,
		Value:     value,
	}, nil
}

// parseShapeDefaultBranch parses default(value).
func (p *parser) parseShapeDefaultBranch() (shapeTemplate, error) {
	fnTok := p.next() // consume "default"
	if _, err := p.expect(tokParenOpen, "expected '(' after default"); err != nil {
		return nil, err
	}

	if p.peek().typ == tokParenClose {
		return nil, p.errorf(fnTok, "default() requires a value")
	}

	value, err := p.parseShapeTemplateValue()
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(tokParenClose, "expected ')' to close default()"); err != nil {
		return nil, err
	}

	return value, nil
}

// parseShapeCondition parses a condition in case().
// Can be: specName (identifier) or node("field") == "value" (inline comparison)
func (p *parser) parseShapeCondition() (shapeCondition, error) {
	tok := p.peek()

	// Check if it's a function call like node()
	if tok.typ == tokIdentifier {
		nextTok := p.peekNext()
		if nextTok.typ == tokParenOpen && strings.ToLower(tok.literal) == "node" {
			// It's node() - parse as inline comparison
			return p.parseShapeInlineComparison()
		}
		// It's a spec name
		p.next() // consume identifier
		return &shapeSpecCondition{SpecName: tok.literal}, nil
	}

	return nil, p.errorf(tok, "case() condition must be a spec name or node() comparison, got %q", tok.literal)
}

// parseShapeInlineComparison parses node("field") == "value" style conditions.
func (p *parser) parseShapeInlineComparison() (shapeCondition, error) {
	// Parse left side (must be node())
	leftTemplate, err := p.parseShapeFunction()
	if err != nil {
		return nil, err
	}

	// Expect a comparison operator
	opTok := p.peek()
	op, err := p.parseShapeComparisonOperator()
	if err != nil {
		return nil, p.errorf(opTok, "expected comparison operator after node() in case condition")
	}

	// Parse right side (literal value or list)
	right, err := p.parseShapeComparisonValue(op)
	if err != nil {
		return nil, err
	}

	return &shapeComparisonCondition{
		Left:     leftTemplate,
		Operator: op,
		Right:    right,
	}, nil
}

// parseShapeComparisonOperator parses comparison operators in shape conditions.
// Handles both symbolic operators (==, !=, >, >=, <, <=) and keyword operators
// (in, not in, has).
func (p *parser) parseShapeComparisonOperator() (ComparisonOperator, error) {
	tok := p.peek()

	switch tok.typ {
	case tokOperator:
		p.next()
		op, err := toComparisonOperator(tok.literal)
		if err != nil {
			return "", fmt.Errorf("unsupported comparison operator %q", tok.literal)
		}
		return op, nil
	case tokIdentifier:
		ident := strings.TrimSpace(tok.literal)
		switch ident {
		case "in":
			p.next()
			return OpIn, nil
		case "has":
			p.next()
			return OpHas, nil
		case "not":
			p.next()
			nextTok := p.peek()
			if nextTok.typ == tokIdentifier && strings.TrimSpace(nextTok.literal) == "in" {
				p.next() // consume "in"
				return OpOut, nil
			}
			return "", fmt.Errorf("expected 'in' after 'not' (did you mean 'not in'?)")
		}
		return "", fmt.Errorf("expected comparison operator, got %q", tok.literal)
	default:
		return "", fmt.Errorf("expected comparison operator, got %q", tok.literal)
	}
}

// parseShapeComparisonValue parses the right-hand side of a comparison.
func (p *parser) parseShapeComparisonValue(op ComparisonOperator) (any, error) {
	tok := p.peek()

	// For == nil and != nil, no value is needed
	if op == OpMissing || op == OpNotMissing {
		return nil, nil
	}

	// For in and not in, expect a list
	if op == OpIn || op == OpOut {
		if tok.typ != tokParenOpen {
			return nil, p.errorf(tok, "%s requires a parenthesized list of values", op)
		}
		return p.parseShapeValueList()
	}

	// Otherwise expect a single value
	switch tok.typ {
	case tokString:
		p.next()
		return tok.literal, nil
	case tokNumber:
		p.next()
		if strings.ContainsAny(tok.literal, ".eE") {
			val, err := strconv.ParseFloat(tok.literal, 64)
			if err != nil {
				return nil, p.errorf(tok, "invalid number literal %q", tok.literal)
			}
			return val, nil
		}
		val, err := strconv.ParseInt(tok.literal, 10, 64)
		if err != nil {
			return nil, p.errorf(tok, "invalid integer literal %q", tok.literal)
		}
		return val, nil
	case tokIdentifier:
		// Check for true/false/null
		switch strings.ToLower(tok.literal) {
		case "true":
			p.next()
			return true, nil
		case "false":
			p.next()
			return false, nil
		case "null":
			p.next()
			return nil, nil
		}
		return nil, p.errorf(tok, "unexpected identifier %q in comparison value", tok.literal)
	default:
		return nil, p.errorf(tok, "expected comparison value, got %q", tok.literal)
	}
}

// parseShapeValueList parses a parenthesized list of values for in and not in.
func (p *parser) parseShapeValueList() ([]any, error) {
	if _, err := p.expect(tokParenOpen, "expected '(' to start value list"); err != nil {
		return nil, err
	}

	values := make([]any, 0)
	for {
		if p.peek().typ == tokParenClose {
			break
		}

		tok := p.peek()
		switch tok.typ {
		case tokString:
			p.next()
			values = append(values, tok.literal)
		case tokNumber:
			p.next()
			if strings.ContainsAny(tok.literal, ".eE") {
				val, err := strconv.ParseFloat(tok.literal, 64)
				if err != nil {
					return nil, p.errorf(tok, "invalid number literal %q", tok.literal)
				}
				values = append(values, val)
			} else {
				val, err := strconv.ParseInt(tok.literal, 10, 64)
				if err != nil {
					return nil, p.errorf(tok, "invalid integer literal %q", tok.literal)
				}
				values = append(values, val)
			}
		case tokIdentifier:
			switch strings.ToLower(tok.literal) {
			case "true":
				p.next()
				values = append(values, true)
			case "false":
				p.next()
				values = append(values, false)
			case "null":
				p.next()
				values = append(values, nil)
			default:
				return nil, p.errorf(tok, "unexpected identifier %q in value list", tok.literal)
			}
		default:
			return nil, p.errorf(tok, "unexpected token %q in value list", tok.literal)
		}

		if p.peek().typ == tokComma {
			p.next()
			continue
		}
		if p.peek().typ == tokParenClose {
			break
		}
		return nil, p.errorf(p.peek(), "expected ',' or ')' in value list")
	}

	if _, err := p.expect(tokParenClose, "expected ')' to close value list"); err != nil {
		return nil, err
	}

	return values, nil
}

func (p *parser) parsePositiveInt(tok token, field string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(tok.literal))
	if err != nil {
		return 0, p.errorf(tok, "%s must be an integer", field)
	}
	if value <= 0 {
		return 0, p.errorf(tok, "%s must be greater than zero", field)
	}
	return value, nil
}

func (p *parser) parseNonNegativeInt(tok token, field string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(tok.literal))
	if err != nil {
		return 0, p.errorf(tok, "%s must be an integer", field)
	}
	if value < 0 {
		return 0, p.errorf(tok, "%s must be zero or greater", field)
	}
	return value, nil
}

func (p *parser) parseComparison() (ExpressionNode, error) {
	fieldTok, err := p.expect(tokIdentifier, "expected field identifier")
	if err != nil {
		return nil, err
	}
	baseField := strings.TrimSpace(fieldTok.literal)
	var cacheHint *int
	var fieldSelections []FieldReference

	for p.match(tokAt) {
		dirTok, dirErr := p.expect(tokIdentifier, "expected directive name after '@'")
		if dirErr != nil {
			return nil, dirErr
		}
		switch strings.ToLower(strings.TrimSpace(dirTok.literal)) {
		case "cache":
			if cacheHint != nil {
				return nil, p.errorf(dirTok, "cache() directive already specified")
			}
			cacheHint, err = p.parseCacheHintDirective()
			if err != nil {
				return nil, err
			}
		case "fields":
			if !strings.EqualFold(baseField, "concept") {
				return nil, p.errorf(dirTok, "@fields() is only supported on concept comparisons")
			}
			if len(fieldSelections) > 0 {
				return nil, p.errorf(dirTok, "@fields() directive already specified")
			}
			fieldSelections, err = p.parseFieldsDirective(dirTok)
			if err != nil {
				return nil, err
			}
			if len(fieldSelections) == 0 {
				return nil, p.errorf(dirTok, "@fields() directive requires at least one field")
			}
		default:
			return nil, p.errorf(dirTok, "unsupported directive %q", dirTok.literal)
		}
	}

	// Check for keyword-based operators: in, has, not in
	var op ComparisonOperator
	var opTok token
	var value any

	if p.peek().typ == tokIdentifier {
		ident := strings.TrimSpace(p.peek().literal)
		switch ident {
		case "in":
			opTok = p.next()
			op = OpIn
			value, err = p.parseComparisonValue(op)
			if err != nil {
				return nil, err
			}
		case "has":
			opTok = p.next()
			op = OpHas
			value, err = p.parseComparisonValue(op)
			if err != nil {
				return nil, err
			}
		case "not":
			opTok = p.next()
			// Expect "in" after "not"
			nextTok := p.peek()
			if nextTok.typ == tokIdentifier && strings.TrimSpace(nextTok.literal) == "in" {
				p.next() // consume "in"
				op = OpOut
				value, err = p.parseComparisonValue(op)
				if err != nil {
					return nil, err
				}
			} else {
				return nil, p.errorf(opTok, "expected 'in' after 'not' (did you mean 'not in'?)")
			}
		default:
			if isSpecReferenceCandidate(baseField) {
				name := strings.TrimSpace(baseField)
				return nil, p.errorf(fieldTok, "spec %q must be invoked with parentheses: %s()", name, name)
			}
			return nil, p.errorf(fieldTok, "expected comparison operator after field")
		}
	} else if p.peek().typ == tokOperator {
		opTok, err = p.expect(tokOperator, "expected comparison operator after field")
		if err != nil {
			return nil, err
		}

		op, err = toComparisonOperator(opTok.literal)
		if err != nil {
			return nil, p.errorf(opTok, "%s", err.Error())
		}

		value, err = p.parseComparisonValue(op)
		if err != nil {
			return nil, err
		}

		// Handle == nil/null and != nil/null → OpMissing/OpNotMissing
		if (op == OpEq || op == OpNe) && value != nil {
			if strVal, ok := value.(string); ok && (strVal == "nil" || strVal == "null") {
				if op == OpEq {
					op = OpMissing
				} else {
					op = OpNotMissing
				}
				value = nil
			}
		}
	} else {
		if isSpecReferenceCandidate(baseField) {
			name := strings.TrimSpace(baseField)
			return nil, p.errorf(fieldTok, "spec %q must be invoked with parentheses: %s()", name, name)
		}
		return nil, p.errorf(fieldTok, "expected comparison operator after field")
	}

	if cacheHint != nil {
		if !strings.EqualFold(baseField, "concept") {
			return nil, p.errorf(fieldTok, "cache() hints are only supported on concept field")
		}
		if op != OpEq {
			return nil, p.errorf(opTok, "cache() hints require concept==\"name\" comparison")
		}
		strValue, ok := value.(string)
		if !ok || strings.TrimSpace(strValue) == "" {
			return nil, p.errorf(fieldTok, "cache() hints require concept string literal value")
		}
	}

	if len(fieldSelections) > 0 {
		if !strings.EqualFold(baseField, "concept") {
			return nil, p.errorf(fieldTok, "@fields() is only supported on concept comparisons")
		}
		if op != OpEq {
			return nil, p.errorf(opTok, "@fields() requires concept==\"name\" comparison")
		}
		strValue, ok := value.(string)
		if !ok || strings.TrimSpace(strValue) == "" {
			return nil, p.errorf(fieldTok, "@fields() requires concept string literal value")
		}
	}

	// Compatibility: treat concept==memql:version as a built-in service version query.
	// Some clients probe this concept directly instead of calling queryVersion()/memqlVersion().
	if strings.EqualFold(baseField, "concept") && op == OpEq {
		if conceptName, ok := value.(string); ok && strings.EqualFold(strings.TrimSpace(conceptName), "memql:version") {
			if builtin, ok := p.lookupBuiltin("memqlVersion"); ok {
				return &BuiltinFunctionExpression{
					Name:     "memqlVersion",
					Executor: builtin.Executor,
				}, nil
			}
			return &BuiltinFunctionExpression{
				Name:     "memqlVersion",
				Executor: BuiltinExecutorServiceVersion,
			}, nil
		}
	}

	return &ComparisonExpression{
		Field: FieldReference{
			Raw:   fieldTok.literal,
			Parts: splitFieldParts(baseField),
		},
		Operator:         op,
		Value:            value,
		CacheHintSeconds: cacheHint,
		FieldSelections:  fieldSelections,
	}, nil
}

func (p *parser) lookupBuiltin(name string) (*Function, bool) {
	if p == nil || p.builtinByLookup == nil {
		return nil, false
	}
	fn, ok := p.builtinByLookup[strings.ToLower(strings.TrimSpace(name))]
	return fn, ok
}

func (p *parser) parseBuiltinFunctionCall(callName string, fn *Function) (ExpressionNode, error) {
	fnTok := p.next()
	if _, err := p.expect(tokParenOpen, fmt.Sprintf("expected '(' after %s", callName)); err != nil {
		return nil, err
	}

	contract := fn.BuiltinArgs
	if contract == nil {
		contract = &BuiltinArgContract{Profile: BuiltinArgProfileNone}
	}
	profile := contract.Profile
	if profile == "" {
		profile = BuiltinArgProfileNone
	}

	var args map[string]any
	switch profile {
	case BuiltinArgProfileNone:
		if _, err := p.expect(tokParenClose, fmt.Sprintf("%s() does not accept arguments", callName)); err != nil {
			return nil, p.errorf(fnTok, "%s() does not accept arguments", callName)
		}
	case BuiltinArgProfileObject:
		if p.peek().typ != tokBraceOpen {
			return nil, p.errorf(fnTok, "%s() requires a JSON object argument", callName)
		}
		parsed, err := p.parseFunctionArgs()
		if err != nil {
			return nil, p.errorf(fnTok, "%s() argument: %v", callName, err)
		}
		args = parsed
		if _, err := p.expect(tokParenClose, fmt.Sprintf("expected ')' after %s arguments", callName)); err != nil {
			return nil, err
		}
	case BuiltinArgProfileOptionalObject:
		if p.peek().typ == tokBraceOpen {
			parsed, err := p.parseFunctionArgs()
			if err != nil {
				return nil, p.errorf(fnTok, "%s() argument: %v", callName, err)
			}
			args = parsed
		}
		if _, err := p.expect(tokParenClose, fmt.Sprintf("expected ')' after %s arguments", callName)); err != nil {
			return nil, err
		}
	case BuiltinArgProfileStringOrObject:
		parsed, err := p.parseStringOrObjectBuiltinArgs(fnTok, callName, contract, false)
		if err != nil {
			return nil, err
		}
		args = parsed
	case BuiltinArgProfileOptionalString:
		if p.peek().typ == tokString {
			key := contract.StringKey
			if key == "" {
				key = "value"
			}
			args = map[string]any{key: p.next().literal}
		}
		if _, err := p.expect(tokParenClose, fmt.Sprintf("expected ')' after %s arguments", callName)); err != nil {
			return nil, err
		}
	case BuiltinArgProfileOptionalStringOrObject:
		parsed, err := p.parseStringOrObjectBuiltinArgs(fnTok, callName, contract, true)
		if err != nil {
			return nil, err
		}
		args = parsed
	default:
		return nil, p.errorf(fnTok, "unsupported builtin argument profile %q for %s()", profile, callName)
	}

	if err := validateBuiltinCallArgs(callName, args, contract); err != nil {
		return nil, err
	}

	if len(args) == 0 {
		args = nil
	}
	resultName := callName
	if strings.EqualFold(fn.Executor, BuiltinExecutorServiceVersion) {
		// Preserve historical compatibility: both memqlVersion() and serviceVersion()
		// normalize to memqlVersion in the parsed expression.
		resultName = "memqlVersion"
	}

	return &BuiltinFunctionExpression{
		Name:     resultName,
		Executor: fn.Executor,
		Args:     args,
	}, nil
}

func (p *parser) parseStringOrObjectBuiltinArgs(fnTok token, callName string, contract *BuiltinArgContract, optional bool) (map[string]any, error) {
	switch p.peek().typ {
	case tokString:
		key := contract.StringKey
		if key == "" {
			key = "value"
		}
		args := map[string]any{key: p.next().literal}
		if _, err := p.expect(tokParenClose, fmt.Sprintf("expected ')' after %s arguments", callName)); err != nil {
			return nil, err
		}
		return args, nil
	case tokBraceOpen:
		args, err := p.parseFunctionArgs()
		if err != nil {
			return nil, p.errorf(fnTok, "%s() argument: %v", callName, err)
		}
		if _, err := p.expect(tokParenClose, fmt.Sprintf("expected ')' after %s arguments", callName)); err != nil {
			return nil, err
		}
		return args, nil
	case tokParenClose:
		if optional {
			if _, err := p.expect(tokParenClose, fmt.Sprintf("expected ')' after %s arguments", callName)); err != nil {
				return nil, err
			}
			return nil, nil
		}
		key := strings.TrimSpace(contract.StringKey)
		if key != "" {
			return nil, p.errorf(fnTok, "%s() requires a %s argument: %s(\"%s\") or %s({\"%s\": \"...\"})", callName, key, callName, key, callName, key)
		}
		return nil, p.errorf(fnTok, "%s() requires an argument", callName)
	default:
		return nil, p.errorf(fnTok, "%s() argument must be a string or JSON object", callName)
	}
}

func validateBuiltinCallArgs(callName string, args map[string]any, contract *BuiltinArgContract) error {
	if contract == nil {
		return nil
	}
	for _, required := range contract.Required {
		if _, ok := args[required]; !ok {
			return fmt.Errorf("%s() requires '%s' field in argument", callName, required)
		}
	}
	if contract.Properties != nil {
		if contract.AdditionalProperties != nil && !*contract.AdditionalProperties {
			for key := range args {
				if _, ok := contract.Properties[key]; !ok {
					return fmt.Errorf("%s() does not accept '%s' field in argument", callName, key)
				}
			}
		}
		for key, rawVal := range args {
			expected, ok := contract.Properties[key]
			if !ok || expected == "" {
				continue
			}
			if !builtinArgTypeMatches(rawVal, expected) {
				return fmt.Errorf("%s() '%s' field must be %s", callName, key, expected)
			}
			if strings.EqualFold(expected, "string") {
				if s, ok := rawVal.(string); ok && strings.TrimSpace(s) == "" {
					return fmt.Errorf("%s() %s cannot be empty", callName, key)
				}
			}
		}
	}
	return nil
}

func builtinArgTypeMatches(value any, expected string) bool {
	switch strings.ToLower(strings.TrimSpace(expected)) {
	case "string":
		_, ok := value.(string)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "number", "int":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			return true
		}
		return false
	case "boolean", "bool":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "[]string":
		arr, ok := value.([]any)
		if !ok {
			return false
		}
		for _, v := range arr {
			if _, ok := v.(string); !ok {
				return false
			}
		}
		return true
	case "[]int", "[]number":
		arr, ok := value.([]any)
		if !ok {
			return false
		}
		for _, v := range arr {
			switch v.(type) {
			case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
				// ok
			default:
				return false
			}
		}
		return true
	default:
		return true
	}
}

func (p *parser) parseComparisonValue(op ComparisonOperator) (any, error) {
	switch op {
	case OpIn, OpOut:
		// Accept arg() references, caller.X references, [...] array
		// literals, or (...) collection syntax.
		if p.peek().typ == tokIdentifier {
			ident := strings.TrimSpace(p.peek().literal)
			if ident == "arg" {
				return p.parseLiteralValue() // handles ctx.name as a reference
			}
			// The shared lexer emits `actor.X` as a single identifier
			// token (`.` is part of isIdentifierCharNoColon). Route
			// any engine-managed accessor (actor/args/ctx) through
			// the shared dispatcher in parseLiteralValue, which uses
			// baseparser.ClassifyAccessor. caller. retired by #221:
			// the rejection fires there with the canonical migration
			// hint.
			if kind, _, _ := baseparser.ClassifyAccessor(ident); kind == baseparser.KindActor {
				return p.parseLiteralValue() // handles actor.X as a reference
			}
		}
		if p.peek().typ == tokBracketOpen {
			return p.parseBracketCollectionValue()
		}
		return p.parseCollectionValue()
	case OpHas:
		return p.parseLiteralValue()
	case OpMissing, OpNotMissing:
		return nil, nil
	default:
		return p.parseLiteralValue()
	}
}

func (p *parser) parseCollectionValue() ([]any, error) {
	if _, err := p.expect(tokParenOpen, "expected '(' to start collection"); err != nil {
		return nil, err
	}

	values := make([]any, 0)

	if p.match(tokParenClose) {
		return nil, p.errorf(p.previous(), "collection must contain at least one value")
	}

	for {
		val, err := p.parseLiteralValue()
		if err != nil {
			return nil, err
		}
		values = append(values, val)

		if p.match(tokParenClose) {
			break
		}

		if _, err := p.expect(tokComma, "expected ',' between collection values"); err != nil {
			return nil, err
		}
	}

	return values, nil
}

// parseBracketCollectionValue parses Go-style array literals: [val1, val2, ...]
func (p *parser) parseBracketCollectionValue() ([]any, error) {
	if _, err := p.expect(tokBracketOpen, "expected '[' to start collection"); err != nil {
		return nil, err
	}

	values := make([]any, 0)

	if p.match(tokBracketClose) {
		return nil, p.errorf(p.previous(), "collection must contain at least one value")
	}

	for {
		val, err := p.parseLiteralValue()
		if err != nil {
			return nil, err
		}
		values = append(values, val)

		if p.match(tokBracketClose) {
			break
		}

		if _, err := p.expect(tokComma, "expected ',' between collection values"); err != nil {
			return nil, err
		}
	}

	return values, nil
}

func parseInsertFunction(query string) (*MutationNode, error) {
	parser := &insertFunctionParser{
		input: []rune(query),
	}
	parser.skipWhitespace()

	if !parser.consumeKeyword("insert") {
		return nil, parser.errorf(parser.pos, "expected insert(")
	}

	parser.skipWhitespace()
	if !parser.consumeChar('(') {
		return nil, parser.errorf(parser.pos, "expected '(' after insert")
	}

	parser.skipWhitespace()
	concept, err := parser.parseStringLiteral()
	if err != nil {
		return nil, err
	}
	concept = strings.TrimSpace(concept)
	if concept == "" {
		return nil, parser.errorf(parser.pos, "concept name is required")
	}

	parser.skipWhitespace()
	if !parser.consumeChar(',') {
		return nil, parser.errorf(parser.pos, "expected ',' after concept argument")
	}

	var (
		idValue    string
		payloadRaw string
		parentRef  *string
		aliasOfRef *string
		seen       = make(map[string]struct{})
	)

	for {
		parser.skipWhitespace()
		if parser.peek() == ')' {
			break
		}

		argPos := parser.pos
		name, err := parser.parseIdentifier()
		if err != nil {
			return nil, err
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			return nil, parser.errorf(argPos, "argument name is required")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, parser.errorf(argPos, "duplicate argument %q", name)
		}

		parser.skipWhitespace()
		if !parser.consumeChar('=') {
			return nil, parser.errorf(parser.pos, "expected '=' after argument %q", name)
		}
		parser.skipWhitespace()

		switch name {
		case "id":
			value, err := parser.parseStringLiteral()
			if err != nil {
				return nil, err
			}
			idValue = strings.TrimSpace(value)
		case "payload":
			raw, err := parser.parseJSONObject()
			if err != nil {
				return nil, err
			}
			payloadRaw = raw
		case "parent":
			value, err := parser.parseStringLiteral()
			if err != nil {
				return nil, err
			}
			copy := strings.TrimSpace(value)
			parentRef = &copy
		case "aliasof":
			value, err := parser.parseStringLiteral()
			if err != nil {
				return nil, err
			}
			copy := strings.TrimSpace(value)
			aliasOfRef = &copy
		default:
			return nil, parser.errorf(argPos, "unknown argument %q", name)
		}

		seen[name] = struct{}{}

		parser.skipWhitespace()
		switch parser.peek() {
		case ',':
			parser.pos++
		case ')':
			// handled by loop exit
		default:
			return nil, parser.errorf(parser.pos, "expected ',' or ')' after argument")
		}

		if parser.peek() == ')' {
			break
		}
	}

	if !parser.consumeChar(')') {
		return nil, parser.errorf(parser.pos, "expected ')' to close insert()")
	}

	parser.skipWhitespace()
	if !parser.eof() {
		return nil, parser.errorf(parser.pos, "unexpected content after insert()")
	}

	if payloadRaw == "" {
		return nil, parser.errorf(parser.pos, "payload argument is required")
	}

	return &MutationNode{
		Concept:    concept,
		ID:         idValue,
		PayloadRaw: payloadRaw,
		ParentRef:  parentRef,
		AliasOfRef: aliasOfRef,
	}, nil
}

func (p *parser) parseLiteralValue() (any, error) {
	tok := p.peek()
	switch tok.typ {
	case tokString:
		p.next()
		return tok.literal, nil
	case tokNumber:
		p.next()
		num, err := parseNumberLiteral(tok.literal)
		if err != nil {
			return nil, p.errorf(tok, "%s", err.Error())
		}
		return num, nil
	case tokIdentifier:
		// arg(...) was the pre-Phase-1 caller-argument reference;
		// the unified ctx.X form replaces it. Reject at parse time so
		// stragglers in the tree and any new authoring attempts are
		// caught immediately.
		if tok.literal == "arg" {
			p.next() // consume 'arg'
			return nil, p.errorf(tok, "arg(...) is retired — use ctx.<path> instead")
		}
		// actor./args./ctx. -- engine-managed accessors. The shared
		// lexer emits the dotted form as a single identifier token
		// (`.` is part of isIdentifierCharNoColon), so the FULL
		// `tok.literal` is what baseparser.ClassifyAccessor expects.
		// caller. retired by #221: the helper returns the canonical
		// migration-hint error and BOTH parsers route their rejection
		// through it, so the user-facing message is identical
		// regardless of which parser the author's expression reached
		// (#244 / epic #218).
		kind, _, accErr := baseparser.ClassifyAccessor(tok.literal)
		if accErr != nil {
			return nil, p.errorf(tok, "%s", accErr.Error())
		}
		switch kind {
		case baseparser.KindActor:
			return p.parseActorReference()
		case baseparser.KindArgs:
			return p.parseArgsReference()
		case baseparser.KindCtx:
			return p.parseCtxReference()
		}
		p.next()
		value, err := p.readIdentifierLiteral(tok)
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(value) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return value, nil
		}
	default:
		return nil, p.errorf(tok, "unexpected token %q, expected literal value", tok.literal)
	}
}

// ArgReference represents a reference to a named function argument.
// It's stored as the Value in a ComparisonExpression and resolved at
// execution time. Produced by parsing the canonical `ctx.<path>`
// syntax (see parseCtxReference); the legacy `ctx.fieldName` form
// that originally produced this node is retired.
type ArgReference struct {
	Path string // e.g., "spaceId" or "options.limit"
}

// ActorReference represents a reference to a field on the authenticated
// user's AccessContext. Created by parsing `caller.X` syntax in
// comparison-value position; resolved at execution time by reading
// auth.AccessFromContext(ctx). Dotted paths are supported
// (`caller.userId`, `caller.role`, etc.).
type ActorReference struct {
	Path string
}

// parseActorReference consumes an `actor` or `actor.X[.Y...]`
// identifier token and returns a *ActorReference whose Path is the
// dotted suffix (empty for bare `actor`, though only dotted paths
// are currently supported by the resolver).
//
// The shared lexer treats `.` as part of an identifier, so `actor.X`
// arrives as ONE tokIdentifier whose literal is `actor.X`. We just
// strip the prefix.
func (p *parser) parseActorReference() (*ActorReference, error) {
	tok := p.next()
	literal := strings.TrimSpace(tok.literal)
	// Accept `actor.X` -- the canonical auth-context accessor. The
	// AST node and the parser function carry the canonical name too
	// (#221 + #239). caller.X retired by #221; the dispatcher at
	// site-2 above already rejects it with a migration hint, so a
	// caller.X token shouldn't reach this function.
	if !(literal == "actor" || strings.HasPrefix(literal, "actor.")) {
		return nil, p.errorf(tok, "expected actor.X, got %q", literal)
	}
	if literal == "actor" {
		return nil, p.errorf(tok, "actor reference requires a field path, e.g. actor.userId")
	}
	path := strings.TrimSpace(strings.TrimPrefix(literal, "actor."))
	if path == "" {
		return nil, p.errorf(tok, "actor reference requires a field path, e.g. actor.userId")
	}
	if path == "partition" || path == "partitions" {
		return nil, p.errorf(tok, "actor.%s is retired post-#56 phase 5; reference userId/role instead", path)
	}
	return &ActorReference{Path: path}, nil
}

// parseCtxReference consumes a `ctx` or `ctx.X[.Y...]` identifier
// token and returns an *ArgReference whose Path is the dotted suffix
// (empty for bare `ctx`; bare ctx isn't usable in comparison-value
// position today and errors out).
//
// `ctx.X` is the legacy runtime-form caller-passed-argument access
// syntax still emitted by the struct-form rewriter for non-Logic
// receivers. It produces the same AST node as `args.X` so the rest
// of the parser / executor / renderer pipeline is unchanged.
func (p *parser) parseCtxReference() (*ArgReference, error) {
	tok := p.next()
	literal := strings.TrimSpace(tok.literal)
	if literal == "ctx" {
		return nil, p.errorf(tok, "ctx reference requires a field path, e.g. ctx.spaceId")
	}
	if !strings.HasPrefix(literal, "ctx.") {
		return nil, p.errorf(tok, "expected ctx.X, got %q", literal)
	}
	path := strings.TrimSpace(strings.TrimPrefix(literal, "ctx."))
	if path == "" {
		return nil, p.errorf(tok, "ctx reference requires a field path, e.g. ctx.spaceId")
	}
	return &ArgReference{Path: path}, nil
}

// parseArgsReference consumes an `args` or `args.X[.Y...]` identifier
// token and returns an *ArgReference whose Path is the dotted suffix.
// `args.X` is the author-facing caller-passed-argument access syntax.
// Bare `args` is not usable in comparison-value position and errors.
func (p *parser) parseArgsReference() (*ArgReference, error) {
	tok := p.next()
	literal := strings.TrimSpace(tok.literal)
	if literal == "args" {
		return nil, p.errorf(tok, "args reference requires a field path, e.g. args.spaceId")
	}
	if !strings.HasPrefix(literal, "args.") {
		return nil, p.errorf(tok, "expected args.X, got %q", literal)
	}
	path := strings.TrimSpace(strings.TrimPrefix(literal, "args."))
	if path == "" {
		return nil, p.errorf(tok, "args reference requires a field path, e.g. args.spaceId")
	}
	return &ArgReference{Path: path}, nil
}

func (p *parser) readIdentifierLiteral(tok token) (string, error) {
	if strings.TrimSpace(tok.literal) == "" {
		return "", p.errorf(tok, "identifier literal must not be empty")
	}
	builder := strings.Builder{}
	builder.WriteString(tok.literal)
	for p.peek().typ == tokColon {
		colonTok := p.next()
		next := p.peek()
		if next.typ != tokIdentifier {
			return "", p.errorf(colonTok, "expected identifier after ':'")
		}
		p.next()
		builder.WriteRune(':')
		builder.WriteString(next.literal)
	}
	return builder.String(), nil
}

func (p *parser) match(typ tokenType) bool {
	if p.peek().typ == typ {
		p.next()
		return true
	}
	return false
}

func (p *parser) expect(typ tokenType, message string) (token, error) {
	tok := p.peek()
	if tok.typ != typ {
		return token{}, p.errorf(tok, "%s", message)
	}
	p.next()
	return tok, nil
}

func (p *parser) peek() token {
	if p.pos >= len(p.tokens) {
		return token{typ: tokEOF, pos: p.lastPos()}
	}
	return p.tokens[p.pos]
}

func (p *parser) peekNext() token {
	if p.pos+1 >= len(p.tokens) {
		return token{typ: tokEOF, pos: p.lastPos()}
	}
	return p.tokens[p.pos+1]
}

func (p *parser) previous() token {
	if p.pos-1 >= len(p.tokens) {
		return token{typ: tokEOF, pos: p.lastPos()}
	}
	if p.pos-1 < 0 {
		return token{typ: tokEOF, pos: 0}
	}
	return p.tokens[p.pos-1]
}

func (p *parser) next() token {
	tok := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return tok
}

func (p *parser) lastPos() int {
	if len(p.tokens) == 0 {
		return 0
	}
	return p.tokens[len(p.tokens)-1].pos
}

func (p *parser) errorf(tok token, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if tok.typ == tokEOF {
		return fmt.Errorf("%w: %s at end of query", ErrInvalidQuerySyntax, msg)
	}
	return fmt.Errorf("%w: %s at position %d", ErrInvalidQuerySyntax, msg, tok.pos)
}

type insertFunctionParser struct {
	input []rune
	pos   int
}

func (p *insertFunctionParser) eof() bool {
	return p.pos >= len(p.input)
}

func (p *insertFunctionParser) peek() rune {
	if p.eof() {
		return 0
	}
	return p.input[p.pos]
}

func (p *insertFunctionParser) skipWhitespace() {
	for !p.eof() && unicode.IsSpace(p.peek()) {
		p.pos++
	}
}

func (p *insertFunctionParser) consumeKeyword(keyword string) bool {
	if p.pos+len(keyword) > len(p.input) {
		return false
	}
	candidate := string(p.input[p.pos : p.pos+len(keyword)])
	if !strings.EqualFold(candidate, keyword) {
		return false
	}
	p.pos += len(keyword)
	return true
}

func (p *insertFunctionParser) consumeChar(expected rune) bool {
	if p.eof() || p.peek() != expected {
		return false
	}
	p.pos++
	return true
}

func (p *insertFunctionParser) parseIdentifier() (string, error) {
	if p.eof() {
		return "", p.errorf(p.pos, "expected identifier")
	}

	start := p.pos
	if !isIdentifierStart(p.peek()) {
		return "", p.errorf(p.pos, "expected identifier")
	}

	p.pos++
	for !p.eof() && isIdentifierChar(p.peek()) {
		p.pos++
	}

	return string(p.input[start:p.pos]), nil
}

func (p *insertFunctionParser) parseStringLiteral() (string, error) {
	if p.eof() || p.peek() != '"' {
		return "", p.errorf(p.pos, "expected '\"' to start string literal")
	}

	start := p.pos
	p.pos++

	escaped := false
	for !p.eof() {
		r := p.peek()
		p.pos++

		if escaped {
			escaped = false
			continue
		}

		if r == '\\' {
			escaped = true
			continue
		}

		if r == '"' {
			literal := string(p.input[start:p.pos])
			value, err := strconv.Unquote(literal)
			if err != nil {
				return "", p.errorf(start, "invalid string literal: %v", err)
			}
			return value, nil
		}
	}

	return "", p.errorf(start, "unterminated string literal")
}

func (p *insertFunctionParser) parseJSONObject() (string, error) {
	if p.eof() || p.peek() != '{' {
		return "", p.errorf(p.pos, "expected '{' to start payload")
	}

	start := p.pos
	depth := 0
	inString := false
	escaped := false

	for !p.eof() {
		r := p.peek()
		p.pos++

		if inString {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if r == '"' {
				inString = false
			}
			continue
		}

		switch r {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return string(p.input[start:p.pos]), nil
			}
		}
	}

	return "", p.errorf(start, "unterminated JSON payload")
}

func (p *insertFunctionParser) errorf(pos int, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	return fmt.Errorf("%w: %s at position %d", ErrInvalidQuerySyntax, message, pos)
}

// tokenize lexes a runtime query expression by delegating to the
// shared language/parser lexer. Both DSL parsers now share BOTH the
// lexer AND the token-type enum (the local `tokFoo` constants below
// are aliases for `langparser.TokenFoo`), so there is no "translate
// token type to a local vocabulary" step anymore -- only the runtime
// grammar's two semantic adjustments survive (#242 / epic #218):
//
//   - `$` operator is emitted by the lexer for `$var` references
//     in the load-time grammar; the runtime grammar doesn't accept
//     standalone `$`, so we reject here with a clean "unexpected
//     token" error rather than letting it flow into the parser as
//     a generic operator.
//   - `TokenBang` (`!`) is surfaced as a generic `tokOperator` so
//     runtime grammars that don't accept a standalone `!` error at
//     the parse step rather than the lexer step.
//   - Keyword tokens (`TokenKeywordFunc`, `TokenKeywordIn`, ...)
//     are flattened to `tokIdentifier` so the runtime parser's
//     literal-string keyword checks (`in`, `has`, `not`, `nil`,
//     `null`) keep matching. The runtime parser decides for itself
//     whether a given identifier acts as a keyword in context.
//
// Before #242 the function did a per-TokenType `switch` mapping each
// shared token type to a local enum value; that mapping retired when
// the local enum became an alias for `langparser.TokenType`.
func tokenize(query string) ([]token, error) {
	lex := langparser.NewLexer(query)
	tokens := make([]token, 0)
	for {
		tok, err := lex.NextToken()
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidQuerySyntax, err.Error())
		}
		if tok.Type == langparser.TokenOperator && tok.Literal == "$" {
			return nil, fmt.Errorf("%w: unexpected token %q at position %d",
				ErrInvalidQuerySyntax, tok.Literal, tok.Pos)
		}
		converted := token{typ: tok.Type, literal: tok.Literal, pos: tok.Pos}
		switch {
		case tok.Type == langparser.TokenBang:
			converted.typ = tokOperator
			converted.literal = "!"
		case tok.Type >= langparser.TokenKeywordQuery:
			converted.typ = tokIdentifier
		}
		tokens = append(tokens, converted)
		if tok.Type == langparser.TokenEOF {
			break
		}
	}
	return tokens, nil
}

// tokenType is a type alias for langparser.TokenType -- the memql
// query parser shares the language parser's lexer AND its token-type
// enum (#242 / epic #218). The local `tokFoo` constants below alias
// the canonical `langparser.TokenFoo` values so the parser's 250+
// token-type comparisons keep their concise form without committing
// to two enums that have to be kept in sync. The local `token` data
// carrier (struct below) stays parser-local: it's just a 3-field
// record with names that match the rest of this file's idioms.
type tokenType = langparser.TokenType

const (
	tokEOF              tokenType = langparser.TokenEOF
	tokIdentifier       tokenType = langparser.TokenIdentifier
	tokNumber           tokenType = langparser.TokenNumber
	tokString           tokenType = langparser.TokenString
	tokOperator         tokenType = langparser.TokenOperator
	tokParenOpen        tokenType = langparser.TokenParenOpen
	tokParenClose       tokenType = langparser.TokenParenClose
	tokBraceOpen        tokenType = langparser.TokenBraceOpen
	tokBraceClose       tokenType = langparser.TokenBraceClose
	tokBracketOpen      tokenType = langparser.TokenBracketOpen
	tokBracketClose     tokenType = langparser.TokenBracketClose
	tokColon            tokenType = langparser.TokenColon
	tokSemicolon        tokenType = langparser.TokenSemicolon
	tokComma            tokenType = langparser.TokenComma
	tokAt               tokenType = langparser.TokenAt
	tokDefine           tokenType = langparser.TokenDefine
	tokQuestion         tokenType = langparser.TokenQuestion         // ? for ternary operator (future)
	tokQuestionDot      tokenType = langparser.TokenQuestionDot      // ?. for optional/conditional filters
	tokAmpAmp           tokenType = langparser.TokenAmpAmp           // && for logical AND
	tokQuestionQuestion tokenType = langparser.TokenQuestionQuestion // ?? for null coalescing
)

type token struct {
	typ     tokenType
	literal string
	pos     int
}

func (p *parser) parseCacheHintDirective() (*int, error) {
	if _, err := p.expect(tokParenOpen, "cache() directive requires '('"); err != nil {
		return nil, err
	}

	valueTok, err := p.expect(tokNumber, "cache() directive requires a non-negative integer TTL")
	if err != nil {
		return nil, err
	}
	ttl, parseErr := strconv.Atoi(strings.TrimSpace(valueTok.literal))
	if parseErr != nil || ttl < 0 {
		return nil, p.errorf(valueTok, "cache() directive requires a non-negative integer TTL")
	}

	if _, err := p.expect(tokParenClose, "cache() directive requires ')'"); err != nil {
		return nil, err
	}
	return &ttl, nil
}

func (p *parser) parseFieldsDirective(dirTok token) ([]FieldReference, error) {
	if _, err := p.expect(tokParenOpen, "fields() directive requires '('"); err != nil {
		return nil, err
	}
	if p.peek().typ == tokParenClose {
		return nil, p.errorf(dirTok, "fields() directive requires at least one field")
	}
	fields, err := p.parseFieldReferenceList("@fields", false)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, p.errorf(dirTok, "fields() directive requires at least one field")
	}
	if _, err := p.expect(tokParenClose, "fields() directive requires ')'"); err != nil {
		return nil, err
	}
	return fields, nil
}

func populateCacheHints(plan *QueryPlan) {
	if plan == nil || plan.Root == nil {
		return
	}
	hints := make(map[string]int64)
	collectCacheHints(plan.Root, hints)
	if len(hints) > 0 {
		plan.CacheHints = hints
	}
}

func populateConceptFields(plan *QueryPlan) {
	if plan == nil || plan.Root == nil {
		return
	}
	fields := make(map[string][]FieldReference)
	collectConceptFields(plan.Root, fields)
	if len(fields) > 0 {
		plan.ConceptFields = fields
	}
}

func collectConceptFields(expr ExpressionNode, acc map[string][]FieldReference) {
	if expr == nil || acc == nil {
		return
	}

	switch node := expr.(type) {
	case *ComparisonExpression:
		if len(node.FieldSelections) > 0 &&
			len(node.Field.Parts) > 0 &&
			strings.EqualFold(node.Field.Parts[0], "concept") {
			conceptName, ok := node.Value.(string)
			if !ok {
				break
			}
			conceptKey := strings.TrimSpace(conceptName)
			if conceptKey == "" {
				break
			}
			acc[conceptKey] = appendUniqueFieldReferences(acc[conceptKey], node.FieldSelections)
		}
	case *LogicalExpression:
		collectConceptFields(node.Left, acc)
		collectConceptFields(node.Right, acc)
	case *RelationshipExpression:
		collectConceptFields(node.Target, acc)
	case *SortExpression:
		collectConceptFields(node.Target, acc)
	case *PaginateExpression:
		collectConceptFields(node.Target, acc)
	case *SelectExpression:
		collectConceptFields(node.Target, acc)
	case *TimestampExpression:
		collectConceptFields(node.Target, acc)
	case *DepthExpression:
		collectConceptFields(node.Target, acc)
	case *ShapeExpression:
		collectConceptFields(node.Target, acc)
	case *BuiltinFunctionExpression:
		// Builtin functions don't have nested expressions to collect from
	}
}

func appendUniqueFieldReferences(existing []FieldReference, refs []FieldReference) []FieldReference {
	if len(refs) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing))
	for _, ref := range existing {
		key := canonicalField(ref)
		if key == "" {
			continue
		}
		seen[key] = struct{}{}
	}
	for _, ref := range refs {
		key := canonicalField(ref)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		existing = append(existing, ref)
		seen[key] = struct{}{}
	}
	return existing
}

func collectCacheHints(expr ExpressionNode, acc map[string]int64) {
	if expr == nil || acc == nil {
		return
	}

	switch node := expr.(type) {
	case *ComparisonExpression:
		if node.CacheHintSeconds != nil &&
			len(node.Field.Parts) > 0 &&
			strings.EqualFold(node.Field.Parts[0], "concept") {
			if conceptName, ok := node.Value.(string); ok {
				key := strings.ToLower(strings.TrimSpace(conceptName))
				if key != "" {
					hint := int64(*node.CacheHintSeconds)
					if existing, ok := acc[key]; !ok || hint < existing {
						acc[key] = hint
					}
				}
			}
		}
	case *LogicalExpression:
		collectCacheHints(node.Left, acc)
		collectCacheHints(node.Right, acc)
	case *RelationshipExpression:
		collectCacheHints(node.Target, acc)
	case *SortExpression:
		collectCacheHints(node.Target, acc)
	case *PaginateExpression:
		collectCacheHints(node.Target, acc)
	case *SelectExpression:
		collectCacheHints(node.Target, acc)
	case *TimestampExpression:
		collectCacheHints(node.Target, acc)
	case *DepthExpression:
		collectCacheHints(node.Target, acc)
	case *ShapeExpression:
		collectCacheHints(node.Target, acc)
	case *BuiltinFunctionExpression:
		// Builtin functions don't have nested expressions to collect from
	}
}

func splitFieldParts(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ".")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func (p *parser) parseFieldReferenceList(context string, requireComma bool) ([]FieldReference, error) {
	fields := make([]FieldReference, 0, 1)
	if requireComma {
		if _, err := p.expect(tokComma, fmt.Sprintf("expected ',' before first %s field", context)); err != nil {
			return nil, err
		}
	}
	for {
		fieldTok, err := p.expect(tokString, fmt.Sprintf("%s field must be a string literal (e.g. \"payload.title\")", context))
		if err != nil {
			return nil, err
		}
		ref, err := parseFieldReferenceLiteral(fieldTok.literal)
		if err != nil {
			return nil, p.errorf(fieldTok, "%s", err.Error())
		}
		fields = append(fields, ref)

		if p.peek().typ == tokParenClose {
			break
		}
		if _, err := p.expect(tokComma, fmt.Sprintf("expected ',' between %s fields", context)); err != nil {
			return nil, err
		}
	}
	return fields, nil
}

func splitSelectFields(refs []FieldReference) ([]FieldReference, metadataSelection, error) {
	payload := make([]FieldReference, 0, len(refs))
	selection := metadataSelection{
		Fields: make(map[string]struct{}),
	}
	for _, ref := range refs {
		if name, wildcard, ok := metadataReferenceInfo(ref); ok {
			if wildcard {
				selection.IncludeAll = true
				selection.Fields = make(map[string]struct{})
			} else if !selection.IncludeAll {
				selection.Fields[name] = struct{}{}
			}
			continue
		}
		payload = append(payload, ref)
	}
	if selection.IncludeAll {
		return copyFieldReferences(payload), selection, nil
	}
	if selection.Fields == nil {
		selection.Fields = make(map[string]struct{})
	}
	if _, ok := selection.Fields["id"]; !ok {
		selection.Fields["id"] = struct{}{}
	}
	return copyFieldReferences(payload), selection, nil
}

func parseFieldReferenceLiteral(value string) (FieldReference, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return FieldReference{}, fmt.Errorf("field reference must not be empty")
	}
	ref := FieldReference{
		Raw:   trimmed,
		Parts: splitFieldParts(trimmed),
	}
	if len(ref.Parts) == 0 {
		return FieldReference{}, fmt.Errorf("field reference must not be empty")
	}
	if err := validateFieldReference(&ref); err != nil {
		return FieldReference{}, err
	}
	return ref, nil
}

func validateFieldReference(ref *FieldReference) error {
	if ref == nil {
		return fmt.Errorf("field reference is required")
	}
	if len(ref.Parts) == 0 {
		return fmt.Errorf("field reference must not be empty")
	}

	first := strings.TrimSpace(ref.Parts[0])
	// ctx.X in field-reference position (e.g. spec body
	// `ctx.participantType == "human"`) is the unified replacement for
	// `payload.X`. Normalize to payload so the rest of the validator +
	// executor pipeline keeps the single payload-rooted assumption.
	if strings.EqualFold(first, "ctx") {
		ref.Parts[0] = "payload"
		first = "payload"
		if ref.Raw != "" {
			if strings.HasPrefix(ref.Raw, "ctx.") {
				ref.Raw = "payload." + strings.TrimPrefix(ref.Raw, "ctx.")
			} else if strings.HasPrefix(ref.Raw, "ctx") {
				ref.Raw = "payload" + strings.TrimPrefix(ref.Raw, "ctx")
			}
		}
	}
	switch strings.ToLower(first) {
	case "payload":
		// allowed
	case "meta":
		if len(ref.Parts) != 2 {
			return fmt.Errorf("meta fields must use meta.<field> or meta.*")
		}
		if ref.Parts[1] == "*" {
			ref.Wildcard = true
			return nil
		}
		if _, ok := canonicalMetadataFieldName(ref.Parts[1]); !ok {
			return fmt.Errorf("meta field %q is not supported", ref.Parts[1])
		}
		return nil
	case "id", "concept", "type", "createdat", "createdby", "schema":
		if len(ref.Parts) > 1 {
			return fmt.Errorf("intrinsic field %q does not support nested paths", first)
		}
		return nil
	case "provenance":
		// provenance is a JSON-object intrinsic; nested paths address
		// the engine-stamped fields (kind, name, trigger, via). Bare
		// `provenance` returns the whole object.
		if len(ref.Parts) > 2 {
			return fmt.Errorf("provenance paths support one level (e.g. provenance.kind); got %q", ref.Raw)
		}
		if len(ref.Parts) == 2 {
			sub := strings.ToLower(strings.TrimSpace(ref.Parts[1]))
			switch sub {
			case "kind", "name", "trigger", "via":
				// allowed
			default:
				return fmt.Errorf("provenance field %q is not supported (kind|name|trigger|via)", ref.Parts[1])
			}
		}
		return nil
	case "actor":
		// actor.X (auth-context reference) on the LHS of a comparison
		// is valid inside context-spec bodies — `actor.role == "admin"`,
		// `actor.userId == "..."`, etc. The engine evaluates these
		// in-process at call time via the same actor-resolver used by
		// the policy evaluator; nothing is pushed into SQL.
		//
		// The spec registration path inspects whether a spec body
		// touches `payload.*` / intrinsics (row-spec, SQL pushdown) OR
		// only `actor.*` (context-spec, in-process eval) — mixed bodies
		// are flagged separately. Here we just admit the field-ref
		// shape; the row-vs-context classification happens in the spec
		// validator.
		if len(ref.Parts) < 2 {
			return fmt.Errorf("actor field references must include a sub-field (e.g. actor.role, actor.userId)")
		}
		return nil
	case "caller":
		// caller.X retired by #221. The migration-hint string is
		// sourced from baseparser.ErrCallerRetired so every
		// rejection site across both parsers emits identical text
		// (#244 / epic #218).
		return baseparser.ErrCallerRetired
	default:
		return fmt.Errorf("field %q must start with payload., meta., actor., or an intrinsic like id/concept/type/createdAt/createdBy/provenance", ref.Raw)
	}

	if len(ref.Parts) == 1 {
		return nil
	}

	for i := 1; i < len(ref.Parts); i++ {
		part := strings.TrimSpace(ref.Parts[i])
		if part == "" {
			return fmt.Errorf("field %q contains an empty segment", ref.Raw)
		}
	}

	last := ref.Parts[len(ref.Parts)-1]
	if last == "*" {
		if len(ref.Parts) < 3 {
			return fmt.Errorf("payload wildcards must follow at least one property")
		}
		ref.Wildcard = true
	} else {
		ref.Wildcard = false
	}

	for i := 1; i < len(ref.Parts)-1; i++ {
		if ref.Parts[i] == "*" {
			return fmt.Errorf("'*' may only appear as the final payload path segment")
		}
	}

	if ref.Wildcard && !strings.EqualFold(first, "payload") {
		return fmt.Errorf("wildcards are only supported for payload paths")
	}

	return nil
}

func isSpecReferenceCandidate(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	if strings.Contains(trimmed, ".") {
		return false
	}
	switch strings.ToLower(trimmed) {
	case "payload", "meta", "concept", "id", "type", "createdat", "createdby", "schema", "provenance":
		return false
	}
	return specNamePattern.MatchString(trimmed)
}

func parseNumberLiteral(lit string) (any, error) {
	if !strings.ContainsAny(lit, ".eE") {
		if i, err := strconv.ParseInt(lit, 10, 64); err == nil {
			return i, nil
		}
	}
	if f, err := strconv.ParseFloat(lit, 64); err == nil {
		return f, nil
	}
	return nil, fmt.Errorf("invalid numeric literal %q", lit)
}

func toComparisonOperator(value string) (ComparisonOperator, error) {
	switch value {
	case "==":
		return OpEq, nil
	case "!=":
		return OpNe, nil
	case ">":
		return OpGt, nil
	case ">=":
		return OpGe, nil
	case "<":
		return OpLt, nil
	case "<=":
		return OpLe, nil
	case "in":
		return OpIn, nil
	case "not in":
		return OpOut, nil
	case "has":
		return OpHas, nil
	default:
		return "", fmt.Errorf("unknown comparison operator %q", value)
	}
}

func toRelationshipFunction(name string) (RelationshipFunction, bool) {
	switch name {
	case string(RelParentOf):
		return RelParentOf, true
	case string(RelChildOf):
		return RelChildOf, true
	case string(RelAliasOf):
		return RelAliasOf, true
	case string(RelEquals):
		return RelEquals, true
	case string(RelInteractsWith):
		return RelInteractsWith, true
	case string(RelContains):
		return RelContains, true
	case string(RelOwns):
		return RelOwns, true
	case string(RelCreatedBy):
		return RelCreatedBy, true
	case string(RelIds):
		return RelIds, true
	default:
		return "", false
	}
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

func isIdentifierStart(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

func isIdentifierChar(r rune) bool {
	switch {
	case unicode.IsLetter(r), unicode.IsDigit(r):
		return true
	case r == '_', r == '-', r == '.', r == '/', r == '*':
		return true
	default:
		return false
	}
}
