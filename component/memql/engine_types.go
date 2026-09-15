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
	// LogicCall is a top-level call to a logic. When set, Root is nil and
	// Execute runs the logic's statement body through the wired
	// LogicRunner.
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

	// Refine is the query's `refine` clause (memql#5366): an in-process
	// predicate Execute applies to the SQL page before shaping (refine.go).
	// Peeled off its RefineExpression by applyDirectiveWrappers; nil for a
	// query without one.
	Refine *RefineExpression

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

	// SourceFunction is the NAME of the top-level query construct this
	// plan was expanded from ("spaceParticipants"), or "" when the plan
	// did not resolve to one (an ad-hoc filter expression).
	//
	// Stamped from the same place and for the same reason as
	// BoundConcept, one line below it in the validator: after expansion
	// the plan root IS the function's body and there is no name left to
	// read. It exists for the per-query cache-hit series (memql#4532) --
	// "name a query, get its hit ratio" needs a name, and this is the
	// only bounded one available. Deliberately NOT the query text: a
	// registered construct name is a closed corpus, query text is
	// unbounded and can carry a user id.
	SourceFunction string

	// RowAuthzInjected records that enforcement ANDed a declared tier's
	// predicate into Root. It exists so the result-cache key folds the
	// caller identity in whether or not the injected node happens to sit
	// where planReferencesActor's walk reaches: an enforced read depends
	// on the actor by construction, and a shared key hands one caller's
	// rows to another (memql#3172 finding 2).
	RowAuthzInjected bool

	// RequiredRanks maps construct name -> the role slug its
	// `@requiresRank` declares (epic memql#4832, D6). Empty for nearly
	// every plan.
	//
	// It records WHICH floors apply, never whether they were cleared:
	// the plan is cached and shared across callers, and "this caller
	// cleared it" is not a property of a plan.
	RequiredRanks map[string]string

	// RequiredCapabilities maps construct name -> the (verb, resource)
	// grant its `@requiresCapability` demands (epic memql#5166, D11).
	// Collected by the same expansion that collects RequiredRanks and
	// enforced beside it, because a query that EXPANDS a gated construct
	// must clear its gate exactly as a direct call does.
	RequiredCapabilities map[string]CapabilityRequirement

	// RowAuthzConcept names the concept whose declaration was injected,
	// so the ctx-bearing side of the engine can re-read the tier instead
	// of re-deriving it from the expression.
	RowAuthzConcept string

	// RowAuthzRelaxedForConnector records that the injected tier
	// predicate was REMOVED again because the caller is the connector
	// this concept's @origin or @mirroredTo names (epic memql#4378,
	// rowauthz_connector.go).
	//
	// It exists for two reasons and neither is cosmetic. It is what a
	// test can assert on -- "the connector's read was relaxed" is
	// otherwise indistinguishable from "the tier never engaged". And it
	// is folded into planCacheSignature, because a relaxed plan's
	// canonical expression is byte-identical to an ordinary caller's
	// over the same concept, so the caller identity has to stay in the
	// key or the connector's tier-free result is served to whoever asks
	// next.
	RowAuthzRelaxedForConnector bool
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

	// Lambda is the edition-2026 body, `spec c isX = row => ...` (memql#5366),
	// kept as the parsed v1 AST for two readers: the Init pass lowers it into
	// Expr against the resolved binding (lowerAllPushdownPositions), and
	// EvalExpr evaluates it when the predicate is applied in process -- a
	// refine clause, an automation condition -- where there is no SQL. Nil
	// for an inline spec of the internal query form, whose Expr is converted
	// where it is defined (buildInlineSpec).
	Lambda *ast.LambdaExpr

	// Uses are the file-top `use` imports in force where the spec was
	// written: the tree file's for a loaded spec, the construct source's own
	// for an authored one (a bundle's spec slice carries the bundle's import
	// preamble, so its stored row does too). The binding resolves through
	// them FIRST -- an import that names the bound name decides which shape
	// or concept it is, exactly as a query's signature concept resolves --
	// then the spec's own domain, then the whole tree (specBindingShape /
	// specBindingConcept). Parsed once and never mutated, so clones share it.
	Uses []*ast.UseDeclaration
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
		// The parsed v1 AST is never mutated after parsing, so clones share
		// it -- and they must carry it: the registry hands out clones, and a
		// clone without the lambda is a v1 predicate EvalExpr cannot apply.
		Lambda: s.Lambda,
		// And the imports, or a clone would bind differently than its source.
		Uses: s.Uses,
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
	RelParentOf   RelationshipFunction = "parentOf"
	RelChildOf    RelationshipFunction = "childOf"
	RelAliasOf    RelationshipFunction = "aliasOf"
	RelEquals     RelationshipFunction = "equals"
	RelReferences RelationshipFunction = "references"
	RelContains   RelationshipFunction = "contains"
	RelOwns       RelationshipFunction = "owns"
	RelCreatedBy  RelationshipFunction = "createdBy"
	RelIds        RelationshipFunction = "ids"
)

// RelationshipExpression wraps a nested expression within a relationship function invocation.
type RelationshipExpression struct {
	Function RelationshipFunction
	Target   ExpressionNode
	// Label scopes the traversal to edges carrying that `as` domain label
	// (memql#3656). Empty means unscoped -- follow every edge of this type,
	// which is what every traversal predating #3656 does.
	//
	// This struct is built at ten sites across eight files, and any one of
	// them that forgets to carry Label drops a label-scoped traversal back to
	// unscoped SILENTLY -- returning more rows than asked for rather than
	// failing. TestRelationshipLabelSurvivesEveryConstructionSite is the pin
	// against that.
	Label string
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
	// OpStartsWith is the string-prefix comparison (memql#4208): `<field>
	// startsWith <prefix>`, prefix being a string or a list of strings (ANY
	// of). SQL: `(<text path> ^@ ANY(?::text[]))`; in-process:
	// strings.HasPrefix. An empty list and a blank prefix match nothing --
	// see normalizePrefixValues for why.
	OpStartsWith ComparisonOperator = "startsWith"
	// OpIncludes is the substring test (memql#5366): `<field>.includes(<sub>)`,
	// the edition-2026 spelling of what was contains(s, sub). SQL:
	// `(jsonb_typeof(p) = 'string' AND strpos(<text>, ?) > 0)`; in process:
	// strings.Contains. A blank or whitespace-only needle matches nothing, for
	// the reason a blank startsWith prefix does (rule 32): "" occurs in every
	// string, so the literal reading would let an empty search widen a selection
	// to every row.
	OpIncludes ComparisonOperator = "includes"
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
	// BuiltinExecutorDataOrigins projects every concept's data-origins
	// declaration from the live registry (epic memql#4378). Virtual: no
	// row is persisted. See data_origins_read.go.
	BuiltinExecutorDataOrigins = "dataOrigins"
	// BuiltinExecutorSiteHostnameCheck answers whether the caller could
	// create a site at a hostname right now -- the write guard's own shape
	// and uniqueness rules, asked before the write (2026-09-05 design, D7).
	// Virtual; reserves nothing. See site_hostname_check_read.go.
	BuiltinExecutorSiteHostnameCheck = "siteHostnameCheck"
	// BuiltinExecutorCustomDomainCheck is the same question for a client's
	// own domain. Cluster-owner only, as the concept it speaks for is.
	BuiltinExecutorCustomDomainCheck = "customDomainCheck"
	// BuiltinExecutorProviderAuthStatus projects every registered AI
	// provider's availability and credential SOURCE from the live registry
	// (epic memql#4440). Virtual: no row is persisted, and no credential is
	// ever in the payload. Owner-gated in Go. See
	// provider_auth_status_read.go.
	BuiltinExecutorProviderAuthStatus = "providerAuthStatus"
	// BuiltinExecutorFleetModels projects the LIVE model catalog -- every
	// model the caller's machines (plus the shared-inference set) advertise
	// right now (epic memql#4676). Virtual: no row is persisted, because the
	// answer is which laptops are awake and a stored copy's staleness would
	// be indistinguishable from the condition it describes. See
	// fleet_catalog_read.go.
	BuiltinExecutorFleetModels = "fleetModels"
	// BuiltinExecutorInferenceStatus answers, in ONE row, whether this caller
	// can get inference at all and through which of the three doors (epic
	// memql#4676). The portal's first-run gate reads it, from the same
	// catalog and registry the router reads -- eligibility gets no second
	// implementation. See fleet_catalog_read.go.
	BuiltinExecutorInferenceStatus = "inferenceStatus"
	// BuiltinExecutorFleetModelPull asks one of the CALLER'S machines to pull
	// a model, and returns at once (epic memql#5103). The only fleet builtin
	// that makes something happen on somebody's hardware: it decides, records
	// a v1:worker:modelPull row, and an agent replica picks that row up. See
	// fleet_model_pull.go.
	BuiltinExecutorFleetModelPull = "fleetModelPull"
	// BuiltinExecutorFleetPullRecommended asks one of the CALLER'S machines to
	// pull the whole set the catalog recommends for its class, in order (epic
	// memql#5146, D2). It opens one v1:worker:modelPull row per model through
	// the SAME path the per-model act uses -- it adds the SET and the ORDER,
	// which is the part the catalog can answer and a person cannot, and no
	// second pull mechanism. See fleet_recommended_pull.go.
	BuiltinExecutorFleetPullRecommended = "fleetPullRecommended"
	// BuiltinExecutorFleetModelProbe asks one of the CALLER'S machines to
	// MEASURE a model it already has (epic memql#5146, D3). The pull's act one
	// question later: a pull puts a model on a machine, this measures what it
	// does there. Returns at once with the id of the probe record to watch; the
	// figures land on v1:platform:modelMeasurement. See fleet_model_probe.go.
	BuiltinExecutorFleetModelProbe = "fleetModelProbe"
	// BuiltinExecutorFleetRecommended answers what the catalog recommends for
	// one machine, and why anything is blocked (epic memql#5146, D2). The ACT's
	// answer without the act, so the page and fleetPullRecommended read one
	// implementation -- a second one in the browser would drift, and the drift
	// presents as a page offering a pull the act then refuses. See
	// fleet_recommended_read.go.
	BuiltinExecutorFleetRecommended = "fleetRecommended"
	// BuiltinExecutorFleetSharingLedger answers what a shared machine has done
	// this week, for the person who lent it (epic memql#5146, D6). Counts and
	// levels, never content -- the narrowing happens in the engine so the
	// promise holds even if the page is rewritten by somebody who never read
	// the fold. See sharing_ledger_read.go.
	BuiltinExecutorFleetSharingLedger = "fleetSharingLedger"
	// BuiltinExecutorModuleReadiness folds every node's readiness rows into
	// one verdict per module (design record 2026-09-06-configuration-readiness,
	// section 4.5). See readiness_read.go.
	BuiltinExecutorModuleReadiness = "moduleReadiness"
	// BuiltinExecutorReadinessRecompute re-evaluates this node and rewrites
	// its rows; pulled by integration configure paths, never by a client.
	BuiltinExecutorReadinessRecompute = "readinessRecompute"
	// BuiltinExecutorProvidersReload re-resolves provider auth on EVERY node
	// (epic memql#4440). Owner-gated in Go; writes an audit line; broadcasts
	// over the mesh. See provider_reload_propagate.go.
	BuiltinExecutorProvidersReload = "providersReload"
	// BuiltinExecutorProviderVerify makes ONE authenticated, token-free call
	// to a provider's vendor and reports whether the credential was accepted
	// (epic memql#4440). Owner-gated in Go. See provider_verify.go.
	BuiltinExecutorProviderVerify = "providerVerify"
	// BuiltinExecutorProviderFederationSet writes one vendor's workload
	// identity federation ids as globalVariable rows, refusing a partial set
	// (epic memql#4440, made vendor-aware by memql#5088). Owner-or-developer.
	// None of the ids is a credential -- the credential is the projected token
	// the pod holds, which no client ever sends.
	//
	// Its key-sealing sibling is GONE (memql#5088): there is no manually
	// entered vendor API key anywhere in the product, and
	// TestNoVendorApiKeyEntryPoint fails the build on one coming back.
	BuiltinExecutorProviderFederationSet = "providerFederationSet"
)

// ModuleReadinessConcept is the canonical id of one node's per-module
// readiness verdict (design record 2026-09-06-configuration-readiness,
// section 4.4). See readiness_read.go.
const ModuleReadinessConcept = "v1:platform:moduleReadiness"

// WorkerRegistrationConcept is the canonical id of a cockpit machine's
// registration row. Named here because the readiness recompute subscriber
// keys its one graph subscription on it and component/memql may not import
// component/worker (that module requires this one).
const WorkerRegistrationConcept = "v1:worker:registration"

// ModuleVerdictConcept is the canonical id of the virtual, never-persisted
// cluster-wide fold of ModuleReadinessConcept rows, one row per module
// (section 4.5). See readiness_read.go.
const ModuleVerdictConcept = "v1:platform:moduleVerdict"

// ReadinessRecomputeResultConcept is the canonical id of the virtual,
// never-persisted single row readinessRecompute answers with: which node
// re-evaluated and how many rows it wrote.
const ReadinessRecomputeResultConcept = "v1:platform:readinessRecomputeResult"

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

	// AddToSet and RemoveFromSet name array-typed payload fields
	// executeWrite treats as SET MEMBERSHIP: the written elements are
	// unioned into, or removed from, the stored array rather than
	// replacing it. Populated from a mutation's @addToSet /
	// @removeFromSet annotations; always empty otherwise. See memql#4951.
	AddToSet      []string
	RemoveFromSet []string

	// CreateOnlyFields names payload fields an insert-kind (create-or-upsert)
	// mutation writes ONLY on create. When the target id already exists,
	// executeWrite drops them from the delta before the read-merge so the
	// stored value is preserved instead of clobbered -- making a
	// deterministic-id re-stage idempotent for lifecycle fields another
	// writer owns after creation. Populated from a mutation's @createOnly
	// annotation; always empty for raw insert()/update() query strings and
	// unannotated mutations. See fylo#63.
	CreateOnlyFields []string

	// NoUnsetFields names payload fields this mutation may SET but may never
	// blank: on the read-merge path executeWrite drops a named field from the
	// delta when the incoming value is empty and the stored one is not, so a
	// one-way transition (unclaimed -> claimed, unbootstrapped -> bootstrapped)
	// cannot be reversed by an ordinary write. Populated from a mutation's
	// @noUnset annotation; always empty for raw insert()/update() query strings
	// and unannotated mutations. See memql#3415.
	NoUnsetFields []string

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
