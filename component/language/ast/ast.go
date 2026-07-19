// Package ast defines the MemQL Abstract Syntax Tree node types.
//
// These types are emitted by the parser (component/language/parser)
// and consumed by the compiler (component/language/compiler),
// execution engine (component/memql), and language-intelligence
// service (component/memql/sense). Separating the types into their
// own package lets consumers depend on the AST surface without
// pulling in the full lexer/parser machinery.
//
// Phase 2 Item 9 of the language-improvements plan. The parser
// package still re-exports every symbol here as a type alias so the
// existing ~25 consumer files continue to compile unchanged; new
// code should import ast/ directly.
package ast

import "time"

// Node is the interface implemented by all AST nodes.
type Node interface {
	node()
}

// ExpressionNode is the interface implemented by all expression AST nodes.
type ExpressionNode interface {
	Node
	expressionNode()
}

// StatementNode is the interface implemented by all statement AST nodes.
type StatementNode interface {
	Node
	statementNode()
}

// ----------------------------------------------------------------------------
// Core Types
// ----------------------------------------------------------------------------

// FieldReference describes paths to structured fields within memory nodes.
type FieldReference struct {
	Raw      string
	Parts    []string
	Wildcard bool
}

// AttributeMap stores arbitrary key/value arguments discovered in query stages.
type AttributeMap map[string]any

// ----------------------------------------------------------------------------
// Operators
// ----------------------------------------------------------------------------

// LogicalOp identifies logical operators applied between expressions.
type LogicalOp string

const (
	LogicalAnd LogicalOp = "AND"
	LogicalOr  LogicalOp = "OR"
)

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

// SortDirection enumerates supported sort directions.
type SortDirection string

const (
	SortAsc  SortDirection = "asc"
	SortDesc SortDirection = "desc"
)

// SortField captures a field/direction pair applied when ordering results.
type SortField struct {
	Field     string
	Direction SortDirection
}

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

// ----------------------------------------------------------------------------
// Expression Nodes
// ----------------------------------------------------------------------------

// LogicalExpr joins two expressions using a logical operator.
type LogicalExpr struct {
	Op    LogicalOp
	Left  ExpressionNode
	Right ExpressionNode
}

func (*LogicalExpr) node()           {}
func (*LogicalExpr) expressionNode() {}

// ComparisonExpr compares a field to a literal value or collection.
type ComparisonExpr struct {
	Field            FieldReference
	Operator         ComparisonOperator
	Value            any
	CacheHintSeconds *int
	FieldSelections  []FieldReference
}

func (*ComparisonExpr) node()           {}
func (*ComparisonExpr) expressionNode() {}

// ArithmeticExpr is a binary arithmetic operation (#2316): one of
// `+` `-` `*` `/` `%`. Left-associative; `* / %` bind tighter than
// `+ -`. It is evaluated IN-MEMORY only -- the engine AST converter
// admits it in logic bodies / collection lambdas and rejects it in
// specs / query filters (no SQL pushdown). A unary minus on a primary
// is folded into `0 - <primary>` by the parser, so there is no separate
// unary node.
type ArithmeticExpr struct {
	Op    string
	Left  ExpressionNode
	Right ExpressionNode
}

func (*ArithmeticExpr) node()           {}
func (*ArithmeticExpr) expressionNode() {}

// BinaryComparisonExpr is an EXPRESSION-LED comparison (#2542 item 5
// residual): both operands are full expressions rather than the bare
// field-vs-value shape ComparisonExpr models. It is produced by the
// comparison precedence level (parser.parseComparisonLevel) when a
// comparison's left side is NOT a bare identifier -- an arithmetic
// expression (`r.count - 10 > 0`), a literal (`0 < r.count`), a
// parenthesised group, or a call result. Identifier-led comparisons keep
// their Field-led ComparisonExpr shape (consumed one level down in
// parseIdentifierExpression), so this node is purely additive.
//
// Only the six symbol comparison operators appear here (== != < <= > >=);
// the membership operators (in / has / not in) are identifier-led only.
// Like ArithmeticExpr it is evaluated IN-MEMORY only -- the engine AST
// converter admits it under WithCollectionMethods (logic bodies /
// collection lambdas) and rejects it in specs / query filters.
type BinaryComparisonExpr struct {
	Left     ExpressionNode
	Operator ComparisonOperator
	Right    ExpressionNode
}

func (*BinaryComparisonExpr) node()           {}
func (*BinaryComparisonExpr) expressionNode() {}

// RelationshipExpr wraps a nested expression within a relationship function invocation.
type RelationshipExpr struct {
	Function RelationshipFunction
	Target   ExpressionNode
}

func (*RelationshipExpr) node()           {}
func (*RelationshipExpr) expressionNode() {}

// SortExpr wraps an expression whose results should be returned in a defined order.
type SortExpr struct {
	Target ExpressionNode
	Fields []SortField
}

func (*SortExpr) node()           {}
func (*SortExpr) expressionNode() {}

// PaginateExpr limits the results produced by its target expression.
type PaginateExpr struct {
	Target ExpressionNode
	Limit  *int
}

func (*PaginateExpr) node()           {}
func (*PaginateExpr) expressionNode() {}

// SelectExpr projects fields for the results produced by its target expression.
type SelectExpr struct {
	Target ExpressionNode
	Fields []FieldReference
}

func (*SelectExpr) node()           {}
func (*SelectExpr) expressionNode() {}

// TimestampExpr pins execution to a given point in time.
type TimestampExpr struct {
	Target    ExpressionNode
	Timestamp *time.Time
	UseLatest bool
}

func (*TimestampExpr) node()           {}
func (*TimestampExpr) expressionNode() {}

// DepthExpr overrides relationship traversal depth for its target expression.
type DepthExpr struct {
	Target ExpressionNode
	Depth  int
}

func (*DepthExpr) node()           {}
func (*DepthExpr) expressionNode() {}

// CountExpr aggregates its target expression to a numeric row count
// instead of materializing the matching rows. The query returns a
// self-describing {count: N} envelope. Like the other directive
// wrappers it must be the outermost node around a query expression.
type CountExpr struct {
	Target ExpressionNode
}

func (*CountExpr) node()           {}
func (*CountExpr) expressionNode() {}

// ShapeExpr applies a result-shaping template to the target expression.
type ShapeExpr struct {
	Target        ExpressionNode
	Template      any    // ShapeTemplate - kept as any to avoid circular deps
	TemplateName  string // Named shape reference (e.g., "participantFull"); mutually exclusive with Template
	IncludeBundle bool
}

func (*ShapeExpr) node()           {}
func (*ShapeExpr) expressionNode() {}

// FunctionCallExpr references a named function invocation with arguments.
type FunctionCallExpr struct {
	Name string
	Args map[string]any

	// Kind is the construct-kind prefix when the call was written in the
	// kind-prefixed invocation form `<kind> <name>(args)` (Story 2 / #2324):
	// one of "logic", "query", "mutation", "action", "capability", "builtin",
	// "automation". Empty means a bare/unprefixed call (a language primitive or
	// a legacy unprefixed construct call). Downstream semantic resolution keys
	// off Name, so Kind is purely additive metadata — it makes the CQS nature
	// of a call site syntactically visible (a `mutation`/`capability` keyword
	// inside a `logic` body is a contract violation a later callgraph check can
	// read) without changing resolution behaviour.
	Kind string
}

func (*FunctionCallExpr) node()           {}
func (*FunctionCallExpr) expressionNode() {}

// MethodCallExpr is a method-chained collection call (Story 4 / #2302 /
// ADR §2.2): `args.members.where(m => m.active).count()`. Receiver is the
// collection being operated on (a dotted-path arg/spec reference or a prior
// MethodCallExpr in a chain); Method is the collection operator name
// (where/select/count/...); Args are the parsed call arguments, each of which
// may be a LambdaExpr. These nodes are only valid in logic bodies and
// automation forEach; specs and query filters reject them at load.
type MethodCallExpr struct {
	Receiver ExpressionNode
	Method   string
	Args     []ExpressionNode

	// Raw is the verbatim source text of the whole chain, captured by the
	// parser when a collection chain appears as a multi-statement logic step
	// RHS (`active := args.members.where(m => m.active)`, #2317). It lets the
	// step round-trip to its EXACT source so the LogicRunner's in-memory
	// collection evaluator re-parses what the author wrote -- the per-node
	// expressionToString reconstruction is engine-IR (e.g. `arg("members")`),
	// not source, so it cannot reproduce an arg-receiver chain. Empty for the
	// step-result-accessor MethodCallExprs produced elsewhere (Story 5).
	Raw string
}

func (*MethodCallExpr) node()           {}
func (*MethodCallExpr) expressionNode() {}

// LambdaExpr is an arrow lambda used as a collection-method argument
// (Story 4 / #2302): `m => m.active` or `(acc, x) => acc + x`. Params are the
// bound parameter names; Body is the (pure) expression evaluated per element.
type LambdaExpr struct {
	Params []string
	Body   ExpressionNode
}

func (*LambdaExpr) node()           {}
func (*LambdaExpr) expressionNode() {}

// BuiltinFunctionExpr represents a builtin function invocation.
type BuiltinFunctionExpr struct {
	Name     string
	Executor string
}

func (*BuiltinFunctionExpr) node()           {}
func (*BuiltinFunctionExpr) expressionNode() {}

// SpecReferenceExpr references a named specification.
type SpecReferenceExpr struct {
	Name string

	// Trait is set when the predicate was written in the kind-prefixed
	// `trait <name>` form rather than `spec <name>` (Story 2 / #2324). It is
	// additive metadata distinguishing a deliberately-unbound trait predicate
	// from a signature-bound spec at the call site; resolution still keys off
	// Name. Empty/false for bare predicate references and `spec <name>`.
	Trait bool
}

func (*SpecReferenceExpr) node()           {}
func (*SpecReferenceExpr) expressionNode() {}

// ConditionalFilterExpr represents a ?. conditional filter.
type ConditionalFilterExpr struct {
	ArgPath string
	Filter  ExpressionNode
}

func (*ConditionalFilterExpr) node()           {}
func (*ConditionalFilterExpr) expressionNode() {}

// ArgRefExpr references a value from the function arguments.
type ArgRefExpr struct {
	Path string
}

func (*ArgRefExpr) node()           {}
func (*ArgRefExpr) expressionNode() {}

// LiteralExpr wraps a literal value.
type LiteralExpr struct {
	Value any
}

func (*LiteralExpr) node()           {}
func (*LiteralExpr) expressionNode() {}

// AIExpr captures ai() invocation nodes parsed inside MemQL expressions.
type AIExpr struct {
	TemplateId       string
	ProviderOverride *string
	CacheSeconds     *int
}

func (*AIExpr) node()           {}
func (*AIExpr) expressionNode() {}

// ----------------------------------------------------------------------------
// Accessor Function Nodes (NEW - for automation/mutation expressions)
// ----------------------------------------------------------------------------

// VarRefExpr references a configuration variable: var("NAME")
type VarRefExpr struct {
	Name string
}

func (*VarRefExpr) node()           {}
func (*VarRefExpr) expressionNode() {}

// StepRefExpr references a previous step's result: step("id")
type StepRefExpr struct {
	StepId string
}

func (*StepRefExpr) node()           {}
func (*StepRefExpr) expressionNode() {}

// InputRefExpr references the automation input: input()
type InputRefExpr struct{}

func (*InputRefExpr) node()           {}
func (*InputRefExpr) expressionNode() {}

// ItemRefExpr references the current forEach item: item()
type ItemRefExpr struct{}

func (*ItemRefExpr) node()           {}
func (*ItemRefExpr) expressionNode() {}

// IndexRefExpr references the current forEach index: index()
type IndexRefExpr struct{}

func (*IndexRefExpr) node()           {}
func (*IndexRefExpr) expressionNode() {}

// EventRefExpr references the trigger event: event()
type EventRefExpr struct{}

// CallerRefExpr references the caller() accessor, which yields the
// authenticated user's AccessContext at query-evaluation time.
// Exposed fields (resolved by the runtime):
//
//	caller.userId        -- v1:identity:user.id
//	caller.primaryEmail  -- the caller's primary email
//	caller.role          -- cluster-wide role (owner / admin / writer / reader)
//	caller.identityId    -- v1:identity:identity.id used for this request
//	caller.isOwner       -- bool short-circuit for owner-bypass paths
//
// Callable in expression position inside .memql function bodies.
type CallerRefExpr struct{}

func (*EventRefExpr) node()           {}
func (*EventRefExpr) expressionNode() {}

func (*CallerRefExpr) node()           {}
func (*CallerRefExpr) expressionNode() {}

// ErrorRefExpr references the current error in onError context: $error
type ErrorRefExpr struct{}

func (*ErrorRefExpr) node()           {}
func (*ErrorRefExpr) expressionNode() {}

// ErrorExpr creates an error with a message: error("message")
// Used for early returns: return error("something went wrong")
type ErrorExpr struct {
	Message ExpressionNode
}

func (*ErrorExpr) node()           {}
func (*ErrorExpr) expressionNode() {}

// TimestampExprFunc returns the current timestamp: timestamp() or now()
type TimestampExprFunc struct{}

func (*TimestampExprFunc) node()           {}
func (*TimestampExprFunc) expressionNode() {}

// FieldRefExpr accesses a field on an object: field(obj, "key")
type FieldRefExpr struct {
	Object ExpressionNode
	Key    string
}

func (*FieldRefExpr) node()           {}
func (*FieldRefExpr) expressionNode() {}

// ConcatExpr concatenates values: concat(a, b, ...)
type ConcatExpr struct {
	Args []ExpressionNode
}

func (*ConcatExpr) node()           {}
func (*ConcatExpr) expressionNode() {}

// CoalesceExpr returns the first non-null value: coalesce(a, b, ...)
type CoalesceExpr struct {
	Args []ExpressionNode
}

func (*CoalesceExpr) node()           {}
func (*CoalesceExpr) expressionNode() {}

// CondExpr represents a three-argument conditional-value expression:
// cond(predicate, thenValue, elseValue). Evaluates predicate, returns
// thenValue when truthy and elseValue otherwise. Renamed from IfExpr
// so the AST doesn't collide with Go readers' intuition about `if`
// statements.
type CondExpr struct {
	Condition ExpressionNode
	Then      ExpressionNode
	Else      ExpressionNode
}

func (*CondExpr) node()           {}
func (*CondExpr) expressionNode() {}

// TernaryExpr represents the ternary operator: cond ? then : else
type TernaryExpr struct {
	Condition ExpressionNode
	Then      ExpressionNode
	Else      ExpressionNode
}

func (*TernaryExpr) node()           {}
func (*TernaryExpr) expressionNode() {}

// FirstExpr returns the first item: first(expr)
type FirstExpr struct {
	Target ExpressionNode
}

func (*FirstExpr) node()           {}
func (*FirstExpr) expressionNode() {}

// LastExpr returns the last item: last(expr)
type LastExpr struct {
	Target ExpressionNode
}

func (*LastExpr) node()           {}
func (*LastExpr) expressionNode() {}

// LowerExpr converts to lowercase: lower(str)
type LowerExpr struct {
	Target ExpressionNode
}

func (*LowerExpr) node()           {}
func (*LowerExpr) expressionNode() {}

// UpperExpr converts to uppercase: upper(str)
type UpperExpr struct {
	Target ExpressionNode
}

func (*UpperExpr) node()           {}
func (*UpperExpr) expressionNode() {}

// TrimExpr removes whitespace: trim(str)
type TrimExpr struct {
	Target ExpressionNode
}

func (*TrimExpr) node()           {}
func (*TrimExpr) expressionNode() {}

// ContainsExpr checks substring: contains(str, substr)
type ContainsExpr struct {
	Target    ExpressionNode
	Substring ExpressionNode
}

func (*ContainsExpr) node()           {}
func (*ContainsExpr) expressionNode() {}

// HashExpr computes SHA256: hash(str)
type HashExpr struct {
	Target ExpressionNode
}

func (*HashExpr) node()           {}
func (*HashExpr) expressionNode() {}

// ShortIdExpr extracts the bare short id from an id-shaped value:
// shortId(value). The inverse of canonicalId -- it strips the
// `<partition>:<concept>:` prefix from a canonical node id and returns
// the trailing bare slug. Idempotent: a value that is already bare (no
// version-tagged concept prefix) is returned unchanged, so calling it
// on a tool-path short id is a no-op. Used to normalize a foreign-key /
// audit field to one consistent (short) id form regardless of whether
// the caller passes a canonical or bare id (#1859).
type ShortIdExpr struct {
	Target ExpressionNode
}

func (*ShortIdExpr) node()           {}
func (*ShortIdExpr) expressionNode() {}

// CanonicalIdExpr normalizes an id-shaped value to canonical form
// for the named concept: canonicalId(value, "<conceptType>").
//
// The Concept arg is required to be a string literal at parse time
// (not a runtime expression), so it can be validated against the
// concept registry during compilation. Value is any expression that
// resolves to a string at runtime.
type CanonicalIdExpr struct {
	Value   ExpressionNode
	Concept string
}

func (*CanonicalIdExpr) node()           {}
func (*CanonicalIdExpr) expressionNode() {}

// AndExpr returns true if all args are truthy: and(a, b, ...)
type AndExpr struct {
	Args []ExpressionNode
}

func (*AndExpr) node()           {}
func (*AndExpr) expressionNode() {}

// OrExpr returns true if any arg is truthy: or(a, b, ...)
type OrExpr struct {
	Args []ExpressionNode
}

func (*OrExpr) node()           {}
func (*OrExpr) expressionNode() {}

// NotExpr returns boolean negation: not(expr)
type NotExpr struct {
	Target ExpressionNode
}

func (*NotExpr) node()           {}
func (*NotExpr) expressionNode() {}

// EqExpr returns true if a == b: eq(a, b)
type EqExpr struct {
	Left  ExpressionNode
	Right ExpressionNode
}

func (*EqExpr) node()           {}
func (*EqExpr) expressionNode() {}

// LtExpr returns true if a < b: lt(a, b)
type LtExpr struct {
	Left  ExpressionNode
	Right ExpressionNode
}

func (*LtExpr) node()           {}
func (*LtExpr) expressionNode() {}

// GtExpr returns true if a > b: gt(a, b)
type GtExpr struct {
	Left  ExpressionNode
	Right ExpressionNode
}

func (*GtExpr) node()           {}
func (*GtExpr) expressionNode() {}

// LteExpr returns true if a <= b: lte(a, b)
type LteExpr struct {
	Left  ExpressionNode
	Right ExpressionNode
}

func (*LteExpr) node()           {}
func (*LteExpr) expressionNode() {}

// GteExpr returns true if a >= b: gte(a, b)
type GteExpr struct {
	Left  ExpressionNode
	Right ExpressionNode
}

func (*GteExpr) node()           {}
func (*GteExpr) expressionNode() {}

// ToStringExpr converts value to string: toString(expr)
type ToStringExpr struct {
	Target ExpressionNode
}

func (*ToStringExpr) node()           {}
func (*ToStringExpr) expressionNode() {}

// AddDurationExpr adds duration to timestamp: addDuration(timestamp, duration)
type AddDurationExpr struct {
	Timestamp ExpressionNode
	Duration  ExpressionNode
}

func (*AddDurationExpr) node()           {}
func (*AddDurationExpr) expressionNode() {}

// DaysBetweenExpr calculates days between dates: daysBetween(date1, date2)
type DaysBetweenExpr struct {
	Date1 ExpressionNode
	Date2 ExpressionNode
}

func (*DaysBetweenExpr) node()           {}
func (*DaysBetweenExpr) expressionNode() {}

// SubtractTimestampsExpr calculates duration between timestamps: subtractTimestamps(t1, t2)
type SubtractTimestampsExpr struct {
	T1 ExpressionNode
	T2 ExpressionNode
}

func (*SubtractTimestampsExpr) node()           {}
func (*SubtractTimestampsExpr) expressionNode() {}

// YearExpr extracts year from timestamp: year(timestamp)
type YearExpr struct {
	Target ExpressionNode
}

func (*YearExpr) node()           {}
func (*YearExpr) expressionNode() {}

// QuarterExpr extracts quarter (1-4) from timestamp: quarter(timestamp)
type QuarterExpr struct {
	Target ExpressionNode
}

func (*QuarterExpr) node()           {}
func (*QuarterExpr) expressionNode() {}

// MonthExpr extracts month (1-12) from timestamp: month(timestamp)
type MonthExpr struct {
	Target ExpressionNode
}

func (*MonthExpr) node()           {}
func (*MonthExpr) expressionNode() {}

// DayOfMonthExpr extracts day of month (1-31) from timestamp: dayOfMonth(timestamp)
type DayOfMonthExpr struct {
	Target ExpressionNode
}

func (*DayOfMonthExpr) node()           {}
func (*DayOfMonthExpr) expressionNode() {}

// IsAnniversaryExpr checks if checkDate is anniversary of startDate: isAnniversary(startDate, checkDate)
type IsAnniversaryExpr struct {
	StartDate ExpressionNode
	CheckDate ExpressionNode
}

func (*IsAnniversaryExpr) node()           {}
func (*IsAnniversaryExpr) expressionNode() {}

// IsFirstDayOfQuarterExpr checks if date is first day of quarter: isFirstDayOfQuarter(timestamp)
type IsFirstDayOfQuarterExpr struct {
	Target ExpressionNode
}

func (*IsFirstDayOfQuarterExpr) node()           {}
func (*IsFirstDayOfQuarterExpr) expressionNode() {}

// ----------------------------------------------------------------------------
// Statement Nodes (for functions, mutations, automations)
// ----------------------------------------------------------------------------

// MutationKind discriminates between insert() and update() call forms.
//
// insert() -- the legacy, full-payload form: every required field on
// the concept must be present in the payload, validated independently
// per row. Each call appends a new row in the time-series; queries
// resolve to the latest row by createdAt. Existing prior rows are
// NOT consulted.
//
// update() -- partial-payload form added to bridge the time-series-
// vs-mental-model gap. The engine reads the latest existing row by
// id, splats the new payload fields on top, validates the merged
// result against the concept schema, then appends a new row carrying
// the merged payload. Required fields the caller doesn't pass are
// inherited from the prior row. Non-existent prior row -> error
// (update can't synthesize a record from nothing).
//
// Default: MutationKindInsert. Mutations that want partial-update
// semantics use `return update(...)` instead of `return insert(...)`.
type MutationKind string

const (
	MutationKindInsert MutationKind = "insert"
	MutationKindUpdate MutationKind = "update"
)

// MutationStmt captures mutation intents parsed from MemQL statements.
type MutationStmt struct {
	// Kind discriminates insert vs update call form. Empty / unset
	// is equivalent to MutationKindInsert -- consumers should treat a
	// blank Kind as insert for backwards compatibility with AST nodes
	// produced before update() landed.
	Kind    MutationKind
	Concept string
	// IDTemplate preserves the id=... expression (string literal, args.X, concat(...), etc.)
	// for later runtime evaluation by mutation/function execution.
	IDTemplate any
	// CreatedAtTemplate preserves createdAt=... expression for optional CreatedAt overrides.
	// It should evaluate to an RFC3339/RFC3339Nano timestamp string at runtime.
	CreatedAtTemplate any
	PayloadRaw        string
	// ParentTemplate and AliasOfTemplate preserve relationship hints for later evaluation.
	ParentTemplate  any
	AliasOfTemplate any
}

func (*MutationStmt) node()          {}
func (*MutationStmt) statementNode() {}

// QueryStmt represents a query expression as a statement.
type QueryStmt struct {
	Expression ExpressionNode
}

func (*QueryStmt) node()          {}
func (*QueryStmt) statementNode() {}

// ----------------------------------------------------------------------------
// Directive Nodes (for //memql:xxx Go-style directives)
// ----------------------------------------------------------------------------

// Attribute represents a Python-style @decorator attribute.
// Examples:
//   - @enabled
//   - @description("Auto-provisions a user")
//   - @trigger(event="session.opened")
//   - @args({ "userId": { "type": "string" } })
type Attribute struct {
	Name  string         // e.g., "enabled", "trigger", "description"
	Value any            // Single value (string, bool, etc.) or nil for flag attributes
	Args  map[string]any // Named args: key=value pairs
}

func (*Attribute) node() {}

// UseTargets extracts the comma-list of bare names declared in this
// attribute. Returns the names that appeared inside the parens of
// `@useConcept(a, b)` / `@useShape(x)` / etc.
//
// The parser stores `@useConcept(a, b)` with Args=map[a:true b:true]
// (bare identifiers become keys with boolean true). Map iteration
// order is non-deterministic; the returned slice is alphabetically
// sorted for stable ordering across loads.
func (a *Attribute) UseTargets() []string {
	if a == nil || len(a.Args) == 0 {
		return nil
	}
	names := make([]string, 0, len(a.Args))
	for k := range a.Args {
		names = append(names, k)
	}
	// Stable order for downstream consumers (validators, debug
	// output, etc.).
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}

// Common attribute names (used in @name syntax)
const (
	// Lifecycle attributes.
	//
	// CANONICAL SEMANTICS of @enabled / @disabled (applies to every
	// construct that accepts them -- functions/builtins/automations/
	// actions/tools/prompts/specs/traits/seeds/providers/capabilities;
	// the gates
	// landed per kind in #2604-#2608; see the per-decl "lifecycle
	// (engine-side flags)" notes on BuiltinDecl / PromptDecl /
	// ProviderDecl):
	//
	//	@disabled means the construct is NOT loaded/active at runtime
	//	right now. It does NOT mean the construct is deprecated,
	//	abandoned, exempt from updates / maintenance / refactors /
	//	conformance, or that it will not be used in the future. It is a
	//	reversible on/off switch; disabled constructs are still
	//	maintained and may be re-enabled at any time. @enabled is the
	//	explicit-on form -- the default, kept for symmetry.
	//
	// "Deprecated / abandoned" is a separate axis carried by
	// @deprecated (AttrDeprecated), not @disabled.
	AttrEnabled    = "enabled"
	AttrDisabled   = "disabled"
	AttrDeprecated = "deprecated"
	AttrVersion    = "version"

	// Documentation
	AttrDescription = "description"

	// Access control
	AttrInternal   = "internal"
	AttrPublic     = "public"
	AttrRole       = "role"
	AttrPermission = "permission"

	// Performance
	AttrTimeout = "timeout"
	AttrCache   = "cache"
	// AttrNocache is the clearer opt-out alias for @cache(ttl="0") on a
	// query (epic 5, issue 5.6 / memql#1970). It forces "never cache",
	// overriding the default-on caching for pure reads. Equivalent to
	// @cache(ttl="0"); the parser maps it to CacheTTL="0".
	AttrNocache   = "nocache"
	AttrRateLimit = "rateLimit"

	// Reliability
	AttrRetry      = "retry"
	AttrIdempotent = "idempotent"

	// Mutation-specific attributes.
	//
	// @mergeFields("a", "b") opts an update-kind mutation into
	// engine-side deep-merge for the named object-typed payload
	// fields: instead of the partial object REPLACING the stored
	// object wholesale (the default top-level-replace contract every
	// other mutation is written against -- see memql#350), the
	// partial object's keys merge into the stored object so sibling
	// keys survive. Added for toggleComputerUseEnabled,
	// which writes a single key into User.preferences and would
	// otherwise wipe every other preference (memql#1339).
	AttrMergeFields = "mergeFields"

	// @appendFields("a", "b") opts an update-kind mutation into
	// engine-side array APPEND for the named array-typed payload
	// fields: instead of the partial array REPLACING the stored array
	// wholesale (the default top-level-replace contract), the partial
	// array's elements are appended to the stored array. This lets a
	// single-writer mutation accumulate list elements (e.g. attach one
	// id to request.attachmentIds) without a read-merge-append logic
	// wrapper -- the logic-purity-clean form (memql#2240). Like
	// @mergeFields, only valid on update-kind mutations.
	AttrAppendFields = "appendFields"

	// @scrubPii opts an update-kind mutation into engine-side generic
	// PII scrubbing: after the partial payload merges onto the stored
	// row, the engine enumerates EVERY field the bound concept declares
	// with @pii and overwrites it with its type's zero value. This makes
	// the hard-delete data-deletion scrub annotation-driven rather than a
	// hand-maintained field list -- adding a new PII field to a concept
	// requires only the @pii annotation; the scrub covers it
	// automatically and cannot drift out of sync (memql#1711). Only valid
	// on update-kind mutations (enforced at load time in
	// function_loader.go).
	AttrScrubPii = "scrubPii"

	// @createOnly("a", "b") opts an insert-kind (create-or-upsert)
	// mutation into per-field create-only semantics: the named payload
	// fields are written ONLY when the mutation creates the row. When the
	// target id already exists, those fields are DROPPED from the delta
	// before the engine read-merge (memql#1709), so the stored value is
	// preserved instead of clobbered. This makes a deterministic-id
	// re-stage genuinely idempotent for lifecycle fields another writer
	// owns after creation -- e.g. stageOutboundRequest seeds status +
	// attempts at birth but must not reset a row the outbound worker has
	// since moved to sending/sent/failed (fylo#63). The inverse of
	// @mergeFields/@appendFields: only valid on insert-kind mutations
	// (an update always targets an existing row, so a create-only field
	// could never be written -- rejected at load time).
	AttrCreateOnly = "createOnly"

	// Auditing
	AttrAudit = "audit"

	// Triggers (automation only)
	AttrTrigger  = "trigger"
	AttrFilter   = "filter"
	AttrSchedule = "schedule"
	AttrAsync    = "async"

	// Tool-specific attributes
	AttrHandler              = "handler"
	AttrDestructive          = "destructive"
	AttrRequiresConfirmation = "requiresConfirmation"
	AttrExecutionTime        = "executionTime"

	// Builtin-specific attributes
	AttrExecutor = "executor"

	// Dependency-declaration attributes. Every named construct
	// referenced inside a body MUST be declared via the matching
	// @use* annotation. Unused declarations error at load.
	//
	// The legacy concept/shape binding annotations `@useConcept` and
	// `@useShape` were retired in issue #301 -- concepts now bind
	// through the construct's signature (`mutation <Concept> <name>`)
	// and the file-top `use` import; shapes bind through `@shape("name")`
	// (string form, parsed elsewhere).
	AttrUse           = "use"
	AttrUseSpec       = "useSpec"
	AttrUseTrait      = "useTrait"
	AttrUseQuery      = "useQuery"
	AttrUseMutation   = "useMutation"
	AttrUseAutomation = "useAutomation"
	AttrUseLogic      = "useLogic"
	AttrUseTool       = "useTool"
	AttrUsePrompt     = "usePrompt"
	AttrUseProvider   = "useProvider"
	AttrUseBuiltin    = "useBuiltin"
)

// ----------------------------------------------------------------------------
// Go-like Statement Nodes (NEW - for Go-style syntax)
// ----------------------------------------------------------------------------

// AssignStmt represents a := assignment: name := expr or name, err := expr
type AssignStmt struct {
	Names []string       // Variable names (e.g., ["result"] or ["result", "err"])
	Value ExpressionNode // The expression being assigned
}

func (*AssignStmt) node()          {}
func (*AssignStmt) statementNode() {}

// ForRangeStmt represents a for-range loop: for item := range collection { ... }
type ForRangeStmt struct {
	Index      string         // Index variable (optional, e.g., "i")
	Value      string         // Value variable (e.g., "item")
	Collection ExpressionNode // Collection to iterate over
	Filter     ExpressionNode // Optional filter condition (for item := range x if cond)
	Body       []Node         // Statements in the loop body
}

func (*ForRangeStmt) node()          {}
func (*ForRangeStmt) statementNode() {}

// SwitchStmt represents a Go-style switch statement
type SwitchStmt struct {
	Expr    ExpressionNode // Expression being switched on
	Cases   []*CaseClause  // Case clauses
	Default []Node         // Default clause body (nil if no default)
}

func (*SwitchStmt) node()          {}
func (*SwitchStmt) statementNode() {}

// CaseClause represents a single case in a switch statement
type CaseClause struct {
	Values []ExpressionNode // Case values (can be multiple: case "a", "b":)
	Body   []Node           // Statements to execute
}

func (*CaseClause) node() {}

// IfStmt represents a Go-style if statement: if cond { } else { }
type IfStmt struct {
	Init      StatementNode  // Optional init statement: if x, err := foo(); err != nil
	Condition ExpressionNode // Condition to evaluate
	Then      []Node         // Body if condition is true
	Else      []Node         // Body if condition is false (can contain another IfStmt)

	// ThenSteps and ElseSteps hold the body parsed as automation step
	// definitions (the canonical shape for Logic-body if statements).
	// Populated by parseIfStatement when the body contains
	// assignments / for-range / nested if statements rather than the
	// legacy continue/break/return-only shape. ifStatementToSteps
	// reads these to flatten the if into a list of conditional steps.
	ThenSteps []StepDef
	ElseSteps []StepDef
	// ElseIf holds a nested `else if` chain. If non-nil, ElseSteps
	// is unused -- the nested if's steps carry the negated parent
	// condition layered on top of their own.
	ElseIf *IfStmt
}

func (*IfStmt) node()          {}
func (*IfStmt) statementNode() {}

// ContinueStmt represents a continue statement in a loop
type ContinueStmt struct {
	Label string // Optional label for labeled continue
}

func (*ContinueStmt) node()          {}
func (*ContinueStmt) statementNode() {}

// BreakStmt represents a break statement in a loop or switch
type BreakStmt struct {
	Label string // Optional label for labeled break
}

func (*BreakStmt) node()          {}
func (*BreakStmt) statementNode() {}

// ReturnStmt represents a return statement: return expr or return expr, err
type ReturnStmt struct {
	Results []ExpressionNode // Return values (typically [value, error] or [value])
}

func (*ReturnStmt) node()          {}
func (*ReturnStmt) statementNode() {}

// RetryExpr wraps an expression with retry logic: retry(3) expr
type RetryExpr struct {
	Count  int            // Number of retry attempts
	Target ExpressionNode // Expression to retry
}

func (*RetryExpr) node()           {}
func (*RetryExpr) expressionNode() {}

// DotAccessExpr represents dot notation access: event.payload.subject
type DotAccessExpr struct {
	Object ExpressionNode // The object being accessed
	Field  string         // The field name
}

func (*DotAccessExpr) node()           {}
func (*DotAccessExpr) expressionNode() {}

// NilExpr represents the nil literal
type NilExpr struct{}

func (*NilExpr) node()           {}
func (*NilExpr) expressionNode() {}

// ----------------------------------------------------------------------------
// Function Definition Nodes (UPDATED - for Go-style receiver syntax)
// ----------------------------------------------------------------------------

// ReceiverType identifies the function receiver type.
type ReceiverType string

const (
	ReceiverQuery      ReceiverType = "Query"
	ReceiverMutation   ReceiverType = "Mutation"
	ReceiverAutomation ReceiverType = "Automation"
	ReceiverLogic      ReceiverType = "Logic"
	ReceiverSpec       ReceiverType = "Spec"
	ReceiverTool       ReceiverType = "Tool"
	ReceiverBuiltin    ReceiverType = "Builtin"
	ReceiverPrompt     ReceiverType = "Prompt"
	ReceiverProvider   ReceiverType = "Provider"
	ReceiverShape      ReceiverType = "Shape"
	ReceiverPolicy     ReceiverType = "Policy"
)

// FunctionReceiver represents the receiver in func (r Type) syntax
type FunctionReceiver struct {
	Name string       // Optional receiver name (e.g., "a" in "func (a Automation)")
	Type ReceiverType // The receiver type: Query, Mutation, Automation
}

// FunctionDef represents a named function definition.
// Go-style syntax: func (Type) name(args any) (any, error) { ... }
type FunctionDef struct {
	Attributes  []*Attribute      // @name Python-style attributes
	Receiver    *FunctionReceiver // Go-style receiver: (Query), (m Mutation), etc.
	Name        string
	Description string
	Type        FunctionType
	Args        []FunctionArg
	Body        Node     // Can be ExpressionNode, MutationStmt, AutomationDef, or []Node for block
	Returns     []string // Return type names, e.g., ["any", "error"]

	// Parsed directive values
	Enabled    bool   // from //memql:enabled
	Deprecated string // from //memql:deprecated (empty = not deprecated)
	Version    string // from //memql:version
	Internal   bool   // from //memql:internal
	Role       string // from //memql:role
	Permission string // from //memql:permission
	Timeout    string // from //memql:timeout
	CacheTTL   string // from //memql:cache (queries only)
	RateLimit  *RateLimitConfig
	Retry      int  // from //memql:retry
	Idempotent bool // from //memql:idempotent (mutations only)
	Audit      bool // from //memql:audit

	// ArgsSchema is the function's input schema, populated from the
	// file-top `args { ... }` block.
	ArgsSchema *ArgsSchema
}

// RateLimitConfig holds rate limiting configuration.
type RateLimitConfig struct {
	Requests int
	Per      string
}

func (*FunctionDef) node() {}

// FunctionType identifies the kind of function.
type FunctionType string

const (
	FunctionTypeQuery      FunctionType = "query"
	FunctionTypeMutation   FunctionType = "mutation"
	FunctionTypeAutomation FunctionType = "automation"
	FunctionTypeLogic      FunctionType = "logic"
	FunctionTypeSpec       FunctionType = "spec"
	FunctionTypeTool       FunctionType = "tool"
	FunctionTypeBuiltin    FunctionType = "builtin"
	FunctionTypePrompt     FunctionType = "prompt"
	FunctionTypeProvider   FunctionType = "provider"
	FunctionTypeShape      FunctionType = "shape"
	FunctionTypePolicy     FunctionType = "policy"
)

// FunctionArg represents a function argument declaration.
type FunctionArg struct {
	Name     string
	Type     string // optional type hint
	Required bool
	Default  any
}

// ----------------------------------------------------------------------------
// Automation Definition Nodes
// ----------------------------------------------------------------------------

// AutomationDef represents an automation definition.
type AutomationDef struct {
	Attributes  []*Attribute // @name Python-style attributes
	Name        string
	Description string
	Schedule    string // cron expression (from @schedule)
	Trigger     *TriggerDef
	Input       ExpressionNode
	Steps       []StepDef
	OnComplete  *StepDef
	OnError     *StepDef

	// Parsed attribute values
	Enabled    bool   // from @enabled
	Deprecated string // from @deprecated
	Version    string // from @version
	Internal   bool   // from @internal
	Role       string // from @role
	Permission string // from @permission
	Timeout    string // from @timeout
	RateLimit  *RateLimitConfig
	Retry      int  // from @retry
	Audit      bool // from @audit
	Async      bool // from @async
}

func (*AutomationDef) node() {}

// TriggerDef defines event-based triggers for an automation.
type TriggerDef struct {
	Event  string
	Filter string
}

// ArgsSchema defines an args-block schema -- the input schema for a
// procedural function, populated from the file-top `args { ... }`
// block. The struct is named "ArgsSchema" for historical reasons;
// the data it carries is purely the args schema.
type ArgsSchema struct {
	// Target names the block kind ("args").
	Target string
	// Fields defines the expected fields and their types
	Fields []*ArgsField
	// AdditionalProperties controls whether unknown fields are allowed.
	// nil means default behavior.
	AdditionalProperties *bool
}

// ArgsField defines a single field's type assertion.
type ArgsField struct {
	// Name is the field name (e.g., "subject", "email")
	Name string
	// Type is the expected type: "string", "number", "bool", "object", "array"
	Type string
	// Optional indicates if the field can be missing (field? syntax)
	Optional bool
	// Nested contains nested field assertions for object types
	Nested []*ArgsField
	// Enum restricts values to the provided set.
	Enum []any
	// Minimum/Maximum constrain numeric values.
	Minimum *float64
	Maximum *float64
	// Format constrains string format (e.g., date-time).
	Format string
	// MaxLength caps a string's rune count. Zero means unbounded.
	// Applies to string-typed fields only -- validator rejects the
	// arg if rune count exceeds the cap. Drives the
	// carrier#27 free-text caps.
	MaxLength int
	// Pattern is a regex string a value must match. Empty means
	// no pattern constraint. Applies to string-typed fields only;
	// the pattern is compiled at load time so an invalid regex
	// fails loud during DSL parsing, not at validation time.
	// Drives the carrier#28 ID-format enforcement.
	Pattern string
	// AdditionalProperties controls whether unknown fields are allowed for object values.
	AdditionalProperties *bool
	// Items defines array item schema.
	Items *ArgsField
}

// StepDef represents a step in an automation.
type StepDef struct {
	ID         string
	Name       string
	Type       StepType
	Condition  string // "if condition" after step type
	RetryCount int    // from retry(n) wrapper
	OnError    string // error handling strategy (e.g., "continue", "fail")
	Config     any    // Step-type-specific configuration
}

// StepType identifies the kind of step.
type StepType string

const (
	StepTypeQuery      StepType = "query"
	StepTypeMutation   StepType = "mutation"
	StepTypeForEach    StepType = "forEach"
	StepTypeParallel   StepType = "parallel"
	StepTypeSwitch     StepType = "switch"
	StepTypeFunction   StepType = "function"
	StepTypeAutomation StepType = "automation"
	StepTypeAction     StepType = "action"
)

// QueryStepConfig configures a query step.
type QueryStepConfig struct {
	Query ExpressionNode
}

// MutationStepConfig configures a mutation step.
type MutationStepConfig struct {
	Mutation *MutationStmt
}

// FunctionStepConfig configures a named function invocation step.
type FunctionStepConfig struct {
	Name string
	Args map[string]any
}

// ActionStepConfig configures an action-library replay step (#1758, epic
// #1734): `action { ref: "act_x@3", args: {...}, surface: "..." }`. Ref is a
// pinned-by-default action reference (id@version); Surface is an optional
// explicit surface binding the resolver honors first.
type ActionStepConfig struct {
	Ref     string
	Args    map[string]any
	Surface string
}

// ForEachStepConfig configures iteration over a collection.
type ForEachStepConfig struct {
	Source      string
	Filter      string
	As          string // Value variable (e.g., "item" in "for item := range")
	Index       string // Index variable (optional, e.g., "i" in "for i, item := range")
	Concurrency int
	Do          []StepDef
}

// ParallelStepConfig configures concurrent step execution.
type ParallelStepConfig struct {
	Branches []StepDef
	Wait     string
	FailFast bool
}

// SwitchStepConfig configures conditional branching.
type SwitchStepConfig struct {
	Expression string
	Cases      map[string]*SwitchCase
	Default    *SwitchCase
}

// SwitchCase defines what to execute for a switch case.
type SwitchCase struct {
	Steps []StepDef
}

// ----------------------------------------------------------------------------
// File/Module Nodes (NEW - for .memql files with multiple definitions)
// ----------------------------------------------------------------------------

// UseDeclaration represents a `use` import statement at the top of a
// .memql file. Two shapes coexist during the import-model migration:
//
//	Form A (legacy, retired in PR C):
//	  use cognition.participant            -- single concept binding
//	  use cognition.session as cognSession -- aliased single binding
//
//	Form B (canonical post-migration):
//	  use cognition.concepts.{ participant, space }
//	  use common.traits.{ isActiveRecord, isNotDeleted }
//
// Form B names a module file (`cognition.concepts` -> `dsl/cognition/
// concepts.memql`) and lists which exported constructs to pull into
// local scope. References inside the importing file use the bare
// names; the loader rejects collisions across imports.
//
// Names is non-empty for Form B and empty for Form A. Alias is only
// populated by Form A.
type UseDeclaration struct {
	// Path is the dotted module path as written: "cognition.concepts",
	// "common.traits", "agents.agentRole" (legacy).
	Path string
	// Alias is the optional `as <name>` binding -- Form A only.
	Alias string
	// Parts are the split components: ["cognition", "concepts"].
	Parts []string
	// Names is the imported-construct list from Form B. Empty for
	// Form A imports.
	Names []string
	// ResolvedId is filled by the concept resolver: "v1:cognition:participant".
	// Only meaningful for Form A; Form B resolves per-Name through the
	// loader's import index.
	ResolvedId string
}

func (*UseDeclaration) node() {}

// LeafName returns the short name used to reference this concept.
// If aliased, returns the alias; otherwise returns the last path component.
func (u *UseDeclaration) LeafName() string {
	if u.Alias != "" {
		return u.Alias
	}
	if len(u.Parts) > 0 {
		return u.Parts[len(u.Parts)-1]
	}
	return u.Path
}

// ImportDecl represents a `import "./path" [as alias]` entry inside
// an `import (...)` block at the top of a .memql file. This is the
// Go-style file-import surface introduced by the DSL import-model
// refactor (docs/internal/design/dsl-import-model-refactor.md). Imports name files;
// symbols within an imported file are reached as `<alias>.<name>`.
//
// Path is the raw string from source ("./cognition/participant" or
// "../common/space"). It is relative-only (validated at parse time);
// the loader resolves it against the importing file's directory and
// the configured DSL root. The `.memql` suffix is auto-appended at
// resolution time if absent.
//
// Alias is the local binding used in the importing file. If `as` is
// omitted the alias defaults to the path's basename (without the
// `.memql` suffix); the default rule is applied by the loader after
// parsing, so a parsed ImportDecl with Alias == "" carries the
// "default" intent.
//
// ResolvedPath is filled by the loader after path resolution
// (basename joined with the importing file's directory + root cap).
// Empty until the loader runs.
type ImportDecl struct {
	Path         string // raw source, e.g. "./cognition/participant"
	Alias        string // explicit alias from `as <name>`, empty when default
	ResolvedPath string // populated by the loader; canonical path inside the DSL root
}

func (*ImportDecl) node() {}

// File represents a parsed .memql file containing multiple definitions.
type File struct {
	Path        string
	Uses        []*UseDeclaration // legacy use declarations at the top of the file (retired in Commit 3)
	Imports     []*ImportDecl     // file-import entries from an `import (...)` block
	Definitions []Node            // FunctionDef, AutomationDef, ConceptDecl, etc.
}

func (*File) node() {}

// ConceptDecl is the shared-frontend AST node for a concept declaration.
// Introduced in Phase 2 of the language-improvements plan so the
// concept loader (component/database/memory-nodes) can consume AST
// nodes from the shared parser instead of running its own recursive-
// descent parser at concept_parser.go.
//
// The wiring happens in a follow-up: this type is defined up-front so
// downstream consumers (Sense hover, diagnostics, schema builder) can
// reference it before the legacy concept_parser.go is retired.
type ConceptDecl struct {
	Name          string              // concept name (post-colon trailing segment)
	Attributes    []*Attribute        // concept-level annotations (@description, @scope, @cache, ...)
	Properties    []*PropertyDecl     // field declarations
	Relationships []*RelationshipDecl // @relationship() annotations inside the body
	Path          string              // source path, for errors/diagnostics
}

func (*ConceptDecl) node() {}

// BuiltinDecl is the shared-frontend AST node for a struct-form
// builtin declaration (`builtin NAME { field type @required; ... }`).
// Introduced by memql#318 (sub-epic #309 / #306 child C) so the
// unified-kinds loader can parse builtins through the langparser
// instead of the hand-rolled builtin_parser.go mini-parser.
//
// Annotation surface (validated by the converter, not the parser):
//
//	@enabled / @disabled              lifecycle (engine-side flags)
//	@description("text")              documentation
//	@executor("integration.X.Y")      Go executor name -- required
//	@alias("name")                    parser-side alias (multi-valued)
//	@args(profile="...", stringKey="...", additionalProperties=...)
//	                                   parse-time argument contract
//	@sdk                              SDK-generator marker (no engine effect)
//
// Field grammar: `<name> <type> [@required]`. Type accepts primitives
// (string, bool, int, number, object) and `[]primitive` array forms.
// The converter (builtinDeclToFunction in the memql package) walks
// Fields + Attributes to produce a Function with Type=builtin.
type BuiltinDecl struct {
	Name       string          // builtin name
	Attributes []*Attribute    // builtin-level annotations
	Fields     []*BuiltinField // body field declarations
	Path       string          // source path, for errors/diagnostics
}

func (*BuiltinDecl) node() {}

// BuiltinField is a single field declaration inside a BuiltinDecl
// body. The Type string preserves the author-surface form
// (`string`, `bool`, `[]string`, etc.); the converter passes it
// through to BuiltinArgContract.Properties verbatim.
type BuiltinField struct {
	Name       string
	Type       string       // e.g. "string", "bool", "[]string"
	Required   bool         // from @required
	Attributes []*Attribute // any other field-level annotations (tolerated, not yet acted on)
}

// PromptDecl is the shared-frontend AST node for a struct-form
// prompt declaration (`prompt NAME { field type @required; ... }`).
// Introduced by memql#319 (sub-epic #309 / #306 child C) so the
// unified-kinds loader can parse prompts through the langparser
// instead of the hand-rolled prompt_parser.go mini-parser.
//
// Annotation surface (validated by the converter, not the parser):
//
//	@enabled / @disabled              lifecycle (engine-side flags)
//	@description("text")              documentation
//	@defaultProvider("name")          AI provider pinned by default
//	@templateFile("file.tmpl")        sidecar template path (relative to the prompt .memql file)
//
// Field grammar mirrors BuiltinField: `<name> <type> [@required
// @description("...") @enum(...) @default(...)]`. Type accepts
// primitives (string, bool, integer, number, object) and the
// `[]primitive` array-of-primitive shorthand (recorded as Type="[]X"
// like BuiltinField does — the memql-side converter normalises this
// to the prompt-internal "array" + elementType form).
//
// The legacy inline `@template("""...""")` body-level annotation is
// NOT supported on this path; every shipped prompt uses
// @templateFile + a sidecar today. The langparser's lexer doesn't
// tokenise triple-quoted strings, so an attempt to parse an inline
// template fails loud with a migration-pointing error; if the inline
// form becomes load-bearing again, the lexer grows triple-quoted
// support first.
type PromptDecl struct {
	Name       string         // prompt name
	Attributes []*Attribute   // prompt-level annotations
	Fields     []*PromptField // body field declarations (the input schema)
	Path       string         // source path, for errors/diagnostics
}

func (*PromptDecl) node() {}

// ActionDecl is the shared-frontend AST node for the SIMPLIFIED struct-form
// authored `action` declaration (construct-invocation ADR Decision 3, Story 4
// / memql#2328, epic #2322):
//
//	use capabilities.shell.{ script }
//	@description("Check out the memQL repo at a specific version.")
//	action cloneRepoAtVersion {
//	  args { workdir string @required; ref string @required }
//	  capability script(script: "deploy.cloneRepo", workdir: args.workdir, ref: args.ref)
//	}
//
// The action is reduced to an `args` block + a SINGLE `capability <verb>(...)`
// call (ADR Decision 3). The legacy surfaces were dropped: `@kind`,
// `@sideEffect` (the authoritative class lives on the CAPABILITY now, Story 5),
// `@reliability`, `intent` (merged into `@description`), `params` (renamed to
// `args`), and `argTemplate` / `$params.X` (replaced by the typed capability
// call). There is NO `body { }` and no `return`: an action body is exactly the
// one capability call. The bare verb (`script`) resolves through the file-top
// `use capabilities.<path>.{ verb }` import to a full dotted capability name
// (`shell.script`).
//
// An authored action is DECLARATIVE: it names exactly one external capability,
// declares its input schema (`Args`), and binds the capability's call args from
// those inputs (`CapabilityArgs`). It NEVER touches the MemQL graph. The action
// lives in its own registry (component/actions); it is invoked from an
// automation `action <name>(...)` step. The I7 call-graph validator enforces
// the one-external-capability / no-graph rules.
type ActionDecl struct {
	Name       string           // action name (the slice/reference name)
	Attributes []*Attribute     // action-level annotations (@description, @enabled/@disabled)
	Args       *ArgsSchema      // input schema (the `args { ... }` block); nil when omitted
	Capability string           // the bare imported capability verb the single body call names (e.g. "script", "tagRelease"); resolved to a full dotted name by the loader via the file's capability imports
	CallArgs   []*ActionCallArg // the capability call's named args (key -> value expression), in source order
	Path       string           // source path, for errors/diagnostics
}

func (*ActionDecl) node() {}

// ActionCallArg is one named argument of the single `capability <verb>(...)`
// call in an action body: `Key` is the capability-argument name and `Value`
// is its bound value expression (a string/number/bool LiteralExpr, or an
// ArgRefExpr referencing `args.<path>`). Order is source order.
type ActionCallArg struct {
	Key   string
	Value ExpressionNode
}

// CapabilityDecl is the shared-frontend AST node for a top-level
// `capability` declaration (construct-invocation ADR Decision 4, Story
// 5 / memql#2325, epic #2322):
//
//	@sideEffect("write")
//	@description("Create a git tag + GitHub release for a version.")
//	capability integration.github.tagRelease {
//	  args { repo string @required; tag string @required }
//	}
//
// A capability is declared like a typed, side-effect-classified builtin
// with NO body -- it is SURFACE-BACKED: the implementation is supplied
// by the runtime surface (fs / shell / http / integration / mcp), not
// the DSL. Its Name is namespaced/dotted (`integration.github.tagRelease`,
// `fs.readFile`, `shell.script`, `http.get`, `mcp.<tool>`).
//
// The `@sideEffect("read"|"write"|"exec")` annotation -- the UNSPOOFABLE
// side-effect classification -- lives HERE (ADR §7): an authored or
// planner-generated action cannot spoof its risk class, because the
// authoritative class is declared on the capability the action invokes.
// `@description(...)` documents the verb and is the planner embedding
// source.
//
// Args is the optional input schema (`args { name type @required ... }`),
// reusing the same args-block grammar action/builtin/file-top use. It is
// nil when the capability declares no args block.
//
// This node is purely a DECLARATION: the invocation grammar
// (`capability NAME(args)` call sites) and the action rewrite that
// consumes it land in later stories (2 + 4).
type CapabilityDecl struct {
	Name       string       // dotted/namespaced capability verb (e.g. "integration.github.tagRelease")
	Attributes []*Attribute // capability-level annotations (@sideEffect, @description, @enabled/@disabled)
	Args       *ArgsSchema  // input schema from the `args { ... }` block; nil when omitted
	Path       string       // source path, for errors/diagnostics
}

func (*CapabilityDecl) node() {}

// IsDisabled reports whether the capability decl carries @disabled.
// @enabled is an accepted no-op per the lifecycle ruling (#2607). Shared by
// every capability walk (component/memql's name loader and
// component/actions' catalog) so the validation and dispatch sets cannot
// diverge on lifecycle.
func (d *CapabilityDecl) IsDisabled() bool {
	if d == nil {
		return false
	}
	for _, attr := range d.Attributes {
		if attr != nil && attr.Name == AttrDisabled {
			return true
		}
	}
	return false
}

// PromptField is a single field declaration inside a PromptDecl
// body. Mirrors BuiltinField (memql#318) so the per-construct
// playbook for langparser-native struct declarations stays uniform.
type PromptField struct {
	Name       string
	Type       string       // e.g. "string", "object", "[]object"
	Required   bool         // from @required
	Attributes []*Attribute // any other field-level annotations (@description / @enum / @default)
}

// SpecDecl is the shared-frontend AST node for a struct-form spec
// or trait declaration (`spec NAME { <bool-expr> }` /
// `trait NAME { <bool-expr> }`). Introduced by memql#334
// (sub-epic #329 / #310 Stage 1C) so the unified spec loader can
// parse specs + traits through the langparser instead of the
// hand-rolled spec_parser.go mini-parser.
//
// Body grammar: a single boolean expression. The langparser parses
// the body via the shared expression-parsing path and stores the
// resulting typed ExpressionNode here; the memql-side converter
// (specDeclToSpec) runs NewASTConverter().ConvertExpression on it
// + the existing classifier + boolean-shape validator.
//
// Annotation surface (validated by the converter, not the parser):
//
//	@description("text")              documentation (both specs + traits)
//	@enabled / @disabled              lifecycle (traits only; no-op flags)
//
// IsTrait discriminates the two header keywords (`spec NAME { ... }`
// vs `trait NAME { ... }`). Specs and traits share the SpecRegistry
// runtime contract; the trait flag drives the converter's
// classification of whether the entry binds a concept (specs do via
// file-top use + signature; traits are concept-agnostic).
type SpecDecl struct {
	Name       string         // spec / trait name
	BoundName  string         // signature binding: `spec <BoundName> <Name>` resolves to an imported shape XOR concept (specs only; empty for traits)
	IsTrait    bool           // true for `trait NAME { ... }`, false for `spec NAME { ... }`
	Attributes []*Attribute   // declaration-level annotations
	Body       ExpressionNode // parsed boolean expression body (the `return <bool>` body's expression)
	Path       string         // source path, for errors/diagnostics
}

func (*SpecDecl) node() {}

// ShapeDecl is the shared-frontend AST node for a struct-form shape
// declaration (`shape NAME { id; payload.X; ... }`). Introduced by
// memql#315 (sub-epic #309 / #306 child C) so the unified-kinds loader
// can parse shapes through the langparser instead of the hand-rolled
// shape_parser.go mini-parser.
//
// The body is a sequence of dotted-path field references; each path
// is preserved verbatim and the memql-side converter handles the
// kind/namespace prefix translation (`row.X` -> `X`, `<concept>.X`
// -> `payload.X`, etc.).
//
// Annotation surface (validated by the converter, not the parser):
//
//	@description("text")
//	@row                              // shape projects row payload + intrinsics
//	@actor                            // shape projects engine envelope
//	@useConcept(name1, name2, ...)    // bound concept names (bare)
//
// The two-identifier signature `shape <Concept> <name> { ... }` is
// recorded on SignatureConcept; the bound concept is also appended to
// UseConcepts so downstream consumers see one canonical list.
type ShapeDecl struct {
	Name             string       // shape name
	Attributes       []*Attribute // shape-level annotations
	Paths            []string     // body field paths in source order
	SignatureConcept string       // bound concept from the two-identifier signature, if any
	Path             string       // source path, for errors/diagnostics
}

func (*ShapeDecl) node() {}

// PropertyDecl is a single field declaration inside a ConceptDecl.
// Supports both primitive types (string, bool, int, float, datetime,
// enum, array, object) and nested object blocks via the Nested slice.
type PropertyDecl struct {
	Name       string
	Type       *TypeRef
	Attributes []*Attribute    // property-level annotations (@required, @default, @description, ...)
	Nested     []*PropertyDecl // populated when Type.Kind == "object" AND the field is written as `name { ... }`

	// Variants is populated when the property is declared with a
	// @variant(discriminator="fieldName") block attached to the
	// nested object body, e.g.:
	//
	//   credentials object @variant(discriminator="identityType") {
	//     oauth          { provider string @required; externalUserId string @required }
	//     api_key        { keyHash string @required }
	//   }
	//
	// The schema builder emits a `oneOf` with one sub-schema per
	// variant, so JSON-Schema validation rejects payloads whose
	// shape doesn't match the declared discriminator value.
	Variants []*PropertyVariant
}

// PropertyVariant is one branch of a discriminated-union property.
type PropertyVariant struct {
	Name       string          // discriminator value, e.g. "oauth"
	Properties []*PropertyDecl // fields required/allowed when the discriminator matches
}

func (*PropertyDecl) node() {}

// TypeRef describes a property's declared type. Uses a discriminated
// shape rather than a string so downstream consumers don't have to
// re-parse the type expression.
type TypeRef struct {
	// Kind is one of: "string", "bool", "int", "float", "datetime",
	// "enum", "array", "object", "any", or a concept reference
	// (e.g. "v1:cognition:space") for relationship targets.
	Kind       string
	EnumValues []string // populated when Kind == "enum"
	ArrayItem  *TypeRef // populated when Kind == "array"
	Format     string   // optional format hint, e.g. "date-time"
}

// RelationshipDecl is a @relationship() annotation parsed out of a
// concept body. Kept separate from Attributes so downstream consumers
// have typed access without string-splitting annotation values.
type RelationshipDecl struct {
	Type        string // "parent", "child", "alias", "owns", "createdBy", "contains", "interactsWith"
	Field       string
	FieldSource string // "payload" (default) or "table" (edge table)
	Target      string // concept name, e.g. "v1:cognition:space"
	Direction   string // "outgoing", "incoming", "bidirectional"
}

// ProviderDecl is the shared-frontend AST node for an AI provider
// declaration. Mirrors ConceptDecl in role: the langparser produces
// this typed node so the per-construct loader
// (component/memql.LoadUnifiedProviders) can consume it without
// running its own hand-rolled parser.
//
// Annotation surface (validated by the parser's allowed-annotation
// switch):
//
//	@enabled / @disabled              lifecycle (engine-side flags) -- see
//	                                  AttrEnabled/AttrDisabled for the
//	                                  canonical semantics; @disabled on a
//	                                  @base propagates to its @extends children
//	@base                             vendor-level metadata (no @model)
//	@type("OpenAI") / @model("...")   provider type + concrete model
//	@modality("text"|"tts"|"stt")     defaults to text
//	@default                          first @default wins the runtime default
//	@extends("parent") @description   inheritance + docs
//
// Authoring shape:
//
//	@description("OpenAI GPT-5 Mini")
//	@type("OpenAI")
//	@model("gpt-5-mini")
//	@modality("text")
//	provider chat5Mini {
//	  auth {
//	    apiKey  env("MEMQL_AI_OPENAI_API_KEY")
//	  }
//	  params {
//	    maxTokens  4096
//	    temperature  0.7
//	  }
//	}
//
// Base providers (vendor-level metadata, no @model) carry @base:
//
//	@base
//	@type("OpenAI")
//	provider openai {
//	  auth { apiKey env("MEMQL_AI_OPENAI_API_KEY") }
//	}
//
// Child providers inherit Type + Auth from a @extends("...") parent
// at load time. The parser captures Extends but doesn't resolve it;
// that's the loader's job.
//
// Auth values written as `env("VAR")` are surfaced as the literal
// string `${VAR}` so the loader's resolveAuthPlaceholders pass can
// substitute the OS env at startup.
type ProviderDecl struct {
	Name        string
	Description string
	Type        string // "OpenAI" / "Anthropic" / "OpenAIStream" / etc.
	Model       string // empty for @base providers
	Modality    string // "text" (default) / "tts" / "stt"
	IsDefault   bool   // @default flag
	IsBase      bool   // @base flag
	Extends     string // @extends("parentName") -- parent provider name
	// Disabled is the @disabled lifecycle flag (engine-side). A
	// disabled provider is skipped at load -- not registered, no auth
	// resolution attempted -- and a disabled @base disables every
	// child that @extends it. @enabled is the explicit-on form and a
	// no-op default (mirrors functions/builtins/prompts). @disabled
	// means "not loaded/active right now"; it does NOT mean
	// deprecated/abandoned/exempt from maintenance -- it is a
	// reversible on/off switch.
	Disabled bool
	Params   map[string]any
	Auth     map[string]string
}

func (*ProviderDecl) node() {}

// ToolDecl is the shared-frontend AST node for an AI tool
// declaration. Mirrors ConceptDecl / ShapeDecl / ProviderDecl in
// role: the langparser produces this typed node so the per-construct
// loader (component/memql.LoadUnifiedTools) can consume it without
// running its own hand-rolled parser.
//
// Authoring shape:
//
//	@enabled
//	@description("Search for users.")
//	@handler(type="query", query="concept==v1:memql:backend:user")
//	@executionTime("fast")
//	tool searchUsers {
//	  active  boolean  @description("Filter by active status")
//	  limit   integer  @default("10") @description("Max results")
//	}
//
// The handler attribute carries multiple named arguments (type,
// query, name, url, method); the parser splits them out so the
// loader doesn't reparse the @handler args every call. The
// rateLimit attribute is similar (maxCalls, periodSeconds). Field-
// level @autoInjected is captured as a per-field flag.
type ToolDecl struct {
	Name        string
	Description string

	// Handler routes the tool to its execution backend.
	HandlerType   string // "query" / "function" / "webhook" / "delegate"
	HandlerName   string // function name (for type=function) OR query expression (for type=query)
	HandlerURL    string // webhook URL (for type=webhook)
	HandlerMethod string // webhook HTTP method (upper-cased; default applied later by the converter)

	// Annotations.
	Destructive          bool     // @destructive flag
	RequiresConfirmation bool     // @requiresConfirmation flag
	ExecutionTime        string   // "fast" / "medium" / "slow"
	RateLimitMaxCalls    int      // 0 = no rate limit; @rateLimit(maxCalls=...)
	RateLimitPeriod      int      // seconds; paired with RateLimitMaxCalls
	ClientExecution      bool     // @clientExecution flag -- dispatched to the browser via ClientToolCall instead of executing server-side
	AllowedRoles         []string // @allowedRoles("assistant", ...) -- empty = no restriction
	Scopes               []string // @scopes("operator", ...) -- caller must hold a superset
	MCPExposed           bool     // @mcp flag -- opt this tool into the curated MCP connector surface (memql#1596)
	Disabled             bool     // @disabled flag -- the loader skips registration (#2606)

	// Fields populates the tool's input schema.
	Fields []ToolFieldDecl
}

func (*ToolDecl) node() {}

// ToolFieldDecl is one entry in a ToolDecl's input-schema field list.
// Mirrors the trailing-annotation syntax `name type @required
// @description(\"...\") @enum(\"a\", \"b\") @default(\"x\")
// @autoInjected`.
type ToolFieldDecl struct {
	Name         string
	Type         string   // "string" / "integer" / "int" / "number" / "float" / "boolean" / "bool" / "object" / "array"
	Required     bool     // @required flag
	AutoInjected bool     // @autoInjected -- value is stamped server-side; LLM-supplied values are dropped at dispatch
	Description  string   // @description("...") value
	EnumValues   []string // @enum("a", "b", "c") values; empty = no enum constraint
	Default      string   // @default("x") value, stored as a string regardless of the field's declared type
}

// PolicyDecl is the shared-frontend AST node for an AI Router
// routing policy declaration. Mirrors ConceptDecl / ShapeDecl /
// ProviderDecl / ToolDecl in role: the langparser produces this
// typed node so the per-construct loader
// (component/memql.LoadUnifiedPolicies) can consume it without
// running its own hand-rolled parser.
//
// Authoring shape:
//
//	@description("Balanced LLM for most agent replies.")
//	@primary("chat54Mini")
//	@fallback("chat53")
//	@fallback("anthropicSonnet")
//	@maxLatencyMs(8000)
//	@maxTimeToFirstTokenMs(500)
//	@preferredRole("assistant")
//	policy balancedChat { }
//
// The `policy NAME { }` body is empty today; the grammar reserves
// brace space for future per-vendor tuning knobs. Multiple
// `@fallback` / `@preferredRole` annotations on the same policy
// accumulate (order preserved).
//
// Distinct from cross-cutting decision policies (`func (Policy)
// name { ... }`) which are parsed via the general function-parser
// path. This node represents only the AI-Router provider-chain
// shape.
type PolicyDecl struct {
	Name                  string
	Description           string
	Primary               string
	Fallbacks             []string
	MaxLatencyMs          int
	MaxTimeToFirstTokenMs int
	PreferredRoles        []string
}

func (*PolicyDecl) node() {}

// SeedDecl is the shared-frontend AST node for a `seed NAME { ... }`
// declaration. Introduced by memql#335 (sub-epic #329 / #310 Stage 1C)
// so the unified-seeds loader can parse seeds through the langparser
// instead of the hand-rolled seed_parser.go mini-parser.
//
// Authoring shape (canonical, post-migration):
//
//	use agents.concepts.{ agent }
//
//	@scope("perUser")
//	@templateFile("templates/assistant.tmpl")
//	@description("Per-user Assistant baseline.")
//	seed agent assistant {
//	  name:        "Assistant"
//	  description: "Designated fallback."
//	  providerConfig {
//	    llm {
//	      policyName: "balancedChat"
//	      temperature: 0.7
//	    }
//	  }
//	}
//
// The two-identifier signature `seed <Concept> <name> { ... }` binds
// the seed to a concept resolved via the file's Form B imports. The
// legacy single-identifier form (`seed <name>` + file-top
// `use <ns>.<concept>`) is preserved through SignatureConcept = ""
// + non-empty UseNamespace / UseConcept on the converter side.
//
// Body grammar:
//   - `<field>: <scalar>` -- scalar value (string / int / float / bool)
//   - `<field>: ["a", "b"]` -- string-array value (no other element types)
//   - `<field> { ... }` -- nested block (recursive)
//
// Type unions sit on SeedValue.Kind; nested blocks recurse via
// SeedBlock so the loader (and the in-memql converter that bridges
// to the existing `seedDecl` / `seedBlock` internal types) can walk
// the tree without re-parsing.
type SeedDecl struct {
	Name             string     // declaration name (`seed XXX`)
	SignatureConcept string     // bound concept from the two-identifier signature, if any
	Description      string     // @description
	Namespace        string     // @namespace
	Version          string     // @version
	Scope            string     // @scope: "global" | "perUser" (empty -> loader applies default)
	TemplateFile     string     // @templateFile path (optional)
	Disabled         bool       // @disabled -- the loader skips registration (#2606)
	Body             *SeedBlock // root body block (always non-nil)
	Path             string     // source path, for errors/diagnostics
}

func (*SeedDecl) node() {}

// SeedBlock is one `{ ... }` worth of field assignments + nested
// blocks. Insertion order is preserved via Keys so downstream
// diagnostics and the materialiser see fields in author order.
type SeedBlock struct {
	Keys   []string              // insertion order of Fields + Nested keys (union)
	Fields map[string]*SeedValue // scalar / string-array values
	Nested map[string]*SeedBlock // nested blocks, keyed by field name
}

// SeedValueKind enumerates the typed scalar forms a seed field can
// carry. Mirrors the in-memql `seedValueKind` enum 1:1; the converter
// preserves the kind tag without rewriting.
type SeedValueKind int

const (
	SeedValueNone SeedValueKind = iota
	SeedValueString
	SeedValueInt
	SeedValueFloat
	SeedValueBool
	SeedValueStringArray
)

// SeedValue is the typed value of one `<field>: <value>` body
// assignment. Only one of String / Int / Float / Bool / StringArray
// is populated, indicated by Kind.
type SeedValue struct {
	Kind        SeedValueKind
	String      string
	Int         int64
	Float       float64
	Bool        bool
	StringArray []string
}
