package planner

import (
	"encoding/json"
	"strings"
	"testing"
)

// makeFatSpecialist builds a specialist row shaped like
// agentMinimalForDedupe, including the heavy roleEmbedding vector + a
// lineage blob -- the payload that bloated the plannerAgent prompt.
func makeFatSpecialist(id string, embeddingDims int) map[string]any {
	emb := make([]any, embeddingDims)
	for i := range emb {
		emb[i] = 0.123456789 // realistic float width
	}
	return map[string]any{
		"id":           id,
		"name":         "Birdwatcher Bot",
		"role":         "Research Specialist",
		"roleSlug":     "research",
		"kind":         "specialist",
		"description":  strings.Repeat("Knows a great deal about ornithology and field guides. ", 20),
		"capabilities": map[string]any{"skillIds": []any{"research-baseline"}, "toolSlugs": []any{"workbenchHost"}},
		"lineage":      map[string]any{"originatingPlanId": "v1:planner:plan:x", "createdBy": "planner", "history": strings.Repeat("y", 500)},
		"roleEmbedding": emb,
	}
}

func jsonLen(v any) int {
	b, _ := json.Marshal(v)
	return len(b)
}

func TestCompactSpecialists_DropsEmbeddingAndShrinks(t *testing.T) {
	fat := []map[string]any{
		makeFatSpecialist("v1:agents:agent:a1", 1536),
		makeFatSpecialist("v1:agents:agent:a2", 1536),
	}
	lean := compactSpecialistsForPrompt(fat)

	if len(lean) != len(fat) {
		t.Fatalf("compaction must preserve specialist count")
	}
	for _, s := range lean {
		if _, ok := s["roleEmbedding"]; ok {
			t.Fatalf("roleEmbedding must be dropped from the prompt projection")
		}
		if _, ok := s["lineage"]; ok {
			t.Fatalf("lineage must be dropped from the prompt projection")
		}
		// The planner still needs these to pick/dedupe:
		for _, k := range []string{"id", "name", "roleSlug", "capabilities"} {
			if _, ok := s[k]; !ok {
				t.Fatalf("compact specialist must keep %q", k)
			}
		}
	}

	fatLen := jsonLen(fat)
	leanLen := jsonLen(lean)
	reduction := 100.0 * float64(fatLen-leanLen) / float64(fatLen)
	t.Logf("specialists payload: %d -> %d bytes (%.1f%% reduction)", fatLen, leanLen, reduction)
	if reduction < 80.0 {
		t.Fatalf("expected >=80%% reduction from dropping embeddings, got %.1f%%", reduction)
	}
}

func TestCompactPlan_DropsBlobsKeepsDecisionFields(t *testing.T) {
	plan := map[string]any{
		"id":             "v1:planner:plan:p1",
		"kind":           "produceArtifact",
		"status":         "planning",
		"goal":           "Create a list of the top 10 most beautiful birds",
		"retryThreshold": float64(3),
		"phases":         []any{map[string]any{"kind": "produce", "label": "Produce"}},
		"tokenBudget":    float64(2000000),
		"tokenSpent":     float64(1234),
		"input":          map[string]any{"blob": strings.Repeat("z", 5000)},
		"output":         map[string]any{"blob": strings.Repeat("w", 5000)},
		"metrics":        map[string]any{"llmCallCount": float64(2), "noise": strings.Repeat("m", 2000)},
	}
	c := compactPlanForPrompt(plan)
	for _, k := range []string{"input", "output", "metrics"} {
		if _, ok := c[k]; ok {
			t.Fatalf("compact plan must drop the %q blob", k)
		}
	}
	for _, k := range []string{"goal", "phases", "status", "retryThreshold", "tokenBudget", "tokenSpent"} {
		if _, ok := c[k]; !ok {
			t.Fatalf("compact plan must keep decision field %q", k)
		}
	}
}

func TestCompactTasks_DropsIOKeepsError(t *testing.T) {
	tasks := []map[string]any{{
		"id":            "v1:planner:task:t1",
		"kind":          "produce",
		"status":        "failed",
		"logicalStepId": "step-1",
		"phase":         "produce",
		"seq":           float64(0),
		"input":         map[string]any{"blob": strings.Repeat("z", 4000)},
		"output":        map[string]any{"blob": strings.Repeat("w", 4000)},
		"errorMessage":  "the workbench write failed",
	}}
	c := compactTasksForPrompt(tasks)
	if _, ok := c[0]["input"]; ok {
		t.Fatalf("compact task must drop input blob")
	}
	if _, ok := c[0]["output"]; ok {
		t.Fatalf("compact task must drop output blob")
	}
	if c[0]["errorMessage"] != "the workbench write failed" {
		t.Fatalf("compact task must keep errorMessage")
	}
}

func TestTruncate_Bounds(t *testing.T) {
	long := strings.Repeat("a", 5000)
	got := truncate(long, maxGoalChars)
	if len(got) > maxGoalChars+3 { // +3 for the "..." suffix
		t.Fatalf("truncate must bound length, got %d", len(got))
	}
}
