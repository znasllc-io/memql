package harnessrecall

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// staged_read_gate_3984_test.go -- epic memql#3974, task memql#3984.
//
// `recall()` is the second surface whose source concept comes from the DSL call
// rather than being pinned, so an authored, still-staged concept reaches it
// without any further change to the code. Its rows become an agent's working
// memory.
//
// The gate is a Go pre-check rather than a conjunct in recallSQL because that
// statement pins one concept inside its `latest` CTE: deciding before the
// round-trip empties the CTE exactly where an in-CTE conjunct would, without
// having to add `concept` to a projection that does not carry it.

func quietRecall(staged func(string) bool) *Integration {
	i := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if staged != nil {
		i.SetStagedConceptPredicate(staged)
	}
	return i
}

func TestRecallWithholdsStagedConcepts(t *testing.T) {
	i := quietRecall(func(conceptId string) bool { return conceptId == "v1:trained:widget" })

	nodes, err := i.recallHandler(context.Background(), map[string]any{
		"text":    "anything",
		"concept": "v1:trained:widget",
	}, 5)
	if err != nil {
		t.Fatalf("recall over a staged concept errored (%v); it must answer empty, as a concept with "+
			"no memories does", err)
	}
	if len(nodes) != 0 {
		t.Errorf("recall returned %d rows for a staged concept", len(nodes))
	}
}

// The control: an unstaged concept must get past the gate.
func TestRecallProceedsForUnstagedConcepts(t *testing.T) {
	i := quietRecall(func(string) bool { return false })

	_, err := i.recallHandler(context.Background(), map[string]any{
		"text":    "anything",
		"concept": "v1:harness:observation",
	}, 5)
	if err == nil || !strings.Contains(err.Error(), "database not configured") {
		t.Errorf("recall over an unstaged concept = %v, want it to reach the database check", err)
	}
}

func TestRecallRefusesWhenTheStagedPredicateIsMissing(t *testing.T) {
	i := quietRecall(nil)

	_, err := i.recallHandler(context.Background(), map[string]any{
		"text":    "anything",
		"concept": "v1:trained:widget",
	}, 5)
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Errorf("recall with no predicate = %v, want a refusal", err)
	}
}
