package memql

// logic_parser.go is the dedicated parser entry point for
// `logic NAME { args { ... } body { ... } }` declarations.
//
// Logic blocks are procedural building blocks called from automation
// steps -- they wrap a sequence of statements (variable assignments,
// queries, mutations, conditionals) behind a named handle.
//
// The body grammar (statement list, control flow, return) is shared
// with every other procedural construct (query / mutation /
// automation) and lives in component/language/parser. Rather than
// reimplement that grammar per-construct, each dedicated parser is a
// thin shell that:
//
//   1. Validates annotations against this construct's allow-list
//      (hard-rejecting typos / wrong-construct annotations).
//   2. Delegates structural parsing to tryParseNewFunctionSyntax,
//      which applies the rewriter + general parser + conversion to
//      *Function.
//
// The per-construct annotation allow-list is the value the dedicated
// parser adds. Without it the general parser silently drops unknown
// attributes onto FunctionDef.Attributes, and downstream consumers
// have no idea the author made a typo.

import (
	"fmt"

	memoryNodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/memql/baseparser"
)

// allowedLogicAnnotations enumerates the annotations a `logic NAME`
// block may carry. Anything else is hard-rejected at parse time.
//
//   @description("text")            documentation
//   @enabled  /  @disabled          lifecycle (toggle without delete)
//   @deprecated("migration hint")   surfaces a warning at use
//
// Dependency declarations (every named target referenced in the body
// must have a matching declaration):
//   @useConcept(<bareName>)
//   @useShape(<bareName>)
//   @useSpec(<name>)
//   @useTrait(<name>)
//   @useQuery(<name>)
//   @useMutation(<name>)
//   @useLogic(<name>)
//   @useBuiltin(<name>)
//   @usePrompt(<name>)
//   @useTool(<name>)
var allowedLogicAnnotations = map[string]bool{
	"description": true,
	"enabled":     true,
	"disabled":    true,
	"deprecated":  true,
	"useConcept":  true,
	"useShape":    true,
	"useSpec":     true,
	"useTrait":    true,
	"useQuery":    true,
	"useMutation": true,
	"useLogic":    true,
	"useBuiltin":  true,
	"usePrompt":   true,
	"useTool":     true,
}

// parseLogicMemQL is the dedicated entry point for logic sources.
// Validates the annotation surface, then delegates structural
// parsing to the shared function-conversion path.
func parseLogicMemQL(expectedName, origin, source string, conceptRegistry memoryNodes.Registry) (*Function, error) {
	if err := baseparser.ValidateConstructAnnotations(source, "logic", allowedLogicAnnotations); err != nil {
		return nil, fmt.Errorf("%s: %w", origin, err)
	}
	return tryParseNewFunctionSyntax(expectedName, "logic", source, origin, conceptRegistry)
}
