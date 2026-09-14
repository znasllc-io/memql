// Package tiers names the positions an expression can be written in.
//
// It is the tier manifest of D11 in
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md,
// and this is its seed: the positions only. The epic that owns the expression
// language (dsl-v1-expressions) adds, per position, the node kinds and
// functions allowed there and the tier each belongs to (P, pushed down to SQL;
// M, evaluated in process). Until then the conformance corpus's second
// completeness gate reads Positions() alone and asks each position for one
// case that loads and one that is refused (test/conformance/2026/expr/).
//
// A position's value is also the name of its corpus directory, so the values
// are part of the corpus layout and do not change once cases are written
// against them.
package tiers

// Position is one place in the language where an expression is written.
type Position string

const (
	// PositionQueryFilter is a query's `filter` clause, and the query a tool's
	// @handler(query=...) runs.
	PositionQueryFilter Position = "queryFilter"
	// PositionSort is a query's `sort` key.
	PositionSort Position = "sort"
	// PositionSpecBody is the `return` expression of a spec or a trait.
	PositionSpecBody Position = "specBody"
	// PositionRowAuthzArgument is an argument of a concept's @rowAuthz.
	PositionRowAuthzArgument Position = "rowAuthzArgument"
	// PositionAutomationCondition is an automation condition: an `if` step,
	// a precondition, a forEach `where`.
	PositionAutomationCondition Position = "automationCondition"
	// PositionTriggerFilter is an automation's @filter(...) over the
	// triggering event.
	PositionTriggerFilter Position = "triggerFilter"
	// PositionLogicBody is an expression inside a logic body.
	PositionLogicBody Position = "logicBody"
	// PositionMutationValue is a value in a mutation's insert or update block.
	PositionMutationValue Position = "mutationValue"
	// PositionStepArgument is an argument passed to a call from an automation
	// step.
	PositionStepArgument Position = "stepArgument"
	// PositionToolDefault is a tool field's @default value.
	PositionToolDefault Position = "toolDefault"
	// PositionPromptInput is a value bound into a prompt's input.
	PositionPromptInput Position = "promptInput"
)

// Positions lists every position, in the order the record names them.
func Positions() []Position {
	return []Position{
		PositionQueryFilter,
		PositionSort,
		PositionSpecBody,
		PositionRowAuthzArgument,
		PositionAutomationCondition,
		PositionTriggerFilter,
		PositionLogicBody,
		PositionMutationValue,
		PositionStepArgument,
		PositionToolDefault,
		PositionPromptInput,
	}
}
