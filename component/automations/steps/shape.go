package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

// ShapeExecutor transforms data using shape templates.
type ShapeExecutor struct{}

// Execute runs a shape transformation step.
func (e *ShapeExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
	}

	if step.Shape == nil {
		result.Status = "failed"
		result.Error = "shape configuration is required"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("shape configuration is required")
	}

	if stepCtx.Engine == nil {
		result.Status = "failed"
		result.Error = "MemQL engine not configured"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("MemQL engine not configured")
	}

	shapeCfg := step.Shape

	// Resolve the source data
	// IMPORTANT: .memql automations use bare step references like "stepId.result.X"
	// (not "$steps.stepId.result.X"). EvaluateStepReference supports resolving
	// these friendly references.
	sourceValue, err := stepCtx.Evaluator.EvaluateStepReference(shapeCfg.Source)
	if err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("failed to evaluate source: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("failed to evaluate source: %w", err)
	}

	// Build the shape query
	// The shape template is passed as-is to the engine
	templateStr := string(shapeCfg.Template)

	// Build a query that applies the shape template
	// We need to construct a shape() call that the engine can execute
	// For now, we'll use a simplified approach where we directly transform the data

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("executing shape step",
			"step", step.ID,
			"source", shapeCfg.Source,
		)
	}

	// Convert source to a slice for processing
	sourceSlice, err := automations.ToSlice(sourceValue)
	if err != nil {
		// Single item, wrap in slice
		sourceSlice = []any{sourceValue}
	}

	// Check requireNotEmpty constraint
	if shapeCfg.RequireNotEmpty && len(sourceSlice) == 0 {
		result.Status = "failed"
		result.Error = "source is empty but requireNotEmpty is set"
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("source is empty but requireNotEmpty is set")
	}

	// Parse the template
	var templateObj map[string]any
	if err := json.Unmarshal(shapeCfg.Template, &templateObj); err != nil {
		result.Status = "failed"
		result.Error = fmt.Sprintf("invalid template: %v", err)
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("invalid template: %w", err)
	}

	// Apply the template to each item
	var shapedResults []any
	for _, item := range sourceSlice {
		// Create an evaluator with the item context
		itemEval := stepCtx.Evaluator.Clone()
		itemEval.SetItem(item, "node")

		shaped, err := e.applyTemplate(ctx, templateObj, itemEval, stepCtx)
		if err != nil {
			result.Status = "failed"
			result.Error = fmt.Sprintf("template application failed: %v", err)
			result.CompletedAt = time.Now()
			result.Duration = result.CompletedAt.Sub(result.StartedAt)
			return result, fmt.Errorf("template application failed: %w", err)
		}
		shapedResults = append(shapedResults, shaped)
	}

	result.Status = "success"
	result.Result = shapedResults
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)
	result.Metadata = map[string]any{
		"source":    shapeCfg.Source,
		"template":  templateStr,
		"itemCount": len(shapedResults),
	}

	// Record step execution in the database
	runId := ""
	if stepCtx.Execution != nil {
		runId = stepCtx.Execution.ID
	}
	stepRecordQuery := RecordStepExecution(ctx, stepCtx.Engine, StepRecordData{
		RunId:     runId,
		StepId:    step.ID,
		StepType:  "shape",
		Status:    result.Status,
		ItemCount: len(shapedResults),
		Duration:  float64(result.Duration.Milliseconds()),
	})

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("shape step completed",
			"step", step.ID,
			"itemCount", len(shapedResults),
			"stepRecord", stepRecordQuery,
			"duration", formatDuration(result.Duration),
		)
	}

	return result, nil
}

// applyTemplate applies a shape template to produce a result object.
func (e *ShapeExecutor) applyTemplate(ctx context.Context, template map[string]any, eval *automations.Evaluator, stepCtx *Context) (map[string]any, error) {
	result := make(map[string]any)

	for key, value := range template {
		resolved, err := e.evaluateTemplateValue(ctx, value, eval, stepCtx)
		if err != nil {
			return nil, fmt.Errorf("evaluating %q: %w", key, err)
		}
		result[key] = resolved
	}

	return result, nil
}

func (e *ShapeExecutor) evaluateTemplateValue(ctx context.Context, value any, eval *automations.Evaluator, stepCtx *Context) (any, error) {
	switch v := value.(type) {
	case string:
		// $ expressions
		if len(v) > 0 && v[0] == '$' {
			return eval.EvaluateValue(v)
		}
		// String-form shape functions like node("..."), ai("...", {...})
		if isShapeFunction(v) {
			return e.evaluateShapeFunction(ctx, v, eval, stepCtx)
		}
		return v, nil

	case map[string]any:
		// Object-form function calls emitted by the .memql compiler, e.g.
		// {"Name":"node","Args":{"0":"payload.name"}}
		if funcName, args, ok := parseFunctionObject(v); ok {
			return e.evaluateFunctionObject(ctx, funcName, args, eval, stepCtx)
		}
		// Nested object template
		return e.applyTemplate(ctx, v, eval, stepCtx)

	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			resolved, err := e.evaluateTemplateValue(ctx, item, eval, stepCtx)
			if err != nil {
				return nil, err
			}
			out = append(out, resolved)
		}
		return out, nil

	default:
		return value, nil
	}
}

func parseFunctionObject(obj map[string]any) (string, []any, bool) {
	if obj == nil {
		return "", nil, false
	}
	nameVal, ok := obj["Name"]
	if !ok {
		nameVal, ok = obj["name"]
	}
	if !ok {
		return "", nil, false
	}
	name, ok := nameVal.(string)
	if !ok || name == "" {
		return "", nil, false
	}

	argsVal, ok := obj["Args"]
	if !ok {
		argsVal, ok = obj["args"]
	}
	if !ok {
		// Allow zero-arg calls (rare)
		return name, nil, true
	}

	switch a := argsVal.(type) {
	case []any:
		return name, a, true
	case map[string]any:
		// Keys are usually "0","1",...
		type kv struct {
			i int
			v any
		}
		var items []kv
		for k, v := range a {
			idx, err := strconv.Atoi(k)
			if err != nil {
				continue
			}
			items = append(items, kv{i: idx, v: v})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].i < items[j].i })
		out := make([]any, 0, len(items))
		for _, it := range items {
			out = append(out, it.v)
		}
		return name, out, true
	default:
		return name, nil, true
	}
}

func (e *ShapeExecutor) evaluateFunctionObject(ctx context.Context, name string, args []any, eval *automations.Evaluator, stepCtx *Context) (any, error) {
	switch name {
	case "node":
		if len(args) < 1 {
			return nil, fmt.Errorf("node() requires at least 1 argument")
		}
		path, ok := args[0].(string)
		if !ok || path == "" {
			return nil, fmt.Errorf("node() argument must be a non-empty string")
		}
		return eval.EvaluateValue("$node." + path)

	case "ai":
		if stepCtx.Engine == nil {
			return nil, fmt.Errorf("AI function requires MemQL engine")
		}
		if len(args) < 1 {
			return nil, fmt.Errorf("ai() requires a template id as first argument")
		}
		templateId, ok := args[0].(string)
		if !ok || templateId == "" {
			return nil, fmt.Errorf("ai() template id must be a non-empty string")
		}

		var resolvedData map[string]any
		if len(args) >= 2 && args[1] != nil {
			// The data arg is already a parsed object. Evaluate it as a template-like value
			// so nested node() calls and $ expressions resolve.
			resolvedAny, err := e.evaluateTemplateValue(ctx, args[1], eval, stepCtx)
			if err != nil {
				return nil, err
			}
			if m, ok := resolvedAny.(map[string]any); ok {
				resolvedData = m
			} else {
				return nil, fmt.Errorf("ai() data argument must be an object, got %T", resolvedAny)
			}
		}

		return stepCtx.Engine.InvokeAI(ctx, templateId, resolvedData)
	}

	// Unknown function object: return as-is (debuggable)
	return map[string]any{"Name": name, "Args": args}, nil
}

// isShapeFunction checks if a string looks like a shape function call.
func isShapeFunction(s string) bool {
	funcs := []string{"node(", "ai(", "children(", "parent(", "payload("}
	for _, f := range funcs {
		if len(s) >= len(f) && s[:len(f)] == f {
			return true
		}
	}
	return false
}

// evaluateShapeFunction handles shape template function calls.
func (e *ShapeExecutor) evaluateShapeFunction(ctx context.Context, funcCall string, eval *automations.Evaluator, stepCtx *Context) (any, error) {
	// For now, handle the most common cases
	// More complete implementation would parse the function call properly

	if len(funcCall) >= 5 && funcCall[:5] == "node(" {
		// Extract the path from node("path")
		path := extractQuotedArg(funcCall[5:])
		if path == "" {
			return nil, fmt.Errorf("invalid node() call: %s", funcCall)
		}
		// Resolve from the current item
		return eval.EvaluateValue("$node." + path)
	}

	if len(funcCall) >= 3 && funcCall[:3] == "ai(" {
		// AI function call - invoke the engine's AI runtime
		if stepCtx.Engine == nil {
			return nil, fmt.Errorf("AI function requires MemQL engine")
		}

		// Parse the ai() call: ai("templateId", { data })
		templateId, dataStr, err := parseAIFunctionCall(funcCall)
		if err != nil {
			return nil, fmt.Errorf("failed to parse ai()call: %w", err)
		}

		// Parse and resolve the data object (handles node() and $ expressions)
		var resolvedData map[string]any
		if dataStr != "" {
			resolvedData, err = parseShapeDataObject(dataStr, eval)
			if err != nil {
				return nil, fmt.Errorf("failed to parse ai()data: %w", err)
			}
		}

		// Invoke the AI runtime via the engine
		result, err := stepCtx.Engine.InvokeAI(ctx, templateId, resolvedData)
		if err != nil {
			return nil, fmt.Errorf("AI function execution failed: %w", err)
		}

		return result, nil
	}

	// Default: return the function call as-is (unresolved)
	return funcCall, nil
}

// extractQuotedArg extracts the first quoted argument from a function call.
func extractQuotedArg(s string) string {
	// Find opening quote
	start := -1
	quoteChar := byte(0)
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\'' {
			start = i + 1
			quoteChar = s[i]
			break
		}
	}
	if start == -1 {
		return ""
	}

	// Find closing quote
	for i := start; i < len(s); i++ {
		if s[i] == quoteChar {
			return s[start:i]
		}
	}
	return ""
}

// parseAIFunctionCall parses an ai()function call and extracts template ID and raw data string.
// Expected format: ai("templateId", { "key": "value", ... }) or ai("templateId")
// The data object may contain shape function calls like node("field") which are not valid JSON.
func parseAIFunctionCall(funcCall string) (string, string, error) {
	// Remove "ai(" prefix and ")" suffix
	if len(funcCall) < 4 || funcCall[:3] != "ai(" || funcCall[len(funcCall)-1] != ')' {
		return "", "", fmt.Errorf("invalid ai()function call format")
	}

	inner := funcCall[3 : len(funcCall)-1]

	// Extract template ID (first quoted string argument)
	templateId := extractQuotedArg(inner)
	if templateId == "" {
		return "", "", fmt.Errorf("ai() requires a template ID as first argument")
	}

	// Find the data object (everything after the first comma)
	commaIdx := -1
	inQuote := false
	quoteChar := byte(0)
	for i := 0; i < len(inner); i++ {
		if !inQuote && (inner[i] == '"' || inner[i] == '\'') {
			inQuote = true
			quoteChar = inner[i]
		} else if inQuote && inner[i] == quoteChar {
			inQuote = false
		} else if !inQuote && inner[i] == ',' {
			commaIdx = i
			break
		}
	}

	if commaIdx == -1 {
		// No data object provided
		return templateId, "", nil
	}

	// Return raw data string - will be parsed as shape template
	dataStr := inner[commaIdx+1:]
	dataStr = trimWhitespace(dataStr)

	return templateId, dataStr, nil
}

// parseShapeDataObject parses a shape-style data object that may contain function calls.
// Format: { "key": value, "key2": node("field"), ... }
// Values can be: quoted strings, numbers, booleans, null, or function calls like node("x")
func parseShapeDataObject(dataStr string, eval *automations.Evaluator) (map[string]any, error) {
	dataStr = trimWhitespace(dataStr)
	if dataStr == "" {
		return nil, nil
	}

	// Must start with { and end with }
	if len(dataStr) < 2 || dataStr[0] != '{' || dataStr[len(dataStr)-1] != '}' {
		return nil, fmt.Errorf("data object must be enclosed in braces")
	}

	// Remove outer braces
	inner := trimWhitespace(dataStr[1 : len(dataStr)-1])
	if inner == "" {
		return map[string]any{}, nil
	}

	result := make(map[string]any)

	// Parse key-value pairs
	for len(inner) > 0 {
		// Skip whitespace
		inner = trimWhitespace(inner)
		if inner == "" {
			break
		}

		// Parse key (must be quoted string)
		if inner[0] != '"' {
			return nil, fmt.Errorf("expected quoted key, got: %s", truncateStr(inner, 20))
		}

		key := extractQuotedArg(inner)
		if key == "" {
			return nil, fmt.Errorf("invalid key in data object")
		}

		// Skip past the key and find colon
		keyEnd := 1 + len(key) + 1 // opening quote + key + closing quote
		inner = trimWhitespace(inner[keyEnd:])

		if len(inner) == 0 || inner[0] != ':' {
			return nil, fmt.Errorf("expected ':' after key %q", key)
		}
		inner = trimWhitespace(inner[1:])

		// Parse value
		value, remaining, err := parseShapeValue(inner, eval)
		if err != nil {
			return nil, fmt.Errorf("parsing value for key %q: %w", key, err)
		}
		result[key] = value
		inner = trimWhitespace(remaining)

		// Check for comma or end
		if len(inner) > 0 && inner[0] == ',' {
			inner = trimWhitespace(inner[1:])
		}
	}

	return result, nil
}

// parseShapeValue parses a single value from a shape data object.
// Returns the parsed value and the remaining unparsed string.
func parseShapeValue(s string, eval *automations.Evaluator) (any, string, error) {
	s = trimWhitespace(s)
	if s == "" {
		return nil, "", fmt.Errorf("unexpected end of input")
	}

	// Quoted string
	if s[0] == '"' || s[0] == '\'' {
		quoteChar := s[0]
		end := 1
		for end < len(s) {
			if s[end] == quoteChar && (end == 1 || s[end-1] != '\\') {
				break
			}
			end++
		}
		if end >= len(s) {
			return nil, "", fmt.Errorf("unterminated string")
		}
		value := s[1:end]
		// Check if it's a $ expression
		if len(value) > 0 && value[0] == '$' {
			resolved, err := eval.EvaluateValue(value)
			if err != nil {
				return nil, "", fmt.Errorf("evaluating expression %q: %w", value, err)
			}
			return resolved, s[end+1:], nil
		}
		return value, s[end+1:], nil
	}

	// Number
	if (s[0] >= '0' && s[0] <= '9') || s[0] == '-' {
		end := 0
		for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == '.' || s[end] == '-' || s[end] == 'e' || s[end] == 'E' || s[end] == '+') {
			end++
		}
		numStr := s[:end]
		var num any
		if err := json.Unmarshal([]byte(numStr), &num); err != nil {
			return nil, "", fmt.Errorf("invalid number: %s", numStr)
		}
		return num, s[end:], nil
	}

	// Boolean or null
	if len(s) >= 4 && s[:4] == "true" {
		return true, s[4:], nil
	}
	if len(s) >= 5 && s[:5] == "false" {
		return false, s[5:], nil
	}
	if len(s) >= 4 && s[:4] == "null" {
		return nil, s[4:], nil
	}

	// Function call like node("field") or $expression
	if s[0] == '$' {
		// Find end of $ expression
		end := 1
		for end < len(s) && (isAlphaNum(s[end]) || s[end] == '.' || s[end] == '_') {
			end++
		}
		expr := s[:end]
		resolved, err := eval.EvaluateValue(expr)
		if err != nil {
			return nil, "", fmt.Errorf("evaluating expression %q: %w", expr, err)
		}
		return resolved, s[end:], nil
	}

	// Function call like node("field")
	if isAlpha(s[0]) {
		// Find function name
		nameEnd := 0
		for nameEnd < len(s) && isAlphaNum(s[nameEnd]) {
			nameEnd++
		}
		funcName := s[:nameEnd]

		// Must be followed by (
		remaining := s[nameEnd:]
		if len(remaining) == 0 || remaining[0] != '(' {
			return nil, "", fmt.Errorf("unexpected identifier: %s", funcName)
		}

		// Find matching closing paren
		parenDepth := 1
		end := 1
		inQuote := false
		var quoteChar byte
		for end < len(remaining) && parenDepth > 0 {
			ch := remaining[end]
			if !inQuote && (ch == '"' || ch == '\'') {
				inQuote = true
				quoteChar = ch
			} else if inQuote && ch == quoteChar && remaining[end-1] != '\\' {
				inQuote = false
			} else if !inQuote {
				switch ch {
				case '(':
					parenDepth++
				case ')':
					parenDepth--
				}
			}
			end++
		}

		if parenDepth != 0 {
			return nil, "", fmt.Errorf("unmatched parenthesis in function call")
		}

		funcCall := funcName + remaining[:end]

		// Handle node() function
		if funcName == "node" {
			path := extractQuotedArg(remaining[1:])
			if path == "" {
				return nil, "", fmt.Errorf("invalid node() call: %s", funcCall)
			}
			// Resolve from current item using $node prefix
			resolved, err := eval.EvaluateValue("$node." + path)
			if err != nil {
				return nil, "", fmt.Errorf("evaluating node(%q): %w", path, err)
			}
			return resolved, remaining[end:], nil
		}

		return nil, "", fmt.Errorf("unsupported function in ai()data: %s", funcName)
	}

	// Nested object
	if s[0] == '{' {
		// Find matching closing brace
		braceDepth := 1
		end := 1
		inQuote := false
		var quoteChar byte
		for end < len(s) && braceDepth > 0 {
			ch := s[end]
			if !inQuote && (ch == '"' || ch == '\'') {
				inQuote = true
				quoteChar = ch
			} else if inQuote && ch == quoteChar && s[end-1] != '\\' {
				inQuote = false
			} else if !inQuote {
				switch ch {
				case '{':
					braceDepth++
				case '}':
					braceDepth--
				}
			}
			end++
		}

		if braceDepth != 0 {
			return nil, "", fmt.Errorf("unmatched brace in nested object")
		}

		nested, err := parseShapeDataObject(s[:end], eval)
		if err != nil {
			return nil, "", err
		}
		return nested, s[end:], nil
	}

	// Array
	if s[0] == '[' {
		// Find matching closing bracket
		bracketDepth := 1
		end := 1
		inQuote := false
		var quoteChar byte
		for end < len(s) && bracketDepth > 0 {
			ch := s[end]
			if !inQuote && (ch == '"' || ch == '\'') {
				inQuote = true
				quoteChar = ch
			} else if inQuote && ch == quoteChar && s[end-1] != '\\' {
				inQuote = false
			} else if !inQuote {
				switch ch {
				case '[':
					bracketDepth++
				case ']':
					bracketDepth--
				}
			}
			end++
		}

		if bracketDepth != 0 {
			return nil, "", fmt.Errorf("unmatched bracket in array")
		}

		arr, err := parseShapeArray(s[:end], eval)
		if err != nil {
			return nil, "", err
		}
		return arr, s[end:], nil
	}

	return nil, "", fmt.Errorf("unexpected character: %c", s[0])
}

// parseShapeArray parses a shape-style array.
func parseShapeArray(s string, eval *automations.Evaluator) ([]any, error) {
	s = trimWhitespace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("array must be enclosed in brackets")
	}

	inner := trimWhitespace(s[1 : len(s)-1])
	if inner == "" {
		return []any{}, nil
	}

	var result []any
	for len(inner) > 0 {
		value, remaining, err := parseShapeValue(inner, eval)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
		inner = trimWhitespace(remaining)

		if len(inner) > 0 && inner[0] == ',' {
			inner = trimWhitespace(inner[1:])
		}
	}

	return result, nil
}

// isAlpha returns true if c is a letter.
func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isAlphaNum returns true if c is a letter, digit, or underscore.
func isAlphaNum(c byte) bool {
	return isAlpha(c) || (c >= '0' && c <= '9') || c == '_'
}

// truncateStr truncates a string to maxLen characters.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// trimWhitespace removes leading and trailing whitespace from a string.
func trimWhitespace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
