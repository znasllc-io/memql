package memql

import (
	"fmt"
	"regexp"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseregistry"
)

var (
	// functionNamePattern enforces camelCase naming for functions.
	// Must start with lowercase letter, followed by letters/digits.
	functionNamePattern = regexp.MustCompile(`^[a-z]+[A-Za-z0-9]*$`)
)

// FunctionType identifies the kind of function.
const (
	// FunctionTypeBuiltin identifies system functions with Go executor logic.
	FunctionTypeBuiltin = "builtin"
	// FunctionTypeUserDefined identifies user-defined functions from .memql files.
	FunctionTypeUserDefined = ""
)

// BuiltinArgProfile defines supported argument parsing contracts for builtin functions.
type BuiltinArgProfile string

const (
	BuiltinArgProfileNone                   BuiltinArgProfile = "none"
	BuiltinArgProfileObject                 BuiltinArgProfile = "object"
	BuiltinArgProfileOptionalObject         BuiltinArgProfile = "optionalObject"
	BuiltinArgProfileStringOrObject         BuiltinArgProfile = "stringOrObject"
	BuiltinArgProfileOptionalString         BuiltinArgProfile = "optionalString"
	BuiltinArgProfileOptionalStringOrObject BuiltinArgProfile = "optionalStringOrObject"
)

// BuiltinArgContract describes parse-time argument schema for builtin function calls.
type BuiltinArgContract struct {
	Profile              BuiltinArgProfile
	StringKey            string
	Required             []string
	Properties           map[string]string
	AdditionalProperties *bool
}

func (c *BuiltinArgContract) clone() *BuiltinArgContract {
	if c == nil {
		return nil
	}
	out := &BuiltinArgContract{
		Profile:   c.Profile,
		StringKey: c.StringKey,
		Required:  append([]string(nil), c.Required...),
	}
	if c.Properties != nil {
		out.Properties = make(map[string]string, len(c.Properties))
		for k, v := range c.Properties {
			out.Properties[k] = v
		}
	}
	if c.AdditionalProperties != nil {
		flag := *c.AdditionalProperties
		out.AdditionalProperties = &flag
	}
	return out
}

// Function represents a named, reusable MemQL query that can be invoked with functionName({args}) syntax.
type Function struct {
	// Name is the unique identifier (camelCase, e.g., "activeConversations").
	Name string

	// Description provides human-readable context for the function.
	Description string

	// UsageDoc contains the leading documentation comment block extracted from
	// the function .memql file. This is intended for on-demand agent guidance
	// (e.g., via describeFunction/help tooling), not for always-on prompts.
	UsageDoc string

	// ExprSource is the raw MemQL expression string (for user-defined functions).
	ExprSource string

	// BoundConcept is the fully qualified concept ID resolved from the
	// function's single `use` declaration. Set at load time for query
	// and mutation functions. Used to substitute bare `concept` keywords
	// and implicit insert() targets in the expression body.
	BoundConcept string

	// Expr is the parsed expression AST (for user-defined functions).
	Expr ExpressionNode

	// LogicSteps carries the parsed multi-step body for Logic functions
	// whose body has intermediate `name := <call>` steps before the
	// `_return` terminator. When set, the engine dispatches the call
	// through the wired LogicRunner instead of evaluating Expr directly.
	// Single-statement Logic bodies leave this nil and run through Expr.
	LogicSteps *languageParser.AutomationDef

	// MutationTemplate is the parsed mutation template (for mutation functions).
	// Only set when FunctionKind == "mutation".
	MutationTemplate *FunctionMutationTemplate

	// Origin tracks where this function was loaded from (file path).
	Origin string

	// Type identifies the function kind: "builtin" for system functions, empty for user-defined.
	Type string

	// FunctionKind identifies if this is a query, mutation, or automation function.
	// Empty string or "query" for query functions.
	FunctionKind string

	// Executor names the Go executor for builtin functions (e.g., "concepts", "memqlDocs").
	Executor string
	// BuiltinAliases lists alternate names accepted by the parser for this builtin.
	BuiltinAliases []string
	// BuiltinArgs defines parse-time argument contract for builtin calls.
	BuiltinArgs *BuiltinArgContract

	// ArgsSchema defines argument assertions derived from the
	// function's `args { ... }` block.
	ArgsSchema *ArgsSchemaConfig

	// --- Attribute values ---

	// Enabled indicates if the function is active. Default is true; use
	// @disabled to opt out. @enabled is a no-op accepted for legacy DSL.
	Enabled bool

	// Deprecated contains deprecation message if set (empty = not deprecated)
	Deprecated string

	// Version is the function version tag
	Version string

	// Internal indicates if the function is hidden from external API
	Internal bool

	// Role restricts access to users with this role
	Role string

	// Permission requires this permission to call
	Permission string

	// Timeout is the maximum execution time
	Timeout string

	// CacheTTL is the cache duration for query results (queries only)
	CacheTTL string

	// RateLimitRequests is the number of requests allowed
	RateLimitRequests int

	// RateLimitPer is the rate limit window (e.g., "1m", "1h")
	RateLimitPer string

	// Retry is the number of retry attempts (mutations only)
	Retry int

	// Idempotent indicates the mutation is safe to retry (mutations only)
	Idempotent bool

	// Audit indicates all calls should be logged (mutations only)
	Audit bool

	// MCPPromoted indicates the construct carries @mcp: it is promoted into its
	// own first-class MCP tool (queries / mutations) rather than only being
	// reachable through the generic run_query / run_mutation dispatchers.
	// (epic memql#1529 Phase 4 #1534)
	MCPPromoted bool
}

func (f *Function) clone() *Function {
	if f == nil {
		return nil
	}
	var argsSchemaCopy *ArgsSchemaConfig
	if f.ArgsSchema != nil {
		argsSchemaCopy = f.ArgsSchema.clone()
	}
	var mutationCopy *FunctionMutationTemplate
	if f.MutationTemplate != nil {
		clone := *f.MutationTemplate
		clone.PayloadTemplate = cloneAny(clone.PayloadTemplate)
		mutationCopy = &clone
	}
	return &Function{
		Name:             f.Name,
		Description:      f.Description,
		UsageDoc:         f.UsageDoc,
		ExprSource:       f.ExprSource,
		BoundConcept:     f.BoundConcept,
		Expr:             cloneExpressionNode(f.Expr),
		LogicSteps:       f.LogicSteps, // shared parsed AST -- read-only at runtime
		MutationTemplate: mutationCopy,
		Origin:           f.Origin,
		Type:             f.Type,
		FunctionKind:     f.FunctionKind,
		Executor:         f.Executor,
		BuiltinAliases:   append([]string(nil), f.BuiltinAliases...),
		BuiltinArgs:      f.BuiltinArgs.clone(),
		ArgsSchema:       argsSchemaCopy,
		// Attribute values
		Enabled:           f.Enabled,
		Deprecated:        f.Deprecated,
		Version:           f.Version,
		Internal:          f.Internal,
		Role:              f.Role,
		Permission:        f.Permission,
		Timeout:           f.Timeout,
		CacheTTL:          f.CacheTTL,
		RateLimitRequests: f.RateLimitRequests,
		RateLimitPer:      f.RateLimitPer,
		Retry:             f.Retry,
		Idempotent:        f.Idempotent,
		Audit:             f.Audit,
		MCPPromoted:       f.MCPPromoted,
	}
}

func cloneMapAny(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = cloneAny(v)
	}
	return dst
}

func cloneAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneMapAny(t)
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = cloneAny(t[i])
		}
		return out
	default:
		return v
	}
}

// IsBuiltin returns true if this is a builtin function.
func (f *Function) IsBuiltin() bool {
	return f != nil && f.Type == FunctionTypeBuiltin
}

// FunctionRegistry stores globally registered functions.
type FunctionRegistry struct {
	*baseregistry.Registry[Function]
}

func newFunctionRegistry() *FunctionRegistry {
	return &FunctionRegistry{
		Registry: baseregistry.New[Function]("function",
			func(f *Function) *Function { return f.clone() },
			validateFunctionName),
	}
}

// add inserts a function into the registry. Errors when the name
// is already taken.
func (r *FunctionRegistry) add(fn *Function) error {
	if fn == nil {
		return fmt.Errorf("function is nil")
	}
	return r.Registry.Add(fn.Name, fn)
}

// Upsert inserts a function or replaces an existing entry with the
// same name. Used by the unified loader (Pass 2 of the DSL
// restructure) so it can register functions from the new tree on
// top of the legacy loader's registrations without erroring on
// duplicates.
func (r *FunctionRegistry) Upsert(fn *Function) error {
	if fn == nil {
		return fmt.Errorf("function is nil")
	}
	return r.Registry.Upsert(fn.Name, fn)
}

func validateFunctionName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("function name is required")
	}
	if !functionNamePattern.MatchString(trimmed) {
		return fmt.Errorf("function name %q must be camelCase (letters/digits, starting lowercase)", name)
	}
	return nil
}

// ArgRefExpression references a value from the function arguments.
// Used in function expressions as $args.fieldName syntax.
type ArgRefExpression struct {
	// Path is the dot-separated path to the argument field (e.g., "partitionId", "options.limit").
	Path string
}

func (*ArgRefExpression) isExpressionNode() {}

// CallerRefExpression references the caller() accessor -- the
// authenticated user's AccessContext at query-evaluation time.
// Dotted paths like caller.userId / caller.role are resolved by
// pulling the AccessContext from the request ctx via
// component/auth.AccessFromContext.
//
// Exposed fields (resolved at eval time):
//
//	caller.userId        -- v1:identity:user.id
//	caller.primaryEmail  -- primary email
//	caller.role          -- cluster-wide role (owner/admin/writer/reader)
//	caller.identityId    -- v1:identity:identity.id used for the request
//	caller.isOwner       -- bool short-circuit for owner-bypass paths
//
// When no AccessContext is attached (e.g. no-auth dev mode), the
// resolver falls back to treating the caller as an owner.
type CallerRefExpression struct{}

func (*CallerRefExpression) isExpressionNode() {}

// ConditionalFilterExpression wraps a filter that should only be applied
// if the referenced argument field exists. Uses ?filter syntax.
type ConditionalFilterExpression struct {
	// ArgPath is the argument path that must exist for this filter to apply.
	ArgPath string
	// Filter is the underlying comparison expression to conditionally apply.
	Filter ExpressionNode
}

func (*ConditionalFilterExpression) isExpressionNode() {}

// ArgsSchemaConfig defines argument assertions for functions.
// Populated from the function's `args { ... }` block.
type ArgsSchemaConfig struct {
	// Fields defines the expected argument fields and their types
	Fields []*FunctionArgsField
	// AdditionalProperties controls whether unknown top-level args are allowed.
	// nil means default behavior (currently allow).
	AdditionalProperties *bool
}

// clone creates a deep copy of the ArgsSchemaConfig.
func (a *ArgsSchemaConfig) clone() *ArgsSchemaConfig {
	if a == nil {
		return nil
	}
	clone := &ArgsSchemaConfig{
		Fields: make([]*FunctionArgsField, len(a.Fields)),
	}
	if a.AdditionalProperties != nil {
		v := *a.AdditionalProperties
		clone.AdditionalProperties = &v
	}
	for i, f := range a.Fields {
		clone.Fields[i] = f.clone()
	}
	return clone
}

// FunctionArgsField defines a single argument field's type assertion.
type FunctionArgsField struct {
	// Name is the field name (e.g., "userId", "email")
	Name string

	// Type is the expected type: "string", "number", "bool", "object", "array", "any"
	Type string

	// Optional indicates if the field can be missing (false means required)
	Optional bool

	// Nested contains nested field assertions for object types
	Nested []*FunctionArgsField

	// Enum restricts values to the provided set.
	Enum []any
	// Minimum/Maximum constrain numeric values.
	Minimum *float64
	Maximum *float64
	// Format constrains string values (for example, date-time).
	Format string
	// MaxLength caps string args at the given rune count. Zero means
	// no cap. Sourced from `@maxLength(N)` in the DSL.
	MaxLength int
	// Pattern is the source-string of the regex a value must match.
	// Empty means no pattern. Sourced from `@pattern("regex")` in
	// the DSL. Compiled pattern lives on patternRegex (set during
	// function-loader translation; nil when Pattern is empty or the
	// loader hasn't run yet).
	Pattern      string
	patternRegex *regexp.Regexp
	// AdditionalProperties controls unknown keys for object values.
	AdditionalProperties *bool
	// Items defines item schema for array values.
	Items *FunctionArgsField
}

// clone creates a deep copy of the FunctionArgsField.
func (f *FunctionArgsField) clone() *FunctionArgsField {
	if f == nil {
		return nil
	}
	clone := &FunctionArgsField{
		Name:         f.Name,
		Type:         f.Type,
		Optional:     f.Optional,
		Format:       f.Format,
		MaxLength:    f.MaxLength,
		Pattern:      f.Pattern,
		patternRegex: f.patternRegex,
	}
	if len(f.Enum) > 0 {
		clone.Enum = append([]any(nil), f.Enum...)
	}
	if f.Minimum != nil {
		v := *f.Minimum
		clone.Minimum = &v
	}
	if f.Maximum != nil {
		v := *f.Maximum
		clone.Maximum = &v
	}
	if f.AdditionalProperties != nil {
		v := *f.AdditionalProperties
		clone.AdditionalProperties = &v
	}
	if len(f.Nested) > 0 {
		clone.Nested = make([]*FunctionArgsField, len(f.Nested))
		for i, n := range f.Nested {
			clone.Nested[i] = n.clone()
		}
	}
	if f.Items != nil {
		clone.Items = f.Items.clone()
	}
	return clone
}
