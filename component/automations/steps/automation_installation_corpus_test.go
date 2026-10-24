package steps

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise the actual installed recipes through the corpus interpreter. These
// controls verify fixture shapes and DSL branches, not the native ports' actor,
// journal or evidence gates (which have separate integration tests).
func TestAutomationInstallationProbeContracts(t *testing.T) {
	sources := logicCorpusSources(t)
	arm := newAutomationArm(t, "installation-probe", func(f *corpusSource) string { return f.Bodies }, sources)
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	run := func(name string, answers map[string]any) logicRecord {
		a := arm.byName[name]
		require.NotNil(t, a)
		return arm.runWithBuiltinAnswers(automationFixture{Name: name, Event: automationInvocation(a), Rows: 0}, now, answers)
	}
	names := func(record logicRecord) []string {
		out := make([]string, 0, len(record.Calls))
		for _, call := range record.Calls {
			out = append(out, call.Name)
		}
		return out
	}
	selected := func(record logicRecord, name string) []probeCall {
		var out []probeCall
		for _, call := range record.Calls {
			if call.Name == name {
				out = append(out, call)
			}
		}
		return out
	}

	t.Run("preparation reaches promotion with no dependencies", func(t *testing.T) {
		record := run("installationPrepareWorkflow", nil)
		require.False(t, record.Refused)
		require.Equal(t, []string{
			"installationResolvePublications", "installationCheckCompatibility", "installationReservePreparation", "installationApplications",
			"installationCaptureSource", "installationSourceRequirements", "installationVerifySourceGraph", "installationAcknowledgeSource", "installationRenderSource",
			"installationCaptureSource", "installationSourceRequirements", "installationVerifySourceGraph", "installationAcknowledgeSource", "installationRenderSource",
			"installationVerifyResources", "installationArtifactWorkflow", "installationRecheckResources", "installationVerifyStorage", "installationVerifyProtected", "installationContinuityBaseline", "installationReobserveConfiguration", "installationPromotePreparation",
		}, names(record))
		for n, call := range selected(record, "installationCaptureSource") {
			require.Equal(t, map[string]any{"application": "", "role": []string{"rollback", "candidate"}[n]}, call.Args)
		}
	})
	t.Run("dependencies retain application role and native key", func(t *testing.T) {
		record := run("installationPrepareWorkflow", map[string]any{
			"installationApplications":       []any{map[string]any{"key": "engine"}},
			"installationSourceRequirements": []any{map[string]any{"key": "dependency-1"}, map[string]any{"key": "dependency-2"}},
		})
		require.False(t, record.Refused)
		var actual []probeCall
		for _, call := range record.Calls {
			if call.Name == "installationCaptureDependency" || call.Name == "installationVerifyDependency" || call.Name == "installationAcknowledgeDependency" {
				actual = append(actual, call)
			}
		}
		var expected []probeCall
		for _, role := range []string{"rollback", "candidate"} {
			for _, key := range []string{"dependency-1", "dependency-2"} {
				for _, operation := range []string{"installationCaptureDependency", "installationVerifyDependency", "installationAcknowledgeDependency"} {
					expected = append(expected, probeCall{Kind: "builtin", Name: operation, Args: map[string]any{"application": "engine", "role": role, "key": key}})
				}
			}
		}
		require.Equal(t, expected, actual)
		require.Equal(t, "installationPromotePreparation", record.Calls[len(record.Calls)-1].Name)
	})
	t.Run("observer cannot apply even when write is needed", func(t *testing.T) {
		record := run("installationRevisionWorkflow", map[string]any{"installationRevisionRead": map[string]any{"mayApply": false, "needsWrite": true}})
		require.False(t, record.Refused)
		require.Equal(t, []string{"installationApplications", "installationRevisionRead", "installationRevisionObserve"}, names(record))
		require.Equal(t, map[string]any{"application": ""}, record.Calls[2].Args)
	})
	t.Run("admitted revision verifies before begin and apply", func(t *testing.T) {
		record := run("installationRevisionWorkflow", map[string]any{"installationRevisionRead": map[string]any{"mayApply": true, "needsWrite": true}})
		require.False(t, record.Refused)
		require.Equal(t, []string{
			"installationApplications", "installationRevisionRead", "installationRevisionConfiguration", "installationRevisionResolve", "installationRevisionCompatibility",
			"installationRevisionReopenSource", "installationRevisionRenderSource", "installationRevisionReopenSource", "installationRevisionRenderSource",
			"installationRevisionVerifyResources", "installationArtifactWorkflow", "installationRevisionRecheckResources", "installationRevisionVerifyStorage", "installationRevisionVerifyProtected", "installationRevisionReobserveConfig", "installationRevisionBegin", "installationRevisionApply", "installationRevisionObserve",
		}, names(record))
	})
	t.Run("rollback observation does not write", func(t *testing.T) {
		record := run("installationRollbackWorkflow", nil)
		require.False(t, record.Refused)
		require.Equal(t, []string{"installationApplications", "installationRollbackRead", "installationRollbackObserve"}, names(record))
	})
	for _, ready := range []bool{false, true} {
		label := "retirement remains pending"
		if ready {
			label = "retirement completes only after ready observation"
		}
		t.Run(label, func(t *testing.T) {
			record := run("installationRetirementWorkflow", map[string]any{
				"installationRetirementPage":    map[string]any{"items": []any{map[string]any{"intentId": "artifact-1"}}, "complete": ready},
				"installationRetirementObserve": map[string]any{"ready": ready},
			})
			require.False(t, record.Refused)
			expected := []string{"installationRetirementBegin", "installationRetirementStop", "installationRetirementRequirements"}
			for range 2 {
				expected = append(expected, "installationRetirementFence", "installationRetirementRelease", "installationRetirementPage", "installationRetirementArtifact", "installationRetirementPage")
			}
			expected = append(expected, "installationRetirementObserve")
			if ready {
				expected = append(expected, "installationRetirementComplete")
			}
			require.Equal(t, expected, names(record))
			for n, call := range selected(record, "installationRetirementArtifact") {
				require.Equal(t, map[string]any{"role": []string{"candidate", "rollback"}[n], "intentId": "artifact-1"}, call.Args)
			}
		})
	}
	for _, test := range []struct{ automation, operation string }{
		{"installationPrepareWorkflow", "installationApplications"},
		{"installationPrepareWorkflow", "installationSourceRequirements"},
		{"installationRetirementWorkflow", "installationRetirementRequirements"},
	} {
		t.Run("malformed "+test.operation+" refuses", func(t *testing.T) {
			record := run(test.automation, map[string]any{test.operation: map[string]any{"builtin": test.operation}})
			require.True(t, record.Refused)
			require.Equal(t, test.operation, record.Calls[len(record.Calls)-1].Name)
			for _, name := range []string{"installationPromotePreparation", "installationRetirementArtifact", "installationRetirementComplete", "installationRevisionApply"} {
				require.Empty(t, selected(record, name))
			}
		})
	}
}
