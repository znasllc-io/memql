package work

import (
	"fmt"
	"strings"
)

const outcomeRecoveryMarker = "work_outcome_recovery_v1:"
const legacyOutcomeFailurePrefix = `function "runAgentTurn" execution failed: runAgentTurn: invalid delegated work outcome:`

// OutcomeRecoveryError identifies a retained protocol failure across mesh error
// wrapping. Retrying it may inspect the saved stage, never reset its generation
// or replay production. It carries no operation permission or completion claim.
func OutcomeRecoveryError(err error) error { return fmt.Errorf("%s %w", outcomeRecoveryMarker, err) }
func IsLegacyOutcomeFailure(message string) bool {
	return strings.HasPrefix(message, legacyOutcomeFailurePrefix)
}
func IsOutcomeRecoveryFailure(message string) bool {
	return strings.Contains(message, outcomeRecoveryMarker) || IsLegacyOutcomeFailure(message)
}
