package memql

import (
	"time"

	"github.com/znasllc-io/memql/component/language/ast"
)

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

// SIInvocation describes an si() call.
type SIInvocation struct {
	TemplateId       string
	ProviderOverride *string
	// ModelOverride picks the concrete model name within the selected
	// provider (e.g. "claude-sonnet-4-5-20250929"). When non-nil and
	// non-empty, the provider is responsible for honouring it on this
	// one Call; all other invocations continue to use the provider's
	// registry-time cfg.Model.
	//
	// Introduced to let delegateTakeover route a delegated turn to a
	// stronger reasoning model without forcing the same tier on
	// every other agent in the app.
	ModelOverride *string
	// EnablePromptCache toggles provider-side prompt caching for this
	// one invocation. Currently honoured by the Anthropic provider:
	// a cache_control ephemeral block is attached to the system
	// prompt, so the ~5000-token System scope fence in agentReply.tmpl
	// is cached between turns. Cache misses still work; cache hits
	// cut input cost by ~90% and drop first-token latency.
	EnablePromptCache bool
	CacheSeconds      *int
}

// SIExpression captures si() invocation nodes parsed inside MemQL expressions.
type SIExpression struct {
	Invocation *SIInvocation
}

func (*SIExpression) isExpressionNode() {}

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

// PaginateExpression limits and offsets results produced by its target expression.
type PaginateExpression struct {
	Target ExpressionNode
	Limit  *int
	Offset *int
}

func (*PaginateExpression) isExpressionNode() {}

// SelectExpression projects fields for the results produced by its target expression.
type SelectExpression struct {
	Target ExpressionNode
	Fields []FieldReference
}

func (*SelectExpression) isExpressionNode() {}

// TimestampExpression pins execution to a given point in time.
type TimestampExpression struct {
	Target    ExpressionNode
	Timestamp *time.Time
	UseLatest bool
}

func (*TimestampExpression) isExpressionNode() {}

// DepthExpression overrides relationship traversal depth for its target expression.
type DepthExpression struct {
	Target ExpressionNode
	Depth  int
}

func (*DepthExpression) isExpressionNode() {}

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

	Concept    string
	ID         string
	PayloadRaw string
	CreatedAt  *time.Time
	ParentRef  *string
	AliasOfRef *string
}

func cloneSIInvocation(src *SIInvocation) *SIInvocation {
	if src == nil {
		return nil
	}
	clone := &SIInvocation{
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
