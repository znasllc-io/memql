package memql

import (
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/language/ast"
)

// QueryPlan represents the parsed structure of a MemQL expression.
type QueryPlan struct {
	Root      ExpressionNode
	Mutations []MutationNode
	// MutationCall is a top-level call to a mutation function (func (Mutation) ...).
	// When set, Root will be nil and Mutations will be empty; Execute will evaluate the
	// function template and run exactly one insert.
	MutationCall *FunctionCallExpression
	// LogicCall is a top-level call to a multi-step Logic function
	// (func (Logic) ... whose body has intermediate `name := <call>`
	// steps before `_return`). When set, Root is nil and Execute
	// dispatches through the wired LogicRunner so step results bind
	// for later steps + the `_return` expression. Single-statement
	// Logic bodies don't set this -- their fn.Expr is evaluated
	// directly via the normal query expression path.
	LogicCall     *FunctionCallExpression
	Filters       []FilterNode
	Relationships []RelationshipNode
	Timestamp     *time.Time
	UseLatest     bool
	Limit         *int
	// After carries an opaque inbound keyset cursor (see cursor.go). When set,
	// the executor pushes a `WHERE (createdAt, id) <keyset> (?, ?)` predicate
	// into SQL and continues from the encoded position. Threaded onto the plan
	// from the request context (ContextWithCursor) by the engine; nil for the
	// first page and unpaginated queries.
	After             *string
	Depth             *int
	Sort              []SortField
	CacheHints        map[string]int64
	Fields            []FieldReference
	ConceptFields     map[string][]FieldReference
	Metadata          metadataSelection
	PayloadSelect     bool
	ShapeTemplate     shapeTemplate
	ShapeTemplateName string // Named shape reference; resolved at execution time
	IncludeBundle     bool   // when true, include bundle in shape response
	InlineSpecs       map[string]*Spec
	// Count, when true, makes the query return a numeric {count: N}
	// aggregate over the matching set instead of the rows themselves.
	// Peeled off the outermost CountExpression by applyDirectiveWrappers.
	Count bool

	// BoundConcept is the concept the executed construct DECLARES it
	// reads -- copied from the resolved query function's BoundConcept,
	// which the loader fills from the construct's signature and its
	// file-top `use` import.
	//
	// Row-authz enforcement resolves the tier from THIS and never from
	// what the filter says (memql#3172, epic decision A). A filter-text
	// detector answers "" for every spelling that is not a top-level
	// `concept==<id>` equality -- naming a row by id, a top-level `||`,
	// a negated concept -- and under enforcement "I could not tell"
	// silently means "not enforced".
	BoundConcept string

	// RowAuthzInjected records that enforcement ANDed a declared tier's
	// predicate into Root. It exists so the result-cache key folds the
	// caller identity in whether or not the injected node happens to sit
	// where planReferencesActor's walk reaches: an enforced read depends
	// on the actor by construction, and a shared key hands one caller's
	// rows to another (memql#3172 finding 2).
	RowAuthzInjected bool

	// RowAuthzConcept names the concept whose declaration was injected,
	// so the ctx-bearing side of the engine can re-read the tier instead
	// of re-deriving it from the expression.
	RowAuthzConcept string
}

// RelationshipNode identifies relationship traversals declared within a query.
type RelationshipNode struct {
	Alias      string
	Definition RelationshipDefinition
	Filters    []FilterNode
	Depth      int
}

// inlineSpecDefinition captures a `spec name = expr` declaration
// parsed from a query string into a deferred definition the engine
// materialises into a *Spec at compile time.
type inlineSpecDefinition struct {
	Name string
	Expr ExpressionNode
}

// ArgReference represents a reference to a named function argument.
// It's stored as the Value in a ComparisonExpression and resolved at
// execution time. Produced by parsing the canonical `args.<path>`
// syntax (see parseArgsReference) or the equivalent `ctx.<path>`
// shorthand (see parseCtxReference); both produce the same AST node
// so the rest of the validator / executor pipeline stays single-
// shape. The legacy `ctx.input.<path>` longhand was retired in
// memql#302.
type ArgReference struct {
	Path string // e.g., "partitionId" or "options.limit"
}

// ActorReference represents a reference to a field on the authenticated
// user's AccessContext. Created by parsing `caller.X` syntax in
// comparison-value position; resolved at execution time by reading
// auth.AccessFromContext(ctx). Dotted paths are supported
// (`caller.userId`, `caller.role`, etc.).
type ActorReference struct {
	Path string
}

// FunctionCallExpression references a named function invocation with arguments.
type FunctionCallExpression struct {
	Name string
	// Args is the JSON object passed to the function (as map[string]any).
	// All functions require an argument object (can be empty {}).
	Args map[string]any
}

func (*FunctionCallExpression) isExpressionNode() {}

// Spec represents a named boolean predicate.
//
// Row-specs compile to SQL WHERE filters (Kind == SpecKindRow).
// Context-specs evaluate in-process against the caller's auth
// context at call time (Kind == SpecKindContext).
type Spec struct {
	Name        string
	Description string
	ExprSource  string
	Expr        ExpressionNode
	Kind        SpecKind
	UsesAI      bool
	Origin      string

	// BoundName is the spec's signature binding -- the shape XOR
	// concept named by `spec <BoundName> <Name>` (epic #2281). The
	// post-load resolveSpecBindings pass resolves it (shape registry
	// first, then concept registry), rewrites the body's bare field
	// references to their underlying access form, and classifies the
	// spec (an @actor shape -> context-spec; a concept or @row shape ->
	// row-spec). Empty for traits (the deliberately-unbound predicate).
	BoundName string

	// IsTrait flags this entry as a trait rather than a spec.
	// Traits share the runtime contract (atomic boolean predicate)
	// but are deliberately unbound (concept-agnostic).
	IsTrait bool
}

func (s *Spec) clone() *Spec {
	if s == nil {
		return nil
	}
	return &Spec{
		Name:        s.Name,
		Description: s.Description,
		ExprSource:  s.ExprSource,
		Expr:        cloneExpressionNode(s.Expr),
		Kind:        s.Kind,
		UsesAI:      s.UsesAI,
		Origin:      s.Origin,
		BoundName:   s.BoundName,
		IsTrait:     s.IsTrait,
	}
}

// isSpecReferenceCandidate reports whether the given identifier could
// be a reference to a registered spec. Spec names are camelCase and
// must not collide with row intrinsics or the payload alias.
func isSpecReferenceCandidate(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	if strings.Contains(trimmed, ".") {
		return false
	}
	switch strings.ToLower(trimmed) {
	case "payload", "meta", "concept", "id", "type", "createdat", "createdby", "schema", "provenance":
		return false
	}
	return specNamePattern.MatchString(trimmed)
}

// AttributeMap stores arbitrary key/value arguments discovered in query stages.
type AttributeMap map[string]any

// FieldReference describes paths to structured fields within memory nodes.
type FieldReference struct {
	Raw      string
	Parts    []string
	Wildcard bool
}

type metadataSelection struct {
	IncludeAll bool
	Fields     map[string]struct{}
}

// ExpressionNode is the interface implemented by all MemQL expression AST nodes.
type ExpressionNode interface {
	isExpressionNode()
}

// AIInvocation describes an ai() call.
type AIInvocation struct {
	TemplateId       string
	ProviderOverride *string
	// ModelOverride picks the concrete model name within the selected
	// provider (e.g. "claude-sonnet-4-5-20250929"). When non-nil and
	// non-empty, the provider is responsible for honouring it on this
	// one Call; all other invocations continue to use the provider's
	// registry-time cfg.Model.
	//
	// Introduced to let a delegated/nested turn route to a stronger
	// reasoning model without forcing the same tier on every other
	// agent in the app.
	ModelOverride *string
	// EnablePromptCache toggles provider-side prompt caching for this
	// one invocation. Currently honoured by the Anthropic provider:
	// a cache_control ephemeral block is attached to the system
	// prompt, so the ~5000-token System scope fence in agentReply.tmpl
	// is cached between turns. Cache misses still work; cache hits
	// cut input cost by ~90% and drop first-token latency.
	EnablePromptCache bool
	CacheSeconds      *int

	// SemanticNamespace opts THIS invocation into the semantic (vector)
	// AI-call cache under the named classification namespace (5.9). Empty
	// (the default) means the invocation uses only the exact-hash cache --
	// no semantic lookup. A namespace is consulted only when it is BOTH
	// set here AND enabled in the per-namespace registry
	// (ai_semantic_cache_registry.go). Set this only for classification /
	// structured-output calls with a stable input->label mapping; never for
	// free-form generation, where a near-duplicate prompt does not imply the
	// same correct answer.
	SemanticNamespace string
}

// AIExpression captures ai() invocation nodes parsed inside MemQL expressions.
type AIExpression struct {
	Invocation *AIInvocation
}

func (*AIExpression) isExpressionNode() {}

// LogicalOp identifies logical operators applied between expressions.
type LogicalOp string

const (
	// LogicalAnd represents a logical conjunction.
	LogicalAnd LogicalOp = "AND"
	// LogicalOr represents a logical disjunction.
	LogicalOr LogicalOp = "OR"
)

// LogicalExpression joins two expressions using a logical operator.
type LogicalExpression struct {
	Op    LogicalOp
	Left  ExpressionNode
	Right ExpressionNode
}

func (*LogicalExpression) isExpressionNode() {}

// SpecReferenceExpression references a named specification.
type SpecReferenceExpression struct {
	Name string
}

func (*SpecReferenceExpression) isExpressionNode() {}

// RelationshipFunction enumerates supported relationship traversal functions.
type RelationshipFunction string

const (
	RelParentOf      RelationshipFunction = "parentOf"
	RelChildOf       RelationshipFunction = "childOf"
	RelAliasOf       RelationshipFunction = "aliasOf"
	RelEquals        RelationshipFunction = "equals"
	RelInteractsWith RelationshipFunction = "interactsWith"
	RelContains      RelationshipFunction = "contains"
	RelOwns          RelationshipFunction = "owns"
	RelCreatedBy     RelationshipFunction = "createdBy"
	RelIds           RelationshipFunction = "ids"
)

// RelationshipExpression wraps a nested expression within a relationship function invocation.
type RelationshipExpression struct {
	Function RelationshipFunction
	Target   ExpressionNode
}

func (*RelationshipExpression) isExpressionNode() {}

// ComparisonOperator enumerates supported comparison operators.
type ComparisonOperator string

const (
	OpEq         ComparisonOperator = "=="
	OpNe         ComparisonOperator = "!="
	OpGt         ComparisonOperator = ">"
	OpGe         ComparisonOperator = ">="
	OpLt         ComparisonOperator = "<"
	OpLe         ComparisonOperator = "<="
	OpIn         ComparisonOperator = "in"
	OpOut        ComparisonOperator = "not in"
	OpHas        ComparisonOperator = "has"
	OpMissing    ComparisonOperator = "== nil"
	OpNotMissing ComparisonOperator = "!= nil"
)

// ComparisonExpression compares a field to a literal value or collection.
type ComparisonExpression struct {
	Field            FieldReference
	Operator         ComparisonOperator
	Value            any
	CacheHintSeconds *int
	FieldSelections  []FieldReference

	// RowAuthzConcept, when set, names the concept whose `@rowAuthz`
	// declaration produced this comparison (memql#3172). Empty on every
	// author-written node.
	//
	// It exists so the injected term's RHS is canonicalized from the
	// DECLARATION. The generic canonicalize-RHS pass takes its concept
	// from extractConceptFromExpression, which answers "" for any filter
	// that is not a top-level `concept==<id>` equality -- and an owner
	// field is an `@relationship`, so its stored value is canonical
	// (`v1:identity:user:u1`) while `actor.userId` resolves to the bare
	// `u1`. An uncanonicalized injected term matches NOTHING, the owner's
	// own rows included. Carrying the concept on the node keeps the tier
	// resolved from the declaration at the canonicalize step exactly as
	// it is at the injection step.
	RowAuthzConcept string
}

func (*ComparisonExpression) isExpressionNode() {}

// BuiltinFunctionExpression represents a builtin function invocation.
// Builtin functions are system functions with Go executor logic.
type BuiltinFunctionExpression struct {
	// Name is the function name (e.g., "concepts", "memqlDocs", "validate").
	Name string
	// Executor is the identifier for the Go executor logic.
	Executor string
	// Args holds optional arguments for builtins that accept parameters.
	Args map[string]any
}

func (*BuiltinFunctionExpression) isExpressionNode() {}

// Executor identifiers for builtin functions.
const (
	BuiltinExecutorConcepts       = "concepts"
	BuiltinExecutorMemqlDocs      = "memqlDocs"
	BuiltinExecutorValidate       = "validate"
	BuiltinExecutorFunctions      = "functions"
	BuiltinExecutorTools          = "tools"
	BuiltinExecutorHelp           = "help"
	BuiltinExecutorShapeTemplates = "shapeTemplates"
	BuiltinExecutorShapeHelp      = "shapeHelp"
	BuiltinExecutorContentId      = "contentId"
	BuiltinExecutorPreviewInsert  = "previewInsert"
	BuiltinExecutorServiceVersion = "serviceVersion"
	BuiltinExecutorError          = "error"
)

// FilterNode aliases ComparisonExpression for backwards compatibility with earlier plan designs.
type FilterNode = ComparisonExpression

// FilterOperator aliases ComparisonOperator for backwards compatibility.
type FilterOperator = ComparisonOperator

// SortField captures a field/direction pair applied when ordering results.
type SortField struct {
	Field     string
	Direction SortDirection
}

// SortDirection enumerates supported sort directions.
type SortDirection string

const (
	// SortDirectionAsc sorts values in ascending order.
	SortDirectionAsc SortDirection = "asc"
	// SortDirectionDesc sorts values in descending order.
	SortDirectionDesc SortDirection = "desc"
)

// SortExpression wraps an expression whose results should be returned in a defined order.
type SortExpression struct {
	Target ExpressionNode
	Fields []SortField
}

func (*SortExpression) isExpressionNode() {}

// PaginateExpression limits the results produced by its target expression.
type PaginateExpression struct {
	Target ExpressionNode
	Limit  *int
}

func (*PaginateExpression) isExpressionNode() {}

// SelectExpression projects fields for the results produced by its target expression.
type SelectExpression struct {
	Target ExpressionNode
	Fields []FieldReference
}

func (*SelectExpression) isExpressionNode() {}

// TimestampExpression pins execution to a given point in time.
//
// ArgPath / FallbackLatest carry the caller-chosen form
// (`asOf args.at ?? latest`, memql#2992) between parse and argument expansion.
// Expansion resolves them into Timestamp or UseLatest and clears them, so
// applyDirectiveWrappers -- and everything else downstream -- only ever sees
// the two literal forms it already handled.
type TimestampExpression struct {
	Target    ExpressionNode
	Timestamp *time.Time
	UseLatest bool
	// ArgPath is the caller-arg name, without the `args.` prefix. Empty once
	// resolved, and empty for the literal forms.
	ArgPath string
	// FallbackLatest means `?? latest`: an omitted arg behaves exactly as
	// `asOf latest`.
	//
	// INVARIANT: true whenever ArgPath != "" on a node originating in parsed
	// source. The fallback is required (memql#3028) and the parser is its only
	// producer, so asof_arg_resolve.go's no-fallback branch is reachable only
	// from a hand-built AST -- it is kept as defence in depth and says so.
	FallbackLatest bool
}

func (*TimestampExpression) isExpressionNode() {}

// DepthExpression overrides relationship traversal depth for its target expression.
type DepthExpression struct {
	Target ExpressionNode
	Depth  int
}

func (*DepthExpression) isExpressionNode() {}

// CountExpression aggregates its target expression to a numeric row
// count instead of materializing rows. Execution returns a
// self-describing {count: N} envelope. Like the other directive
// wrappers it is peeled off into the plan (plan.Count) by
// applyDirectiveWrappers and must be the outermost node.
type CountExpression struct {
	Target ExpressionNode
}

func (*CountExpression) isExpressionNode() {}

// ShapeExpression applies a result-shaping template to the target expression.
type ShapeExpression struct {
	Target        ExpressionNode
	Template      shapeTemplate
	TemplateName  string // Named shape reference; resolved at execution time via shape registry
	IncludeBundle bool   // when true, include bundle in response
}

func (*ShapeExpression) isExpressionNode() {}

// ErrorRefExpression references the current error in onError context: error()
// This is a compile-time AST node for round-trip fidelity; runtime evaluation
// happens via string-based $error substitution in the automations.Evaluator.
type ErrorRefExpression struct{}

func (*ErrorRefExpression) isExpressionNode() {}

// ErrorExpression creates an error with a message: error("message")
// This is a compile-time AST node for round-trip fidelity; runtime evaluation
// happens via string-based substitution in the automations.Evaluator.
type ErrorExpression struct {
	Message ExpressionNode
}

func (*ErrorExpression) isExpressionNode() {}

// MutationNode captures mutation intents parsed from MemQL statements.
type MutationNode struct {
	// Kind discriminates insert vs update -- the engine dispatches on
	// it to either append a fresh full-payload row or read-merge-validate-
	// write against the latest existing row by id. Empty defaults to
	// insert for backwards compat (legacy callers that constructed a
	// MutationNode literal pre-update).
	Kind ast.MutationKind

	// FromTemplate marks a node rendered from a DSL mutation template
	// (renderMutationTemplate). It is the ONE thing that tells the raw
	// `insert(` literal apart from a named mutation once both reach
	// executeWrite, and the row-authz owner stamp keys off it: a named
	// mutation states who owns the row in its own `stamp { }` block,
	// while the raw surface bypasses `args`/`accept`/`stamp` entirely and
	// has no body to state anything (memql#3059 / #3175).
	//
	// The polarity is deliberate and load-bearing. False is the zero
	// value, so a MutationNode built by any producer that does not say
	// otherwise is treated as RAW and gets stamped. The opposite spelling
	// (`Raw bool`) would default a future producer to unstamped -- a hole
	// opened by omission, which is exactly how this one arrived.
	FromTemplate bool

	Concept    string
	ID         string
	PayloadRaw string
	CreatedAt  *time.Time
	ParentRef  *string
	AliasOfRef *string

	// MergeFields names object-typed payload fields that executeUpdate
	// deep-merges into the stored object instead of replacing it
	// wholesale (the default top-level-replace contract). Populated
	// from a mutation's @mergeFields annotation; always empty for raw
	// update() query strings and unannotated mutations. See memql#1339.
	MergeFields []string

	// AppendFields names array-typed payload fields whose partial-write
	// elements executeUpdate APPENDS to the stored array instead of
	// replacing it wholesale. Populated from a mutation's @appendFields
	// annotation; always empty otherwise. See memql#2240.
	AppendFields []string

	// CreateOnlyFields names payload fields an insert-kind (create-or-upsert)
	// mutation writes ONLY on create. When the target id already exists,
	// executeWrite drops them from the delta before the read-merge so the
	// stored value is preserved instead of clobbered -- making a
	// deterministic-id re-stage idempotent for lifecycle fields another
	// writer owns after creation. Populated from a mutation's @createOnly
	// annotation; always empty for raw insert()/update() query strings and
	// unannotated mutations. See fylo#63.
	CreateOnlyFields []string

	// ScrubPii is set from a mutation's @scrubPii annotation: when true,
	// executeUpdate enumerates every @pii-annotated field on the bound
	// concept (Concept.PIIFields()) and zeroes it after the partial
	// payload merges, making the hard-delete PII scrub annotation-driven
	// rather than a hand-maintained field list. Always false for raw
	// update() query strings and unannotated mutations. See memql#1711.
	ScrubPii bool
}

func cloneAIInvocation(src *AIInvocation) *AIInvocation {
	if src == nil {
		return nil
	}
	clone := &AIInvocation{
		TemplateId: src.TemplateId,
	}
	if src.ProviderOverride != nil {
		value := *src.ProviderOverride
		clone.ProviderOverride = &value
	}
	if src.CacheSeconds != nil {
		value := *src.CacheSeconds
		clone.CacheSeconds = &value
	}
	return clone
}
