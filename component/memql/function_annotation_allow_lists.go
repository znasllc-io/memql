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
// Note on the `useConcept` / `useShape` / `useTrait` / `useSpec`
// entries: those annotations were retired in #301 (no live `.memql`
// file across any backend repo uses them; the AST no longer carries
// a matching token), but the strings are kept in the allow-list as
// a tolerated no-op so any straggler file that still carries the
// annotation in a comment-stripped header doesn't trip the validator.
// A follow-up sweep can prune them once the audit confirms zero
// remaining occurrences anywhere in the tree.

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
