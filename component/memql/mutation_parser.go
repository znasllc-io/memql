package memql

// mutation_parser.go is the dedicated parser entry point for
// `mutation NAME { args { ... } insert { ... } | update { ... } }`
// declarations.
//
// Mutations are write functions over the memory graph. The
// struct-form body is rewritten to the procedural
// `func (Mutation) NAME(ctx any) (any, error) { insert(...); ... }`
// form; the body grammar (insert/update blocks, field assignments,
// defaults, computed expressions) lives in the general parser.
//
// This parser is a thin shell whose value is the per-construct
// annotation allow-list -- typos / wrong-construct annotations
// surface as hard parse-time errors.

import (
	"fmt"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// allowedMutationAnnotations enumerates the annotations a
// `mutation NAME` declaration may carry.
//
//	@description("text")              documentation
//	@enabled / @disabled               lifecycle
//	@deprecated("migration hint")      surfaces a warning at use
//	@internal                          hide from external surfaces
//
// Mutation-specific behaviour:
//
//	@idempotent                        mark as safe to retry
//	@destructive                       semantic flag: deletes data
//	@requiresConfirmation              UI / SI must confirm before invoking
//	@actor("system" | "caller")        which actor identity stamps createdBy
//
// Performance / reliability:
//
//	@timeout("30s")                    hard execution cap
//	@rateLimit(requests=N, per="1h")   per-caller throttle
//	@retry(N)                          retry count on failure
//	@audit                             emit v1:identity:auditEvent on call
//
// Access control:
//
//	@role("admin")
//	@permission("...")
//
// Dependency declarations:
//
//	@useConcept(<bareName>)            REQUIRED -- a mutation always
//	                                   writes to exactly one concept
//	@useShape(<bareName>)
//	@useSpec(<name>)
//	@useTrait(<name>)
//	@useQuery(<name>)                  mutations may read via queries
//	@useMutation(<name>)               mutations may compose mutations
//	@useLogic(<name>)
//	@useBuiltin(<name>)
var allowedMutationAnnotations = map[string]bool{
	"description":          true,
	"enabled":              true,
	"disabled":             true,
	"deprecated":           true,
	"internal":             true,
	"idempotent":           true,
	"destructive":          true,
	"requiresConfirmation": true,
	"actor":                true,
	"timeout":              true,
	"rateLimit":            true,
	"retry":                true,
	"audit":                true,
	"role":                 true,
	"permission":           true,
	"useConcept":           true,
	"useShape":             true,
	"useSpec":              true,
	"useTrait":             true,
	"useQuery":             true,
	"useMutation":          true,
	"useLogic":             true,
	"useBuiltin":           true,
	// `@public` is a parse-only marker introduced by issue #54
	// (per-row authorization audit). It carries no runtime
	// semantics; the validator treats it as the author's explicit
	// acknowledgement that this mutation is intentionally callable
	// without a caller-scope check (sign-up / login flows that run
	// pre-authentication, etc.). Documented in
	// `docs/auth/per-row-authz-audit.md`.
	"public": true,
}

// parseMutationMemQL is the dedicated entry point for mutation
// sources. Validates the annotation surface, then delegates
// structural parsing to the shared function-conversion path.
func parseMutationMemQL(expectedName, origin, source string, conceptRegistry memoryNodes.Registry) (*Function, error) {
	if err := baseparser.ValidateConstructAnnotations(source, "mutation", allowedMutationAnnotations); err != nil {
		return nil, fmt.Errorf("%s: %w", origin, err)
	}
	return tryParseNewFunctionSyntax(expectedName, "mutation", source, origin, conceptRegistry)
}
