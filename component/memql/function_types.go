package memql

import (
	"fmt"
	"regexp"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/baseregistry"
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
	// DocComment carries the /// doc-comment block captured above the
	// declaration (memql#2633). Populated, not yet consumed: description
	// sourcing flips to it in #2634 (/// wins over @description).
	DocComment string

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

	// V1Filter is a query's edition-2026 filter lambda as written (memql#5366).
	// Expr already holds its lowering -- the loader lowers it while specs are
	// still loading, so without the predicate registry -- and the engine's
	// Init pass lowers it again from here with the registry in hand, to check
	// every predicate application's kind (lowerAllPushdownPositions). Nil for
	// a legacy filter.
	V1Filter *languageParser.LambdaExpr

	// LogicBody is a logic's statement body (epic memql#5370), compiled at
	// LOAD by compiler.CompileBody: the executor's step list, in source
	// order, in the shape an automation's `steps` compile to. The sequence
	// runner executes it (LogicRunner). Nil for every other function.
	LogicBody []map[string]any

	// MutationTemplate is the parsed mutation template (for mutation functions).
	// Only set when FunctionKind == "mutation".
	MutationTemplate *FunctionMutationTemplate

	// Origin tracks where this function was loaded from (file path).
	Origin string

	// Type identifies the function kind: "builtin" for system functions, empty for user-defined.
	Type string

	// FunctionKind identifies if this is a query, mutation, logic,
	// automation, or builtin function ("builtin" stamped by the converter
	// since #2608).
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

	// ServerOnly marks the construct as not callable by clients over the
	// wire (memql#2800). Set by @serverOnly; default false = callable.
	//
	// Distinct from Enabled, which is binary and applies to EVERY caller:
	// @disabled would also break the server-side Go that needs these
	// constructs, which is exactly the case @serverOnly exists to serve --
	// the auth path resolving `sub` -> user before an actor exists, the
	// kill-switch automation acting on a user other than the actor.
	//
	// Enforced in functionValidator.expandFunctionCall and the engine's
	// mutation/logic dispatch, against auth.OriginFromContext. Unlike the
	// retired @internal (#2620 / #2708), which only hid a construct from
	// discovery while leaving it callable, this one is checked at execution.
	ServerOnly bool

	// RequiresRank is the role slug a caller must hold, at that rank or
	// above, to invoke this construct -- `@requiresRank("developer")`.
	// Empty on every construct that declares none, which is nearly all of
	// them.
	//
	// This is the SERVER-SIDE COUNTERPART to the OS shell's per-surface
	// role requirement (epic memql#4832, D6). It is declared on the
	// CONSTRUCT and not on a surface, and that is the crux of the
	// decision: a surface is a set of constructs, and an app id arriving
	// from a browser is a claim rather than a fact -- gating on one would
	// trust the client to report the very thing being gated. The app
	// manifest's `roles` field becomes the presentation MIRROR of this.
	RequiresRank string

	// RequiresCapability is the (verb, resource) grant a caller must hold to
	// invoke this construct -- `@requiresCapability("read", "principal")`.
	// The zero value on every construct that declares none.
	//
	// THE SIBLING OF RequiresRank, NOT AN ALTERNATIVE TO IT (epic memql#5166,
	// D11). A rank is a FLOOR on the ladder; a capability is a GRANT, and a
	// cluster can hold one without the other -- developer ranks above admin
	// and holds strictly fewer principal verbs. Both together require both.
	RequiresCapability CapabilityRequirement

	// CacheTTL is the cache duration for query results (queries only),
	// from @cache(N). "0" means never cache.
	CacheTTL string

	// Deprecated / Version / Timeout / RateLimitRequests / RateLimitPer /
	// Retry / Idempotent / Audit were fields here until memql#5375. The
	// registry hands out CLONES, so a field this struct's Clone does not
	// name is a field every registered construct loses -- which is how the
	// RequiresRank note above came to be written. These went the other
	// way: they were cloned faithfully, rendered by help() and by editor
	// hover, and populated by annotations every allow-list refused, so the
	// only value any of them ever held was the zero value.

	// ShopperFormExtension is the route this mutation adds its own fields
	// to -- `@shopperFormExtension(pack="wholesale", form="application")`.
	// Nil on every construct that declares none, which is nearly all of
	// them (design record 2026-09-21).
	ShopperFormExtension *ShopperFormExtensionDecl

	// Audit indicates all calls should be logged (mutations only)
	Audit bool

	// MCPPromoted indicates the construct carries @mcp: it is promoted into its
	// own first-class MCP tool (queries / mutations) rather than only being
	// reachable through the generic run_query / run_mutation dispatchers.
	// (epic memql#1529 Phase 4 #1534)
	MCPPromoted bool

	// LatestMode marks a query whose `asOf latest` clause reads the live
	// tip of the append-only stream, so its result is CLOCK-DEPENDENT
	// (not reproducible). It is the contract surface for temporal
	// visibility (core-builtins ADR §2.3, story memql#2305): consumers
	// read it to see the query is time-dependent. Auto-derived at load
	// from a `asOf latest` clause in the body (authoritative) OR an
	// explicit `@latestMode` annotation. A query with `asOf <explicit
	// timestamp>` is deterministic and leaves this false. Queries only.
	LatestMode bool
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
		Name:         f.Name,
		Description:  f.Description,
		DocComment:   f.DocComment,
		UsageDoc:     f.UsageDoc,
		ExprSource:   f.ExprSource,
		BoundConcept: f.BoundConcept,
		Expr:         cloneExpressionNode(f.Expr),
		// The v1 filter as written: a parsed AST, read-only after parsing,
		// so shared. It MUST be listed -- the registry hands out clones, and
		// a clone without it is a query the Init pass cannot re-check.
		V1Filter:         f.V1Filter,
		LogicBody:        f.LogicBody, // compiled at load -- read-only at runtime
		MutationTemplate: mutationCopy,
		Origin:           f.Origin,
		Type:             f.Type,
		FunctionKind:     f.FunctionKind,
		Executor:         f.Executor,
		BuiltinAliases:   append([]string(nil), f.BuiltinAliases...),
		BuiltinArgs:      f.BuiltinArgs.clone(),
		ArgsSchema:       argsSchemaCopy,
		// Attribute values
		Enabled:    f.Enabled,
		ServerOnly: f.ServerOnly,
		// The actor-rank floor (epic memql#4832, D6). It has to be listed
		// here, and the reason is worth a line: the registry hands out CLONES,
		// so a field this function does not name is a field every registered
		// construct loses. The loader resolved @requiresRank correctly and the
		// enforcement read it correctly, and the floor was still absent
		// everywhere -- a gate that parsed, validated and gated nothing.
		RequiresRank:       f.RequiresRank,
		RequiresCapability: f.RequiresCapability,
		// LISTED FOR THE REASON DIRECTLY ABOVE: the registry hands out
		// clones, so an unlisted field is a field every registered construct
		// loses. An extension that parsed and then vanished from the clone
		// would be a form silently not collecting a client's fields, which
		// is the exact failure this seam was built to end.
		ShopperFormExtension: f.ShopperFormExtension,
		CacheTTL:             f.CacheTTL,
		MCPPromoted:          f.MCPPromoted,
		LatestMode:           f.LatestMode,
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
	return r.Registry.Add(QualifyConstruct(ConstructNamespaceForOrigin(fn.Origin), fn.Name), fn)
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
	return r.Registry.Upsert(QualifyConstruct(ConstructNamespaceForOrigin(fn.Origin), fn.Name), fn)
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
//	caller.role          -- cluster-wide role (owner/admin/developer/writer/reader)
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
	// Description is the resolved field documentation -- the /// doc
	// comment captured above the field (memql#2634). It is the ONLY
	// channel: an args-field @description is rejected at load (memql#3336).
	Description string
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
	// Secret marks an arg that writes into a field the bound concept
	// annotates `@secret`. A rejected value on such a field is never quoted
	// into a validation error message -- the message names the argument and
	// the declared constraint, and substitutes redactedArgValue for the value
	// itself (memql#3036).
	//
	// Set during function-loader translation, exactly as patternRegex is:
	// resolving it needs the concept registry, which the loader has and the
	// validator deliberately does not (the validator is ctx-free and
	// registry-free by design). See markSecretArgsFields.
	//
	// The zero value is false, so a field the loader never stamped -- a test
	// fixture, an unbound function, a concept the registry cannot resolve --
	// behaves exactly as before. That is the safe default for a diagnostic,
	// though note it fails OPEN: a concept that fails to resolve leaves its
	// secret args unredacted rather than over-redacting every message.
	Secret bool
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
		Description:  f.Description,
		Type:         f.Type,
		Optional:     f.Optional,
		Format:       f.Format,
		MaxLength:    f.MaxLength,
		Pattern:      f.Pattern,
		patternRegex: f.patternRegex,
		Secret:       f.Secret,
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
