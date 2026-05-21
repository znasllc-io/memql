package memql

// query_parser.go is the dedicated parser entry point for
// `query NAME { args { ... } filter ...; shape ... }` declarations.
//
// Queries are read-only filter+projection functions over the
// memory graph. The struct-form body is rewritten to the procedural
// `func (Query) NAME(ctx any) (any, error) { return <expr>, nil }`
// form by the existing rewriter; the body grammar (filter
// expressions, `;` separation, sort/limit/depth modifiers, shape
// reference) is the responsibility of the general parser.
//
// This parser is a thin shell whose value is the per-construct
// annotation allow-list -- typos / wrong-construct annotations
// surface as hard parse-time errors instead of silently sticking
// to FunctionDef.Attributes.

import (
	"fmt"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// allowedQueryAnnotations enumerates the annotations a `query NAME`
// declaration may carry.
//
//	@description("text")            documentation
//	@enabled / @disabled             lifecycle
//	@deprecated("migration hint")    surfaces a warning at use
//	@internal                        hide from external surfaces (docs, SI tools)
//
// Performance / behaviour annotations:
//
//	@cacheTTL("5m")                  cache hint (see engine cache subsystem)
//	@timeout("30s")                  hard execution cap
//	@rateLimit(requests=N, per="1h") per-caller throttle
//	@retry(N)                        retry count on failure
//	@audit                           emit v1:identity:auditEvent on call
//
// Access control:
//
//	@role("admin")                   required cluster role
//	@permission("...")               required permission key
//
// Dependency declarations (every named target referenced in the
// body must be declared):
//
//	@useConcept(<bareName>)
//	@useShape(<bareName>)
//	@useSpec(<name>)
//	@useTrait(<name>)
//	@useQuery(<name>)        -- queries may compose other queries
//	@useLogic(<name>)        -- queries may invoke logic blocks
//	@useBuiltin(<name>)
var allowedQueryAnnotations = map[string]bool{
	"description": true,
	"enabled":     true,
	"disabled":    true,
	"deprecated":  true,
	"internal":    true,
	"cacheTTL":    true,
	"timeout":     true,
	"rateLimit":   true,
	"retry":       true,
	"audit":       true,
	"role":        true,
	"permission":  true,
	"useConcept":  true,
	"useShape":    true,
	"useSpec":     true,
	"useTrait":    true,
	"useQuery":    true,
	"useLogic":    true,
	"useBuiltin":  true,
	// `@public` is a parse-only marker introduced by issue #54
	// (per-row authorization audit). It carries no runtime
	// semantics; the validator treats it as the author's explicit
	// acknowledgement that this query is intentionally readable
	// without a caller-scope filter (concept catalogs, login-path
	// lookups before authentication, etc.). Documented in
	// `docs/auth/per-row-authz-audit.md`.
	"public": true,
}

// parseQueryMemQL is the dedicated entry point for query sources.
// Validates the annotation surface, then delegates structural
// parsing to the shared function-conversion path.
func parseQueryMemQL(expectedName, origin, source string, conceptRegistry memoryNodes.Registry) (*Function, error) {
	if err := baseparser.ValidateConstructAnnotations(source, "query", allowedQueryAnnotations); err != nil {
		return nil, fmt.Errorf("%s: %w", origin, err)
	}
	return tryParseNewFunctionSyntax(expectedName, "query", source, origin, conceptRegistry)
}
