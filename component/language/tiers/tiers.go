// Package tiers names the positions an expression can be written in, and says
// what each of them admits.
//
// It is the tier manifest of D11 in
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md.
// This file holds the positions. manifest.go holds, per position, the tier it
// evaluates in (P, pushed down to SQL; M, evaluated in process), the node
// kinds and catalog functions it admits, and whether a spec or trait may be
// applied there; the functions themselves are catalogued in
// component/language/functions. The conformance corpus's second completeness
// gate reads Positions() and asks each position for one case that loads and
// one that is refused (test/conformance/2026/expr/).
//
// A position's value is also the name of its corpus directory, so the values
// are part of the corpus layout and do not change once cases are written
// against them.
package tiers

// Position is one place in the language where an expression is written.
type Position string

const (
	PositionBeforeWriteValue Position = "beforeWriteValue"
	// PositionQueryFilter is a query's `filter` clause, and the query a tool's
	// @handler(query=...) runs.
	PositionQueryFilter Position = "queryFilter"
	// PositionSort is a query's `sort` key.
	PositionSort Position = "sort"
	// PositionSpecBody is the `return` expression of a spec or a trait.
	PositionSpecBody Position = "specBody"
	// PositionRowAuthzArgument is an argument of a concept's @rowAuthz.
	PositionRowAuthzArgument Position = "rowAuthzArgument"
	// PositionAutomationCondition is an automation condition: an `if` or
	// `else if` condition, a `for` filter, a `switch` subject, a
	// precondition.
	PositionAutomationCondition Position = "automationCondition"
	// PositionTriggerFilter is an automation's @filter(...) over the
	// triggering event.
	PositionTriggerFilter Position = "triggerFilter"
	// PositionLogicBody is an expression inside a logic body.
	PositionLogicBody Position = "logicBody"
	// PositionMutationValue is a value in a mutation's insert or update block.
	PositionMutationValue Position = "mutationValue"
	// PositionStepArgument is an argument of a call statement in an
	// automation.
	PositionStepArgument Position = "stepArgument"
	// PositionToolDefault is a tool field's @default value.
	PositionToolDefault Position = "toolDefault"
	// PositionPromptInput is a value bound into a prompt's input.
	PositionPromptInput Position = "promptInput"
	// PositionQueryRefine is a query's `refine` clause: the one named construct
	// that evaluates an expression IN PROCESS over a row set, and only over the
	// bounded page `paginate` already read (D11: evaluating in process over a
	// bounded page is a named construct, never an automatic escape). It is the
	// twelfth position, added by the expression-language epic, because a
	// predicate over rows that runs in process belongs to neither the pushdown
	// filter nor any body position.
	PositionQueryRefine Position = "queryRefine"
)

// Positions lists every position, in the order the record names them.
func Positions() []Position {
	return []Position{
		PositionBeforeWriteValue,
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
		PositionQueryRefine,
	}
}
