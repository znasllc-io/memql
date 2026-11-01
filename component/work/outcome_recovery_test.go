package work

import (
	"errors"
	"fmt"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOutcomeRecoverySignalStopsOperationalRepair(t *testing.T) {
	for _, message := range []string{legacyOutcomeFailurePrefix + " missing marker", fmt.Errorf("function failed: %w", OutcomeRecoveryError(errors.New("format failed"))).Error()} {
		symptom, evidence, known := ClassifyByRules(Signal{ErrorMessage: message})
		require.True(t, known)
		require.Equal(t, SymptomHuman, symptom)
		require.Equal(t, "human.outcomeRecovery", evidence.RuleId)
	}
	for _, message := range []string{"the result has the wrong format", "vendor said: invalid delegated work outcome", "free text does not complete this task"} {
		require.False(t, IsOutcomeRecoveryFailure(message))
	}
	_, evidence, _ := ClassifyByRules(Signal{ErrorCode: "budget_exhausted", ErrorMessage: OutcomeRecoveryError(errors.New("refused")).Error()})
	require.Equal(t, RuleIdBudgetExhausted, evidence.RuleId)
}
