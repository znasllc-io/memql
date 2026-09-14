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
// function, mutation, query or builtin that the registry does not carry.
// Deterministic order (tool name, then target) so a boot failure reads the
// same on every replica.
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
	for _, name := range names {
		tool := snapshot[name]
		for _, target := range toolHandlerTargets(tool) {
			if functionOrAliasExists(functions, target) {
				continue
			}
			errs = append(errs, fmt.Errorf(
				"tool %q (%s): @handler names %q, which is not a registered function, query, mutation or builtin -- the tool registers and is advertised to the model anyway, and fails only when a model calls it",
				tool.Name, describeToolOrigin(tool), target))
		}
	}
	return errs
}

// functionOrAliasExists resolves a name the way the runtime does: exact match
// on the primary name first, then a case-insensitive scan of the builtins'
// declared @alias names, which is how `memqlVersion()` reaches the
// `serviceVersion` builtin (lookupBuiltinFunction, #2707).
//
// A gate that refuses a boot must not be narrower than the resolver it stands
// in front of, or it invents a failure the runtime would not have had.
func functionOrAliasExists(functions *FunctionRegistry, name string) bool {
	trimmed := strings.TrimSpace(name)
	if functions.Has(trimmed) {
		return true
	}
	found := false
	functions.Range(func(_ string, cand *Function) bool {
		if cand == nil || !cand.IsBuiltin() {
			return true
		}
		for _, alias := range cand.BuiltinAliases {
			if strings.EqualFold(alias, trimmed) {
				found = true
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
		})
	}
	return errs
}
