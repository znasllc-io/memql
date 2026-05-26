package parser

// Auto-generated type aliases for backward compatibility with consumers
// that imported AST types from the parser package. New code should
// import "component/language/ast" directly.
//
// Phase 2 Item 9 of the memql-language-improvements plan moved the
// concrete AST types to a dedicated ast package; this file preserves
// the parser.<Type> surface via type aliases so the ~25 existing
// consumer files keep compiling unchanged.

import "github.com/znasllc-io/memql/component/language/ast"

// Interface aliases (require Go 1.24+; we are on 1.26.1).
type (
	Node           = ast.Node
	ExpressionNode = ast.ExpressionNode
	StatementNode  = ast.StatementNode
)

// Struct / named-type aliases for every AST type.
type (
	FieldReference          = ast.FieldReference
	AttributeMap            = ast.AttributeMap
	LogicalOp               = ast.LogicalOp
	ComparisonOperator      = ast.ComparisonOperator
	SortDirection           = ast.SortDirection
	SortField               = ast.SortField
	RelationshipFunction    = ast.RelationshipFunction
	LogicalExpr             = ast.LogicalExpr
	ComparisonExpr          = ast.ComparisonExpr
	RelationshipExpr        = ast.RelationshipExpr
	SortExpr                = ast.SortExpr
	PaginateExpr            = ast.PaginateExpr
	SelectExpr              = ast.SelectExpr
	TimestampExpr           = ast.TimestampExpr
	DepthExpr               = ast.DepthExpr
	ShapeExpr               = ast.ShapeExpr
	FunctionCallExpr        = ast.FunctionCallExpr
	BuiltinFunctionExpr     = ast.BuiltinFunctionExpr
	SpecReferenceExpr       = ast.SpecReferenceExpr
	ConditionalFilterExpr   = ast.ConditionalFilterExpr
	ArgRefExpr              = ast.ArgRefExpr
	LiteralExpr             = ast.LiteralExpr
	SIExpr                  = ast.SIExpr
	VarRefExpr              = ast.VarRefExpr
	StepRefExpr             = ast.StepRefExpr
	InputRefExpr            = ast.InputRefExpr
	ItemRefExpr             = ast.ItemRefExpr
	IndexRefExpr            = ast.IndexRefExpr
	EventRefExpr            = ast.EventRefExpr
	CallerRefExpr           = ast.CallerRefExpr
	ErrorRefExpr            = ast.ErrorRefExpr
	ErrorExpr               = ast.ErrorExpr
	TimestampExprFunc       = ast.TimestampExprFunc
	FieldRefExpr            = ast.FieldRefExpr
	ConcatExpr              = ast.ConcatExpr
	CoalesceExpr            = ast.CoalesceExpr
	CondExpr                = ast.CondExpr
	TernaryExpr             = ast.TernaryExpr
	FirstExpr               = ast.FirstExpr
	LastExpr                = ast.LastExpr
	LowerExpr               = ast.LowerExpr
	UpperExpr               = ast.UpperExpr
	TrimExpr                = ast.TrimExpr
	ContainsExpr            = ast.ContainsExpr
	HashExpr                = ast.HashExpr
	CanonicalIdExpr         = ast.CanonicalIdExpr
	AndExpr                 = ast.AndExpr
	OrExpr                  = ast.OrExpr
	NotExpr                 = ast.NotExpr
	EqExpr                  = ast.EqExpr
	LtExpr                  = ast.LtExpr
	GtExpr                  = ast.GtExpr
	LteExpr                 = ast.LteExpr
	GteExpr                 = ast.GteExpr
	ToStringExpr            = ast.ToStringExpr
	AddDurationExpr         = ast.AddDurationExpr
	DaysBetweenExpr         = ast.DaysBetweenExpr
	SubtractTimestampsExpr  = ast.SubtractTimestampsExpr
	YearExpr                = ast.YearExpr
	QuarterExpr             = ast.QuarterExpr
	MonthExpr               = ast.MonthExpr
	DayOfMonthExpr          = ast.DayOfMonthExpr
	IsAnniversaryExpr       = ast.IsAnniversaryExpr
	IsFirstDayOfQuarterExpr = ast.IsFirstDayOfQuarterExpr
	MutationStmt            = ast.MutationStmt
	MutationKind            = ast.MutationKind
	QueryStmt               = ast.QueryStmt
	Attribute               = ast.Attribute
	AssignStmt              = ast.AssignStmt
	ForRangeStmt            = ast.ForRangeStmt
	SwitchStmt              = ast.SwitchStmt
	CaseClause              = ast.CaseClause
	IfStmt                  = ast.IfStmt
	ContinueStmt            = ast.ContinueStmt
	BreakStmt               = ast.BreakStmt
	ReturnStmt              = ast.ReturnStmt
	RetryExpr               = ast.RetryExpr
	DotAccessExpr           = ast.DotAccessExpr
	NilExpr                 = ast.NilExpr
	ReceiverType            = ast.ReceiverType
	FunctionReceiver        = ast.FunctionReceiver
	FunctionDef             = ast.FunctionDef
	RateLimitConfig         = ast.RateLimitConfig
	FunctionType            = ast.FunctionType
	FunctionArg             = ast.FunctionArg
	AutomationDef           = ast.AutomationDef
	TriggerDef              = ast.TriggerDef
	ArgsSchema              = ast.ArgsSchema
	ArgsField               = ast.ArgsField
	StepDef                 = ast.StepDef
	StepType                = ast.StepType
	QueryStepConfig         = ast.QueryStepConfig
	MutationStepConfig      = ast.MutationStepConfig
	FunctionStepConfig      = ast.FunctionStepConfig
	ForEachStepConfig       = ast.ForEachStepConfig
	ParallelStepConfig      = ast.ParallelStepConfig
	SwitchStepConfig        = ast.SwitchStepConfig
	SwitchCase              = ast.SwitchCase
	UseDeclaration          = ast.UseDeclaration
	ImportDecl              = ast.ImportDecl
	File                    = ast.File
	ConceptDecl             = ast.ConceptDecl
	ShapeDecl               = ast.ShapeDecl
	BuiltinDecl             = ast.BuiltinDecl
	BuiltinField            = ast.BuiltinField
	SpecDecl                = ast.SpecDecl
	ProviderDecl            = ast.ProviderDecl
	ToolDecl                = ast.ToolDecl
	ToolFieldDecl           = ast.ToolFieldDecl
	PromptDecl              = ast.PromptDecl
	PromptField             = ast.PromptField
	PolicyDecl              = ast.PolicyDecl
	SeedDecl                = ast.SeedDecl
	SeedBlock               = ast.SeedBlock
	SeedValue               = ast.SeedValue
	SeedValueKind           = ast.SeedValueKind
	PropertyDecl            = ast.PropertyDecl
	PropertyVariant         = ast.PropertyVariant
	TypeRef                 = ast.TypeRef
	RelationshipDecl        = ast.RelationshipDecl
)

// Enum / typed-string constants re-exported as vars (Go const aliases
// do not carry through simple `=` redeclaration; using var is idiomatic
// for this compatibility shim and the values are immutable in practice).
var (
	AttrAsync                = ast.AttrAsync
	AttrAudit                = ast.AttrAudit
	AttrCache                = ast.AttrCache
	AttrDeprecated           = ast.AttrDeprecated
	AttrDescription          = ast.AttrDescription
	AttrDestructive          = ast.AttrDestructive
	AttrDisabled             = ast.AttrDisabled
	AttrEnabled              = ast.AttrEnabled
	AttrExecutionTime        = ast.AttrExecutionTime
	AttrExecutor             = ast.AttrExecutor
	AttrFilter               = ast.AttrFilter
	AttrHandler              = ast.AttrHandler
	AttrIdempotent           = ast.AttrIdempotent
	AttrInternal             = ast.AttrInternal
	AttrPermission           = ast.AttrPermission
	AttrPublic               = ast.AttrPublic
	AttrRateLimit            = ast.AttrRateLimit
	AttrRequiresConfirmation = ast.AttrRequiresConfirmation
	AttrRetry                = ast.AttrRetry
	AttrRole                 = ast.AttrRole
	AttrSchedule             = ast.AttrSchedule
	AttrUse                  = ast.AttrUse
	AttrUseSpec              = ast.AttrUseSpec
	AttrUseTrait             = ast.AttrUseTrait
	AttrUseQuery             = ast.AttrUseQuery
	AttrUseMutation          = ast.AttrUseMutation
	AttrUseAutomation        = ast.AttrUseAutomation
	AttrUseLogic             = ast.AttrUseLogic
	AttrUseTool              = ast.AttrUseTool
	AttrUsePrompt            = ast.AttrUsePrompt
	AttrUseProvider          = ast.AttrUseProvider
	AttrUseBuiltin           = ast.AttrUseBuiltin
	AttrTimeout              = ast.AttrTimeout
	AttrTrigger              = ast.AttrTrigger
	AttrVersion              = ast.AttrVersion
	FunctionTypeAutomation   = ast.FunctionTypeAutomation
	FunctionTypeBuiltin      = ast.FunctionTypeBuiltin
	FunctionTypeLogic        = ast.FunctionTypeLogic
	FunctionTypeMutation     = ast.FunctionTypeMutation
	FunctionTypePolicy       = ast.FunctionTypePolicy
	FunctionTypePrompt       = ast.FunctionTypePrompt
	FunctionTypeProvider     = ast.FunctionTypeProvider
	FunctionTypeQuery        = ast.FunctionTypeQuery
	FunctionTypeShape        = ast.FunctionTypeShape
	FunctionTypeSpec         = ast.FunctionTypeSpec
	FunctionTypeTool         = ast.FunctionTypeTool
	LogicalAnd               = ast.LogicalAnd
	LogicalOr                = ast.LogicalOr
	OpEq                     = ast.OpEq
	OpGe                     = ast.OpGe
	OpGt                     = ast.OpGt
	OpHas                    = ast.OpHas
	OpIn                     = ast.OpIn
	OpLe                     = ast.OpLe
	OpLt                     = ast.OpLt
	OpMissing                = ast.OpMissing
	OpNe                     = ast.OpNe
	OpNotMissing             = ast.OpNotMissing
	OpOut                    = ast.OpOut
	ReceiverAutomation       = ast.ReceiverAutomation
	ReceiverBuiltin          = ast.ReceiverBuiltin
	ReceiverLogic            = ast.ReceiverLogic
	ReceiverMutation         = ast.ReceiverMutation
	ReceiverPolicy           = ast.ReceiverPolicy
	ReceiverPrompt           = ast.ReceiverPrompt
	ReceiverProvider         = ast.ReceiverProvider
	ReceiverQuery            = ast.ReceiverQuery
	ReceiverShape            = ast.ReceiverShape
	ReceiverSpec             = ast.ReceiverSpec
	ReceiverTool             = ast.ReceiverTool
	RelAliasOf               = ast.RelAliasOf
	RelChildOf               = ast.RelChildOf
	RelContains              = ast.RelContains
	RelCreatedBy             = ast.RelCreatedBy
	RelEquals                = ast.RelEquals
	RelIds                   = ast.RelIds
	RelInteractsWith         = ast.RelInteractsWith
	RelOwns                  = ast.RelOwns
	RelParentOf              = ast.RelParentOf
	SortAsc                  = ast.SortAsc
	SortDesc                 = ast.SortDesc
	StepTypeAutomation       = ast.StepTypeAutomation
	StepTypeForEach          = ast.StepTypeForEach
	StepTypeFunction         = ast.StepTypeFunction
	StepTypeMutation         = ast.StepTypeMutation
	StepTypeParallel         = ast.StepTypeParallel
	StepTypeQuery            = ast.StepTypeQuery
	StepTypeSwitch           = ast.StepTypeSwitch

	MutationKindInsert = ast.MutationKindInsert
	MutationKindUpdate = ast.MutationKindUpdate
)
