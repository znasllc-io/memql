package memql

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/core/logger"
)

// ASTConverterComponentName is the component name for logging.
const ASTConverterComponentName = common.ComponentName("astConverter")

// ASTConverter converts parser AST nodes to memql AST nodes.
// This is necessary because the parser package produces its own AST types
// which must be converted to the memql package's AST types for execution.
type ASTConverter struct {
	logger *slog.Logger
}

// ASTConverterOption configures the ASTConverter.
type ASTConverterOption func(*ASTConverter)

// WithASTConverterLogger sets a custom logger for the converter.
func WithASTConverterLogger(logger *slog.Logger) ASTConverterOption {
	return func(c *ASTConverter) {
		c.logger = logger
	}
}

// NewASTConverter creates a new AST converter with the given options.
func NewASTConverter(opts ...ASTConverterOption) *ASTConverter {
	c := &ASTConverter{}
	for _, opt := range opts {
		opt(c)
	}
	if c.logger == nil {
		c.logger = NewASTConverterLogger()
	}
	return c
}

// NewASTConverterLogger creates a new logger for the AST converter component.
func NewASTConverterLogger() *slog.Logger {
	return logger.New(ASTConverterComponentName, os.Stdout, resolveASTConverterLogLevel())
}

func resolveASTConverterLogLevel() slog.Level {
	reader := env.NewEnvReader("AST_CONVERTER")
	for _, key := range astConverterLoggerLevelKeys() {
		if value, ok := reader.String(key); ok {
			if strings.TrimSpace(value) == "" {
				continue
			}
			var level slog.Level
			if err := level.UnmarshalText([]byte(strings.ToLower(value))); err == nil {
				return level
			}
		}
	}
	return slog.LevelInfo
}

func astConverterLoggerLevelKeys() []string {
	return []string{
		"CAPABILITIES_LOGGING_LOG_LEVEL",
		"LOGGER_LEVEL",
		"LOG_LEVEL",
	}
}

// ConvertExpression converts a languageParser.ExpressionNode to a memql.ExpressionNode.
// Returns nil if the input is nil.
func (c *ASTConverter) ConvertExpression(expr languageParser.ExpressionNode) (ExpressionNode, error) {
	if expr == nil {
		return nil, nil
	}

	c.logger.Debug("converting expression",
		"type", fmt.Sprintf("%T", expr))

	switch node := expr.(type) {
	case *languageParser.LogicalExpr:
		return c.convertLogicalExpr(node)
	case *languageParser.ComparisonExpr:
		return c.convertComparisonExpr(node)
	case *languageParser.RelationshipExpr:
		return c.convertRelationshipExpr(node)
	case *languageParser.SortExpr:
		return c.convertSortExpr(node)
	case *languageParser.PaginateExpr:
		return c.convertPaginateExpr(node)
	case *languageParser.SelectExpr:
		return c.convertSelectExpr(node)
	case *languageParser.TimestampExpr:
		return c.convertTimestampExpr(node)
	case *languageParser.DepthExpr:
		return c.convertDepthExpr(node)
	case *languageParser.ShapeExpr:
		return c.convertShapeExpr(node)
	case *languageParser.FunctionCallExpr:
		return c.convertFunctionCallExpr(node)
	case *languageParser.BuiltinFunctionExpr:
		return c.convertBuiltinFunctionExpr(node)
	case *languageParser.SpecReferenceExpr:
		return c.convertSpecReferenceExpr(node)
	case *languageParser.ConditionalFilterExpr:
		return c.convertConditionalFilterExpr(node)
	case *languageParser.ArgRefExpr:
		return c.convertArgRefExpr(node)
	case *languageParser.CallerRefExpr:
		return &CallerRefExpression{}, nil
	case *languageParser.SIExpr:
		return c.convertSIExpr(node)
	case *languageParser.LiteralExpr:
		return c.convertLiteralExpr(node)
	case *languageParser.ErrorRefExpr:
		return c.convertErrorRefExpr(node)
	case *languageParser.ErrorExpr:
		return c.convertErrorExpr(node)
	// Typed expression-builtin nodes -- the parser produces these
	// dedicated AST types for `coalesce(...)`, `cond(...)`, `concat(...)`,
	// `hash(...)`, `canonicalId(...)`, `first(...)`, `last(...)`,
	// `lower(...)`, `upper(...)`, `trim(...)`. The step-RHS path
	// normalises these into a generic FunctionCallExpr via
	// `expressionToFunctionCall` in the parser; the body-expression
	// path (Logic return values, conditional step bodies, etc.) lands
	// here unwrapped, so the converter normalises them the same way.
	// Without these cases a body that ends with `return coalesce(a, b)`
	// would fail to load with "unsupported parser expression type:
	// *ast.CoalesceExpr".
	case *languageParser.CoalesceExpr:
		return c.convertPositionalBuiltin("coalesce", node.Args)
	case *languageParser.CondExpr:
		return c.convertPositionalBuiltin("cond",
			[]languageParser.ExpressionNode{node.Condition, node.Then, node.Else})
	case *languageParser.ConcatExpr:
		return c.convertPositionalBuiltin("concat", node.Args)
	case *languageParser.HashExpr:
		return c.convertPositionalBuiltin("hash",
			[]languageParser.ExpressionNode{node.Target})
	case *languageParser.CanonicalIdExpr:
		// canonicalId(value, concept) -- Concept is a string literal
		// at parse time (the concept's bare name), so encode it as a
		// literal positional arg rather than recursing.
		value, err := c.ConvertExpression(node.Value)
		if err != nil {
			return nil, fmt.Errorf("canonicalId arg 0: %w", err)
		}
		return &FunctionCallExpression{Name: "canonicalId", Args: map[string]any{
			"0": value,
			"1": node.Concept,
		}}, nil
	case *languageParser.FirstExpr:
		return c.convertPositionalBuiltin("first",
			[]languageParser.ExpressionNode{node.Target})
	case *languageParser.LastExpr:
		return c.convertPositionalBuiltin("last",
			[]languageParser.ExpressionNode{node.Target})
	case *languageParser.LowerExpr:
		return c.convertPositionalBuiltin("lower",
			[]languageParser.ExpressionNode{node.Target})
	case *languageParser.UpperExpr:
		return c.convertPositionalBuiltin("upper",
			[]languageParser.ExpressionNode{node.Target})
	case *languageParser.TrimExpr:
		return c.convertPositionalBuiltin("trim",
			[]languageParser.ExpressionNode{node.Target})
	case *languageParser.NilExpr:
		// `nil` literal in a Logic body (e.g. `return ctx.x != nil ?
		// ctx.x : default`). Represented as an engine LiteralValueNode
		// carrying Go nil; the executor's truthiness + comparison
		// helpers handle it the same as any other zero-valued literal.
		return &LiteralValueNode{Value: nil}, nil
	default:
		return nil, fmt.Errorf("unsupported parser expression type: %T", expr)
	}
}

// convertPositionalBuiltin wraps a list of positionally-indexed
// argument expressions into a FunctionCallExpression with the given
// builtin name. The runtime evaluator dispatches the call by name
// (see runtime_evaluator.go's EvaluateCoalesce / EvaluateConcat /
// etc.), so the typed-AST -> FunctionCallExpression rewrite is
// transparent at execution time.
func (c *ASTConverter) convertPositionalBuiltin(name string, args []languageParser.ExpressionNode) (*FunctionCallExpression, error) {
	out := make(map[string]any, len(args))
	for i, arg := range args {
		converted, err := c.ConvertExpression(arg)
		if err != nil {
			return nil, fmt.Errorf("%s arg %d: %w", name, i, err)
		}
		out[strconv.Itoa(i)] = converted
	}
	return &FunctionCallExpression{Name: name, Args: out}, nil
}

// convertLogicalExpr converts languageParser.LogicalExpr to memql.LogicalExpression.
func (c *ASTConverter) convertLogicalExpr(expr *languageParser.LogicalExpr) (*LogicalExpression, error) {
	if expr == nil {
		return nil, nil
	}

	left, err := c.ConvertExpression(expr.Left)
	if err != nil {
		return nil, fmt.Errorf("convert logical left: %w", err)
	}

	right, err := c.ConvertExpression(expr.Right)
	if err != nil {
		return nil, fmt.Errorf("convert logical right: %w", err)
	}

	return &LogicalExpression{
		Op:    convertLogicalOp(expr.Op),
		Left:  left,
		Right: right,
	}, nil
}

// convertComparisonExpr converts languageParser.ComparisonExpr to memql.ComparisonExpression.
func (c *ASTConverter) convertComparisonExpr(expr *languageParser.ComparisonExpr) (*ComparisonExpression, error) {
	if expr == nil {
		return nil, nil
	}

	// Convert value - might be an ArgReference from parser
	value := expr.Value
	if argRef, ok := expr.Value.(*languageParser.ArgRefExpr); ok {
		value = &ArgReference{Path: argRef.Path}
	}

	result := &ComparisonExpression{
		Field:    convertFieldReference(expr.Field),
		Operator: convertComparisonOperator(expr.Operator),
		Value:    value,
	}

	if expr.CacheHintSeconds != nil {
		hint := *expr.CacheHintSeconds
		result.CacheHintSeconds = &hint
	}

	if len(expr.FieldSelections) > 0 {
		result.FieldSelections = make([]FieldReference, len(expr.FieldSelections))
		for i, field := range expr.FieldSelections {
			result.FieldSelections[i] = convertFieldReference(field)
		}
	}

	return result, nil
}

// convertRelationshipExpr converts languageParser.RelationshipExpr to memql.RelationshipExpression.
func (c *ASTConverter) convertRelationshipExpr(expr *languageParser.RelationshipExpr) (*RelationshipExpression, error) {
	if expr == nil {
		return nil, nil
	}

	target, err := c.ConvertExpression(expr.Target)
	if err != nil {
		return nil, fmt.Errorf("convert relationship target: %w", err)
	}

	return &RelationshipExpression{
		Function: convertRelationshipFunction(expr.Function),
		Target:   target,
	}, nil
}

// convertSortExpr converts languageParser.SortExpr to memql.SortExpression.
func (c *ASTConverter) convertSortExpr(expr *languageParser.SortExpr) (*SortExpression, error) {
	if expr == nil {
		return nil, nil
	}

	target, err := c.ConvertExpression(expr.Target)
	if err != nil {
		return nil, fmt.Errorf("convert sort target: %w", err)
	}

	fields := make([]SortField, len(expr.Fields))
	for i, field := range expr.Fields {
		fields[i] = SortField{
			Field:     field.Field,
			Direction: convertSortDirection(field.Direction),
		}
	}

	return &SortExpression{
		Target: target,
		Fields: fields,
	}, nil
}

// convertPaginateExpr converts languageParser.PaginateExpr to memql.PaginateExpression.
func (c *ASTConverter) convertPaginateExpr(expr *languageParser.PaginateExpr) (*PaginateExpression, error) {
	if expr == nil {
		return nil, nil
	}

	target, err := c.ConvertExpression(expr.Target)
	if err != nil {
		return nil, fmt.Errorf("convert paginate target: %w", err)
	}

	result := &PaginateExpression{
		Target: target,
	}

	if expr.Limit != nil {
		limit := *expr.Limit
		result.Limit = &limit
	}

	if expr.Offset != nil {
		offset := *expr.Offset
		result.Offset = &offset
	}

	return result, nil
}

// convertSelectExpr converts languageParser.SelectExpr to memql.SelectExpression.
func (c *ASTConverter) convertSelectExpr(expr *languageParser.SelectExpr) (*SelectExpression, error) {
	if expr == nil {
		return nil, nil
	}

	target, err := c.ConvertExpression(expr.Target)
	if err != nil {
		return nil, fmt.Errorf("convert select target: %w", err)
	}

	fields := make([]FieldReference, len(expr.Fields))
	for i, field := range expr.Fields {
		fields[i] = convertFieldReference(field)
	}

	return &SelectExpression{
		Target: target,
		Fields: fields,
	}, nil
}

// convertTimestampExpr converts languageParser.TimestampExpr to memql.TimestampExpression.
func (c *ASTConverter) convertTimestampExpr(expr *languageParser.TimestampExpr) (*TimestampExpression, error) {
	if expr == nil {
		return nil, nil
	}

	target, err := c.ConvertExpression(expr.Target)
	if err != nil {
		return nil, fmt.Errorf("convert timestamp target: %w", err)
	}

	return &TimestampExpression{
		Target:    target,
		Timestamp: expr.Timestamp,
		UseLatest: expr.UseLatest,
	}, nil
}

// convertDepthExpr converts languageParser.DepthExpr to memql.DepthExpression.
func (c *ASTConverter) convertDepthExpr(expr *languageParser.DepthExpr) (*DepthExpression, error) {
	if expr == nil {
		return nil, nil
	}

	target, err := c.ConvertExpression(expr.Target)
	if err != nil {
		return nil, fmt.Errorf("convert depth target: %w", err)
	}

	return &DepthExpression{
		Target: target,
		Depth:  expr.Depth,
	}, nil
}

// convertShapeExpr converts languageParser.ShapeExpr to memql.ShapeExpression.
func (c *ASTConverter) convertShapeExpr(expr *languageParser.ShapeExpr) (*ShapeExpression, error) {
	if expr == nil {
		return nil, nil
	}

	target, err := c.ConvertExpression(expr.Target)
	if err != nil {
		return nil, fmt.Errorf("convert shape target: %w", err)
	}

	// Named shape reference -- template will be resolved at execution time
	// via the shape registry.
	if expr.TemplateName != "" {
		return &ShapeExpression{
			Target:        target,
			TemplateName:  expr.TemplateName,
			IncludeBundle: expr.IncludeBundle,
		}, nil
	}

	// Convert the inline shape template
	// The template is stored as `any` in both parser and memql packages
	// We need to convert it to the memql shapeTemplate type
	template, err := c.convertShapeTemplate(expr.Template)
	if err != nil {
		return nil, fmt.Errorf("convert shape template: %w", err)
	}

	return &ShapeExpression{
		Target:        target,
		Template:      template,
		IncludeBundle: expr.IncludeBundle,
	}, nil
}

// convertShapeTemplate converts a parser shape template to a memql shapeTemplate.
// Shape templates can be complex nested structures.
func (c *ASTConverter) convertShapeTemplate(template any) (shapeTemplate, error) {
	if template == nil {
		return nil, nil
	}

	switch t := template.(type) {
	case map[string]any:
		// Object template - convert each value
		result := &shapeObject{
			Fields: make(map[string]shapeTemplate),
		}
		for key, value := range t {
			converted, err := c.convertShapeTemplate(value)
			if err != nil {
				return nil, fmt.Errorf("convert template field %q: %w", key, err)
			}
			result.Fields[key] = converted
		}
		return result, nil

	case string:
		// String literal
		return &shapeLiteral{Value: t}, nil

	case *languageParser.FunctionCallExpr:
		// Handle shape template functions like node(), json(), children(), etc.
		return c.convertShapeTemplateFunction(t)

	case *shapeNodeFunc:
		// Already converted
		return t, nil

	case *shapeObject:
		// Already converted
		return t, nil

	case *shapeLiteral:
		// Already converted
		return t, nil

	case *shapeRelationFunc:
		// Already converted
		return t, nil

	case *shapeMatchExpr:
		// Already converted
		return t, nil

	case *shapeSIValue:
		// Already converted
		return t, nil

	case *shapeJSONFunc:
		// Already converted
		return t, nil

	case *shapeArray:
		// Already converted
		return t, nil

	default:
		// For other types, wrap as literal
		return &shapeLiteral{Value: t}, nil
	}
}

// convertShapeTemplateFunction converts a parser FunctionCallExpr to the appropriate shape template type.
// Handles node(), json(), children(), contains(), owns(), aliases(), createdBy(), interactions().
func (c *ASTConverter) convertShapeTemplateFunction(fn *languageParser.FunctionCallExpr) (shapeTemplate, error) {
	if fn == nil {
		return nil, nil
	}

	name := strings.ToLower(fn.Name)

	switch name {
	case "node":
		// node("path") or node("path1", "path2", ...) -> shapeNodeFunc
		fields := make([]FieldReference, 0)
		for i := 0; ; i++ {
			key := strconv.Itoa(i)
			arg, ok := fn.Args[key]
			if !ok {
				break
			}
			path, ok := arg.(string)
			if !ok {
				return nil, fmt.Errorf("node() argument %d must be a string, got %T", i, arg)
			}
			fields = append(fields, FieldReference{
				Raw:   path,
				Parts: strings.Split(path, "."),
			})
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf("node() requires at least one path argument")
		}
		return &shapeNodeFunc{Fields: fields}, nil

	case "json":
		// json(innerTemplate) -> shapeJSONFunc
		// The inner template could be another function call or an object
		innerArg, ok := fn.Args["0"]
		if !ok {
			return nil, fmt.Errorf("json() requires an inner template argument")
		}
		inner, err := c.convertShapeTemplate(innerArg)
		if err != nil {
			return nil, fmt.Errorf("json() inner template: %w", err)
		}
		return &shapeJSONFunc{Inner: inner}, nil

	case "children", "contains", "owns", "aliases", "createdby", "interactions":
		// Relationship functions -> shapeRelationFunc
		// children(template), contains(template), etc.
		templateArg, ok := fn.Args["0"]
		if !ok {
			return nil, fmt.Errorf("%s() requires a template argument", fn.Name)
		}
		innerTemplate, err := c.convertShapeTemplate(templateArg)
		if err != nil {
			return nil, fmt.Errorf("%s() template: %w", fn.Name, err)
		}
		return &shapeRelationFunc{
			Relation: name,
			Template: innerTemplate,
		}, nil

	default:
		// Unknown function - wrap as literal (preserves the function call structure)
		c.logger.Warn("unknown shape template function, treating as literal",
			"function", fn.Name)
		return &shapeLiteral{Value: fn}, nil
	}
}

// convertFunctionCallExpr converts languageParser.FunctionCallExpr to memql.FunctionCallExpression.
func (c *ASTConverter) convertFunctionCallExpr(expr *languageParser.FunctionCallExpr) (*FunctionCallExpression, error) {
	if expr == nil {
		return nil, nil
	}

	// Clone the args map
	args := make(map[string]any)
	for k, v := range expr.Args {
		args[k] = v
	}

	return &FunctionCallExpression{
		Name: expr.Name,
		Args: args,
	}, nil
}

// convertBuiltinFunctionExpr converts languageParser.BuiltinFunctionExpr to memql.BuiltinFunctionExpression.
func (c *ASTConverter) convertBuiltinFunctionExpr(expr *languageParser.BuiltinFunctionExpr) (*BuiltinFunctionExpression, error) {
	if expr == nil {
		return nil, nil
	}

	return &BuiltinFunctionExpression{
		Name:     expr.Name,
		Executor: expr.Executor,
	}, nil
}

// convertSpecReferenceExpr converts languageParser.SpecReferenceExpr to memql.SpecReferenceExpression.
func (c *ASTConverter) convertSpecReferenceExpr(expr *languageParser.SpecReferenceExpr) (*SpecReferenceExpression, error) {
	if expr == nil {
		return nil, nil
	}

	return &SpecReferenceExpression{
		Name: expr.Name,
	}, nil
}

// convertConditionalFilterExpr converts languageParser.ConditionalFilterExpr to memql.ConditionalFilterExpression.
func (c *ASTConverter) convertConditionalFilterExpr(expr *languageParser.ConditionalFilterExpr) (*ConditionalFilterExpression, error) {
	if expr == nil {
		return nil, nil
	}

	filter, err := c.ConvertExpression(expr.Filter)
	if err != nil {
		return nil, fmt.Errorf("convert conditional filter: %w", err)
	}

	return &ConditionalFilterExpression{
		ArgPath: expr.ArgPath,
		Filter:  filter,
	}, nil
}

// convertArgRefExpr converts languageParser.ArgRefExpr to memql.ArgRefExpression.
func (c *ASTConverter) convertArgRefExpr(expr *languageParser.ArgRefExpr) (*ArgRefExpression, error) {
	if expr == nil {
		return nil, nil
	}

	return &ArgRefExpression{
		Path: expr.Path,
	}, nil
}

// convertSIExpr converts languageParser.SIExpr to memql.SIExpression.
func (c *ASTConverter) convertSIExpr(expr *languageParser.SIExpr) (*SIExpression, error) {
	if expr == nil {
		return nil, nil
	}

	invocation := &SIInvocation{
		TemplateId: expr.TemplateId,
	}

	if expr.ProviderOverride != nil {
		provider := *expr.ProviderOverride
		invocation.ProviderOverride = &provider
	}

	if expr.CacheSeconds != nil {
		seconds := *expr.CacheSeconds
		invocation.CacheSeconds = &seconds
	}

	return &SIExpression{
		Invocation: invocation,
	}, nil
}

// convertLiteralExpr converts languageParser.LiteralExpr to memql.LiteralValueNode.
func (c *ASTConverter) convertLiteralExpr(expr *languageParser.LiteralExpr) (*LiteralValueNode, error) {
	if expr == nil {
		return nil, nil
	}

	return &LiteralValueNode{
		Value: expr.Value,
	}, nil
}

// convertErrorRefExpr converts languageParser.ErrorRefExpr to memql.ErrorRefExpression.
func (c *ASTConverter) convertErrorRefExpr(_ *languageParser.ErrorRefExpr) (*ErrorRefExpression, error) {
	return &ErrorRefExpression{}, nil
}

// convertErrorExpr converts languageParser.ErrorExpr to memql.ErrorExpression.
func (c *ASTConverter) convertErrorExpr(expr *languageParser.ErrorExpr) (*ErrorExpression, error) {
	if expr == nil {
		return nil, nil
	}

	message, err := c.ConvertExpression(expr.Message)
	if err != nil {
		return nil, fmt.Errorf("convert error message: %w", err)
	}

	return &ErrorExpression{
		Message: message,
	}, nil
}

// --- Helper conversion functions ---

// convertLogicalOp converts languageParser.LogicalOp to memql.LogicalOp.
func convertLogicalOp(op languageParser.LogicalOp) LogicalOp {
	switch op {
	case languageParser.LogicalAnd:
		return LogicalAnd
	case languageParser.LogicalOr:
		return LogicalOr
	default:
		return LogicalAnd
	}
}

// convertComparisonOperator converts languageParser.ComparisonOperator to memql.ComparisonOperator.
func convertComparisonOperator(op languageParser.ComparisonOperator) ComparisonOperator {
	switch op {
	case languageParser.OpEq:
		return OpEq
	case languageParser.OpNe:
		return OpNe
	case languageParser.OpGt:
		return OpGt
	case languageParser.OpGe:
		return OpGe
	case languageParser.OpLt:
		return OpLt
	case languageParser.OpLe:
		return OpLe
	case languageParser.OpIn:
		return OpIn
	case languageParser.OpOut:
		return OpOut
	case languageParser.OpHas:
		return OpHas
	case languageParser.OpMissing:
		return OpMissing
	case languageParser.OpNotMissing:
		return OpNotMissing
	default:
		return OpEq
	}
}

// convertRelationshipFunction converts languageParser.RelationshipFunction to memql.RelationshipFunction.
func convertRelationshipFunction(fn languageParser.RelationshipFunction) RelationshipFunction {
	switch fn {
	case languageParser.RelParentOf:
		return RelParentOf
	case languageParser.RelChildOf:
		return RelChildOf
	case languageParser.RelAliasOf:
		return RelAliasOf
	case languageParser.RelEquals:
		return RelEquals
	case languageParser.RelInteractsWith:
		return RelInteractsWith
	case languageParser.RelContains:
		return RelContains
	case languageParser.RelOwns:
		return RelOwns
	case languageParser.RelCreatedBy:
		return RelCreatedBy
	case languageParser.RelIds:
		return RelIds
	default:
		return RelParentOf
	}
}

// convertSortDirection converts languageParser.SortDirection to memql.SortDirection.
func convertSortDirection(dir languageParser.SortDirection) SortDirection {
	switch dir {
	case languageParser.SortAsc:
		return SortDirectionAsc
	case languageParser.SortDesc:
		return SortDirectionDesc
	default:
		return SortDirectionDesc
	}
}

// convertFieldReference converts languageParser.FieldReference to memql.FieldReference.
func convertFieldReference(ref languageParser.FieldReference) FieldReference {
	parts := make([]string, len(ref.Parts))
	copy(parts, ref.Parts)

	return FieldReference{
		Raw:      ref.Raw,
		Parts:    parts,
		Wildcard: ref.Wildcard,
	}
}

// ConvertSpecDef has been retired. Specs + traits now have their own
// dedicated parser at component/memql/spec_parser.go that builds the
// Spec directly without routing through FunctionDef. The legacy
// rewriter pipeline (spec_rewrite -> trait_rewrite -> general
// parser -> ConvertSpecDef) is gone.

// joinAllowedNames returns a deterministic, comma-separated list of
// annotation names from the allow-list, for error-message rendering.
func joinAllowedNames(allowed map[string]bool) string {
	names := make([]string, 0, len(allowed))
	for name := range allowed {
		names = append(names, "@"+name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// classifySpecKind walks the spec body and returns SpecKindRow when
// the body only references row fields (payload / intrinsics) or
// SpecKindContext when it only references the auth-context envelope
// (caller.*). Mixed bodies are rejected; a real consumer that needs
// both should compose a row-spec + context-spec via a policy.
func classifySpecKind(expr ExpressionNode) (SpecKind, error) {
	hasRow := false
	hasContext := false
	walkSpecRefs(expr, func(ref FieldReference) {
		if len(ref.Parts) == 0 {
			return
		}
		first := strings.ToLower(strings.TrimSpace(ref.Parts[0]))
		switch first {
		case "caller":
			hasContext = true
		case "payload", "meta", "id", "concept", "type", "createdat", "createdby", "schema":
			hasRow = true
		}
	})
	switch {
	case hasRow && hasContext:
		return "", fmt.Errorf("spec body mixes row references (payload / intrinsics) with context references (caller.*) -- split into a row-spec + a context-spec and compose via a policy")
	case hasContext:
		return SpecKindContext, nil
	default:
		// Default to row-spec even when the body is a literal-only
		// predicate (rare; e.g. `true` / `false`). The SQL pushdown
		// path handles constant predicates correctly.
		return SpecKindRow, nil
	}
}

// walkSpecRefs visits every FieldReference reached from the
// expression tree and invokes the callback. Used by the
// row-vs-context classifier; could grow into a more general
// authoring-rule sense walker as needed.
func walkSpecRefs(expr ExpressionNode, visit func(FieldReference)) {
	if expr == nil || visit == nil {
		return
	}
	switch node := expr.(type) {
	case *ComparisonExpression:
		visit(node.Field)
	case *LogicalExpression:
		walkSpecRefs(node.Left, visit)
		walkSpecRefs(node.Right, visit)
	case *RelationshipExpression:
		walkSpecRefs(node.Target, visit)
	}
}

func rejectBareSpecReferences(expr languageParser.ExpressionNode) error {
	if expr == nil {
		return nil
	}
	switch node := expr.(type) {
	case *languageParser.SpecReferenceExpr:
		return fmt.Errorf("spec reference %q must use parentheses: %s()", node.Name, strings.TrimSpace(node.Name))
	case *languageParser.LogicalExpr:
		if err := rejectBareSpecReferences(node.Left); err != nil {
			return err
		}
		return rejectBareSpecReferences(node.Right)
	case *languageParser.RelationshipExpr:
		return rejectBareSpecReferences(node.Target)
	default:
		return nil
	}
}

func normalizeSpecCallsToReferences(expr ExpressionNode) (ExpressionNode, error) {
	if expr == nil {
		return nil, nil
	}
	switch node := expr.(type) {
	case *LogicalExpression:
		left, err := normalizeSpecCallsToReferences(node.Left)
		if err != nil {
			return nil, err
		}
		right, err := normalizeSpecCallsToReferences(node.Right)
		if err != nil {
			return nil, err
		}
		return &LogicalExpression{
			Op:    node.Op,
			Left:  left,
			Right: right,
		}, nil
	case *RelationshipExpression:
		target, err := normalizeSpecCallsToReferences(node.Target)
		if err != nil {
			return nil, err
		}
		return &RelationshipExpression{
			Function: node.Function,
			Target:   target,
		}, nil
	case *FunctionCallExpression:
		name := strings.TrimSpace(node.Name)
		if len(node.Args) > 0 {
			return nil, fmt.Errorf("spec reference %q must not accept arguments", name)
		}
		if !isSpecReferenceCandidate(name) {
			return nil, fmt.Errorf("unsupported function call %q in spec expression; only specName() references are allowed", name)
		}
		return &SpecReferenceExpression{Name: name}, nil
	default:
		return cloneExpressionNode(expr), nil
	}
}

// expressionToSource converts a parser expression back to source string.
// This is a best-effort conversion for display purposes.
func expressionToSource(expr languageParser.ExpressionNode) string {
	if expr == nil {
		return ""
	}

	switch node := expr.(type) {
	case *languageParser.LogicalExpr:
		left := expressionToSource(node.Left)
		right := expressionToSource(node.Right)
		sep := ";"
		if node.Op == languageParser.LogicalOr {
			sep = ","
		}
		return left + sep + right

	case *languageParser.ComparisonExpr:
		field := node.Field.Raw
		op := string(node.Operator)
		value := fmt.Sprintf("%v", node.Value)
		if s, ok := node.Value.(string); ok {
			value = fmt.Sprintf("%q", s)
		}
		return field + op + value

	case *languageParser.RelationshipExpr:
		inner := expressionToSource(node.Target)
		return string(node.Function) + "(" + inner + ")"

	case *languageParser.SpecReferenceExpr:
		return node.Name

	default:
		return fmt.Sprintf("<%T>", expr)
	}
}
