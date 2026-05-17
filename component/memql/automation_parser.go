package memql

// automation_parser.go is the dedicated parser entry point for
// `automation NAME { step <name> { ... } ... }` declarations.
//
// Automations are event-triggered workflows. The struct-form body
// (a list of named `step <name> { ... }` blocks) is rewritten to
// the procedural `func (Automation) NAME(ctx any) { name :=
// <callOrBlock>; ...; return ctx, nil }` form; the body grammar
// lives in the general parser + the automation step-grammar.
//
// This parser is a thin shell whose value is the per-construct
// annotation allow-list -- typos / wrong-construct annotations
// surface as hard parse-time errors.
//
// NOTE: automations land in the SpecRegistry's sibling
// FunctionRegistry via the unified function loader, but the
// automations engine actually consumes them via
// component/automations/loader.go. See unified_loader.go in that
// package for the runtime registration path; this parser only
// handles the FunctionRegistry side (used by the SI tool surface
// and documentation queries).

import (
	"fmt"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// allowedAutomationAnnotations enumerates the annotations an
// `automation NAME` declaration may carry.
//
//   @description("text")              documentation
//   @enabled / @disabled               lifecycle
//   @deprecated("migration hint")      surfaces a warning at use
//
// Trigger surface (one of @trigger or @schedule is required):
//   @trigger(event="...", concept=..., partition="*")
//                                     event-driven trigger
//   @trigger(event="<system-event>")   non-graph triggers (system.startup,
//                                     cognition.capability.unmet, etc.)
//   @schedule("0 */5 * * * *")         cron-driven trigger
//   @async                             dispatch async (default: sync)
//   @filter("expr")                    additional filter on triggering events
//
// Dependency declarations:
//   @useConcept(<bareName>)
//   @useShape(<bareName>)
//   @useSpec(<name>)
//   @useTrait(<name>)
//   @useQuery(<name>)                  automations read via queries
//   @useMutation(<name>)               automations write via mutations
//   @useLogic(<name>)                  step bodies call logic blocks
//   @useBuiltin(<name>)
//   @usePrompt(<name>)
//   @useTool(<name>)
//   @useAutomation(<name>)             nested-automation step references
var allowedAutomationAnnotations = map[string]bool{
	"description":   true,
	"enabled":       true,
	"disabled":      true,
	"deprecated":    true,
	"trigger":       true,
	"schedule":      true,
	"async":         true,
	"filter":        true,
	"useConcept":    true,
	"useShape":      true,
	"useSpec":       true,
	"useTrait":      true,
	"useQuery":      true,
	"useMutation":   true,
	"useLogic":      true,
	"useBuiltin":    true,
	"usePrompt":     true,
	"useTool":       true,
	"useAutomation": true,
}

// parseAutomationMemQL is the dedicated entry point for automation
// sources. Validates the annotation surface, then delegates
// structural parsing to the shared function-conversion path.
func parseAutomationMemQL(expectedName, origin, source string, conceptRegistry memoryNodes.Registry) (*Function, error) {
	if err := baseparser.ValidateConstructAnnotations(source, "automation", allowedAutomationAnnotations); err != nil {
		return nil, fmt.Errorf("%s: %w", origin, err)
	}
	return tryParseNewFunctionSyntax(expectedName, "automation", source, origin, conceptRegistry)
}
