package memql

// tool_handler_resolution.go closes the second half of memql#3625: a tool's
// `@handler` names a TARGET, and until this pass existed nothing ever checked
// that the target was there.
//
// ValidateTool (tool_types.go) answers "is this handler well-FORMED" -- a
// known type, a non-empty name. It cannot answer "does the thing it names
// exist", because a tool declaration does not carry the registry. So a tool
// could name a function that was never written, or a query calling a construct
// that had been renamed, register cleanly, and be advertised to the model. The
// failure surfaced only when a model actually called it, as a mid-turn
// `function "x" not found`.
//
// Resolution runs at boot, after the function + builtin registries are
// populated and after registerFunctionTools, and records one baseloader.Skip
// per unresolved target -- so an unresolvable handler refuses a strict boot
// exactly like a construct that failed to parse, and `MEMQL_DSL_ALLOW_SKIPS`
// is the same operator break-glass.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// toolHandlerTargets returns every registry name a tool's handler depends on,
// in declaration order, or nil when the handler resolves against nothing (a
// webhook URL, a delegate).
func toolHandlerTargets(tool *Tool) []string {
	if tool == nil || tool.Handler == nil {
		return nil
	}
	switch strings.TrimSpace(strings.ToLower(tool.Handler.Type)) {
	case "function":
		name := strings.TrimSpace(tool.Handler.FunctionName)
		if name == "" {
			return nil // ValidateTool already refuses this.
		}
		return []string{name}
	case "query":
		// A handler names its one target in its AST: parsed at load for a
		// tool declared in `.memql`, parsed here for one built in Go. A Go
		// handler that is not a handler names nothing -- its call refuses.
		plan := tool.Handler.queryV1
		if plan == nil {
			parsed, err := prepareToolQueryV1(strings.TrimSpace(tool.Handler.Query))
			if err != nil {
				return nil
			}
			plan = parsed
		}
		return []string{plan.target()}
	default:
		// webhook / delegate resolve against no registry.
		return nil
	}
}

// validateToolHandlerTargets returns one error per tool whose handler names a
// function, mutation, query or builtin that the registry does not carry, and
// one per argument a builtin handler REQUIRES that the tool does not declare
// (toolHandlerArgError). Deterministic order (tool name, then target, then
// field) so a boot failure reads the same on every replica.
//
// It walks each tool ONCE. The lookup index holds a tool under its qualified
// name and its bare alias, and walking the index reported one problem twice.
func validateToolHandlerTargets(tools *ToolRegistry, functions *FunctionRegistry) []error {
	if tools == nil || functions == nil {
		return nil
	}
	snapshot := tools.LookupIndex()
	names := make([]string, 0, len(snapshot))
	for name := range snapshot {
		names = append(names, name)
	}
	sort.Strings(names)

	var errs []error
	seen := make(map[*Tool]bool, len(snapshot))
	for _, name := range names {
		tool := snapshot[name]
		if tool == nil || seen[tool] {
			continue
		}
		seen[tool] = true
		for _, target := range toolHandlerTargets(tool) {
			fn := resolveFunctionOrAlias(functions, target)
			if fn == nil {
				errs = append(errs, fmt.Errorf(
					"tool %q (%s): @handler names %q, which is not a registered function, query, mutation or builtin -- the tool registers and is advertised to the model anyway, and fails only when a model calls it",
					tool.Name, describeToolOrigin(tool), target))
				continue
			}
			errs = append(errs, undeclaredBuiltinArgs(tool, fn)...)
		}
	}
	return errs
}

// ruleToolHandlerArgUndeclared is the code of a tool whose builtin handler
// requires an argument the tool never passes.
const ruleToolHandlerArgUndeclared = "tool_handler_arg_undeclared"

// toolHandlerArgError is a tool whose `@handler(type="function")` names a
// builtin that REQUIRES a field the tool does not declare (memql#5436).
//
// A builtin's required fields are enforced on every call
// (validateBuiltinCallArgs), and a tool call hands its builtin the tool's own
// fields -- what the model writes, plus the ones the server stamps
// @autoInjected. So a required field the tool does not declare is one no call
// supplies, and the tool fails on every use while loading, registering and
// being advertised to the model as if it worked. That is how
// requestUserFeedback went dark: its tool moved to `runId` with the work spine
// and its builtin still required `planId`.
type toolHandlerArgError struct {
	tool, origin, builtin, field string
}

func (e *toolHandlerArgError) Error() string {
	return fmt.Sprintf(
		"tool %q (%s): @handler calls builtin %q, which requires %q, and the tool declares no field %q -- so every call fails with \"%s() requires '%s' field in argument\". Declare %q on the tool (an @autoInjected field when the server stamps it), or rename the builtin's field to the one the tool passes [%s]",
		e.tool, e.origin, e.builtin, e.field, e.field, e.builtin, e.field, e.field, ruleToolHandlerArgUndeclared)
}

// RuleCode implements baseloader.CodedRefusal.
func (e *toolHandlerArgError) RuleCode() string { return ruleToolHandlerArgUndeclared }

// undeclaredBuiltinArgs returns one error per field fn requires that tool's
// input schema does not declare, or nil when fn is not a builtin with a
// declared contract (a query, a mutation and a logic take the tool's
// arguments through their own args blocks).
func undeclaredBuiltinArgs(tool *Tool, fn *Function) []error {
	if tool == nil || fn == nil || !fn.IsBuiltin() || fn.BuiltinArgs == nil || len(fn.BuiltinArgs.Required) == 0 {
		return nil
	}
	declared := toolDeclaredFields(tool)
	var errs []error
	for _, field := range fn.BuiltinArgs.Required {
		if _, ok := declared[field]; ok {
			continue
		}
		errs = append(errs, &toolHandlerArgError{
			tool: tool.Name, origin: describeToolOrigin(tool), builtin: fn.Name, field: field,
		})
	}
	return errs
}

// toolDeclaredFields is the set of field names a tool's input schema declares.
func toolDeclaredFields(tool *Tool) map[string]struct{} {
	out := map[string]struct{}{}
	if tool == nil || len(tool.InputSchema) == 0 {
		return out
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		return out
	}
	for name := range schema.Properties {
		out[name] = struct{}{}
	}
	return out
}

// functionOrAliasExists resolves a name the way the runtime does: exact match
// on the primary name first, then a case-insensitive scan of the builtins'
// declared @alias names, which is how `memqlVersion()` reaches the
// `serviceVersion` builtin (lookupBuiltinFunction, #2707).
//
// A gate that refuses a boot must not be narrower than the resolver it stands
// in front of, or it invents a failure the runtime would not have had.
func functionOrAliasExists(functions *FunctionRegistry, name string) bool {
	return resolveFunctionOrAlias(functions, name) != nil
}

// resolveFunctionOrAlias is functionOrAliasExists returning what it found.
func resolveFunctionOrAlias(functions *FunctionRegistry, name string) *Function {
	trimmed := strings.TrimSpace(name)
	if fn, err := functions.Get(trimmed); err == nil && fn != nil {
		return fn
	}
	var found *Function
	functions.Range(func(_ string, cand *Function) bool {
		if cand == nil || !cand.IsBuiltin() {
			return true
		}
		for _, alias := range cand.BuiltinAliases {
			if strings.EqualFold(alias, trimmed) {
				found = cand
				return false
			}
		}
		return true
	})
	return found
}

// describeToolOrigin renders a tool's origin for an error message, falling
// back to a stable placeholder for programmatically-built tools.
func describeToolOrigin(tool *Tool) string {
	if origin := strings.TrimSpace(tool.Origin); origin != "" {
		return origin
	}
	return "no origin recorded"
}

// recordToolHandlerTargetProblems runs the resolution pass and folds each
// unresolved target onto the load report as a Skip, so the existing
// strict-boot gate refuses the boot. Returns the problems it recorded, so the
// caller can log the same slice rather than resolving twice.
func recordToolHandlerTargetProblems(report *LoadReport, tools *ToolRegistry, functions *FunctionRegistry) []error {
	errs := validateToolHandlerTargets(tools, functions)
	for _, err := range errs {
		report.AddSkip(baseloader.Skip{
			Component: "memql.toolHandlerResolution",
			Keyword:   "tool",
			Phase:     "resolve",
			Err:       err.Error(),
			Code:      baseloader.RuleCode(err),
		})
	}
	return errs
}
