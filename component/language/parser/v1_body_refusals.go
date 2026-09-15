package parser

// v1_body_refusals.go -- what the edition-2026 statement parser refuses, and
// how (epic memql#5370, task memql#5371; D12, D15 and D24 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// Every refusal carries a stable code, the last thing its message prints, in
// square brackets: the conformance corpus and Sense key on the code, never on
// the wording. A retired form's message names the replacement and the
// migrator that writes it, `memqlmigrate --rewrite=bodies`.

import (
	"fmt"
	"sort"
)

// bodyMigrator is the codemod a retired body form's refusal names.
const bodyMigrator = "memqlmigrate --rewrite=bodies"

// The refusal codes of the statement parser. Named constants, so a
// misspelled code is a compile error rather than a refusal the corpus cannot
// find.
const (
	codeBodyStepRetired              = "body_step_retired"
	codeBodyBlockRetired             = "body_block_retired"
	codeBodyTerseRetired             = "body_terse_retired"
	codeBodyStepsReferenceRetired    = "body_steps_reference_retired"
	codeBodyForEachRetired           = "body_foreach_retired"
	codeBodyForRangeRetired          = "body_for_range_retired"
	codeBodyConditionalAssignRetired = "body_conditional_assign_retired"
	codeBodyPublishEventRetired      = "body_publish_event_retired"
	codeBodyAccessorRetired          = "body_accessor_retired"
	codeBodyCallKindMissing          = "body_call_kind_missing"
	codeBodyPositionalArgument       = "body_positional_argument"
	codeBodyCallInExpression         = "body_call_in_expression"
	codeBodyOneStatementPerLine      = "body_one_statement_per_line"
	codeBodyElsePlacement            = "body_else_placement"
	codeBodyRetryPlacement           = "body_retry_placement"
	codeBodyOnErrorPlacement         = "body_on_error_placement"
	codeBodySurfacePlacement         = "body_surface_placement"
	codeBodyClauseOrder              = "body_clause_order"
	codeBodyDefaultClause            = "body_default_clause"
	codeBodyCaseLabel                = "body_case_label"
	codeBodyWaitValue                = "body_wait_value"
	codeBodyEmpty                    = "body_empty"
	codeBodyMissingExpression        = "body_missing_expression"
	codeTriggerPartitionRetired      = "trigger_partition_retired"
	codeTriggerScheduleRetired       = "trigger_schedule_synonym_retired"
)

// BodyRefusalCodes lists every code the statement parser can refuse with,
// sorted. The refusal tests hold it to their cases in both directions.
func BodyRefusalCodes() []string {
	out := []string{
		codeBodyStepRetired, codeBodyBlockRetired, codeBodyTerseRetired,
		codeBodyStepsReferenceRetired, codeBodyForEachRetired, codeBodyForRangeRetired,
		codeBodyConditionalAssignRetired, codeBodyPublishEventRetired, codeBodyAccessorRetired,
		codeBodyCallKindMissing,
		codeBodyPositionalArgument, codeBodyCallInExpression, codeBodyOneStatementPerLine,
		codeBodyElsePlacement, codeBodyRetryPlacement, codeBodyOnErrorPlacement,
		codeBodySurfacePlacement, codeBodyClauseOrder, codeBodyDefaultClause,
		codeBodyCaseLabel, codeBodyWaitValue, codeBodyEmpty, codeBodyMissingExpression,
		codeTriggerPartitionRetired, codeTriggerScheduleRetired,
	}
	sort.Strings(out)
	return out
}

// BodyStatementForms lists every statement form and trailing clause the
// parser accepts, in the words the conformance corpus uses for its
// directories.
func BodyStatementForms() []string {
	return []string{
		"assign", "call", "if", "else", "for", "switch", "parallel", "publish", "return",
		"retry", "onError", "onSurface", "wait",
	}
}

// BodyStatementKeywords lists the words that open a statement, in the order
// BodyStatementForms lists them. A statement also opens with the name it
// binds (`x := ...`) or with a construct call's kind (BodyCallKinds); `else`
// follows a closing brace on its line, and `case` / `default` open the lines
// of a switch and `branch` those of a parallel.
func BodyStatementKeywords() []string {
	return []string{"if", "for", "switch", "parallel", "publish", "return"}
}

// BodyCallKinds lists the construct kinds a statement calls, in the order a
// refusal names them.
func BodyCallKinds() []string {
	return []string{"query", "mutation", "logic", "builtin", "automation", "action"}
}

// BodyRefusal is a parse-time refusal of a body, with its stable code. It
// unwraps to the positioned *ParseError, so every consumer that reads a parse
// error's position reads this one's.
type BodyRefusal struct {
	Code  string
	Parse *ParseError
}

// Error is the positioned message with the code last, in brackets (D24).
func (e *BodyRefusal) Error() string { return e.Parse.Error() + " [" + e.Code + "]" }

// Unwrap returns the positioned parse error.
func (e *BodyRefusal) Unwrap() error { return e.Parse }

// bodyRefuse refuses at tok with a code.
func bodyRefuse(tok Token, code, format string, args ...any) error {
	return &BodyRefusal{Code: code, Parse: v1ParseErrorAt(tok, fmt.Sprintf(format, args...))}
}

// bodyRetired refuses a retired form: `<spelling> is retired in edition 2026:
// <instead> (memqlmigrate --rewrite=bodies rewrites it)`.
func bodyRetired(tok Token, code, spelling, instead string) error {
	return bodyRefuse(tok, code, "%s is retired in edition 2026: %s (%s rewrites it)", spelling, instead, bodyMigrator)
}
