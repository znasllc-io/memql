package compiler

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
)

// bareDottedIdentifier returns true when s looks like a dotted reference
// path (e.g. `agent.id`, `foo.bar.baz`). Condition-body expressions use
// this shape for step references (`stepName.payload.x`) and for forEach
// variables (`item.name`). A leading or trailing dot disqualifies the
// string — those fall out of the bare-reference category.
func bareDottedIdentifier(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if !isSimpleIdentifier(p) {
			return false
		}
	}
	return true
}

// isSimpleIdentifier returns true for [a-zA-Z_][a-zA-Z0-9_]*.
func isSimpleIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				return false
			}
			continue
		}
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// convertEventReferences converts event.xxx references to $event.xxx for evaluator resolution.
// For example: event.payload.subject becomes $event.payload.subject
func convertEventReferences(query string) string {
	// Simple string replacement approach:
	// Replace "event." with "$event." but not when already prefixed with $
	result := query

	// Handle patterns where event. appears after operators or delimiters
	patterns := []struct {
		old string
		new string
	}{
		{"==event.", "==$event."},
		{"!=event.", "!=$event."},
		{";event.", ";$event."},
		{",event.", ",$event."},
		{"(event.", "($event."},
		{" event.", " $event."},
		{"\tevent.", "\t$event."},
		{":event.", ":$event."}, // Object property values
		{"{event.", "{$event."}, // Object start
		{"[event.", "[$event."}, // Array element
	}

	for _, p := range patterns {
		result = strings.ReplaceAll(result, p.old, p.new)
	}

	// Handle case where event. is at the start of the string
	if strings.HasPrefix(result, "event.") {
		result = "$" + result
	}

	return result
}

// compileStepHelperValue converts function-call args to evaluator format.
// Handles ExpressionNode (stringify), map (recursive), string (event refs).
func (c *Compiler) compileStepHelperValue(v any) any {
	if v == nil {
		return nil
	}
	if expr, ok := v.(parser.ExpressionNode); ok {
		return convertEventReferences(c.expressionToString(expr))
	}
	if m, ok := v.(map[string]any); ok {
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[k] = c.compileStepHelperValue(val)
		}
		return out
	}
	if s, ok := v.(string); ok {
		if strings.HasPrefix(s, "event.") && !strings.HasPrefix(s, "$") {
			return "$" + s
		}
		if strings.HasPrefix(s, "item.") && !strings.HasPrefix(s, "$") {
			return "$" + s
		}
		if (strings.Contains(s, ".result.") || strings.Contains(s, ".metadata.")) && !strings.HasPrefix(s, "$") {
			return "$" + s
		}
	}
	return v
}

// isRuntimeReference checks if a string value is a runtime reference that should not be quoted.
// These are patterns like event.xxx, item.xxx, input.xxx that the evaluator will resolve.
func isRuntimeReference(s string) bool {
	// List of prefixes that indicate runtime references
	refPrefixes := []string{
		"event.",
		"item.",
		"input.",
		"$event.",
		"$item.",
		"$input.",
		"$steps.",
		"$var.",
		"$timestamp",
		"$error",
	}
	for _, prefix := range refPrefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}

	// Also check for function calls that return runtime values
	funcPrefixes := []string{
		"concat(",
		"var(",
		"coalesce(",
		"first(",
		"last(",
		"timestamp(",
	}
	for _, prefix := range funcPrefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}

	// Bare dotted identifiers (e.g., agent.id) should be preserved for runtime resolution.
	// If authors want a literal string containing dots, they should quote it explicitly.
	if bareDottedIdentifier(strings.TrimSpace(s)) {
		return true
	}

	return false
}

// compileAutomation converts an automation FunctionDef to JSON output.
func (c *Compiler) compileAutomation(def *parser.FunctionDef) (*AutomationOutput, error) {
	automation, ok := def.Body.(*parser.AutomationDef)
	if !ok {
		return nil, fmt.Errorf("expected AutomationDef body for automation %q", def.Name)
	}

	output := make(map[string]any)

	// Basic metadata
	output["name"] = def.Name
	if automation.Description != "" {
		output["description"] = automation.Description
	}
	if automation.Schedule != "" {
		output["schedule"] = automation.Schedule
	}

	// Trigger (event-based)
	if automation.Trigger != nil {
		trigger := map[string]any{
			"event": automation.Trigger.Event,
		}
		if automation.Trigger.Filter != "" {
			trigger["filter"] = automation.Trigger.Filter
		}
		output["trigger"] = trigger
	}

	// Input (if defined)
	if automation.Input != nil {
		input, err := c.compileInputExpression(automation.Input)
		if err != nil {
			return nil, err
		}
		output["input"] = input
	}

	// Steps -- two-pass compile with topological sort.
	//
	// Pass 1 collects every step ID into a symbol table and builds a
	// dependency graph by extracting step references from both the
	// condition string AND the step's expression content (function
	// arguments, query strings, mutation payloads). This catches
	// dependencies like `coalesce(autoRole.First().payload.value, ...)`.
	//
	// Pass 2 topologically sorts the steps by their dependency graph and
	// emits them in dependency order. This means authors can declare
	// steps in any order -- the compiler reorders them so every step
	// executes after all its dependencies. Cycles produce a clear
	// compile-time error.
	//
	// Unknown references (typos) still surface as compile-time errors.
	stepIds := make(map[string]struct{}, len(automation.Steps))
	stepsById := make(map[string]*parser.StepDef, len(automation.Steps))
	var orderedIds []string
	var returnStep *parser.StepDef

	for i := range automation.Steps {
		step := &automation.Steps[i]
		if step.ID == "_return" {
			returnStep = step
			continue
		}
		stepIds[step.ID] = struct{}{}
		stepsById[step.ID] = step
		orderedIds = append(orderedIds, step.ID)
	}

	// Build dependency graph: step -> set of step IDs it depends on.
	deps := make(map[string]map[string]struct{}, len(orderedIds))
	for _, id := range orderedIds {
		step := stepsById[id]
		refs := collectAllStepReferences(c, step, stepIds)
		deps[id] = refs
	}

	// Topological sort (Kahn's algorithm). Preserves source order among
	// steps with equal depth so the output is deterministic.
	sorted, err := topoSortSteps(def.Name, orderedIds, deps)
	if err != nil {
		return nil, err
	}

	steps := make([]map[string]any, 0, len(sorted))
	for _, id := range sorted {
		step := stepsById[id]
		compiledStep, err := c.compileStep(step)
		if err != nil {
			return nil, err
		}
		steps = append(steps, compiledStep)
	}

	output["steps"] = steps

	// OnComplete hook (using return if defined)
	if automation.OnComplete != nil {
		onComplete, err := c.compileStep(automation.OnComplete)
		if err != nil {
			return nil, err
		}
		output["onComplete"] = onComplete
	}

	// OnError hook
	if automation.OnError != nil {
		onError, err := c.compileStep(automation.OnError)
		if err != nil {
			return nil, err
		}
		output["onError"] = onError
	}

	// Enabled state
	output["enabled"] = automation.Enabled

	// If there's a return statement, add it as final computation metadata
	if returnStep != nil {
		// The return is handled in the automation executor
		// We can add it as a special marker
		output["_return"] = c.expressionToString(returnStep.Config.(*parser.QueryStepConfig).Query)
	}

	return &AutomationOutput{
		Name:        def.Name,
		Description: automation.Description,
		JSON:        output,
	}, nil
}

// collectAllStepReferences extracts step-name dependencies from every
// part of a step: its condition string, its query/function expression,
// and its mutation payloads. Returns a set of step IDs from allSteps
// that this step depends on. Reserved names (event, item, etc.) and
// self-references are excluded.
func collectAllStepReferences(c *Compiler, step *parser.StepDef, allSteps map[string]struct{}) map[string]struct{} {
	raw := make(map[string]struct{})

	// 1. Condition string.
	if step.Condition != "" {
		for name := range extractStepReferences(step.Condition) {
			raw[name] = struct{}{}
		}
	}

	// 2. Step expression content (query string, function args, mutation
	//    payloads). Serialise to the same string form the evaluator sees
	//    and scan for step references.
	for _, s := range stepExpressionStrings(c, step) {
		for name := range extractStepReferences(s) {
			raw[name] = struct{}{}
		}
	}

	// Filter to actual step IDs (exclude reserved names, self, unknowns).
	out := make(map[string]struct{}, len(raw))
	for name := range raw {
		if name == step.ID {
			continue
		}
		if isReservedReferenceName(name) {
			continue
		}
		if _, ok := allSteps[name]; !ok {
			continue // unknown -- will be caught by the topoSort unknown-ref check
		}
		out[name] = struct{}{}
	}
	return out
}

// stepExpressionStrings serialises a step's config expressions into the
// string form the evaluator would see, so extractStepReferences can
// pull references from function arguments, query strings, and mutation
// payloads.
func stepExpressionStrings(c *Compiler, step *parser.StepDef) []string {
	var out []string
	switch cfg := step.Config.(type) {
	case *parser.QueryStepConfig:
		if cfg.Query != nil {
			out = append(out, c.expressionToString(cfg.Query))
		}
	case *parser.FunctionStepConfig:
		for _, v := range cfg.Args {
			collectStringsFromValue(c, v, &out)
		}
	case *parser.MutationStepConfig:
		if cfg.Mutation != nil {
			if cfg.Mutation.PayloadRaw != "" {
				out = append(out, cfg.Mutation.PayloadRaw)
			}
			collectStringsFromValue(c, cfg.Mutation.IDTemplate, &out)
			collectStringsFromValue(c, cfg.Mutation.ParentTemplate, &out)
			collectStringsFromValue(c, cfg.Mutation.AliasOfTemplate, &out)
		}
	case *parser.ForEachStepConfig:
		if cfg.Source != "" {
			out = append(out, cfg.Source)
		}
		for i := range cfg.Do {
			out = append(out, stepExpressionStrings(c, &cfg.Do[i])...)
		}
	case *parser.ParallelStepConfig:
		for i := range cfg.Branches {
			out = append(out, stepExpressionStrings(c, &cfg.Branches[i])...)
		}
	}
	return out
}

// collectStringsFromValue recursively serialises an argument value to
// its string form for step-reference extraction.
func collectStringsFromValue(c *Compiler, v any, out *[]string) {
	switch val := v.(type) {
	case parser.ExpressionNode:
		*out = append(*out, c.expressionToString(val))
	case string:
		*out = append(*out, val)
	case map[string]any:
		for _, inner := range val {
			collectStringsFromValue(c, inner, out)
		}
	case []any:
		for _, inner := range val {
			collectStringsFromValue(c, inner, out)
		}
	}
}

// topoSortSteps sorts step IDs by their dependency graph using Kahn's
// algorithm. Among steps with equal depth (no ordering constraint),
// the original source order is preserved so the output is deterministic.
// Returns an error if a cycle is detected or if a step references an
// unknown step ID (typo detection).
func topoSortSteps(automationName string, sourceOrder []string, deps map[string]map[string]struct{}) ([]string, error) {
	allSteps := make(map[string]struct{}, len(sourceOrder))
	for _, id := range sourceOrder {
		allSteps[id] = struct{}{}
	}

	// Check for unknown references (typos).
	for id, depSet := range deps {
		for dep := range depSet {
			if _, ok := allSteps[dep]; !ok {
				return nil, fmt.Errorf(
					"automation %q: step %q references unknown step %q -- check for a typo, or add the step",
					automationName, id, dep)
			}
		}
	}

	// Kahn's algorithm.
	inDegree := make(map[string]int, len(sourceOrder))
	for _, id := range sourceOrder {
		inDegree[id] = 0
	}
	for _, depSet := range deps {
		for dep := range depSet {
			inDegree[dep] += 0 // ensure key exists
		}
	}
	// Reverse: for each step, count how many OTHER steps depend on IT.
	// Actually Kahn's uses in-degree of the CONSUMER, not the provider.
	// Let me re-think: deps[A] = {B, C} means "A depends on B and C".
	// In the DAG, edges go from B->A and C->A (B must run before A).
	// In-degree of A = number of dependencies it has = len(deps[A]).
	for _, id := range sourceOrder {
		inDegree[id] = len(deps[id])
	}

	// Queue: steps with no dependencies, in source order.
	queue := make([]string, 0)
	for _, id := range sourceOrder {
		if inDegree[id] == 0 {
			queue = append(queue, id)
		}
	}

	// Reverse adjacency: provider -> list of consumers (steps that
	// depend on it). We need this to decrement in-degree when a
	// provider is emitted.
	consumers := make(map[string][]string)
	for id, depSet := range deps {
		for dep := range depSet {
			consumers[dep] = append(consumers[dep], id)
		}
	}

	sorted := make([]string, 0, len(sourceOrder))
	for len(queue) > 0 {
		// Pop first (preserves source order among equal-depth steps).
		id := queue[0]
		queue = queue[1:]
		sorted = append(sorted, id)

		// Every step that depends on `id` loses one dependency.
		for _, consumer := range consumers[id] {
			inDegree[consumer]--
			if inDegree[consumer] == 0 {
				queue = append(queue, consumer)
			}
		}
	}

	if len(sorted) != len(sourceOrder) {
		// Cycle detected -- find the participating steps.
		var cycle []string
		for _, id := range sourceOrder {
			if inDegree[id] > 0 {
				cycle = append(cycle, id)
			}
		}
		return nil, fmt.Errorf(
			"automation %q: dependency cycle among steps %v -- each step references one or more of the others",
			automationName, cycle)
	}

	return sorted, nil
}

// extractStepReferences pulls every identifier that appears in a
// position where the runtime would resolve it as a step-name reference.
// Returns a set of candidate names; the caller filters against the
// actual step symbol table and a reserved-name list.
//
// Implementation walks the shared-lexer token stream rather than
// regex-matching the condition source. Two reference forms surface:
//
//   - first(name) / last(name) -- consumed as (ident "first"|"last",
//     paren-open, ident name, paren-close) sequences.
//   - bare dotted identifiers -- the shared lexer emits `foo.bar.baz`
//     as a single identifier because `.` is part of the identifier
//     character class, so recognising them is a single token test.
//     Hand-written whitespace (`foo . bar`) splits into three tokens;
//     we join consecutive ident-dot-ident runs before checking.
func extractStepReferences(condition string) map[string]struct{} {
	out := make(map[string]struct{})
	if condition == "" {
		return out
	}
	tokens, err := parser.NewLexer(condition).Tokenize()
	if err != nil {
		return out
	}
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.Type != parser.TokenIdentifier {
			continue
		}
		literal := tok.Literal
		// first(name) / last(name) -- consume the call sequence.
		if literal == "first" || literal == "last" {
			if i+3 < len(tokens) &&
				tokens[i+1].Type == parser.TokenParenOpen &&
				tokens[i+2].Type == parser.TokenIdentifier &&
				tokens[i+3].Type == parser.TokenParenClose {
				out[tokens[i+2].Literal] = struct{}{}
				i += 3
				continue
			}
		}
		// Join `ident . ident . ident` runs produced by hand-written
		// whitespace, then apply the same bareDottedIdentifier test
		// as the single-token case.
		combined, consumed := joinDottedIdentifierRun(tokens, i)
		if bareDottedIdentifier(combined) {
			if dot := strings.Index(combined, "."); dot > 0 {
				out[combined[:dot]] = struct{}{}
			}
			i += consumed - 1
		}
	}
	return out
}

// joinDottedIdentifierRun collapses a sequence of [ident (.ident)*]
// tokens into the concatenated identifier string. Returns the combined
// literal and the number of tokens it consumed. When the starting
// token is not an identifier, returns ("", 0).
func joinDottedIdentifierRun(tokens []parser.Token, start int) (string, int) {
	if start >= len(tokens) || tokens[start].Type != parser.TokenIdentifier {
		return "", 0
	}
	var sb strings.Builder
	sb.WriteString(tokens[start].Literal)
	consumed := 1
	for i := start + 1; i+1 < len(tokens); {
		a, b := tokens[i], tokens[i+1]
		// Accept either an explicit `.` colon-like connector (won't
		// appear with the shared lexer's `.`-in-identifier rule) or
		// a trailing-dot identifier followed by another identifier
		// (arises when the first chunk ended cleanly and the next
		// starts with `.`, e.g. `foo` + `.bar.baz`).
		if a.Type == parser.TokenIdentifier && strings.HasPrefix(a.Literal, ".") {
			sb.WriteString(a.Literal)
			i++
			consumed++
			continue
		}
		if a.Type == parser.TokenIdentifier && b.Type == parser.TokenIdentifier &&
			strings.HasPrefix(b.Literal, ".") {
			// Shouldn't normally happen because consecutive idents
			// would have merged, but defensively join them.
			sb.WriteString(a.Literal)
			sb.WriteString(b.Literal)
			i += 2
			consumed += 2
			continue
		}
		break
	}
	return sb.String(), consumed
}

// isReservedReferenceName returns true for identifiers that appear in
// condition strings but are never step names -- event helpers, runtime
// accessors, literal keywords.
func isReservedReferenceName(name string) bool {
	switch name {
	case "event", "item", "index", "arg", "var", "input", "error",
		"step", "field", "timestamp", "now", "true", "false", "nil", "null":
		return true
	}
	return false
}

// compileInputExpression converts an input expression to JSON format.
func (c *Compiler) compileInputExpression(expr parser.ExpressionNode) (map[string]any, error) {
	queryStr := c.expressionToString(expr)

	input := map[string]any{
		"query": queryStr,
	}

	// Extract limit/offset if present
	if pag, ok := expr.(*parser.PaginateExpr); ok {
		if pag.Limit != nil {
			input["limit"] = *pag.Limit
		}
		if pag.Offset != nil {
			input["offset"] = *pag.Offset
		}
	}

	return input, nil
}

// compileStep converts a StepDef to JSON format.
func (c *Compiler) compileStep(step *parser.StepDef) (map[string]any, error) {
	output := map[string]any{
		"id":   step.ID,
		"type": string(step.Type),
	}

	if step.Name != "" {
		output["name"] = step.Name
	}

	if step.Condition != "" {
		output["condition"] = c.translateCondition(step.Condition)
	}

	if step.OnError != "" {
		output["onError"] = step.OnError
	}

	if step.RetryCount > 0 {
		output["retryCount"] = step.RetryCount
	}

	// Type-specific configuration
	switch step.Type {
	case parser.StepTypeQuery:
		if cfg, ok := step.Config.(*parser.QueryStepConfig); ok {
			query := c.expressionToString(cfg.Query)
			// Convert event.xxx references to $event.xxx for evaluator
			query = convertEventReferences(query)
			output["query"] = map[string]any{
				"query": query,
			}
		}

	case parser.StepTypeMutation:
		if cfg, ok := step.Config.(*parser.MutationStepConfig); ok {
			// Output mutation as structured config, not as a query string
			// The mutation executor will evaluate expressions and build proper JSON
			mutationConfig := c.compileMutationConfig(cfg.Mutation)
			output["mutation"] = mutationConfig
		}

	case parser.StepTypeFunction:
		if cfg, ok := step.Config.(*parser.FunctionStepConfig); ok {
			helperArgs := cfg.Args
			if len(helperArgs) == 1 {
				if obj, ok := helperArgs["0"].(map[string]any); ok {
					helperArgs = obj
				}
			}
			handled := false
			switch cfg.Name {
			case "query":
				if q, ok := helperArgs["query"]; ok {
					qs := fmt.Sprintf("%v", q)
					output["type"] = "query"
					output["query"] = map[string]any{"query": convertEventReferences(qs)}
					handled = true
				}
			case "mutation":
				if _, ok := helperArgs["concept"]; ok {
					mutCfg := map[string]any{}
					if v, ok := helperArgs["concept"]; ok {
						mutCfg["concept"] = fmt.Sprintf("%v", v)
					}
					if v, ok := helperArgs["id"]; ok {
						mutCfg["id"] = c.compileStepHelperValue(v)
					}
					if v, ok := helperArgs["payload"]; ok {
						mutCfg["payload"] = c.compileStepHelperValue(v)
					}
					if v, ok := helperArgs["parent"]; ok {
						mutCfg["parent"] = c.compileStepHelperValue(v)
					}
					if v, ok := helperArgs["aliasOf"]; ok {
						mutCfg["aliasOf"] = c.compileStepHelperValue(v)
					}
					output["type"] = "mutation"
					output["mutation"] = mutCfg
					handled = true
				}
			case "shape":
				if src, ok := helperArgs["source"]; ok {
					output["type"] = "shape"
					shapeCfg := map[string]any{"source": fmt.Sprintf("%v", src)}
					if tpl, ok := helperArgs["template"]; ok {
						shapeCfg["template"] = tpl
					}
					output["shape"] = shapeCfg
					handled = true
				}
			case "event", "publishEvent":
				if topic, ok := helperArgs["topic"]; ok {
					output["type"] = "event"
					eventCfg := map[string]any{"topic": c.compileStepHelperValue(topic)}
					if kind, ok := helperArgs["kind"]; ok {
						eventCfg["kind"] = fmt.Sprintf("%v", kind)
					}
					if payload, ok := helperArgs["payload"]; ok {
						eventCfg["payload"] = c.compileStepHelperValue(payload)
					}
					output["event"] = eventCfg
					handled = true
				}
			case "webhook":
				if url, ok := helperArgs["url"]; ok {
					output["type"] = "webhook"
					whCfg := map[string]any{
						"url":    c.compileStepHelperValue(url),
						"method": "POST",
					}
					if m, ok := helperArgs["method"]; ok {
						whCfg["method"] = fmt.Sprintf("%v", m)
					}
					if h, ok := helperArgs["headers"]; ok {
						whCfg["headers"] = c.compileStepHelperValue(h)
					}
					if b, ok := helperArgs["body"]; ok {
						whCfg["body"] = c.compileStepHelperValue(b)
					}
					if t, ok := helperArgs["timeout"]; ok {
						whCfg["timeout"] = fmt.Sprintf("%v", t)
					}
					output["webhook"] = whCfg
					handled = true
				}
			}
			if !handled {
				// Sub-automation dispatch: a function-call step whose name
				// starts with the `automation` prefix is an
				// automation-within-automation invocation. The kindPrefix
				// rewriter at the parser side turns
				// `automation seedWelcomeCurriculum { ... }` into
				// `automationSeedWelcomeCurriculum({ ... })`; here we
				// recognise that shape and emit a StepTypeAutomation step
				// so the runtime AutomationExecutor handles it (rather
				// than the function executor trying to find a function
				// named "automationSeedWelcomeCurriculum" in the registry,
				// which doesn't exist).
				if strings.HasPrefix(cfg.Name, "automation") && len(cfg.Name) > len("automation") {
					subName := cfg.Name[len("automation"):]
					subName = strings.ToLower(subName[:1]) + subName[1:]
					compiledArgs := make(map[string]any, len(cfg.Args))
					for k, v := range cfg.Args {
						compiledArgs[k] = c.compileStepHelperValue(v)
					}
					autoCfg := map[string]any{"name": subName}
					if len(compiledArgs) > 0 {
						autoCfg["args"] = compiledArgs
					}
					output["type"] = "automation"
					output["automation"] = autoCfg
					handled = true
				}
			}
			if !handled {
				function := map[string]any{"name": cfg.Name}
				if len(cfg.Args) > 0 {
					compiled := make(map[string]any, len(cfg.Args))
					for k, v := range cfg.Args {
						compiled[k] = c.compileStepHelperValue(v)
					}
					function["args"] = compiled
				}
				output["function"] = function
			}
		}

	case parser.StepTypeForEach:
		if cfg, ok := step.Config.(*parser.ForEachStepConfig); ok {
			forEach := map[string]any{
				"source": cfg.Source,
			}
			if cfg.Filter != "" {
				forEach["filter"] = cfg.Filter
			}
			if cfg.As != "" {
				forEach["as"] = cfg.As
			}
			if cfg.Concurrency > 0 {
				forEach["concurrency"] = cfg.Concurrency
			}

			// Compile nested steps
			doSteps := []map[string]any{}
			for _, doStep := range cfg.Do {
				compiled, err := c.compileStep(&doStep)
				if err != nil {
					return nil, err
				}
				doSteps = append(doSteps, compiled)
			}
			forEach["do"] = doSteps

			output["forEach"] = forEach
		}

	case parser.StepTypeParallel:
		if cfg, ok := step.Config.(*parser.ParallelStepConfig); ok {
			parallel := map[string]any{}
			if cfg.Wait != "" {
				parallel["wait"] = cfg.Wait
			}
			parallel["failFast"] = cfg.FailFast

			// Compile branches
			branches := []map[string]any{}
			for _, branch := range cfg.Branches {
				compiled, err := c.compileStep(&branch)
				if err != nil {
					return nil, err
				}
				branches = append(branches, compiled)
			}
			parallel["branches"] = branches

			output["parallel"] = parallel
		}

	case parser.StepTypeSwitch:
		if cfg, ok := step.Config.(*parser.SwitchStepConfig); ok {
			switchCfg := map[string]any{
				"expression": cfg.Expression,
			}

			// Compile cases
			cases := make(map[string]any)
			for caseVal, caseBody := range cfg.Cases {
				caseSteps := []map[string]any{}
				for _, caseStep := range caseBody.Steps {
					compiled, err := c.compileStep(&caseStep)
					if err != nil {
						return nil, err
					}
					caseSteps = append(caseSteps, compiled)
				}
				cases[caseVal] = map[string]any{"steps": caseSteps}
			}
			switchCfg["cases"] = cases

			// Default case
			if cfg.Default != nil {
				defaultSteps := []map[string]any{}
				for _, defStep := range cfg.Default.Steps {
					compiled, err := c.compileStep(&defStep)
					if err != nil {
						return nil, err
					}
					defaultSteps = append(defaultSteps, compiled)
				}
				switchCfg["default"] = map[string]any{"steps": defaultSteps}
			}

			output["switch"] = switchCfg
		}
	}

	return output, nil
}

// expressionToString converts an expression node back to MemQL string format.
func (c *Compiler) expressionToString(expr parser.ExpressionNode) string {
	if expr == nil {
		return ""
	}

	switch e := expr.(type) {
	case *parser.LiteralExpr:
		switch v := e.Value.(type) {
		case string:
			return fmt.Sprintf("%q", v)
		default:
			return fmt.Sprintf("%v", v)
		}

	case *parser.ComparisonExpr:
		// Unary operators (== nil, != nil) have no right-hand value
		if e.Operator == parser.OpMissing || e.Operator == parser.OpNotMissing {
			if e.Operator == parser.OpMissing {
				return fmt.Sprintf("%s==nil", e.Field.Raw)
			}
			return fmt.Sprintf("%s!=nil", e.Field.Raw)
		}
		// Keyword operators need spacing
		opStr := string(e.Operator)
		switch e.Operator {
		case parser.OpIn, parser.OpOut, parser.OpHas:
			return fmt.Sprintf("%s %s %v", e.Field.Raw, opStr, c.valueToString(e.Value))
		default:
			return fmt.Sprintf("%s%s%v", e.Field.Raw, opStr, c.valueToString(e.Value))
		}

	case *parser.LogicalExpr:
		left := c.expressionToString(e.Left)
		right := c.expressionToString(e.Right)
		sep := ";"
		if e.Op == parser.LogicalOr {
			sep = ","
		}
		return fmt.Sprintf("%s%s%s", left, sep, right)

	case *parser.FunctionCallExpr:
		args := []string{}
		for k, v := range e.Args {
			args = append(args, fmt.Sprintf("%s=%v", k, c.valueToString(v)))
		}
		if len(args) == 0 {
			return fmt.Sprintf("%s()", e.Name)
		}
		return fmt.Sprintf("%s(%s)", e.Name, strings.Join(args, ", "))

	case *parser.SortExpr:
		fields := []string{}
		for _, f := range e.Fields {
			prefix := ""
			if f.Direction == parser.SortDesc {
				prefix = "-"
			}
			fields = append(fields, prefix+f.Field)
		}
		return fmt.Sprintf("sort(%s)(%s)", strings.Join(fields, ","), c.expressionToString(e.Target))

	case *parser.PaginateExpr:
		args := []string{}
		if e.Limit != nil {
			args = append(args, fmt.Sprintf("limit=%d", *e.Limit))
		}
		if e.Offset != nil {
			args = append(args, fmt.Sprintf("offset=%d", *e.Offset))
		}
		return fmt.Sprintf("paginate(%s)(%s)", strings.Join(args, ","), c.expressionToString(e.Target))

	case *parser.SpecReferenceExpr:
		return e.Name

	case *parser.ConditionalFilterExpr:
		return fmt.Sprintf("?.%s", c.expressionToString(e.Filter))

	case *parser.ArgRefExpr:
		return fmt.Sprintf("arg(%q)", e.Path)

	// New accessor expressions
	case *parser.VarRefExpr:
		return fmt.Sprintf("var(%q)", e.Name)

	case *parser.StepRefExpr:
		return fmt.Sprintf("step(%q)", e.StepId)

	case *parser.InputRefExpr:
		return "input()"

	case *parser.ItemRefExpr:
		return "item()"

	case *parser.IndexRefExpr:
		return "index()"

	case *parser.EventRefExpr:
		return "event()"

	case *parser.ErrorRefExpr:
		return "error()"

	case *parser.ErrorExpr:
		return fmt.Sprintf("error(%s)", c.expressionToString(e.Message))

	case *parser.TimestampExprFunc:
		return "timestamp()"

	case *parser.FieldRefExpr:
		return fmt.Sprintf("field(%s, %q)", c.expressionToString(e.Object), e.Key)

	case *parser.DotAccessExpr:
		// Serialize `DotAccess{DotAccess{call, "payload"}, "name"}` back
		// as `call.payload.name`. The runtime evaluator's resolvePath
		// parses the flat dotted string via its parts split — see
		// automations/evaluator.go:resolvePath.
		return fmt.Sprintf("%s.%s", c.expressionToString(e.Object), e.Field)

	case *parser.ConcatExpr:
		args := make([]string, len(e.Args))
		for i, arg := range e.Args {
			args[i] = c.expressionToString(arg)
		}
		return fmt.Sprintf("concat(%s)", strings.Join(args, ", "))

	case *parser.CoalesceExpr:
		args := make([]string, len(e.Args))
		for i, arg := range e.Args {
			args[i] = c.expressionToString(arg)
		}
		return fmt.Sprintf("coalesce(%s)", strings.Join(args, ", "))

	case *parser.CondExpr:
		return fmt.Sprintf("cond(%s, %s, %s)",
			c.expressionToString(e.Condition),
			c.expressionToString(e.Then),
			c.expressionToString(e.Else))

	case *parser.TernaryExpr:
		return fmt.Sprintf("%s ? %s : %s",
			c.expressionToString(e.Condition),
			c.expressionToString(e.Then),
			c.expressionToString(e.Else))

	case *parser.FirstExpr:
		return fmt.Sprintf("first(%s)", c.expressionToString(e.Target))

	case *parser.LastExpr:
		return fmt.Sprintf("last(%s)", c.expressionToString(e.Target))

	case *parser.LowerExpr:
		return fmt.Sprintf("lower(%s)", c.expressionToString(e.Target))

	case *parser.UpperExpr:
		return fmt.Sprintf("upper(%s)", c.expressionToString(e.Target))

	case *parser.TrimExpr:
		return fmt.Sprintf("trim(%s)", c.expressionToString(e.Target))

	case *parser.HashExpr:
		return fmt.Sprintf("hash(%s)", c.expressionToString(e.Target))

	case *parser.CanonicalIdExpr:
		// Round-trips canonicalId(value, "<conceptType>") back to its
		// source form. Without this case, the default branch below
		// would emit `*ast.CanonicalIdExpr` (the Go %T type name)
		// into the generated code, which the engine then takes as
		// the literal arg value -- exactly the leak that produced
		// `default:v1:agents:agent:*ast.CanonicalIdExpr` rows in
		// the participant table.
		return fmt.Sprintf("canonicalId(%s, %q)",
			c.expressionToString(e.Value),
			e.Concept)

	case *parser.ContainsExpr:
		return fmt.Sprintf("contains(%s, %s)",
			c.expressionToString(e.Target),
			c.expressionToString(e.Substring))

	case *parser.NotExpr:
		return fmt.Sprintf("not(%s)", c.expressionToString(e.Target))

	case *parser.AndExpr:
		args := make([]string, len(e.Args))
		for i, arg := range e.Args {
			args[i] = c.expressionToString(arg)
		}
		return fmt.Sprintf("and(%s)", strings.Join(args, ", "))

	case *parser.OrExpr:
		args := make([]string, len(e.Args))
		for i, arg := range e.Args {
			args[i] = c.expressionToString(arg)
		}
		return fmt.Sprintf("or(%s)", strings.Join(args, ", "))

	default:
		// SAFETY: emitting `%T` here puts the Go type name (e.g.
		// `*parser.CanonicalIdExpr`) into the generated code as a
		// literal -- silently breaking downstream evaluation. If you
		// added a new AST node, add an explicit case above. Until
		// then, surround the type name with `<<>>` so the breakage
		// is loud at runtime instead of producing plausible-looking
		// id strings.
		return fmt.Sprintf("<<unsupported expression %T>>", expr)
	}
}

// mutationToString converts a mutation statement to MemQL string format.
func (c *Compiler) mutationToString(m *parser.MutationStmt) string {
	if m == nil {
		return ""
	}

	parts := []string{fmt.Sprintf("%q", m.Concept)}

	if m.IDTemplate != nil {
		switch id := m.IDTemplate.(type) {
		case string:
			if strings.TrimSpace(id) != "" {
				parts = append(parts, fmt.Sprintf("id=%q", id))
			}
		case parser.ExpressionNode:
			parts = append(parts, fmt.Sprintf("id=%s", c.expressionToString(id)))
		default:
			// Best-effort: treat as literal string
			parts = append(parts, fmt.Sprintf("id=%q", fmt.Sprintf("%v", id)))
		}
	}

	if m.PayloadRaw != "" {
		trimmed := strings.TrimSpace(m.PayloadRaw)
		// Check if PayloadRaw is the full object literal syntax (contains both id and payload)
		// In that case, output directly without "payload=" prefix
		if strings.HasPrefix(trimmed, "{") && (strings.Contains(trimmed, "id:") || strings.Contains(trimmed, "\"id\":")) && m.IDTemplate == nil {
			// Object literal syntax: insert("concept", { id: ..., payload: {...} })
			parts = append(parts, trimmed)
		} else {
			// Named argument syntax: insert("concept", payload={...})
			parts = append(parts, fmt.Sprintf("payload=%s", m.PayloadRaw))
		}
	}

	if m.ParentTemplate != nil {
		switch v := m.ParentTemplate.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				parts = append(parts, fmt.Sprintf("parent=%q", v))
			}
		case parser.ExpressionNode:
			parts = append(parts, fmt.Sprintf("parent=%s", c.expressionToString(v)))
		default:
			parts = append(parts, fmt.Sprintf("parent=%q", fmt.Sprintf("%v", v)))
		}
	}

	if m.AliasOfTemplate != nil {
		switch v := m.AliasOfTemplate.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				parts = append(parts, fmt.Sprintf("aliasOf=%q", v))
			}
		case parser.ExpressionNode:
			parts = append(parts, fmt.Sprintf("aliasOf=%s", c.expressionToString(v)))
		default:
			parts = append(parts, fmt.Sprintf("aliasOf=%q", fmt.Sprintf("%v", v)))
		}
	}

	return fmt.Sprintf("insert(%s)", strings.Join(parts, ", "))
}

// compileMutationConfig converts a MutationStmt to a structured config map.
// This parses the mutation into a format that can be evaluated at runtime
// by the mutation executor.
func (c *Compiler) compileMutationConfig(m *parser.MutationStmt) map[string]any {
	if m == nil {
		return nil
	}

	config := map[string]any{
		"concept": m.Concept,
	}

	// Handle ID - can be a literal or expression
	if m.IDTemplate != nil {
		switch id := m.IDTemplate.(type) {
		case string:
			if strings.TrimSpace(id) != "" {
				config["id"] = convertEventReferences(id)
			}
		case parser.ExpressionNode:
			config["id"] = convertEventReferences(c.expressionToString(id))
		default:
			config["id"] = convertEventReferences(fmt.Sprintf("%v", id))
		}
	}

	// Parse the PayloadRaw into a structured payload map
	if m.PayloadRaw != "" {
		parsed := c.parsePayloadRaw(m.PayloadRaw)
		if parsed != nil {
			// Check if this is object literal syntax with id and payload inside
			// e.g., {id: ..., payload: {...}}
			if idVal, hasId := parsed["id"]; hasId {
				config["id"] = idVal
				delete(parsed, "id")
			}
			if payloadVal, hasPayload := parsed["payload"]; hasPayload {
				// The payload is nested inside
				if payloadMap, ok := payloadVal.(map[string]any); ok {
					config["payload"] = payloadMap
				} else {
					config["payload"] = payloadVal
				}
			} else {
				// The entire parsed object is the payload (named argument style)
				config["payload"] = parsed
			}
		}
	}

	if m.ParentTemplate != nil {
		switch v := m.ParentTemplate.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				config["parent"] = convertEventReferences(v)
			}
		case parser.ExpressionNode:
			config["parent"] = convertEventReferences(c.expressionToString(v))
		default:
			config["parent"] = convertEventReferences(fmt.Sprintf("%v", v))
		}
	}

	if m.AliasOfTemplate != nil {
		switch v := m.AliasOfTemplate.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				config["aliasOf"] = convertEventReferences(v)
			}
		case parser.ExpressionNode:
			config["aliasOf"] = convertEventReferences(c.expressionToString(v))
		default:
			config["aliasOf"] = convertEventReferences(fmt.Sprintf("%v", v))
		}
	}

	return config
}

// parsePayloadRaw parses the raw payload string into a structured map.
// It handles MemQL object syntax and converts expressions to evaluatable format.
func (c *Compiler) parsePayloadRaw(raw string) map[string]any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}

	// Parse the raw payload using a simple state machine
	result := c.parseObjectLiteral(trimmed)
	return result
}

// parseObjectLiteral parses a MemQL object literal like {key: value, key2: value2}
// into a Go map, preserving expressions as strings for runtime evaluation.
func (c *Compiler) parseObjectLiteral(s string) map[string]any {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil
	}

	// Remove outer braces
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return map[string]any{}
	}

	result := make(map[string]any)

	// Parse key-value pairs
	// This is a simplified parser that handles nested objects and common expressions
	pos := 0
	for pos < len(inner) {
		// Skip whitespace
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r') {
			pos++
		}
		if pos >= len(inner) {
			break
		}

		// Shorthand: a bare dotted path like `event.payload.spaceId` or
		// `registerNode.result.node.id` with no `key:` prefix infers
		// the key from the path's terminal segment. Only multi-segment
		// paths whose segments are all simple identifiers are eligible;
		// anything with parens, brackets, or method calls falls through
		// to the verbose key:value parser below.
		if key, rawPath, next, ok := tryParseBarePathShorthand(inner, pos); ok {
			result[key] = rawPath
			pos = next
			// Skip trailing whitespace/comma.
			for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r' || inner[pos] == ',') {
				pos++
			}
			continue
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
		value, newPos := c.parseValue(inner, pos)
		result[key] = value
		pos = newPos

		// Skip comma if present
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r' || inner[pos] == ',') {
			pos++
		}
	}

	return result
}

// tryParseBarePathShorthand parses a bare dotted path like
// `event.payload.spaceId` or `registerNode.result.node.id` that appears
// inside an object literal without a `key:` prefix. On match it returns
// the inferred key (terminal path segment), the raw path string to stash
// as the value, and the new position past the path.
//
// Rules:
//   - The path must have at least two dotted segments (single-identifier
//     expressions are not eligible -- they'd collide with step references
//     like `allAgents` which are meant to resolve to the step's result).
//   - Every segment must be a simple identifier `[A-Za-z_][A-Za-z0-9_]*`.
//   - The character immediately following the path must be whitespace,
//     `,`, or `}`. Anything else (`(`, `[`, `.`, `:`) means the expression
//     has more structure and we route back to the verbose parser.
func tryParseBarePathShorthand(s string, pos int) (string, string, int, bool) {
	// Skip leading whitespace without permanently advancing on no-match.
	start := pos
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	if start >= len(s) {
		return "", "", pos, false
	}

	// First segment must start with a letter or underscore.
	if !isIdentStart(s[start]) {
		return "", "", pos, false
	}

	scan := start
	// Read the dotted path. Track segment boundaries so we can extract
	// the terminal segment without re-scanning.
	lastSegmentStart := start
	segmentCount := 0
	for scan < len(s) {
		c := s[scan]
		if isIdentChar(c) {
			scan++
			continue
		}
		if c == '.' {
			segmentCount++
			scan++
			if scan >= len(s) || !isIdentStart(s[scan]) {
				return "", "", pos, false
			}
			lastSegmentStart = scan
			continue
		}
		break
	}
	// Account for the final segment.
	if lastSegmentStart < scan {
		segmentCount++
	}

	// Require at least 2 segments (i.e. at least one dot).
	if segmentCount < 2 {
		return "", "", pos, false
	}

	// Terminal character must not continue the expression.
	if scan < len(s) {
		c := s[scan]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != ',' && c != '}' {
			return "", "", pos, false
		}
	}

	path := s[start:scan]
	key := s[lastSegmentStart:scan]
	return key, path, scan, true
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// parseValue parses a value starting at position pos and returns the value and new position.
func (c *Compiler) parseValue(s string, pos int) (any, int) {
	if pos >= len(s) {
		return nil, pos
	}

	// Skip whitespace
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\n' || s[pos] == '\r') {
		pos++
	}

	if pos >= len(s) {
		return nil, pos
	}

	ch := s[pos]

	// Array literal
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
			if inString && cur == '\\' {
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
				case '[':
					depth++
				case ']':
					depth--
					if depth == 0 {
						pos++
						arrStr := s[start:pos]
						return c.parseArrayLiteral(arrStr), pos
					}
				}
			}
			pos++
		}
		return nil, pos
	}

	// Nested object
	if ch == '{' {
		depth := 0
		start := pos
		for pos < len(s) {
			switch s[pos] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					pos++
					objStr := s[start:pos]
					return c.parseObjectLiteral(objStr), pos
				}
			}
			pos++
		}
		return nil, pos
	}

	// String literal
	if ch == '"' {
		pos++
		start := pos
		for pos < len(s) && s[pos] != '"' {
			if s[pos] == '\\' && pos+1 < len(s) {
				pos += 2
			} else {
				pos++
			}
		}
		value := s[start:pos]
		if pos < len(s) {
			pos++ // skip closing quote
		}
		return value, pos
	}

	// Boolean true
	if strings.HasPrefix(s[pos:], "true") {
		return true, pos + 4
	}

	// Boolean false
	if strings.HasPrefix(s[pos:], "false") {
		return false, pos + 5
	}

	// Number
	if ch >= '0' && ch <= '9' || ch == '-' {
		start := pos
		pos++
		for pos < len(s) && ((s[pos] >= '0' && s[pos] <= '9') || s[pos] == '.') {
			pos++
		}
		numStr := s[start:pos]
		if strings.Contains(numStr, ".") {
			// Float
			if f, err := strconv.ParseFloat(numStr, 64); err == nil {
				return f, pos
			}
			return numStr, pos
		}
		// Int
		if i, err := strconv.ParseInt(numStr, 10, 64); err == nil {
			return i, pos
		}
		return numStr, pos
	}

	// Expression (identifier, function call, etc.)
	// This includes: event.payload.xxx, concat(...), var(...), if(...), etc.
	// We track both parentheses depth AND brace depth to handle:
	// - Function calls: if(lt(...), "a", "b")
	// - Block-style if: if cond { then } else { else }
	start := pos
	parenDepth := 0
	braceDepth := 0
	inString := false
	escaped := false
	for pos < len(s) {
		ch := s[pos]
		if escaped {
			escaped = false
			pos++
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			pos++
			continue
		}
		if ch == '"' {
			inString = !inString
			pos++
			continue
		}
		if !inString {
			switch ch {
			case '(':
				parenDepth++
			case ')':
				parenDepth--
				if parenDepth < 0 {
					// Unbalanced closing paren - stop here
					goto done
				}
			case '{':
				braceDepth++
			case '}':
				braceDepth--
				if braceDepth < 0 {
					// Unbalanced closing brace - this is the end of the object
					goto done
				}
			case ',':
				if parenDepth == 0 && braceDepth == 0 {
					// Comma at top level - end of this value
					goto done
				}
			case ']':
				if parenDepth == 0 && braceDepth == 0 {
					// End of array - stop here
					goto done
				}
			}
		}
		pos++
	}
done:

	expr := strings.TrimSpace(s[start:pos])
	// Convert event references to $event format
	expr = convertEventReferences(expr)
	// Ensure expression starts with $ if it's a reference
	if strings.HasPrefix(expr, "event.") {
		expr = "$" + expr
	}
	return expr, pos
}

// parseArrayLiteral parses a JSON-like array literal like [a, b, {x: 1}, []]
// into a Go []any, preserving expressions as strings for runtime evaluation.
func (c *Compiler) parseArrayLiteral(s string) []any {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []any{}
	}

	items := make([]any, 0)
	pos := 0
	for pos < len(inner) {
		// Skip whitespace and commas
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r' || inner[pos] == ',') {
			pos++
		}
		if pos >= len(inner) {
			break
		}

		val, newPos := c.parseValue(inner, pos)
		items = append(items, val)
		pos = newPos

		// Skip trailing whitespace/commas
		for pos < len(inner) && (inner[pos] == ' ' || inner[pos] == '\t' || inner[pos] == '\n' || inner[pos] == '\r' || inner[pos] == ',') {
			pos++
		}
	}

	return items
}

// valueToString converts a value to its string representation.
func (c *Compiler) valueToString(v any) string {
	switch val := v.(type) {
	case string:
		// Check if the string is a reference that should not be quoted
		// These are runtime references that the evaluator will resolve
		if isRuntimeReference(val) {
			return val
		}
		return fmt.Sprintf("%q", val)
	case float64:
		if float64(int(val)) == val {
			return fmt.Sprintf("%d", int(val))
		}
		return fmt.Sprintf("%f", val)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case []any:
		items := []string{}
		for _, item := range val {
			items = append(items, c.valueToString(item))
		}
		return fmt.Sprintf("(%s)", strings.Join(items, ","))
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s: %s", k, c.valueToString(val[k])))
		}
		return fmt.Sprintf("{%s}", strings.Join(parts, ", "))
	case parser.ExpressionNode:
		return c.expressionToString(val)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// translateCondition converts MemQL condition syntax to automation JSON
// syntax by walking the shared-lexer token stream instead of pattern-
// matching the raw string with regex. Supported rewrites:
//
//   - first(stepName) / first(stepName).path
//     → $steps.stepName.result.Bundle.nodes.0[.path]
//   - last(stepName) / last(stepName).path
//     → $steps.stepName.result.Bundle.nodes.-1[.path]
//   - stepName.metadata.field (bare ident-token containing dots)
//     → $steps.stepName.metadata.field
//   - step("name"), step("name").metadata, step("name").result etc.
//     → $steps.name.metadata / $steps.name.result (legacy sugar)
//
// Everything that doesn't match one of these shapes passes through
// verbatim in the canonical spacing parseConditionExpression produces.
func (c *Compiler) translateCondition(cond string) string {
	if cond == "" {
		return cond
	}
	tokens, err := parser.NewLexer(cond).Tokenize()
	if err != nil {
		// Fall back to the raw string on lex error; the runtime
		// evaluator will surface a richer message than the compiler
		// can here.
		return cond
	}
	return rewriteConditionTokens(tokens)
}

func rewriteConditionTokens(tokens []parser.Token) string {
	var out strings.Builder
	lastWasOperator := true
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.Type == parser.TokenEOF {
			break
		}

		// first(name)[.path] / last(name)[.path]
		if tok.Type == parser.TokenIdentifier && (tok.Literal == "first" || tok.Literal == "last") {
			rendered, consumed, ok := rewriteFirstLastCall(tokens, i, tok.Literal)
			if ok {
				writeConditionToken(&out, rendered, &lastWasOperator, false)
				i += consumed - 1
				continue
			}
		}

		// step("name")[.metadata|.result|...] -- legacy accessor.
		if tok.Type == parser.TokenIdentifier && tok.Literal == "step" {
			rendered, consumed, ok := rewriteLegacyStepCall(tokens, i)
			if ok {
				writeConditionToken(&out, rendered, &lastWasOperator, false)
				i += consumed - 1
				continue
			}
		}

		// Bare dotted identifier (shared lexer emits `a.b.c` as a
		// single token). Rewrite `stepName.metadata.x` → `$steps.stepName.metadata.x`.
		if tok.Type == parser.TokenIdentifier && bareDottedIdentifier(tok.Literal) {
			if rewritten, ok := rewriteBareStepReference(tok.Literal); ok {
				writeConditionToken(&out, rewritten, &lastWasOperator, false)
				continue
			}
		}

		writeConditionToken(&out, renderConditionToken(tok), &lastWasOperator,
			tok.Type == parser.TokenParenOpen || tok.Type == parser.TokenParenClose ||
				tok.Literal == ".")
	}
	return out.String()
}

// rewriteFirstLastCall consumes the tokens for `first(name)[.path]` or
// `last(name)[.path]` starting at index i. Returns the rewritten
// string, the number of tokens consumed, and whether the match was
// valid.
func rewriteFirstLastCall(tokens []parser.Token, i int, kind string) (string, int, bool) {
	if i+3 >= len(tokens) {
		return "", 0, false
	}
	if tokens[i+1].Type != parser.TokenParenOpen ||
		tokens[i+2].Type != parser.TokenIdentifier ||
		tokens[i+3].Type != parser.TokenParenClose {
		return "", 0, false
	}
	stepName := tokens[i+2].Literal
	index := "0"
	if kind == "last" {
		index = "-1"
	}
	base := fmt.Sprintf("$steps.%s.result.Bundle.nodes.%s", stepName, index)
	// Consume any trailing `.path` that the lexer either attached to
	// the next token (leading-dot identifier) or split via whitespace.
	path, extra := consumeTrailingDottedPath(tokens, i+4)
	return base + path, 4 + extra, true
}

// rewriteLegacyStepCall handles `step("name")[.segment ...]` → $steps.name[.segment ...].
// Supports both `step("x").result` and the pre-existing sugar where
// `step("x")` alone implies `.result`.
func rewriteLegacyStepCall(tokens []parser.Token, i int) (string, int, bool) {
	if i+3 >= len(tokens) {
		return "", 0, false
	}
	if tokens[i+1].Type != parser.TokenParenOpen ||
		tokens[i+2].Type != parser.TokenString ||
		tokens[i+3].Type != parser.TokenParenClose {
		return "", 0, false
	}
	name := tokens[i+2].Literal
	path, extra := consumeTrailingDottedPath(tokens, i+4)
	if path == "" {
		// Historical behaviour: `step("x")` with no accessor expands
		// to `$steps.x.result`.
		path = ".result"
	}
	return fmt.Sprintf("$steps.%s%s", name, path), 4 + extra, true
}

// rewriteBareStepReference rewrites `stepName.metadata.x` →
// `$steps.stepName.metadata.x` and `stepName.result.x` →
// `$steps.stepName.result.x`. Returns the rewritten form and true on
// match; otherwise returns the original and false.
//
// Only segments that begin with `.metadata` or `.result` (the two
// runtime-accessor paths) are rewritten here; forEach iteration vars
// and other dotted identifiers pass through untouched.
func rewriteBareStepReference(literal string) (string, bool) {
	dot := strings.Index(literal, ".")
	if dot <= 0 {
		return literal, false
	}
	rest := literal[dot:]
	if strings.HasPrefix(rest, ".metadata") {
		return "$steps." + literal, true
	}
	return literal, false
}

// consumeTrailingDottedPath gathers a `.a.b.c` suffix that follows a
// parenthesised call. The shared lexer collapses `).payload.value`
// into (paren-close, ident ".payload.value"), so typically a single
// identifier token captures the whole tail; hand-written whitespace
// (`).payload . value`) fans out into multiple idents each starting
// with `.` (or with whitespace-trimmed segments). We accept either.
func consumeTrailingDottedPath(tokens []parser.Token, start int) (string, int) {
	var sb strings.Builder
	consumed := 0
	for i := start; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.Type != parser.TokenIdentifier {
			break
		}
		literal := tok.Literal
		if !strings.HasPrefix(literal, ".") {
			if sb.Len() == 0 {
				break
			}
			// Whitespace-split continuation (e.g. `... . value` →
			// ident ".", ident "value") -- join with a dot.
			sb.WriteRune('.')
			sb.WriteString(literal)
		} else {
			sb.WriteString(literal)
		}
		consumed++
	}
	return sb.String(), consumed
}

// renderConditionToken returns the canonical spelling of a token.
// String literals are re-wrapped in quotes to round-trip through the
// runtime evaluator's condition parser.
func renderConditionToken(tok parser.Token) string {
	if tok.Type == parser.TokenString {
		return `"` + tok.Literal + `"`
	}
	return tok.Literal
}

// writeConditionToken emits a token's text, inserting a single space
// separator between non-adjacency-sensitive runs of tokens. It mirrors
// the spacing rules parseConditionExpression uses so the round-trip
// through translateCondition remains byte-stable for legacy fixtures.
func writeConditionToken(out *strings.Builder, text string, lastWasOperator *bool, noSpaceAfter bool) {
	if text == "" {
		return
	}
	needsSpace := !*lastWasOperator
	first := text[0]
	if first == '(' || first == ')' || first == '.' || first == ',' {
		needsSpace = false
	}
	if out.Len() > 0 {
		last := out.String()[out.Len()-1]
		if last == '(' || last == '.' || last == ',' {
			needsSpace = false
		}
	}
	if needsSpace && out.Len() > 0 {
		out.WriteByte(' ')
	}
	out.WriteString(text)
	*lastWasOperator = noSpaceAfter || text == "." || text == "(" || text == ","
}

// expressionToJSONExpr converts an expression to JSON $ expression format.
// This is used for values in automation step configurations.
func (c *Compiler) expressionToJSONExpr(expr parser.ExpressionNode) string {
	if expr == nil {
		return ""
	}

	switch e := expr.(type) {
	case *parser.LiteralExpr:
		switch v := e.Value.(type) {
		case string:
			return v // No quotes for JSON expression values
		default:
			return fmt.Sprintf("%v", v)
		}

	case *parser.ArgRefExpr:
		return fmt.Sprintf("$args.%s", e.Path)

	case *parser.VarRefExpr:
		return fmt.Sprintf("$var.%s", e.Name)

	case *parser.StepRefExpr:
		return fmt.Sprintf("$steps.%s.result", e.StepId)

	case *parser.InputRefExpr:
		return "$input"

	case *parser.ItemRefExpr:
		return "$item"

	case *parser.IndexRefExpr:
		return "$index"

	case *parser.EventRefExpr:
		return "$event"

	case *parser.ErrorRefExpr:
		return "$error"

	case *parser.TimestampExprFunc:
		return "$timestamp"

	case *parser.FieldRefExpr:
		obj := c.expressionToJSONExpr(e.Object)
		return fmt.Sprintf("%s.%s", obj, e.Key)

	case *parser.ConcatExpr:
		args := make([]string, len(e.Args))
		for i, arg := range e.Args {
			args[i] = c.expressionToJSONExpr(arg)
		}
		return fmt.Sprintf("concat(%s)", strings.Join(args, ", "))

	case *parser.CoalesceExpr:
		args := make([]string, len(e.Args))
		for i, arg := range e.Args {
			args[i] = c.expressionToJSONExpr(arg)
		}
		return fmt.Sprintf("coalesce(%s)", strings.Join(args, ", "))

	case *parser.CondExpr:
		return fmt.Sprintf("cond(%s, %s, %s)",
			c.expressionToJSONExpr(e.Condition),
			c.expressionToJSONExpr(e.Then),
			c.expressionToJSONExpr(e.Else))

	case *parser.TernaryExpr:
		return fmt.Sprintf("cond(%s, %s, %s)",
			c.expressionToJSONExpr(e.Condition),
			c.expressionToJSONExpr(e.Then),
			c.expressionToJSONExpr(e.Else))

	case *parser.FirstExpr:
		return fmt.Sprintf("first(%s)", c.expressionToJSONExpr(e.Target))

	case *parser.LastExpr:
		return fmt.Sprintf("last(%s)", c.expressionToJSONExpr(e.Target))

	case *parser.LowerExpr:
		return fmt.Sprintf("lower(%s)", c.expressionToJSONExpr(e.Target))

	case *parser.UpperExpr:
		return fmt.Sprintf("upper(%s)", c.expressionToJSONExpr(e.Target))

	case *parser.TrimExpr:
		return fmt.Sprintf("trim(%s)", c.expressionToJSONExpr(e.Target))

	case *parser.HashExpr:
		return fmt.Sprintf("hash(%s)", c.expressionToJSONExpr(e.Target))

	case *parser.ContainsExpr:
		return fmt.Sprintf("contains(%s, %s)",
			c.expressionToJSONExpr(e.Target),
			c.expressionToJSONExpr(e.Substring))

	case *parser.ComparisonExpr:
		// Unary operators (== nil, != nil) have no right-hand value
		if e.Operator == parser.OpMissing || e.Operator == parser.OpNotMissing {
			if e.Operator == parser.OpMissing {
				return fmt.Sprintf("%s==nil", e.Field.Raw)
			}
			return fmt.Sprintf("%s!=nil", e.Field.Raw)
		}
		opStr := string(e.Operator)
		switch e.Operator {
		case parser.OpIn, parser.OpOut, parser.OpHas:
			return fmt.Sprintf("%s %s %v", e.Field.Raw, opStr, c.valueToString(e.Value))
		default:
			return fmt.Sprintf("%s%s%v", e.Field.Raw, opStr, c.valueToString(e.Value))
		}

	case *parser.LogicalExpr:
		left := c.expressionToJSONExpr(e.Left)
		right := c.expressionToJSONExpr(e.Right)
		op := " && "
		if e.Op == parser.LogicalOr {
			op = " || "
		}
		return fmt.Sprintf("%s%s%s", left, op, right)

	default:
		// Fall back to string format
		return c.expressionToString(expr)
	}
}
