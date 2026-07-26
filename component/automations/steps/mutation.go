package steps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
)

// MutationExecutor executes MemQL mutation (insert) steps.
// Unlike the query executor, this properly evaluates all expressions
// in the payload before building the final JSON for the engine.
type MutationExecutor struct{}

// Execute runs a mutation step.
func (e *MutationExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
	}

	if step.Mutation == nil {
		result.Status = "failed"
		result.Error = "mutation configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("mutation configuration is required")
	}

	if stepCtx.Engine == nil {
		result.Status = "failed"
		result.Error = "MemQL engine not configured"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("MemQL engine not configured")
	}

	cfg := step.Mutation

	// Evaluate the concept (should be a literal, but support expressions)
	concept := cfg.Concept
	if strings.HasPrefix(concept, "$") {
		evaluated, err := stepCtx.Evaluator.EvaluateString(concept)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Sprintf("failed to evaluate concept: %v", err)
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result, fmt.Errorf("failed to evaluate concept: %w", err)
		}
		concept = evaluated
	}

	// Evaluate the ID if present (use evaluateValue to handle concat() and other functions)
	id := cfg.ID
	if id != "" {
		evaluated, err := e.evaluateValue(stepCtx.Evaluator, id)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Sprintf("failed to evaluate id: %v", err)
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result, fmt.Errorf("failed to evaluate id: %w", err)
		}
		id = fmt.Sprintf("%v", evaluated)
	}

	// Evaluate all expressions in the payload
	evaluatedPayload, err := e.evaluatePayload(stepCtx.Evaluator, cfg.Payload)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to evaluate payload: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("failed to evaluate payload: %w", err)
	}

	// Build the insert query with proper JSON payload
	query := e.buildInsertQuery(concept, id, evaluatedPayload, cfg.Parent, cfg.AliasOf)

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("executing mutation step",
			"step", step.ID,
			"concept", concept,
			"query", query,
		)
	}

	// Execute the mutation
	execResult, err := stepCtx.Engine.Execute(ctx, query)
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("mutation execution failed: %w", err)
	}

	// Extract result info
	result.Status = "success"
	result.Result = convertMutationResult(execResult)
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)

	// Extract item count
	itemCount := extractMutationItemCount(execResult)

	// Build result query for retrieving the inserted record
	resultQuery := buildMutationResultQuery(concept, execResult)

	result.Metadata = map[string]any{
		"concept":     concept,
		"itemCount":   itemCount,
		"resultQuery": resultQuery,
	}

	// Record step execution
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:       runId,
		StepId:      step.ID,
		StepType:    "mutation",
		Status:      result.Status,
		Query:       query,
		ResultQuery: resultQuery,
		ItemCount:   itemCount,
		Duration:    float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("mutation step completed",
			"step", step.ID,
			"concept", concept,
			"resultQuery", resultQuery,
			"itemCount", itemCount,
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}

// evaluatePayload recursively evaluates all expressions in the payload.
func (e *MutationExecutor) evaluatePayload(evaluator *automations.Evaluator, payload map[string]any) (map[string]any, error) {
	if payload == nil {
		return nil, nil
	}

	result := make(map[string]any)
	for key, value := range payload {
		evaluated, err := e.evaluateValue(evaluator, value)
		if err != nil {
			return nil, fmt.Errorf("evaluating %q: %w", key, err)
		}
		result[key] = evaluated
	}
	return result, nil
}

// stepMethodAccessors maps MemQL's method-style step accessors to their
// dotted-shorthand form that the engine evaluator's resolvePath understands.
// resolvePath navigates the dotted form (see evaluator.go); the arg-time
// resolver only recognizes the dotted form, so we normalize the call form
// here. Story 5 (ADR §2.2 / #2303) retired the capitalized aliases, so the
// canonical lowercase spellings are the only ones normalized (`.Ran()` is the
// lone capitalized survivor -- it has no lowercase pair).
//
// Without this, expressions like autoRole.first().payload.value end up
// failing LooksLikePath (which rejects parens) and fall through to
// EvaluateString -- which returns the raw string, defeating any wrapping
// coalesce() or cond() that depends on detecting nil to pick a fallback.
var stepMethodAccessors = map[string]string{
	".first()": ".first",
	".last()":  ".last",
	".empty()": ".empty",
	".count()": ".count",
	".nodes()": ".nodes",
	".Ran()":   ".Ran",
}

// normalizeStepMethodAccessors rewrites any occurrence of a method-style
// step accessor (e.g. ".first()") to its dotted-shorthand equivalent
// (e.g. ".first"), preserving the rest of the expression. Safe to call
// on any string -- if no method accessor is present, the string is
// returned unchanged.
func normalizeStepMethodAccessors(s string) string {
	for m, repl := range stepMethodAccessors {
		if strings.Contains(s, m) {
			s = strings.ReplaceAll(s, m, repl)
		}
	}
	return s
}

// evaluateValue evaluates a single value, handling nested structures.
func (e *MutationExecutor) evaluateValue(evaluator *automations.Evaluator, value any) (any, error) {
	switch v := value.(type) {
	case string:
		// Normalize step method accessors up-front: autoRole.first().payload.value
		// -> autoRole.first.payload.value. Downstream resolvers understand the
		// dotted-shorthand form; they do not understand the method form.
		v = normalizeStepMethodAccessors(v)
		// Check if it's an expression that needs evaluation
		if strings.HasPrefix(v, "$") {
			return evaluator.EvaluateValue(v)
		}
		// Handle quoted string literals - strip the outer quotes
		// This handles cases like "inProgress" from if() function results
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			return v[1 : len(v)-1], nil
		}
		// Handle timestamp() or now() function - returns current UTC time in ISO 8601 format
		if v == "timestamp()" || v == "now()" {
			return time.Now().UTC().Format(time.RFC3339), nil
		}
		// Handle function calls with chained property access.
		// e.g., first(getFirstStep).payload.targetData
		// This splits the expression into the function call and the property path,
		// evaluates the function call (which now ends with ")"), then resolves
		// the remaining path on the result object, preserving its Go type.
		if funcCall, chainedPath, ok := splitFuncCallAndPath(v); ok {
			result, err := e.evaluateValue(evaluator, funcCall)
			if err != nil {
				return nil, err
			}
			if result == nil {
				return nil, nil
			}
			return resolveChainedPath(result, chainedPath)
		}
		// Date/duration builtins (#2541 -- addDuration and daysBetween
		// since the #2707 retirement of the calendar builtins) evaluate
		// through the shared evaluator dispatch so a mutation/function
		// step arg computes the same value the logic-time path does (the
		// conformance matrix pins the two entry points together). A
		// non-date call name falls through to the handlers below
		// unchanged.
		if val, handled, err := evaluator.TryEvaluateDateBuiltin(v); handled || err != nil {
			return val, err
		}
		// Handle concat() function
		if strings.HasPrefix(v, "concat(") {
			return e.evaluateConcat(evaluator, v)
		}
		// Handle var() function
		if strings.HasPrefix(v, "var(") {
			return e.evaluateVar(evaluator, v)
		}
		// Handle cond() function - conditional expression
		if strings.HasPrefix(v, "cond(") {
			return e.evaluateCond(evaluator, v)
		}
		// Handle first() function - get first element
		if strings.HasPrefix(v, "first(") {
			return e.evaluateFirst(evaluator, v)
		}
		// Handle field() function - access nested field
		if strings.HasPrefix(v, "field(") {
			return e.evaluateField(evaluator, v)
		}
		// Handle lt() function - less than comparison
		if strings.HasPrefix(v, "lt(") {
			return e.evaluateLt(evaluator, v)
		}
		// Handle add() function - addition
		if strings.HasPrefix(v, "add(") {
			return e.evaluateAdd(evaluator, v)
		}
		// Handle sub() function - subtraction
		if strings.HasPrefix(v, "sub(") {
			return e.evaluateSub(evaluator, v)
		}
		// Handle index() function - array element access
		if strings.HasPrefix(v, "index(") {
			return e.evaluateIndex(evaluator, v)
		}
		// Handle append() function - append to array
		if strings.HasPrefix(v, "append(") {
			return e.evaluateAppend(evaluator, v)
		}
		// Handle coalesce() function - first non-null value
		if strings.HasPrefix(v, "coalesce(") {
			return e.evaluateCoalesce(evaluator, v)
		}
		// Handle hash() function - sha256 hex digest of the argument.
		// Mirrors the engine-side evalHash (component/memql/mutation_templates.go)
		// so ids like concat("user-", hash(event.payload.email)) resolve the
		// same way whether computed at arg-resolution or query-execute time.
		if strings.HasPrefix(v, "hash(") {
			return e.evaluateHash(evaluator, v)
		}
		// Handle canonicalId(value, "<conceptType>") - normalize an
		// id-shaped value to canonical `<partition>:<conceptType>:
		// <bareSlug>` form. Mirrors the engine-side
		// evalCanonicalId so ids like
		//   concat("participant-", hash(concat(canonicalId(ctx.partitionId, "v1:cognition:space"), ...)))
		// resolve identically at arg-resolution and query-execute time.
		// Without this branch, an automation that references a foreign
		// key whose form differs from the eventual stored canonical
		// form would derive a different participant id than the
		// browser-issued mutation that uses the same logical reference.
		if strings.HasPrefix(v, "canonicalId(") {
			return e.evaluateCanonicalId(evaluator, v)
		}
		// Handle len() function - array length
		if strings.HasPrefix(v, "len(") {
			return e.evaluateLen(evaluator, v)
		}
		// Handle mean() function - arithmetic mean
		if strings.HasPrefix(v, "mean(") {
			return e.evaluateMean(evaluator, v)
		}
		// Handle pluck() function - extract field from array of objects
		if strings.HasPrefix(v, "pluck(") {
			return e.evaluatePluck(evaluator, v)
		}
		// Handle whereEq() function - filter array by field equality
		if strings.HasPrefix(v, "whereEq(") {
			return e.evaluateWhereEq(evaluator, v)
		}
		// CRITICAL FIX: Handle object literal strings like "{key: value, ...}"
		// These appear when functions like append() receive objects as arguments
		if strings.HasPrefix(v, "{") && strings.HasSuffix(v, "}") {
			return e.parseAndEvaluateObjectLiteral(evaluator, v)
		}
		// Handle array literal strings like "[a, b, c]"
		if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
			return e.parseAndEvaluateArrayLiteral(evaluator, v)
		}
		// Strict bare-path resolution: only known step IDs or reserved "item.*"
		if automations.LooksLikePath(v) {
			resolved, ok, err := evaluator.ResolveStrictBarePath(v)
			if err != nil {
				return nil, err
			}
			if ok {
				return resolved, nil
			}
		}
		// Bare known-step identifier (no dot, no call): resolve to the step's
		// result. A selector builtin like `coalesce(getActiveGA, getFallbackGA)`
		// passes its step args here as bare identifiers; without this they fall
		// through to EvaluateString below and render as their OWN NAME (a literal
		// string), which is non-nil/non-empty -- so coalesce returns the string
		// "getActiveGA" and the chained `getGA.first().id` read finds no nodes,
		// silently defeating autoJoinAI -> ghost AI (memql#574). Guarded on a
		// real step id so non-step bare literals keep their prior behaviour.
		if isBareStepIdentifier(v) {
			if result, ok := evaluator.StepResultValue(v); ok {
				return result, nil
			}
			// G2/G5 (#2364/#2367): bare args-field values (incl. G3 puns)
			// resolve to the bound value; declared-but-absent -> nil so
			// coalesce defaults apply. Args-less automations unaffected.
			if result, ok := evaluator.ResolveBareArg(v); ok {
				return result, nil
			}
		} else if strings.Contains(v, ".") && !strings.ContainsAny(v, " \t(){}[]\"=<>!&|,") {
			// Dotted path whose head is an args field (`node.id`) -- same
			// tier, walked into the bound object. (#2367)
			if result, ok := evaluator.ResolveArgsPath(v); ok {
				return result, nil
			}
		}
		// Resolve embedded $ references (e.g. "lead-$event.payload.id").
		// This intentionally does not attempt to type-coerce; it returns a string.
		evaluated, err := evaluator.EvaluateString(v)
		if err != nil {
			return nil, err
		}
		return evaluated, nil
	case map[string]any:
		return e.evaluatePayload(evaluator, v)
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			evaluated, err := e.evaluateValue(evaluator, item)
			if err != nil {
				return nil, err
			}
			result[i] = evaluated
		}
		return result, nil
	case bool, int, int64, float64, nil:
		return v, nil
	default:
		// Try to convert to string and evaluate
		s := fmt.Sprintf("%v", v)
		if strings.HasPrefix(s, "$") {
			return evaluator.EvaluateValue(s)
		}
		if s == "timestamp()" || s == "now()" {
			return time.Now().UTC().Format(time.RFC3339), nil
		}
		if strings.HasPrefix(s, "concat(") {
			return e.evaluateConcat(evaluator, s)
		}
		if strings.HasPrefix(s, "var(") {
			return e.evaluateVar(evaluator, s)
		}
		if strings.HasPrefix(s, "cond(") {
			return e.evaluateCond(evaluator, s)
		}
		if strings.HasPrefix(s, "first(") {
			return e.evaluateFirst(evaluator, s)
		}
		if strings.HasPrefix(s, "field(") {
			return e.evaluateField(evaluator, s)
		}
		if strings.HasPrefix(s, "lt(") {
			return e.evaluateLt(evaluator, s)
		}
		if strings.HasPrefix(s, "add(") {
			return e.evaluateAdd(evaluator, s)
		}
		if strings.HasPrefix(s, "sub(") {
			return e.evaluateSub(evaluator, s)
		}
		if strings.HasPrefix(s, "index(") {
			return e.evaluateIndex(evaluator, s)
		}
		if strings.HasPrefix(s, "append(") {
			return e.evaluateAppend(evaluator, s)
		}
		if strings.HasPrefix(s, "coalesce(") {
			return e.evaluateCoalesce(evaluator, s)
		}
		if strings.HasPrefix(s, "len(") {
			return e.evaluateLen(evaluator, s)
		}
		if strings.HasPrefix(s, "mean(") {
			return e.evaluateMean(evaluator, s)
		}
		if strings.HasPrefix(s, "pluck(") {
			return e.evaluatePluck(evaluator, s)
		}
		if strings.HasPrefix(s, "whereEq(") {
			return e.evaluateWhereEq(evaluator, s)
		}
		return v, nil
	}
}

// evaluateConcat evaluates a concat() function call.
// Format: concat("literal", $expr, "literal", ...)
func (e *MutationExecutor) evaluateConcat(evaluator *automations.Evaluator, expr string) (string, error) {
	// Extract arguments from concat(...)
	if !strings.HasPrefix(expr, "concat(") || !strings.HasSuffix(expr, ")") {
		return expr, nil
	}

	argsStr := expr[7 : len(expr)-1] // Remove "concat(" and ")"
	args := e.splitConcatArgs(argsStr)

	var result strings.Builder
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}

		// String literal
		if strings.HasPrefix(arg, "\"") && strings.HasSuffix(arg, "\"") {
			result.WriteString(arg[1 : len(arg)-1])
			continue
		}

		// Evaluate using the same rules as mutation payload evaluation.
		// This supports timestamp(), var(), nested concat(), and strict bare step/item paths.
		evaluated, err := e.evaluateValue(evaluator, arg)
		if err != nil {
			return "", fmt.Errorf("evaluating concat arg %q: %w", arg, err)
		}
		result.WriteString(automations.FormatValue(evaluated))
	}

	return result.String(), nil
}

// splitConcatArgs splits concat arguments handling nested parentheses.
func (e *MutationExecutor) splitConcatArgs(s string) []string {
	var args []string
	var current strings.Builder
	depth := 0
	inString := false
	escaped := false

	for _, ch := range s {
		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}

		if ch == '\\' && inString {
			escaped = true
			current.WriteRune(ch)
			continue
		}

		if ch == '"' {
			inString = !inString
			current.WriteRune(ch)
			continue
		}

		if !inString {
			if ch == '(' {
				depth++
			} else if ch == ')' {
				depth--
			} else if ch == ',' && depth == 0 {
				args = append(args, current.String())
				current.Reset()
				continue
			}
		}

		current.WriteRune(ch)
	}

	if current.Len() > 0 {
		args = append(args, current.String())
	}

	return args
}

// evaluateVar evaluates a var() function call.
// Format: var("VARIABLE_NAME")
func (e *MutationExecutor) evaluateVar(evaluator *automations.Evaluator, expr string) (string, error) {
	// Extract variable name from var("...")
	if !strings.HasPrefix(expr, "var(") || !strings.HasSuffix(expr, ")") {
		return expr, nil
	}

	nameArg := strings.TrimSpace(expr[4 : len(expr)-1])

	// Remove quotes if present
	if strings.HasPrefix(nameArg, "\"") && strings.HasSuffix(nameArg, "\"") {
		nameArg = nameArg[1 : len(nameArg)-1]
	}

	// Try to resolve using $var.NAME
	resolved, err := evaluator.EvaluateValue("$var." + nameArg)
	if err != nil {
		return "", fmt.Errorf("resolving var(%s): %w", nameArg, err)
	}

	return automations.FormatValue(resolved), nil
}

// evaluateCond evaluates a cond() conditional-value expression.
// Format: cond(predicate, thenValue, elseValue)
func (e *MutationExecutor) evaluateCond(evaluator *automations.Evaluator, expr string) (any, error) {
	if !strings.HasPrefix(expr, "cond(") || !strings.HasSuffix(expr, ")") {
		return expr, nil
	}

	argsStr := expr[5 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 3 {
		return nil, fmt.Errorf("cond() requires exactly 3 arguments: cond(predicate, thenValue, elseValue), got %d", len(args))
	}

	// Evaluate the predicate. A COMPARISON predicate (`role == "admin"`,
	// `x != y`, connectives) must go through the full condition evaluator --
	// treating it as a value made every comparison-predicate a non-empty
	// LITERAL STRING, i.e. always truthy, silently picking the then-branch
	// (S9 #2407 / P1 #2368 fallout).
	condArg := strings.TrimSpace(args[0])
	if strings.Contains(condArg, "==") || strings.Contains(condArg, "!=") ||
		strings.Contains(condArg, "&&") || strings.Contains(condArg, "||") ||
		strings.Contains(condArg, "<") || strings.Contains(condArg, ">") {
		ok, cerr := evaluator.EvaluateCondition(condArg)
		if cerr != nil {
			return nil, fmt.Errorf("evaluating cond() predicate: %w", cerr)
		}
		branch := strings.TrimSpace(args[2])
		if ok {
			branch = strings.TrimSpace(args[1])
		}
		return e.evaluateValue(evaluator, branch)
	}
	condVal, err := e.evaluateValue(evaluator, condArg)
	if err != nil {
		return nil, fmt.Errorf("evaluating cond() predicate: %w", err)
	}

	// Determine truthiness
	isTruthy := false
	switch v := condVal.(type) {
	case bool:
		isTruthy = v
	case string:
		isTruthy = v != "" && v != "false" && v != "0"
	case int, int64, float64:
		isTruthy = v != 0
	case nil:
		isTruthy = false
	default:
		isTruthy = condVal != nil
	}

	// Evaluate the appropriate branch
	if isTruthy {
		thenArg := strings.TrimSpace(args[1])
		return e.evaluateValue(evaluator, thenArg)
	}
	elseArg := strings.TrimSpace(args[2])
	return e.evaluateValue(evaluator, elseArg)
}

// evaluateFirst evaluates a first() function to get the first element.
// Format: first(stepResult) where stepResult is a step reference
func (e *MutationExecutor) evaluateFirst(evaluator *automations.Evaluator, expr string) (any, error) {
	if !strings.HasPrefix(expr, "first(") || !strings.HasSuffix(expr, ")") {
		return expr, nil
	}

	argStr := strings.TrimSpace(expr[6 : len(expr)-1])

	// CRITICAL FIX: Check if argStr is a bare step name (no dots, no $, not a function call)
	// If so, resolve it directly from the evaluator's step results
	if !strings.Contains(argStr, ".") && !strings.HasPrefix(argStr, "$") && !strings.Contains(argStr, "(") {
		// This looks like a bare step ID (e.g., "getProgress")
		if nodes, ok := evaluator.GetStepNodes(argStr); ok {
			if len(nodes) > 0 {
				return nodes[0], nil
			}
			return nil, nil
		}
		// If step doesn't exist, fall through to normal evaluation
	}

	arg, err := e.evaluateValue(evaluator, argStr)
	if err != nil {
		return nil, fmt.Errorf("evaluating first() argument: %w", err)
	}

	// Handle different types
	switch v := arg.(type) {
	case []any:
		if len(v) > 0 {
			return v[0], nil
		}
		return nil, nil
	case []map[string]any:
		if len(v) > 0 {
			return v[0], nil
		}
		return nil, nil
	case map[string]any:
		// Try to extract Bundle.nodes or result.Bundle.nodes
		if bundle, ok := v["Bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok && len(nodes) > 0 {
				return nodes[0], nil
			}
		}
		if result, ok := v["result"].(map[string]any); ok {
			if bundle, ok := result["Bundle"].(map[string]any); ok {
				if nodes, ok := bundle["nodes"].([]any); ok && len(nodes) > 0 {
					return nodes[0], nil
				}
			}
		}
		return v, nil
	default:
		return arg, nil
	}
}

// evaluateField evaluates a field() function to access nested fields.
// Format: field(object, "path.to.field")
func (e *MutationExecutor) evaluateField(evaluator *automations.Evaluator, expr string) (any, error) {
	if !strings.HasPrefix(expr, "field(") || !strings.HasSuffix(expr, ")") {
		return expr, nil
	}

	argsStr := expr[6 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 2 {
		return nil, fmt.Errorf("field() requires exactly 2 arguments, got %d", len(args))
	}

	// Evaluate the object
	objArg := strings.TrimSpace(args[0])
	obj, err := e.evaluateValue(evaluator, objArg)
	if err != nil {
		return nil, fmt.Errorf("evaluating field() object: %w", err)
	}

	// Get the path
	pathArg := strings.TrimSpace(args[1])
	// Remove quotes if present
	if strings.HasPrefix(pathArg, "\"") && strings.HasSuffix(pathArg, "\"") {
		pathArg = pathArg[1 : len(pathArg)-1]
	}

	// Navigate the path
	return getNestedField(obj, pathArg), nil
}

// getNestedField extracts a nested field from an object using dot notation.
func getNestedField(obj any, path string) any {
	// Peel a Bundle-wrapped engine envelope so field(decide.result, "x") reads
	// the flat value a logic/query step returned, not the opaque envelope. The
	// resolvePath `.result` branch already unwraps for path-style reads; this
	// covers the field() route defensively. #2271.
	obj = automations.UnwrapStepResult(obj)
	if obj == nil || path == "" {
		return nil
	}

	parts := strings.Split(path, ".")
	current := obj

	for _, part := range parts {
		if current == nil {
			return nil
		}

		switch v := current.(type) {
		case map[string]any:
			current = v[part]
		default:
			return nil
		}
	}

	return current
}

// evaluateLt evaluates a lt() less-than comparison.
// Format: lt(a, b) - returns true if a < b
func (e *MutationExecutor) evaluateLt(evaluator *automations.Evaluator, expr string) (bool, error) {
	if !strings.HasPrefix(expr, "lt(") || !strings.HasSuffix(expr, ")") {
		return false, nil
	}

	argsStr := expr[3 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 2 {
		return false, fmt.Errorf("lt() requires exactly 2 arguments, got %d", len(args))
	}

	// Evaluate both arguments
	aVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[0]))
	if err != nil {
		return false, fmt.Errorf("evaluating lt() first argument: %w", err)
	}
	bVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[1]))
	if err != nil {
		return false, fmt.Errorf("evaluating lt() second argument: %w", err)
	}

	return compareValues(aVal, bVal) < 0, nil
}

// compareValues compares two values numerically or lexically.
// Returns -1 if a < b, 0 if a == b, 1 if a > b
func compareValues(a, b any) int {
	aNum := toNumber(a)
	bNum := toNumber(b)

	if aNum < bNum {
		return -1
	}
	if aNum > bNum {
		return 1
	}
	return 0
}

// toNumber converts a value to float64 for comparison.
func toNumber(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return f
		}
		return 0
	default:
		return 0
	}
}

// evaluateAdd evaluates an add() function for addition.
// Format: add(a, b)
func (e *MutationExecutor) evaluateAdd(evaluator *automations.Evaluator, expr string) (any, error) {
	if !strings.HasPrefix(expr, "add(") || !strings.HasSuffix(expr, ")") {
		return expr, nil
	}

	argsStr := expr[4 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 2 {
		return nil, fmt.Errorf("add() requires exactly 2 arguments, got %d", len(args))
	}

	aVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[0]))
	if err != nil {
		return nil, fmt.Errorf("evaluating add() first argument: %w", err)
	}
	bVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[1]))
	if err != nil {
		return nil, fmt.Errorf("evaluating add() second argument: %w", err)
	}

	// Try to return int if both are ints
	aInt, aIsInt := toInt(aVal)
	bInt, bIsInt := toInt(bVal)
	if aIsInt && bIsInt {
		return aInt + bInt, nil
	}

	return toNumber(aVal) + toNumber(bVal), nil
}

// toInt converts a value to int64 if possible.
func toInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if float64(int64(n)) == n {
			return int64(n), true
		}
		return 0, false
	default:
		return 0, false
	}
}

// evaluateSub evaluates a sub() function for subtraction.
// Format: sub(a, b)
func (e *MutationExecutor) evaluateSub(evaluator *automations.Evaluator, expr string) (any, error) {
	if !strings.HasPrefix(expr, "sub(") || !strings.HasSuffix(expr, ")") {
		return expr, nil
	}

	argsStr := expr[4 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 2 {
		return nil, fmt.Errorf("sub() requires exactly 2 arguments, got %d", len(args))
	}

	aVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[0]))
	if err != nil {
		return nil, fmt.Errorf("evaluating sub() first argument: %w", err)
	}
	bVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[1]))
	if err != nil {
		return nil, fmt.Errorf("evaluating sub() second argument: %w", err)
	}

	// Try to return int if both are ints
	aInt, aIsInt := toInt(aVal)
	bInt, bIsInt := toInt(bVal)
	if aIsInt && bIsInt {
		return aInt - bInt, nil
	}

	return toNumber(aVal) - toNumber(bVal), nil
}

// evaluateIndex evaluates an index() function for array access.
// Format: index(array, i)
func (e *MutationExecutor) evaluateIndex(evaluator *automations.Evaluator, expr string) (any, error) {
	if !strings.HasPrefix(expr, "index(") || !strings.HasSuffix(expr, ")") {
		return expr, nil
	}

	argsStr := expr[6 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 2 {
		return nil, fmt.Errorf("index() requires exactly 2 arguments, got %d", len(args))
	}

	arrVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[0]))
	if err != nil {
		return nil, fmt.Errorf("evaluating index() array argument: %w", err)
	}
	idxVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[1]))
	if err != nil {
		return nil, fmt.Errorf("evaluating index() index argument: %w", err)
	}

	idx := int(toNumber(idxVal))

	switch arr := arrVal.(type) {
	case []any:
		if idx >= 0 && idx < len(arr) {
			return arr[idx], nil
		}
		return nil, nil
	case []map[string]any:
		if idx >= 0 && idx < len(arr) {
			return arr[idx], nil
		}
		return nil, nil
	default:
		return nil, nil
	}
}

// evaluateAppend evaluates an append() function to append to an array.
// Format: append(array, item)
func (e *MutationExecutor) evaluateAppend(evaluator *automations.Evaluator, expr string) (any, error) {
	if !strings.HasPrefix(expr, "append(") || !strings.HasSuffix(expr, ")") {
		return expr, nil
	}

	argsStr := expr[7 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 2 {
		return nil, fmt.Errorf("append() requires exactly 2 arguments, got %d", len(args))
	}

	// Normalize the array argument the same way evaluateValue does internally
	// (`.First()` -> `.first`, etc.) so the unresolved-path guard below can
	// compare the resolved value against the form EvaluateString would echo.
	arrArg := normalizeStepMethodAccessors(strings.TrimSpace(args[0]))
	arrVal, err := e.evaluateValue(evaluator, arrArg)
	if err != nil {
		return nil, fmt.Errorf("evaluating append() array argument: %w", err)
	}
	itemVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[1]))
	if err != nil {
		return nil, fmt.Errorf("evaluating append() item argument: %w", err)
	}

	switch arr := arrVal.(type) {
	case []any:
		return append(arr, itemVal), nil
	case nil:
		return []any{itemVal}, nil
	case string:
		// #1848: the array argument did NOT resolve to an array -- it came back
		// as raw source text. This happens when the read-merge source path
		// (`existing.first.payload.attachmentIds`) points at a row field that is
		// absent/null: the compiler renders the unresolved member-access as a
		// quoted string literal, so evaluateValue strips the quotes and echoes
		// the path text verbatim. Appending to THAT literal is exactly the
		// corrupted `["existing.first.payload.attachmentIds", "att-..."]` write
		// reported in #1848 (same "raw source must never reach a mutation" class
		// as #574/#580). When the leftover string is path-shaped (a leaked
		// expression, not real user data) treat the array as absent and start
		// fresh with the item only.
		if automations.LooksLikePath(arr) {
			return []any{itemVal}, nil
		}
		// A genuine scalar string value: preserve the historical 2-element shape.
		return []any{arrVal, itemVal}, nil
	default:
		// If it's not an array, create a new array with both elements
		return []any{arrVal, itemVal}, nil
	}
}

// evaluateCoalesce evaluates a coalesce() function to return the first non-null value.
// Format: coalesce(a, b, ...)
func (e *MutationExecutor) evaluateCoalesce(evaluator *automations.Evaluator, expr string) (any, error) {
	if !strings.HasPrefix(expr, "coalesce(") || !strings.HasSuffix(expr, ")") {
		return expr, nil
	}

	argsStr := expr[9 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)

	for i, arg := range args {
		val, err := e.evaluateValue(evaluator, strings.TrimSpace(arg))
		if err != nil {
			continue // Skip errors, try next
		}
		// #1614: the FINAL arg is the ultimate fallback -- return it even
		// when it resolves to "" (so coalesce(absent, "") -> "" reaches a
		// string field; the #1605 artifact-index fallback). A NON-final arg
		// that resolves to nil or "" is treated as missing and skipped, so
		// coalesce(emptyUserId, subject) keeps falling through to subject
		// and the "builtin arg resolved to nil" WARN clears.
		if i == len(args)-1 {
			return val, nil
		}
		if val != nil {
			if s, ok := val.(string); ok && s == "" {
				continue
			}
			return val, nil
		}
	}

	return nil, nil
}

// evaluateCanonicalId evaluates canonicalId(value, "<conceptType>")
// against the engine's id-canonicalization helper. Falls back to the
// raw value when no resolver is wired (engine not available, or
// running standalone tests without the engine boot path).
//
// Mirrors the engine-side canonicalizeIdValue contract: bare slugs get
// the concept's partition prefix prepended; canonical ids with a
// matching concept return as-is; canonical ids with a mismatching
// concept are an error (caller has the wrong type tag).
func (e *MutationExecutor) evaluateCanonicalId(evaluator *automations.Evaluator, expr string) (string, error) {
	if !strings.HasPrefix(expr, "canonicalId(") || !strings.HasSuffix(expr, ")") {
		return "", fmt.Errorf("invalid canonicalId() expression: %s", expr)
	}
	argsStr := expr[len("canonicalId(") : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 2 {
		return "", fmt.Errorf("canonicalId() requires exactly 2 arguments (value, \"<conceptType>\"), got %d", len(args))
	}
	val, err := e.evaluateValue(evaluator, strings.TrimSpace(args[0]))
	if err != nil {
		return "", fmt.Errorf("evaluating canonicalId() value argument: %w", err)
	}
	rawConcept := strings.TrimSpace(args[1])
	if !(strings.HasPrefix(rawConcept, `"`) && strings.HasSuffix(rawConcept, `"`) && len(rawConcept) >= 2) {
		return "", fmt.Errorf("canonicalId(): second argument must be a quoted concept name (e.g. \"v1:identity:user\")")
	}
	conceptType := rawConcept[1 : len(rawConcept)-1]
	if strings.TrimSpace(conceptType) == "" {
		return "", fmt.Errorf("canonicalId(): concept name is empty")
	}
	var input string
	if val == nil {
		return "", nil
	}
	input = fmt.Sprintf("%v", val)
	if strings.TrimSpace(input) == "" {
		return "", nil
	}
	resolver := evaluator.CanonicalIdResolver()
	if resolver == nil {
		// No engine wiring: degrade to identity. Logged so the operator
		// can spot misconfigured environments where automation-derived
		// ids might silently diverge from mutation-derived ones.
		return input, nil
	}
	return resolver(context.Background(), input, conceptType)
}

// evaluateHash evaluates a hash(x) expression to a sha256 hex digest,
// matching the engine-side implementation in component/memql/mutation_templates.go
// so an id built via concat("user-", hash(event.payload.email)) resolves the
// same whether computed at arg-resolution time (here) or query-execute time.
//
// Accepts a single argument which is evaluated (to resolve $refs and nested
// builtins) before hashing its string form.
func (e *MutationExecutor) evaluateHash(evaluator *automations.Evaluator, expr string) (string, error) {
	if !strings.HasPrefix(expr, "hash(") || !strings.HasSuffix(expr, ")") {
		return "", fmt.Errorf("invalid hash() expression: %s", expr)
	}

	argsStr := expr[5 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 1 {
		return "", fmt.Errorf("hash() requires exactly 1 argument, got %d", len(args))
	}

	val, err := e.evaluateValue(evaluator, strings.TrimSpace(args[0]))
	if err != nil {
		return "", fmt.Errorf("evaluating hash() argument: %w", err)
	}

	var input string
	if val == nil {
		input = ""
	} else {
		input = fmt.Sprintf("%v", val)
	}
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:]), nil
}

// evaluateLen evaluates a len() function to get array length.
// Format: len(array)
func (e *MutationExecutor) evaluateLen(evaluator *automations.Evaluator, expr string) (int, error) {
	if !strings.HasPrefix(expr, "len(") || !strings.HasSuffix(expr, ")") {
		return 0, fmt.Errorf("invalid len() expression: %s", expr)
	}

	argsStr := expr[4 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 1 {
		return 0, fmt.Errorf("len() requires exactly 1 argument, got %d", len(args))
	}

	arrVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[0]))
	if err != nil {
		return 0, fmt.Errorf("evaluating len() argument: %w", err)
	}

	switch arr := arrVal.(type) {
	case []any:
		return len(arr), nil
	case []map[string]any:
		return len(arr), nil
	case nil:
		return 0, nil
	default:
		return 0, fmt.Errorf("len() requires an array, got %T", arrVal)
	}
}

// evaluateMean evaluates a mean() function to calculate arithmetic mean.
// Format: mean(array)
func (e *MutationExecutor) evaluateMean(evaluator *automations.Evaluator, expr string) (float64, error) {
	if !strings.HasPrefix(expr, "mean(") || !strings.HasSuffix(expr, ")") {
		return 0, fmt.Errorf("invalid mean() expression: %s", expr)
	}

	argsStr := expr[5 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 1 {
		return 0, fmt.Errorf("mean() requires exactly 1 argument, got %d", len(args))
	}

	arrVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[0]))
	if err != nil {
		return 0, fmt.Errorf("evaluating mean() argument: %w", err)
	}

	var values []float64
	switch arr := arrVal.(type) {
	case []any:
		for _, v := range arr {
			values = append(values, toNumber(v))
		}
	case []float64:
		values = arr
	case nil:
		return 0, nil
	default:
		return 0, fmt.Errorf("mean() requires an array, got %T", arrVal)
	}

	if len(values) == 0 {
		return 0, nil
	}

	var sum float64
	for _, v := range values {
		sum += v
	}

	return sum / float64(len(values)), nil
}

// evaluatePluck evaluates a pluck() function to extract a field from each object in an array.
// Format: pluck(array, "fieldName")
func (e *MutationExecutor) evaluatePluck(evaluator *automations.Evaluator, expr string) ([]any, error) {
	if !strings.HasPrefix(expr, "pluck(") || !strings.HasSuffix(expr, ")") {
		return nil, fmt.Errorf("invalid pluck() expression: %s", expr)
	}

	argsStr := expr[6 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 2 {
		return nil, fmt.Errorf("pluck() requires exactly 2 arguments, got %d", len(args))
	}

	arrVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[0]))
	if err != nil {
		return nil, fmt.Errorf("evaluating pluck() array argument: %w", err)
	}

	// Get the field name
	fieldArg := strings.TrimSpace(args[1])
	// Remove quotes if present
	if strings.HasPrefix(fieldArg, "\"") && strings.HasSuffix(fieldArg, "\"") {
		fieldArg = fieldArg[1 : len(fieldArg)-1]
	}

	var result []any

	switch arr := arrVal.(type) {
	case []any:
		for _, item := range arr {
			if obj, ok := item.(map[string]any); ok {
				result = append(result, getNestedField(obj, fieldArg))
			} else {
				result = append(result, nil)
			}
		}
	case []map[string]any:
		for _, obj := range arr {
			result = append(result, getNestedField(obj, fieldArg))
		}
	case nil:
		return []any{}, nil
	default:
		return nil, fmt.Errorf("pluck() requires an array, got %T", arrVal)
	}

	return result, nil
}

// evaluateWhereEq evaluates a whereEq() function to filter an array by field equality.
// Format: whereEq(array, "fieldName", value)
func (e *MutationExecutor) evaluateWhereEq(evaluator *automations.Evaluator, expr string) ([]any, error) {
	if !strings.HasPrefix(expr, "whereEq(") || !strings.HasSuffix(expr, ")") {
		return nil, fmt.Errorf("invalid whereEq() expression: %s", expr)
	}

	argsStr := expr[8 : len(expr)-1]
	args := e.splitFunctionArgs(argsStr)
	if len(args) != 3 {
		return nil, fmt.Errorf("whereEq() requires exactly 3 arguments, got %d", len(args))
	}

	arrVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[0]))
	if err != nil {
		return nil, fmt.Errorf("evaluating whereEq() array argument: %w", err)
	}

	// Get the field name
	fieldArg := strings.TrimSpace(args[1])
	// Remove quotes if present
	if strings.HasPrefix(fieldArg, "\"") && strings.HasSuffix(fieldArg, "\"") {
		fieldArg = fieldArg[1 : len(fieldArg)-1]
	}

	// Get the comparison value
	compareVal, err := e.evaluateValue(evaluator, strings.TrimSpace(args[2]))
	if err != nil {
		return nil, fmt.Errorf("evaluating whereEq() value argument: %w", err)
	}

	var result []any

	switch arr := arrVal.(type) {
	case []any:
		for _, item := range arr {
			if obj, ok := item.(map[string]any); ok {
				fieldVal := getNestedField(obj, fieldArg)
				if valuesEqual(fieldVal, compareVal) {
					result = append(result, item)
				}
			}
		}
	case []map[string]any:
		for _, obj := range arr {
			fieldVal := getNestedField(obj, fieldArg)
			if valuesEqual(fieldVal, compareVal) {
				result = append(result, obj)
			}
		}
	case nil:
		return []any{}, nil
	default:
		return nil, fmt.Errorf("whereEq() requires an array, got %T", arrVal)
	}

	return result, nil
}

// valuesEqual checks if two values are equal for whereEq filtering.
func valuesEqual(a, b any) bool {
	// Handle nil cases
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	// Try string comparison first (most common)
	aStr := fmt.Sprintf("%v", a)
	bStr := fmt.Sprintf("%v", b)
	if aStr == bStr {
		return true
	}

	// Try numeric comparison
	aNum := toNumber(a)
	bNum := toNumber(b)
	return aNum == bNum && aNum != 0
}

// parseAndEvaluateObjectLiteral parses an object literal string like "{key: value, ...}"
// and evaluates all values recursively. This handles objects embedded in function arguments.
func (e *MutationExecutor) parseAndEvaluateObjectLiteral(evaluator *automations.Evaluator, s string) (map[string]any, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("invalid object literal: %s", s)
	}

	// Remove outer braces
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return map[string]any{}, nil
	}

	result := make(map[string]any)

	// Parse key-value pairs
	pos := 0
	for pos < len(inner) {
		// Skip whitespace
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r') {
			pos++
		}
		if pos >= len(inner) {
			break
		}

		// Parse key
		keyStart := pos
		for pos < len(inner) && inner[pos] != ':' && inner[pos] != ' ' && inner[pos] != '\t' {
			pos++
		}
		key := strings.TrimSpace(inner[keyStart:pos])

		// Skip to colon
		for pos < len(inner) && inner[pos] != ':' {
			pos++
		}
		if pos >= len(inner) {
			break
		}
		pos++ // skip colon

		// Skip whitespace after colon
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r') {
			pos++
		}

		// Parse value
		// No progress check here: this loop advances past a `:` before every
		// value scan, so it cannot spin, and parseValueFromString never
		// returns a position below its input. The array loop below is the one
		// that needs the guard (memql#2785).
		valueStr, newPos := e.parseValueFromString(inner, pos)
		pos = newPos

		// Evaluate the value
		evaluated, err := e.evaluateValue(evaluator, valueStr)
		if err != nil {
			return nil, fmt.Errorf("evaluating key %q: %w", key, err)
		}
		result[key] = evaluated

		// Skip comma if present
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r' || inner[pos] == ',') {
			pos++
		}
	}

	return result, nil
}

// parseAndEvaluateArrayLiteral parses an array literal string like "[a, b, c]"
// and evaluates all values recursively.
func (e *MutationExecutor) parseAndEvaluateArrayLiteral(evaluator *automations.Evaluator, s string) ([]any, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("invalid array literal: %s", s)
	}

	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []any{}, nil
	}

	var result []any
	pos := 0
	for pos < len(inner) {
		// Skip whitespace and commas
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r' || inner[pos] == ',') {
			pos++
		}
		if pos >= len(inner) {
			break
		}

		valueStr, newPos := e.parseValueFromString(inner, pos)
		if newPos <= pos {
			// No progress: parseValueFromString returns its input pos
			// unchanged on an unbalanced literal and on a stray depth-0
			// closer, so the loop appended forever -- an unbounded memory grow
			// and a hang (memql#2785). Unlike the load-time copies of this
			// parser, this one runs at EVENT-DISPATCH time, so the failure is
			// a spinning automation worker on a live node.
			//
			// The leading skip has already consumed commas and whitespace, so
			// a non-advancing return cannot be a legitimately-empty element.
			return nil, fmt.Errorf("malformed array literal at offset %d: %q", pos, inner)
		}
		pos = newPos

		evaluated, err := e.evaluateValue(evaluator, valueStr)
		if err != nil {
			return nil, fmt.Errorf("evaluating array element: %w", err)
		}
		result = append(result, evaluated)

		// Skip trailing whitespace/commas
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r' || inner[pos] == ',') {
			pos++
		}
	}

	return result, nil
}

// parseValueFromString extracts a single value starting at position pos.
// Returns the value string and new position.
func (e *MutationExecutor) parseValueFromString(s string, pos int) (string, int) {
	if pos >= len(s) {
		return "", pos
	}

	// Skip leading whitespace
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\n' || s[pos] == '\r') {
		pos++
	}
	if pos >= len(s) {
		return "", pos
	}

	ch := s[pos]

	// Nested object
	if ch == '{' {
		depth := 0
		start := pos
		inString := false
		escaped := false
		for pos < len(s) {
			cur := s[pos]
			if escaped {
				escaped = false
				pos++
				continue
			}
			if cur == '\\' && inString {
				escaped = true
				pos++
				continue
			}
			if cur == '"' {
				inString = !inString
			}
			if !inString {
				if cur == '{' {
					depth++
				} else if cur == '}' {
					depth--
					if depth == 0 {
						pos++
						return s[start:pos], pos
					}
				}
			}
			pos++
		}
		return s[start:pos], pos
	}

	// Array
	if ch == '[' {
		depth := 0
		start := pos
		inString := false
		escaped := false
		for pos < len(s) {
			cur := s[pos]
			if escaped {
				escaped = false
				pos++
				continue
			}
			if cur == '\\' && inString {
				escaped = true
				pos++
				continue
			}
			if cur == '"' {
				inString = !inString
			}
			if !inString {
				if cur == '[' {
					depth++
				} else if cur == ']' {
					depth--
					if depth == 0 {
						pos++
						return s[start:pos], pos
					}
				}
			}
			pos++
		}
		return s[start:pos], pos
	}

	// String literal
	if ch == '"' {
		pos++
		start := pos
		escaped := false
		for pos < len(s) {
			cur := s[pos]
			if escaped {
				escaped = false
				pos++
				continue
			}
			if cur == '\\' {
				escaped = true
				pos++
				continue
			}
			if cur == '"' {
				value := s[start:pos]
				pos++ // skip closing quote
				return value, pos
			}
			pos++
		}
		return s[start:pos], pos
	}

	// Boolean, number, null, or expression
	// Parse until we hit a delimiter (comma, closing brace/bracket) at depth 0
	start := pos
	parenDepth := 0
	braceDepth := 0
	bracketDepth := 0
	inString := false
	escaped := false

	for pos < len(s) {
		cur := s[pos]
		if escaped {
			escaped = false
			pos++
			continue
		}
		if cur == '\\' && inString {
			escaped = true
			pos++
			continue
		}
		if cur == '"' {
			inString = !inString
			pos++
			continue
		}
		if !inString {
			switch cur {
			case '(':
				parenDepth++
			case ')':
				parenDepth--
				if parenDepth < 0 {
					return strings.TrimSpace(s[start:pos]), pos
				}
			case '{':
				braceDepth++
			case '}':
				braceDepth--
				if braceDepth < 0 {
					return strings.TrimSpace(s[start:pos]), pos
				}
			case '[':
				bracketDepth++
			case ']':
				bracketDepth--
				if bracketDepth < 0 {
					return strings.TrimSpace(s[start:pos]), pos
				}
			case ',':
				if parenDepth == 0 && braceDepth == 0 && bracketDepth == 0 {
					return strings.TrimSpace(s[start:pos]), pos
				}
			}
		}
		pos++
	}

	return strings.TrimSpace(s[start:pos]), pos
}

// splitFunctionArgs splits function arguments, handling nested parentheses and braces.
func (e *MutationExecutor) splitFunctionArgs(s string) []string {
	var args []string
	var current strings.Builder
	parenDepth := 0
	braceDepth := 0
	bracketDepth := 0
	inString := false
	escaped := false

	for _, ch := range s {
		if escaped {
			current.WriteRune(ch)
			escaped = false
			continue
		}

		if ch == '\\' && inString {
			escaped = true
			current.WriteRune(ch)
			continue
		}

		if ch == '"' {
			inString = !inString
			current.WriteRune(ch)
			continue
		}

		if !inString {
			switch ch {
			case '(':
				parenDepth++
			case ')':
				parenDepth--
			case '{':
				braceDepth++
			case '}':
				braceDepth--
			case '[':
				bracketDepth++
			case ']':
				bracketDepth--
			case ',':
				if parenDepth == 0 && braceDepth == 0 && bracketDepth == 0 {
					args = append(args, current.String())
					current.Reset()
					continue
				}
			}
		}

		current.WriteRune(ch)
	}

	if current.Len() > 0 {
		args = append(args, current.String())
	}

	return args
}

// buildInsertQuery constructs the insert query with properly escaped JSON payload.
func (e *MutationExecutor) buildInsertQuery(concept, id string, payload map[string]any, parent, aliasOf string) string {
	parts := []string{fmt.Sprintf("%q", concept)}

	if id != "" {
		parts = append(parts, fmt.Sprintf("id=%q", id))
	}

	if payload != nil {
		// Convert payload to JSON
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			// Fallback to empty object if marshal fails
			payloadJSON = []byte("{}")
		}
		parts = append(parts, fmt.Sprintf("payload=%s", string(payloadJSON)))
	}

	if parent != "" {
		parts = append(parts, fmt.Sprintf("parent=%q", parent))
	}

	if aliasOf != "" {
		parts = append(parts, fmt.Sprintf("aliasOf=%q", aliasOf))
	}

	return fmt.Sprintf("insert(%s)", strings.Join(parts, ", "))
}

// convertMutationResult converts the mutation result to an automation-friendly format.
func convertMutationResult(execResult any) any {
	if execResult == nil {
		return nil
	}
	return execResult
}

// extractMutationItemCount extracts the number of created items.
func extractMutationItemCount(execResult any) int {
	if execResult == nil {
		return 0
	}

	if er, ok := execResult.(*memql.ExecuteResult); ok {
		if er == nil || er.Bundle == nil {
			return 0
		}
		return len(er.Bundle.GetNodes())
	}

	return 1 // Assume 1 for successful insert
}

// buildMutationResultQuery constructs a query to retrieve the inserted record.
func buildMutationResultQuery(concept string, execResult any) string {
	if er, ok := execResult.(*memql.ExecuteResult); ok && er != nil && er.Bundle != nil {
		nodes := er.Bundle.GetNodes()
		if len(nodes) > 0 {
			node := nodes[0]
			id := node.GetId()
			if concept != "" && id != "" {
				// Use quoted concept and id for proper MemQL syntax
				return fmt.Sprintf(`concept==%s;id==%s`, jsonString(concept), jsonString(id))
			}
		}
	}
	return ""
}

// isBareStepIdentifier reports whether expr is a single bare identifier --
// no dot, no parens, no '$', no spaces -- that could name a step (e.g.
// `getActiveGA`). Used to decide whether to resolve it against the step
// table rather than treat it as a literal string. A leading digit is
// rejected so numeric literals stay literals. See memql#574.
func isBareStepIdentifier(expr string) bool {
	if expr == "" {
		return false
	}
	for i, r := range expr {
		isAlpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		isDigit := r >= '0' && r <= '9'
		if i == 0 {
			if !isAlpha {
				return false
			}
			continue
		}
		if !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

// splitFuncCallAndPath splits an expression like "first(getFirstStep).payload.targetData"
// into the function call portion ("first(getFirstStep)") and the chained property path
// ("payload.targetData"). Returns ok=true only when a chained path exists after the
// function call's closing parenthesis.
//
// This handles nested parentheses correctly, e.g.:
//
//	"index(concat(a,b), 0).payload.field" → ("index(concat(a,b), 0)", "payload.field", true)
//	"first(step)"                         → ("", "", false)  — no chained path
//	"hello.world"                         → ("", "", false)  — not a function call

func splitFuncCallAndPath(expr string) (funcCall, chainedPath string, ok bool) {
	openIdx := strings.Index(expr, "(")
	if openIdx <= 0 {
		return "", "", false
	}

	// Verify the part before '(' looks like a function name (alphanumeric / underscore).
	for _, r := range expr[:openIdx] {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
			return "", "", false
		}
	}

	// Walk forward from the opening paren to find its matching close.
	depth := 0
	inString := false
	escaped := false
	closeIdx := -1
	for i := openIdx; i < len(expr); i++ {
		ch := expr[i]
		if escaped {
			escaped = false
			continue
		}
		if inString {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				closeIdx = i
			}
		}
		if closeIdx >= 0 {
			break
		}
	}

	if closeIdx < 0 {
		return "", "", false // unbalanced parens
	}

	// There must be a '.' immediately after the closing paren for a chained path.
	remaining := expr[closeIdx+1:]
	if !strings.HasPrefix(remaining, ".") {
		return "", "", false
	}

	return expr[:closeIdx+1], remaining[1:], true // strip the leading '.'
}

// resolveChainedPath navigates a dot-separated property path on an object,
// preserving the original Go type (map, slice, nil, string, number, etc.).
// It returns nil without error when the path cannot be followed (e.g. the
// intermediate value is nil or not a map).
func resolveChainedPath(obj any, path string) (any, error) {
	segments := strings.Split(path, ".")
	current := obj
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		if current == nil {
			return nil, nil
		}
		switch v := current.(type) {
		case map[string]any:
			current = v[seg]
		default:
			// Cannot navigate further (e.g. scalar or unsupported type).
			return nil, nil
		}
	}
	return current, nil
}
