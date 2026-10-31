package work

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestUnfinishedStepNativeControlSurvivesTransport(t *testing.T) {
	gap := "Save the remaining output after reconciling its existing receipts."
	wrapped := fmt.Errorf("function execution failed: %w", UnfinishedStepError(gap)).Error()
	got, ok := UnfinishedStepGap(wrapped)
	require.True(t, ok)
	require.Equal(t, gap, got)
	symptom, evidence, known := ClassifyByRules(Signal{ErrorMessage: wrapped})
	require.True(t, known)
	require.Equal(t, SymptomPlan, symptom)
	require.Equal(t, "plan.unfinishedStep", evidence.RuleId)
	for _, text := range []string{"I have not saved the file.", "The operation is unfinished.", "work_step_unfinished_v1:broken", "work_step_unfinished_v1:e30"} {
		_, ok := UnfinishedStepGap(text)
		require.False(t, ok)
	}
}
