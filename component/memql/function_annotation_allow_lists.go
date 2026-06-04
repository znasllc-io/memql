package memql

// function_annotation_allow_lists.go centralises the four per-
// construct annotation allow-lists the unified function loader's
// pre-parse validator consults. The lists previously lived inside
// the dedicated per-construct sub-parsers
// (query_parser.go / mutation_parser.go / logic_parser.go /
// automation_parser.go); those sub-parsers were retired in #310
// alongside the rest of the legacy hand-rolled parser surface, but
// the annotation allow-list pre-flight stays -- it's a load-time
// gate that surfaces typo'd or stale annotations as hard parse
// errors instead of letting them slip through to FunctionDef's
// generic attribute map.
//
// Maintaining these lists in one place (instead of one per kind
// across four files) makes adding a new construct-level annotation
// a single-edit operation, and the unified function loader's
// `constructAnnotationAllowLists` map (in unified_functions_loader.go)
// is the sole consumer.
//
// The retired `@use*` family (`useConcept` / `useShape` / `useTrait` /
// `useSpec` / `useQuery` / `useMutation` / `useLogic` / `useBuiltin` /
// `usePrompt` / `useTool` / `useAutomation`) is deliberately NOT listed
// here: `ValidateConstructAnnotations` (baseparser/iface.go) hard-rejects
// anything matching the `@use*` prefix with a migration hint BEFORE the
// allow-list is consulted, so listing them was a dead no-op. The audit in
// #964 confirmed zero remaining occurrences across the tree; the no-op
// entries were pruned in #966.

var allowedQueryAnnotations = map[string]bool{
	"description": true,
	"enabled":     true,
	"disabled":    true,
	"internal":    true,
	// `@public` is a parse-only marker introduced by issue #54
	// (per-row authorization audit). It carries no runtime
	// semantics; the validator treats it as the author's explicit
	// acknowledgement that this query is intentionally readable
	// without a caller-scope filter (concept catalogs, login-path
	// lookups before authentication, etc.). Documented in
	// `docs/auth/per-row-authz-audit.md`.
	"public": true,
}

var allowedMutationAnnotations = map[string]bool{
	"description": true,
	"enabled":     true,
	"disabled":    true,
	"internal":    true,
	"actor":       true,
	// `@public` is a parse-only marker introduced by issue #54
	// (per-row authorization audit). It carries no runtime
	// semantics; the validator treats it as the author's explicit
	// acknowledgement that this mutation is intentionally callable
	// without a caller-scope check (sign-up / login flows that run
	// pre-authentication, etc.). Documented in
	// `docs/auth/per-row-authz-audit.md`.
	"public": true,
}

var allowedLogicAnnotations = map[string]bool{
	"description": true,
	"enabled":     true,
	"disabled":    true,
}

var allowedAutomationAnnotations = map[string]bool{
	"description": true,
	"enabled":     true,
	"disabled":    true,
	"trigger":     true,
	"filter":      true,
}
