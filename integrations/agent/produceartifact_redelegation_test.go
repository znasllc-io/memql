package agent

import "testing"

// TestIsProduceArtifactExecutionTurn covers the lineage signal that gates the
// re-delegation guard (memql#1133): a turn is the produceArtifact EXECUTOR turn
// iff it carries hints[deliverable_surface]=workbench (the same marker that
// scopes canvasPublish out per memql#950).
func TestIsProduceArtifactExecutionTurn(t *testing.T) {
	cases := []struct {
		name  string
		hints map[string]string
		want  bool
	}{
		{"workbench surface -> executor", map[string]string{"deliverable_surface": "workbench"}, true},
		{"case-insensitive value", map[string]string{"deliverable_surface": "Workbench"}, true},
		{"other surface", map[string]string{"deliverable_surface": "canvas"}, false},
		{"absent hint", map[string]string{"execution_lane": "background"}, false},
		{"nil hints", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsProduceArtifactExecutionTurn(tc.hints); got != tc.want {
				t.Fatalf("IsProduceArtifactExecutionTurn(%v) = %v, want %v", tc.hints, got, tc.want)
			}
		})
	}
}

// TestGuardProduceArtifactRedelegation_RefusesWithinExecutorTurn is the core
// FIX-2 guard: a produceArtifact tool call from within a produceArtifact
// executor turn (the acting plan is already kind=produceArtifact) is REFUSED
// with a clear, model-facing error -- no new plan is minted. This caps
// re-delegation at depth 1 and stops the plan-level runaway the per-turn
// breaker (memql#1128) can't see.
func TestGuardProduceArtifactRedelegation_RefusesWithinExecutorTurn(t *testing.T) {
	tc := turnContext{IsProduceArtifactExecution: true}
	refuse := guardProduceArtifactRedelegation("produceArtifact", tc)
	if refuse == "" {
		t.Fatal("expected produceArtifact re-delegation to be refused inside a produceArtifact executor turn")
	}
	if refuse != produceArtifactRedelegationError {
		t.Fatalf("refusal message = %q, want the canonical produceArtifactRedelegationError", refuse)
	}
}

// TestGuardProduceArtifactRedelegation_AllowsFromNormalTurn asserts the guard is
// a no-op on a NORMAL (non-produceArtifact-executor) turn: the Assistant's
// first produceArtifact delegation must still go through (and mint exactly one
// plan). The guard returns empty string => "dispatch normally".
func TestGuardProduceArtifactRedelegation_AllowsFromNormalTurn(t *testing.T) {
	// A plain chat turn -- no deliverable_surface hint.
	if refuse := guardProduceArtifactRedelegation("produceArtifact", turnContext{}); refuse != "" {
		t.Fatalf("produceArtifact from a normal turn must NOT be refused, got %q", refuse)
	}
}

// TestGuardProduceArtifactRedelegation_OtherToolsUnaffected asserts the guard
// only targets the produceArtifact delegation tool: a produceArtifact executor
// turn must still be allowed to call workbenchHost (that IS how it produces the
// deliverable) and every other tool.
func TestGuardProduceArtifactRedelegation_OtherToolsUnaffected(t *testing.T) {
	tc := turnContext{IsProduceArtifactExecution: true}
	for _, tool := range []string{"workbenchHost", "respondToUser", "canvasPublish", "workerHost"} {
		if refuse := guardProduceArtifactRedelegation(tool, tc); refuse != "" {
			t.Fatalf("tool %q must NOT be refused on a produceArtifact executor turn, got %q", tool, refuse)
		}
	}
}
