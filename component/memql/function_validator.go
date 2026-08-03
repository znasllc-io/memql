package memql

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/ast"
)

// functionValidator validates and resolves function references in MemQL expressions.
type functionValidator struct {
	functions map[string]*Function
	specs     *SpecRegistry
	resolved  map[string]ExpressionNode
	resolving map[string]struct{}

	// origin is where the call came from (memql#2800). The parse path is
	// ctx-free by design, so the engine reads auth.OriginFromContext once in
	// executeWith and hands the result down rather than threading a context
	// through the parser. The ZERO VALUE is auth.OriginClient -- a validator
	// built without an explicit origin (tests, internal tooling) is treated as
	// untrusted, so forgetting to pass it denies rather than grants.
	origin auth.CallOrigin
}

func newFunctionValidator(functions map[string]*Function, specs *SpecRegistry) *functionValidator {
	return newFunctionValidatorWithOrigin(functions, specs, auth.OriginClient)
}

func newFunctionValidatorWithOrigin(functions map[string]*Function, specs *SpecRegistry, origin auth.CallOrigin) *functionValidator {
	return &functionValidator{
		functions: functions,
		specs:     specs,
		resolved:  make(map[string]ExpressionNode),
		resolving: make(map[string]struct{}),
		origin:    origin,
	}
}

// rejectUnknownArgs returns an error naming the first caller-supplied
// argument that the function's args { ... } block does not declare, unless
// the function explicitly opted into open args via @additionalProperties(true).
// Used by the strict MCP mutation-call path (memql#1633) to turn a silent
// arg-drop into a loud failure. A function with no declared args schema is
// treated as open (nothing to validate against).
func rejectUnknownArgs(fn *Function, args map[string]any) error {
	if fn == nil || fn.ArgsSchema == nil || len(fn.ArgsSchema.Fields) == 0 {
		return nil
	}
	if fn.ArgsSchema.AdditionalProperties != nil && *fn.ArgsSchema.AdditionalProperties {
		return nil
	}
	declared := make(map[string]struct{}, len(fn.ArgsSchema.Fields))
	for _, field := range fn.ArgsSchema.Fields {
		if field == nil || strings.TrimSpace(field.Name) == "" {
			continue
		}
		declared[field.Name] = struct{}{}
	}
	for key := range args {
		if _, ok := declared[key]; !ok {
			return fmt.Errorf("mutation %q: unexpected argument %q (not declared in the mutation's args; supplying it would silently drop the value -- check the argument name)", fn.Name, key)
		}
	}
	return nil
}

// validateFunctionArgs validates the provided arguments against the function's assertions.
func (v *functionValidator) validateFunctionArgs(fn *Function, args map[string]any) error {
	if fn == nil || fn.ArgsSchema == nil || len(fn.ArgsSchema.Fields) == 0 {
		return nil // No assertions defined, any args allowed
	}

	fieldMap := make(map[string]*FunctionArgsField, len(fn.ArgsSchema.Fields))
	for _, field := range fn.ArgsSchema.Fields {
		if field == nil || strings.TrimSpace(field.Name) == "" {
			continue
		}
		fieldMap[field.Name] = field
	}

	// Reject unknown args when additionalProperties is explicitly false.
	if fn.ArgsSchema.AdditionalProperties != nil && !*fn.ArgsSchema.AdditionalProperties {
		for key := range args {
			if _, ok := fieldMap[key]; !ok {
				return fmt.Errorf("function %q: argument validation failed: unexpected argument %q", fn.Name, key)
			}
		}
	}

	// Validate each field assertion
	var errors []string
	for _, field := range fieldMap {
		if err := validateArgsField(args, field, ""); err != nil {
			errors = append(errors, err.Error())
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("function %q: argument validation failed: %s", fn.Name, strings.Join(errors, "; "))
	}
	return nil
}

// validateArgsField validates a single field against its assertion.
func validateArgsField(args map[string]any, field *FunctionArgsField, prefix string) error {
	fieldPath := field.Name
	if prefix != "" {
		fieldPath = prefix + "." + field.Name
	}

	value, exists := args[field.Name]

	// Check if required field is missing
	if !exists || value == nil {
		if !field.Optional {
			return fmt.Errorf("required argument %q is missing", fieldPath)
		}
		return nil // Optional field not provided, that's OK
	}

	// Validate type
	if !validateArgType(value, field.Type) {
		return fmt.Errorf("argument %q: expected %s, got %s", fieldPath, field.Type, actualArgType(value))
	}

	if len(field.Enum) > 0 && !valueInEnum(value, field.Enum) {
		return fmt.Errorf("argument %q: value %v is not in enum %v", fieldPath, value, field.Enum)
	}
	if field.Minimum != nil || field.Maximum != nil {
		num, ok := numericValue(value)
		if !ok {
			return fmt.Errorf("argument %q: expected numeric value for minimum/maximum validation", fieldPath)
		}
		if field.Minimum != nil && num < *field.Minimum {
			return fmt.Errorf("argument %q: value %v is less than minimum %v", fieldPath, value, *field.Minimum)
		}
		if field.Maximum != nil && num > *field.Maximum {
			return fmt.Errorf("argument %q: value %v is greater than maximum %v", fieldPath, value, *field.Maximum)
		}
	}
	if strings.TrimSpace(field.Format) != "" {
		if err := validateFieldFormat(fieldPath, value, field.Format); err != nil {
			return err
		}
	}
	if field.MaxLength > 0 {
		s, ok := value.(string)
		if ok && utf8.RuneCountInString(s) > field.MaxLength {
			return fmt.Errorf("argument %q: value too long (%d runes, max %d)", fieldPath, utf8.RuneCountInString(s), field.MaxLength)
		}
	}
	if field.patternRegex != nil {
		s, ok := value.(string)
		if ok && !field.patternRegex.MatchString(s) {
			return fmt.Errorf("argument %q: value %q does not match pattern %q", fieldPath, s, field.Pattern)
		}
	}

	// Recursively validate nested fields for objects
	if field.Type == "object" {
		nestedMap, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("argument %q: expected object for nested validation", fieldPath)
		}
		if len(field.Nested) > 0 {
			nestedFieldMap := make(map[string]*FunctionArgsField, len(field.Nested))
			for _, nestedField := range field.Nested {
				if nestedField == nil || strings.TrimSpace(nestedField.Name) == "" {
					continue
				}
				nestedFieldMap[nestedField.Name] = nestedField
			}
			if field.AdditionalProperties != nil && !*field.AdditionalProperties {
				for key := range nestedMap {
					if _, ok := nestedFieldMap[key]; !ok {
						return fmt.Errorf("argument %q: unexpected field %q", fieldPath, key)
					}
				}
			}
			for _, nestedField := range nestedFieldMap {
				if err := validateArgsField(nestedMap, nestedField, fieldPath); err != nil {
					return err
				}
			}
		}
	}
	if field.Type == "array" && field.Items != nil {
		items, ok := value.([]any)
		if !ok {
			// []map[string]any can surface directly.
			if typed, ok := value.([]map[string]any); ok {
				items = make([]any, len(typed))
				for i := range typed {
					items[i] = typed[i]
				}
			} else {
				return fmt.Errorf("argument %q: expected array for item validation", fieldPath)
			}
		}
		for i, item := range items {
			itemField := field.Items.clone()
			if strings.TrimSpace(itemField.Name) == "" {
				itemField.Name = "__item"
			}
			itemMap := map[string]any{itemField.Name: item}
			if err := validateArgsField(itemMap, itemField, fmt.Sprintf("%s[%d]", fieldPath, i)); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateArgType checks if a value matches the expected type.
func validateArgType(value any, expectedType string) bool {
	if expectedType == "any" {
		return true
	}

	switch expectedType {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			return true
		}
		return false
	case "int", "integer":
		switch n := value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
			return float64(int64(n)) == n
		case float32:
			return float32(int32(n)) == n
		}
		return false
	case "bool", "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		switch value.(type) {
		case []any, []map[string]any:
			return true
		}
		return false
	default:
		return true // Unknown type, accept any value
	}
}

func valueInEnum(value any, options []any) bool {
	for _, option := range options {
		if reflect.DeepEqual(value, option) {
			return true
		}
	}
	return false
}

func numericValue(value any) (float64, bool) {
	switch n := value.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

func validateFieldFormat(fieldPath string, value any, format string) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "none":
		return nil
	case "date-time":
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("argument %q: expected string for format %q", fieldPath, format)
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return fmt.Errorf("argument %q: expected RFC3339 date-time, got %q", fieldPath, s)
		}
		return nil
	case "int", "integer":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return nil
		case float64:
			if float64(int64(value.(float64))) == value.(float64) {
				return nil
			}
		case string:
			if _, err := strconv.ParseInt(value.(string), 10, 64); err == nil {
				return nil
			}
		}
		return fmt.Errorf("argument %q: expected integer-compatible value", fieldPath)
	default:
		return nil
	}
}

// actualArgType returns a human-readable type name for a value.
func actualArgType(value any) string {
	if value == nil {
		return "null"
	}
	switch value.(type) {
	case string:
		return "string"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return "number"
	case bool:
		return "bool"
	case map[string]any:
		return "object"
	case []any, []map[string]any:
		return "array"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// expandFunctionCall resolves a function call with its arguments.
// This validates the arguments against the schema, then expands the function expression
// with the arguments substituted into arg() references and conditional filters processed.
func (v *functionValidator) expandFunctionCall(call *FunctionCallExpression) (ExpressionNode, error) {
	key := strings.TrimSpace(call.Name)
	if key == "" {
		return nil, fmt.Errorf("function name is required")
	}

	fn, ok := v.functions[key]
	if !ok || fn == nil {
		// A @disabled spec keeps its name reserved -- say so rather than
		// "not found" (#2607).
		if v.specs != nil && v.specs.IsDisabled(key) {
			return nil, fmt.Errorf("spec %q is disabled", key)
		}
		// Specs are invoked with the same call syntax but cannot accept args.
		if v.specs != nil && v.specs.Has(key) {
			if len(call.Args) > 0 {
				return nil, fmt.Errorf("spec %q does not accept arguments", key)
			}
			spec, err := v.specs.Get(key)
			if err != nil {
				return nil, err
			}
			return cloneExpressionNode(spec.Expr), nil
		}
		return nil, fmt.Errorf("function %q not found", key)
	}
	if strings.EqualFold(strings.TrimSpace(fn.FunctionKind), "mutation") {
		return nil, fmt.Errorf("function %q is a mutation and cannot be used inside query expressions", key)
	}
	// #2605: @disabled gates execution on every function kind. Mutations and
	// logic reject in the engine call paths (engine.go); queries and
	// builtins expand here, so this is their execution gate. The builtin
	// exemption that shielded the dead Enabled flag is gone: #2608 made the
	// flag honest, so a disabled builtin rejects like everything else.
	if !fn.Enabled {
		return nil, fmt.Errorf("function %q is disabled", key)
	}
	// #2800: @serverOnly bars client-originated calls while leaving
	// server-side Go free to call the construct. This sits beside the
	// @disabled gate because it is the same kind of check at the same depth
	// -- the resolved *Function is in hand -- but it is NOT the same
	// property: @disabled stops every caller, which would break the very
	// server-side paths @serverOnly exists to keep working (the auth path
	// resolving `sub` -> user before an actor exists; the kill-switch
	// automation acting on a user other than the actor).
	if fn.ServerOnly && !v.origin.IsInternal() {
		return nil, fmt.Errorf("function %q is server-only and cannot be called by a client", key)
	}

	// Validate arguments against schema. Normalise positional
	// object-literal wrapping first: the language parser produces
	// `funcName({a: 1, b: 2})` as Args = {"0": {a: 1, b: 2}} (single
	// positional arg holding the named-args object); the engine's own
	// expression parser produces flat args directly. Both shapes are
	// valid input here; downstream code (validator, ArgRef
	// substitution, mutation-template rendering) expects the flat
	// shape. Reducing here means callers never have to think about
	// which parser produced the call. The F.6 path
	// (`substituteArgRefsAndCallArgs`) does the same flattening
	// after substitution.
	args := flattenPositionalArgs(call.Args)
	if args == nil {
		args = make(map[string]any)
	}
	if err := v.validateFunctionArgs(fn, args); err != nil {
		return nil, err
	}

	// Builtin functions have no DSL body -- their behaviour lives in a
	// Go executor named by fn.Executor. The validator emits a
	// BuiltinFunctionExpression so the evaluator dispatches through
	// `evaluateBuiltinFunctionExpression` (which calls the registered
	// builtinExecutorHandler) instead of treating the call as an
	// unexpanded user function.
	if fn.Executor != "" && fn.Expr == nil {
		return &BuiltinFunctionExpression{
			Name:     fn.Name,
			Executor: fn.Executor,
			Args:     args,
		}, nil
	}

	// Clone the function expression so we don't modify the original
	expr := cloneExpressionNode(fn.Expr)

	// Expand the function expression with args context for substitution
	expanded, err := v.expandExpressionWithArgs(expr, args)
	if err != nil {
		return nil, fmt.Errorf("function %q: %w", key, err)
	}

	return expanded, nil
}

// flattenPositionalArgs reduces a single-positional object-literal
// call args shape (`{"0": {a: 1, b: 2}}`) to its flat form
// (`{a: 1, b: 2}`). Non-positional inputs pass through unchanged.
func flattenPositionalArgs(args map[string]any) map[string]any {
	if len(args) != 1 {
		return args
	}
	inner, ok := args["0"].(map[string]any)
	if !ok {
		return args
	}
	return inner
}

// expandFunctionCallAllowMutationLeaf expands a Logic function call
// to its return expression with args substituted, permitting the leaf
// to be a mutation call. This is the F.6 hook for top-level Logic
// invocations whose body returns a mutation call -- the plan
// resolver hoists the resulting FunctionCallExpression into
// plan.MutationCall so the engine dispatches it through
// executeMutationFunctionCall.
//
// Returns (nil, nil) if the call is not a Logic, the Logic's
// expression is missing, or the expansion does not yield a
// top-level FunctionCallExpression.
func (v *functionValidator) expandFunctionCallAllowMutationLeaf(call *FunctionCallExpression) (ExpressionNode, error) {
	if call == nil {
		return nil, nil
	}
	key := strings.TrimSpace(call.Name)
	if key == "" {
		return nil, nil
	}
	fn, ok := v.functions[key]
	if !ok || fn == nil {
		return nil, nil
	}
	if !strings.EqualFold(strings.TrimSpace(fn.FunctionKind), "logic") {
		return nil, nil
	}
	// This is the F.6 single-mutation-leaf hoist, and it is a FOURTH dispatch
	// point -- it reaches a construct without going through
	// expandFunctionCall, so it inherits none of that function's gates. It
	// checked neither, which meant a @disabled logic still hoisted, and a
	// @serverOnly one would have hoisted for a client (memql#2800).
	//
	// @serverOnly is not currently registrable on Logic
	// (annotations/registry.go offers it on Query and Mutation only), so the
	// second half is defence rather than a live hole today. It is checked
	// anyway: the reason the other three points are all gated is that a
	// missing one is invisible until someone finds it, and "the annotation
	// cannot be applied here yet" is a fact about a registry that can change
	// in one line.
	if !fn.Enabled {
		return nil, fmt.Errorf("function %q is disabled", key)
	}
	if fn.ServerOnly && !v.origin.IsInternal() {
		return nil, fmt.Errorf("function %q is server-only and cannot be called by a client", key)
	}
	if fn.Expr == nil {
		return nil, nil
	}

	args := call.Args
	if args == nil {
		args = make(map[string]any)
	}
	if err := v.validateFunctionArgs(fn, args); err != nil {
		return nil, err
	}

	expr := cloneExpressionNode(fn.Expr)
	// Substitute caller args against the Logic's body. We can't call
	// the regular expandExpressionWithArgs path because that recurses
	// into expandFunctionCall which rejects mutations. Substitute
	// ArgReference nodes manually -- they're the only things in a
	// Logic's `return <mutationCall(args.X)>` body that need
	// rewriting (the call target itself stays a FunctionCallExpression
	// pointing at the mutation).
	substituted, err := v.substituteArgRefsAndCallArgs(expr, args)
	if err != nil {
		return nil, err
	}
	return substituted, nil
}

// substituteArgRefsAndCallArgs handles the F.6 case where a Logic's
// `return <expr>` body is a top-level mutation call -- e.g.
// `return mutationCreateSpace({ id: args.id, name: args.name })`.
// We need a fully-substituted top-level FunctionCallExpression so
// resolvePlanFunctions can hoist it into plan.MutationCall. ArgReference
// nodes live in the call-args value space (not the ExpressionNode
// hierarchy), so this walker only fires on FunctionCallExpression
// targets and rewrites their Args map; non-call expressions pass
// through unchanged.
//
// Single-positional object-literal calls produced by the language
// parser (`mutationFoo({a: 1, b: 2})` -> Args = {"0": {a:1, b:2}})
// are flattened to the inner map so the downstream mutation
// validator sees the canonical flat-args shape. The engine's own
// expression parser produces flat args directly; this normalisation
// closes the gap between the two parsers.
func (v *functionValidator) substituteArgRefsAndCallArgs(expr ExpressionNode, args map[string]any) (ExpressionNode, error) {
	call, ok := expr.(*FunctionCallExpression)
	if !ok || call == nil {
		return expr, nil
	}
	newArgs := make(map[string]any, len(call.Args))
	for k, val := range call.Args {
		// memql#2870: fold here too. This is a FOURTH dispatch point that
		// reaches a construct without going through expandExpressionWithArgs, so
		// it inherits none of that path's argument handling -- exactly the
		// asymmetry that let `args.x ?? ""` reach validation as an AST node in
		// the first place. No shipped logic currently returns a mutation leaf,
		// so this is not a live failure; it is the next instance of the class,
		// pre-empted rather than left for whoever writes that construct.
		folded, err := foldArgExpression(substituteArgRefValue(val, args), args)
		if err != nil {
			return nil, fmt.Errorf("argument %q of mutation-leaf call %q: %w", k, call.Name, err)
		}
		newArgs[k] = folded
	}
	if len(newArgs) == 1 {
		if inner, ok := newArgs["0"].(map[string]any); ok {
			newArgs = inner
		}
	}
	return &FunctionCallExpression{Name: call.Name, Args: newArgs}, nil
}

// foldArgExpression evaluates an argument that is still an engine expression
// node down to a concrete value, so it reaches argument validation (and then a
// Go executor or SQL builder) as the value the author wrote rather than as the
// AST of that value. memql#2870.
//
// It routes through evalCollScalar rather than growing a fourth evaluator.
// There are already three -- MutationExecutor.evaluateValue for the string step
// path, evalCollScalar for engine AST, and a scatter of one-off special cases
// bolted onto this file (IsDateBuiltin, cond, and the *ast.ArgRefExpr case in
// substituteArgRefValue added for the #1840 outage). Adding a fifth is how that
// scatter got here.
//
// DELIBERATELY NARROW. Only node kinds that are self-contained VALUE
// expressions are folded. A sub-call to a user function or a builtin is left
// alone for expandFunctionCall to expand, because folding it here would need
// the whole function registry and would change what "an argument" means. An
// unrecognised node is returned untouched, so any behaviour this does not
// explicitly claim is exactly what it was before.
func foldArgExpression(v any, args map[string]any) (any, error) {
	node, ok := v.(ExpressionNode)
	if !ok {
		return v, nil // already a concrete value
	}
	if !foldableArgExpression(node) {
		return v, nil
	}
	folded, err := evalCollScalar(node, args, nil)
	if err != nil {
		return nil, err
	}
	return folded, nil
}

// foldableArgExpression reports whether node is a self-contained value
// expression evalCollScalar can evaluate without a function registry.
func foldableArgExpression(node ExpressionNode) bool {
	switch n := node.(type) {
	case *FunctionCallExpression:
		// The converter-normalised positional builtins, and only those: a call
		// to anything else is a construct invocation that must be expanded, not
		// evaluated.
		if n == nil {
			return false
		}
		return n.Name == "coalesce" || n.Name == "concat" || n.Name == "cond" || IsDateBuiltin(n.Name)
	case *LiteralValueNode:
		// Covers bare `now`, which the converter wraps as a literal carrying an
		// *ast.TimestampExprFunc (ast_converter.go). resolveLiteralValue is what
		// turns it into a timestamp.
		return true
	case *ArithmeticExpression, *ComparisonExpression, *BinaryComparisonExpression, *LogicalExpression:
		return true
	default:
		return false
	}
}

// substituteArgRefValue walks a value (which may be a nested map /
// slice produced by parsing object-literal call args) and replaces
// *ArgReference leaves with values pulled from args. Non-ref values
// pass through unchanged.
func substituteArgRefValue(v any, args map[string]any) any {
	switch t := v.(type) {
	case *ArgReference:
		if t == nil {
			return nil
		}
		if val, ok := getNestedValue(args, t.Path); ok {
			return val
		}
		return nil
	case *ast.ArgRefExpr:
		// The PARSER arg-ref node. A nested call inside a Logic body
		// (`recordRequestEvent({ note: args.note })`) reaches the F.6
		// hoist with its object-literal arg values still carried as raw parser
		// nodes -- convertFunctionCallExpr shallow-copies a FunctionCallExpr's
		// Args verbatim, so the inner `args.note` is never converted to the
		// engine-side *ArgReference. Without this case the node passed straight
		// through to the mutation validator as `*ast.ArgRefExpr`, which is the
		// #1840 forge approval-pipeline outage ("expected string, got
		// *ast.ArgRefExpr" across routing / audit / mentoring / attach). Resolve
		// it the same way as *ArgReference -- the parser already stripped the
		// `args.` prefix, so Path keys directly into the caller args map.
		if t == nil {
			return nil
		}
		if val, ok := getNestedValue(args, t.Path); ok {
			return val
		}
		return nil
	case *ArgRefExpression:
		// The engine-side arg-ref node (convertArgRefExpr's output). Same
		// resolution as the parser node above; handled here so any path that
		// produces the converted form is covered symmetrically.
		if t == nil {
			return nil
		}
		if val, ok := getNestedValue(args, t.Path); ok {
			return val
		}
		return nil
	case *ComparisonExpression:
		// A comparison PREDICATE over an arg (memql#2962).
		//
		// `cond(args.role == "owner", ...)` parses to this node with
		// Field = FieldReference{Raw: "args.role"} -- a field reference, not
		// an *ArgRefExpression -- so none of the arg-ref cases above fire and
		// the default below used to return it untouched. The evaluator then
		// resolved "args.role" against the lambda scope, found nothing, and
		// compared nil: the cond took the else branch for EVERY input,
		// silently, and any gate written that way was open or closed by
		// accident rather than by its condition.
		//
		// Resolving it HERE rather than at evaluation time is what makes the
		// Execute path work. Execute substitutes args during expansion and
		// then evaluates the plan root with no args map at all, so a fix that
		// only taught the evaluator to consult args would work through the
		// seam and still fail end to end -- which is exactly what the first
		// cut of this fix did, and what the Execute-level test in
		// cond_comparison_predicate_2962_test.go caught.
		//
		// Only an `args.`-rooted field is rewritten. Any other field
		// reference is a row/lambda reference that must stay lazy, so it falls
		// through untouched and this cannot change how an existing comparison
		// resolves.
		if t == nil {
			return nil
		}
		parts := t.Field.Parts
		if len(parts) == 0 {
			parts = strings.Split(t.Field.Raw, ".")
		}
		if len(parts) < 2 || parts[0] != "args" {
			return t
		}
		resolved, ok := getNestedValue(args, strings.Join(parts[1:], "."))
		if !ok {
			return t
		}
		// Expression-led form, so both sides are plain operands the
		// evaluator compares with the same runtimeCompareValues the
		// field-led path uses. The RHS is recursively substituted so
		// `args.a == args.b` resolves on both sides.
		return &BinaryComparisonExpression{
			Left:     &LiteralValueNode{Value: resolved},
			Operator: t.Operator,
			Right:    asExpressionOperand(substituteArgRefValue(t.Value, args)),
		}
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = substituteArgRefValue(val, args)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = substituteArgRefValue(val, args)
		}
		return out
	default:
		return v
	}
}

// asExpressionOperand wraps a substituted comparison operand so the
// expression-led evaluator can take it. A value that is already an
// ExpressionNode is used as-is; anything else is a resolved Go value and
// becomes a literal. Companion to the *ComparisonExpression case above
// (memql#2962).
func asExpressionOperand(v any) ExpressionNode {
	if node, ok := v.(ExpressionNode); ok {
		return node
	}
	return &LiteralValueNode{Value: v}
}

// getNestedValue retrieves a value from a nested map using dot-separated path.
// Returns the value and a boolean indicating if the path exists.
func getNestedValue(data map[string]any, path string) (any, bool) {
	if data == nil || path == "" {
		return nil, false
	}

	parts := strings.Split(path, ".")
	current := any(data)

	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		val, exists := m[part]
		if !exists {
			return nil, false
		}
		current = val
	}

	return current, true
}

// expandExpression recursively expands function calls in an expression tree.
func (v *functionValidator) expandExpression(expr ExpressionNode) (ExpressionNode, error) {
	return v.expandExpressionWithArgs(expr, nil)
}

// expandExpressionWithArgs recursively expands function calls, with optional args context
// for substituting arg() references.
func (v *functionValidator) expandExpressionWithArgs(expr ExpressionNode, args map[string]any) (ExpressionNode, error) {
	if expr == nil {
		return nil, nil
	}

	switch node := expr.(type) {
	case *FunctionCallExpression:
		// Substitute ArgRef nodes in the call's args before resolving
		// the target. Without this step, a Logic body like
		// `return someBuiltin({userId: args.event.payload.userId})`
		// reaches the builtin executor with `args["userId"] =
		// *ArgReference{Path: "event.payload.userId"}` instead of the
		// resolved string -- and the executor's `asString(...)` returns
		// empty. The Logic-call-mutation path already does this via
		// `substituteArgRefsAndCallArgs` (F.6); the builtin / nested-
		// call path needs the same treatment, here in the generic
		// recursive expansion. Surfaced via memql-cockpit#49 -- daily-
		// space provisioning fired but every call into
		// `integration.dailyspace.ensureForUser` saw an empty userId.
		if args != nil && len(node.Args) > 0 {
			substituted := make(map[string]any, len(node.Args))
			for k, val := range node.Args {
				substituted[k] = substituteArgRefValue(val, args)
			}
			node = &FunctionCallExpression{
				Name: node.Name,
				Args: substituted,
			}
		}
		// Date/duration builtins (#2541) have no DSL body to expand -- they
		// are evaluated in-memory at execution time (evalCollScalar / the
		// engine's plan-root branch), like the other converter-normalised
		// typed builtins. Without this short-circuit a logic terminal return
		// such as `return addDuration(args.start, "P1D")` would be looked up
		// as a user function and fail "function not found".
		if IsDateBuiltin(node.Name) {
			return node, nil
		}
		// cond(predicate, then, else) (#2542 item 2) also has no DSL body -- it
		// is evaluated in-memory (evalCollScalar / the plan-root branch). Its
		// positional operands may be collection chains whose `args.X` receiver
		// must fold to a concrete collection, so recurse through
		// expandExpressionWithArgs per operand (substituteArgRefValue above only
		// substitutes a top-level arg ref, not a chain receiver), then keep it a
		// cond call for the evaluator. Without this the call is looked up as a
		// user function and fails "function not found".
		//
		// coalesce / concat (#2870) are the same shape and belong in the same
		// branch: converter-normalised positional builtins with no DSL body. The
		// allowlist here named only cond, so `return args.gate.passed ?? false`
		// -- a coalesce in ROOT position rather than in an argument -- was looked
		// up as a user function and failed `function "coalesce" not found`.
		if node.Name == "cond" || node.Name == "coalesce" || node.Name == "concat" {
			expanded := make(map[string]any, len(node.Args))
			for k, val := range node.Args {
				if en, ok := val.(ExpressionNode); ok {
					ev, err := v.expandExpressionWithArgs(en, args)
					if err != nil {
						return nil, err
					}
					expanded[k] = ev
					continue
				}
				expanded[k] = val
			}
			return &FunctionCallExpression{Name: node.Name, Args: expanded}, nil
		}

		// memql#2870: substituting an arg REF is not the same as evaluating an
		// arg EXPRESSION, and only the former was happening above.
		// `args.event.node.id ?? ""` is a coalesce node, not an arg ref, so it
		// passed through untouched and reached validateFunctionArgs as an AST
		// node --
		//
		//	argument "userId": expected string, got *ast.CoalesceExpr
		//
		// -- which is the computer-use kill switch, the workbench release sweep
		// and the magic-link expiry sweep, all three of which had never run.
		// (`magicLinkExpirySweep` passes a bare `now`, which is what proves the
		// shape is "any expression in an argument", not a coalesce bug.)
		//
		// THIS MUST RUN AFTER THE SHORT-CIRCUITS ABOVE, NOT WITH THE
		// SUBSTITUTION. Folding in the substitution block reached the operands
		// of cond / coalesce / concat / the date builtins as well, and those
		// keep their operands as expression nodes ON PURPOSE -- their own
		// evaluators need them. It broke cond outright (`cond() arg 0 is not an
		// expression (bool)`) and would have evaluated cond's untaken branch.
		// Only a call heading for expandFunctionCall -- a real construct
		// invocation, whose arguments must be VALUES by the time
		// validateFunctionArgs sees them -- gets folded.
		if args != nil && len(node.Args) > 0 {
			folded := make(map[string]any, len(node.Args))
			for k, val := range node.Args {
				fv, err := foldArgExpression(val, args)
				if err != nil {
					return nil, fmt.Errorf("argument %q of call %q: %w", k, node.Name, err)
				}
				folded[k] = fv
			}
			node = &FunctionCallExpression{Name: node.Name, Args: folded}
		}
		// Resolve, validate, and expand the function with its arguments
		return v.expandFunctionCall(node)

	case *LogicalExpression:
		left, err := v.expandExpressionWithArgs(node.Left, args)
		if err != nil {
			return nil, err
		}
		right, err := v.expandExpressionWithArgs(node.Right, args)
		if err != nil {
			return nil, err
		}
		// Handle nil results from conditional filter processing
		if left == nil && right == nil {
			return nil, nil
		}
		if left == nil {
			return right, nil
		}
		if right == nil {
			return left, nil
		}
		return &LogicalExpression{
			Op:    node.Op,
			Left:  left,
			Right: right,
		}, nil

	case *RelationshipExpression:
		target, err := v.expandExpressionWithArgs(node.Target, args)
		if err != nil {
			return nil, err
		}
		return &RelationshipExpression{
			Function: node.Function,
			Target:   target,
		}, nil

	case *SortExpression:
		target, err := v.expandExpressionWithArgs(node.Target, args)
		if err != nil {
			return nil, err
		}
		return &SortExpression{
			Target: target,
			Fields: node.Fields,
		}, nil

	case *PaginateExpression:
		target, err := v.expandExpressionWithArgs(node.Target, args)
		if err != nil {
			return nil, err
		}
		return &PaginateExpression{
			Target: target,
			Limit:  node.Limit,
		}, nil

	case *SelectExpression:
		target, err := v.expandExpressionWithArgs(node.Target, args)
		if err != nil {
			return nil, err
		}
		return &SelectExpression{
			Target: target,
			Fields: node.Fields,
		}, nil

	case *TimestampExpression:
		target, err := v.expandExpressionWithArgs(node.Target, args)
		if err != nil {
			return nil, err
		}
		// Resolve a caller-chosen instant HERE (memql#2992), which is the
		// first point where args are known. Everything downstream --
		// applyDirectiveWrappers, the SQL builder, the shadow analyzer -- then
		// sees only the two literal forms it already handled, so this feature
		// adds no case anywhere else.
		resolved, err := resolveAsOfArg(node, args)
		if err != nil {
			return nil, err
		}
		resolved.Target = target
		return resolved, nil

	case *DepthExpression:
		target, err := v.expandExpressionWithArgs(node.Target, args)
		if err != nil {
			return nil, err
		}
		return &DepthExpression{
			Target: target,
			Depth:  node.Depth,
		}, nil

	case *CountExpression:
		target, err := v.expandExpressionWithArgs(node.Target, args)
		if err != nil {
			return nil, err
		}
		return &CountExpression{
			Target: target,
		}, nil

	case *ShapeExpression:
		target, err := v.expandExpressionWithArgs(node.Target, args)
		if err != nil {
			return nil, err
		}
		return &ShapeExpression{
			Target:        target,
			Template:      node.Template,
			TemplateName:  node.TemplateName,
			IncludeBundle: node.IncludeBundle,
		}, nil

	case *BuiltinFunctionExpression:
		// Builtin functions are handled directly by the executor
		return &BuiltinFunctionExpression{
			Name:     node.Name,
			Executor: node.Executor,
			Args:     node.Args,
		}, nil

	case *CollectionMethodExpression:
		// Story 4 (#2302 / ADR §2.2): expand the receiver and each
		// argument with the caller args so an `args.X` receiver folds to
		// a concrete collection literal. Lambda parameter references are
		// untouched (they bind per-element at evaluation time).
		receiver, err := v.expandExpressionWithArgs(node.Receiver, args)
		if err != nil {
			return nil, err
		}
		newArgs := make([]ExpressionNode, len(node.Args))
		for i, a := range node.Args {
			ea, err := v.expandExpressionWithArgs(a, args)
			if err != nil {
				return nil, err
			}
			newArgs[i] = ea
		}
		return &CollectionMethodExpression{
			Receiver: receiver,
			Method:   node.Method,
			Args:     newArgs,
		}, nil

	case *LambdaExpression:
		// Expand the lambda body with caller args (substitutes any outer
		// `args.X` references); the param bindings resolve at evaluation
		// time, so bare element-field references pass through unchanged.
		body, err := v.expandExpressionWithArgs(node.Body, args)
		if err != nil {
			return nil, err
		}
		return &LambdaExpression{Params: append([]string(nil), node.Params...), Body: body}, nil

	case *DotAccessExpression:
		// #2542 item 4: fold an `args.X` object into a concrete literal so a
		// `return args.rows.first().createdAt` body evaluates over the
		// caller's collection; the field segment is static.
		object, err := v.expandExpressionWithArgs(node.Object, args)
		if err != nil {
			return nil, err
		}
		return &DotAccessExpression{Object: object, Field: node.Field}, nil

	case *ArithmeticExpression:
		// #2316: fold any `args.X` operand into a concrete literal (lambda
		// param references pass through unchanged for per-element binding).
		left, err := v.expandExpressionWithArgs(node.Left, args)
		if err != nil {
			return nil, err
		}
		right, err := v.expandExpressionWithArgs(node.Right, args)
		if err != nil {
			return nil, err
		}
		return &ArithmeticExpression{Op: node.Op, Left: left, Right: right}, nil

	case *BinaryComparisonExpression:
		// #2542 item 5 residual: fold any `args.X` operand of an
		// expression-led comparison into a concrete literal (lambda param
		// references pass through unchanged for per-element binding), exactly
		// like ArithmeticExpression above.
		left, err := v.expandExpressionWithArgs(node.Left, args)
		if err != nil {
			return nil, err
		}
		right, err := v.expandExpressionWithArgs(node.Right, args)
		if err != nil {
			return nil, err
		}
		return &BinaryComparisonExpression{Left: left, Operator: node.Operator, Right: right}, nil

	case *ConditionalFilterExpression:
		// Process conditional filter: check if the arg is present AND
		// non-nil. A present-but-nil value is treated as absent: tool
		// handlers materialize an omitted optional arg as an explicit
		// `null` (e.g. todosList's `todos({done: $args.done})`
		// collapses the unfilled placeholder to `null`), so a plain
		// `exists` check would let nil leak into the guarded predicate
		// (`payload.done == args.done`) and fail the query with
		// "unsupported literal type <nil>". Dropping on nil keeps the
		// `when(args.x) { ... }` guard's "absent -> drop the predicate"
		// semantics intact for that path. #1631.
		if args != nil {
			value, exists := getNestedValue(args, node.ArgPath)
			if !exists || value == nil {
				// Argument not provided (or explicitly nil) - skip this
				// filter entirely. Return nil to indicate this node
				// should be removed.
				return nil, nil
			}
		}
		// Argument exists (or no args context) - expand the underlying filter
		filter, err := v.expandExpressionWithArgs(node.Filter, args)
		if err != nil {
			return nil, err
		}
		// Return the expanded filter directly (unwrap the conditional)
		return filter, nil

	case *ArgRefExpression:
		// Substitute arg reference with actual value if we have args context
		if args != nil {
			value, exists := getNestedValue(args, node.Path)
			if !exists {
				return nil, fmt.Errorf("required argument %q not provided", node.Path)
			}
			// Return the value directly - it will be used as the comparison value
			return &LiteralValueNode{Value: value}, nil
		}
		// No args context - keep as ArgRefExpression (will be resolved later)
		return &ArgRefExpression{Path: node.Path}, nil

	case *ComparisonExpression:
		// Check if the comparison value is an ArgRefExpression that needs substitution
		if argRef, ok := node.Value.(*ArgReference); ok && args != nil {
			value, exists := getNestedValue(args, argRef.Path)
			if !exists {
				return nil, fmt.Errorf("required argument %q not provided", argRef.Path)
			}
			// Preserve CacheHintSeconds and FieldSelections alongside the
			// substituted value -- same-shape failure to the ShapeExpression
			// TemplateName drop, just rarer because cache hints / field
			// selections don't currently appear in argumented comparisons
			// in the shipped .memql files.
			clone := cloneExpressionNode(expr).(*ComparisonExpression)
			clone.Value = value
			return clone, nil
		}
		// canonicalId(args.X, concept) on a comparison RHS (#1360):
		// the loaded filter tree carries a typed *ast.CanonicalIdExpr
		// whose inner value is still the parser arg reference
		// (`*ast.ArgRefExpr`) -- call-arg binding never reached inside
		// the canonicalId wrapper, so the #1109 runtime pre-walk
		// (resolveCanonicalIdComparisons, which only resolves
		// *ast.LiteralExpr inners) left the node alone and the literal
		// evaluator failed the whole query with "unsupported literal
		// type *ast.CanonicalIdExpr". Substitute the bound arg value
		// into a NEW CanonicalIdExpr with a literal inner here, at the
		// same altitude where plain `args.X` comparison values get
		// substituted; the runtime pre-walk then canonicalizes it like
		// any other literal inner. Building a fresh node (never
		// mutating cid in place) preserves the cached-plan invariant:
		// cloneExpressionNode shallow-copies ComparisonExpression.Value,
		// so cid is shared with the registered function's stored tree.
		// `actor.`-prefixed paths are not caller args -- pass through
		// to the existing downstream error path.
		if cid, ok := node.Value.(*ast.CanonicalIdExpr); ok && cid != nil && args != nil {
			if inner, ok := cid.Value.(*ast.ArgRefExpr); ok && inner != nil && !strings.HasPrefix(inner.Path, "actor.") {
				value, exists := getNestedValue(args, inner.Path)
				if !exists {
					return nil, fmt.Errorf("required argument %q not provided", inner.Path)
				}
				clone := cloneExpressionNode(expr).(*ComparisonExpression)
				clone.Value = &ast.CanonicalIdExpr{
					Value:   &ast.LiteralExpr{Value: value},
					Concept: cid.Concept,
				}
				return clone, nil
			}
		}
		// Clone the comparison as-is
		return cloneExpressionNode(expr), nil

	default:
		// For other expression types (SpecReferenceExpression, etc.),
		// just clone and return
		return cloneExpressionNode(expr), nil
	}
}

// LiteralValueNode represents a literal value in the expression tree.
// Used when substituting arg() references with actual values.
type LiteralValueNode struct {
	Value any
}

func (*LiteralValueNode) isExpressionNode() {}

// resolvePlanFunctions resolves all function calls in a query plan.
// This should be called after spec resolution but before execution.
func resolvePlanFunctions(plan *QueryPlan, functions *FunctionRegistry, specs *SpecRegistry) error {
	return resolvePlanFunctionsWithOrigin(plan, functions, specs, auth.OriginClient)
}

// resolvePlanFunctionsWithOrigin is resolvePlanFunctions with the call origin
// made explicit (memql#2800). The no-origin wrapper defaults to OriginClient
// so any caller that has not been taught about origin gets the restricted
// treatment.
func resolvePlanFunctionsWithOrigin(plan *QueryPlan, functions *FunctionRegistry, specs *SpecRegistry, origin auth.CallOrigin) error {
	if plan == nil || plan.Root == nil {
		return nil
	}

	var snapshot map[string]*Function
	if functions != nil {
		snapshot = functions.Snapshot()
	}
	validator := newFunctionValidatorWithOrigin(snapshot, specs, origin)

	// Special case: top-level mutation function call.
	// We don't expand it to an expression; Execute will evaluate it into an insert.
	if call, ok := plan.Root.(*FunctionCallExpression); ok && call != nil {
		if functions != nil {
			if fn, err := functions.Get(call.Name); err == nil && fn != nil && strings.EqualFold(strings.TrimSpace(fn.FunctionKind), "mutation") {
				plan.MutationCall = call
				plan.Root = nil
				return nil
			}
		}
	}

	// F.5 -- Multi-step Logic dispatch: when a top-level call is a
	// Logic function whose body has intermediate `name := <call>` steps
	// before the `_return` terminator, hoist it to plan.LogicCall so
	// the engine dispatches it through the wired LogicRunner. The
	// runner walks the steps via the automation step registry,
	// binding each result for later steps + the `_return` expression.
	if call, ok := plan.Root.(*FunctionCallExpression); ok && call != nil && functions != nil {
		if fn, err := functions.Get(call.Name); err == nil && fn != nil && strings.EqualFold(strings.TrimSpace(fn.FunctionKind), "logic") && fn.LogicSteps != nil {
			plan.LogicCall = call
			plan.Root = nil
			return nil
		}
	}

	// F.6 -- Logic-calls-mutation dispatch: when a Logic function's
	// `return <expr>` body is a top-level mutation call, the resolved
	// expression is a FunctionCallExpression whose target is a
	// mutation. expandFunctionCall would normally reject that with
	// "function X is a mutation and cannot be used inside query
	// expressions". Instead, treat it the same as a direct top-level
	// mutation call: hoist it to plan.MutationCall so Execute dispatches
	// through executeMutationFunctionCall.
	if call, ok := plan.Root.(*FunctionCallExpression); ok && call != nil && functions != nil {
		if expanded, err := validator.expandFunctionCallAllowMutationLeaf(call); err == nil && expanded != nil {
			if inner, isCall := expanded.(*FunctionCallExpression); isCall && inner != nil {
				if fn, err := functions.Get(inner.Name); err == nil && fn != nil && strings.EqualFold(strings.TrimSpace(fn.FunctionKind), "mutation") {
					plan.MutationCall = inner
					plan.Root = nil
					return nil
				}
			}
		}
	}

	resolved, err := validator.expandExpression(plan.Root)
	if err != nil {
		return err
	}
	plan.Root = resolved
	return nil
}

// hasFunctionCalls checks if an expression tree contains any FunctionCallExpression nodes.
// Note: BuiltinFunctionExpression nodes are not counted as they are handled directly.
func hasFunctionCalls(expr ExpressionNode) bool {
	if expr == nil {
		return false
	}

	switch node := expr.(type) {
	case *FunctionCallExpression:
		return true
	case *BuiltinFunctionExpression:
		// Builtin functions are handled directly, not via the registry
		return false
	case *LogicalExpression:
		return hasFunctionCalls(node.Left) || hasFunctionCalls(node.Right)
	case *RelationshipExpression:
		return hasFunctionCalls(node.Target)
	case *SortExpression:
		return hasFunctionCalls(node.Target)
	case *PaginateExpression:
		return hasFunctionCalls(node.Target)
	case *SelectExpression:
		return hasFunctionCalls(node.Target)
	case *TimestampExpression:
		return hasFunctionCalls(node.Target)
	case *DepthExpression:
		return hasFunctionCalls(node.Target)
	case *CountExpression:
		return hasFunctionCalls(node.Target)
	case *ShapeExpression:
		return hasFunctionCalls(node.Target)
	case *ConditionalFilterExpression:
		return hasFunctionCalls(node.Filter)
	default:
		return false
	}
}
