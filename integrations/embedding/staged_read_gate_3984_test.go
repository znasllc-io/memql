package embedding

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// staged_read_gate_3984_test.go -- epic memql#3974, task memql#3984.
//
// `findSimilar` is the DSL-callable retrieval tool: its `concept` comes from
// the caller and its rows go straight into LLM context, which makes it the
// highest-exposure hand-rolled read in the tree. Its statement is also the one
// memql#3984's other half (PR #4037) gives a latest-version collapse; the two
// are independent because this gate is concept-grain, and an id's concept does
// not vary across its versions.

func quietFindSimilar(staged func(string) bool) *Integration {
	i := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if staged != nil {
		i.SetStagedConceptPredicate(staged)
	}
	return i
}

// TestFindSimilarWithholdsStagedConcepts: empty, and empty rather than an
// error, so a staged concept is indistinguishable from one with no rows.
func TestFindSimilarWithholdsStagedConcepts(t *testing.T) {
	i := quietFindSimilar(func(conceptId string) bool { return conceptId == "v1:trained:widget" })

	nodes, err := i.findSimilarHandler(context.Background(), map[string]any{
		"text":    "anything",
		"concept": "v1:trained:widget",
	}, 5)
	if err != nil {
		t.Fatalf("findSimilar over a staged concept errored (%v); it must answer empty", err)
	}
	if len(nodes) != 0 {
		t.Errorf("findSimilar returned %d rows for a staged concept", len(nodes))
	}
}

// TestFindSimilarProceedsForUnstagedConcepts is the control: an unstaged
// concept must get past the gate, observable as the next check failing.
func TestFindSimilarProceedsForUnstagedConcepts(t *testing.T) {
	i := quietFindSimilar(func(string) bool { return false })

	_, err := i.findSimilarHandler(context.Background(), map[string]any{
		"text":    "anything",
		"concept": "v1:knowledge:documentChunk",
	}, 5)
	if err == nil || !strings.Contains(err.Error(), "database not configured") {
		t.Errorf("findSimilar over an unstaged concept = %v, want it to reach the database check", err)
	}
}

// TestFindSimilarRefusesWhenTheStagedPredicateIsMissing: unwired is refused,
// never read as "nothing is staged".
func TestFindSimilarRefusesWhenTheStagedPredicateIsMissing(t *testing.T) {
	i := quietFindSimilar(nil)

	_, err := i.findSimilarHandler(context.Background(), map[string]any{
		"text":    "anything",
		"concept": "v1:trained:widget",
	}, 5)
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Errorf("findSimilar with no predicate = %v, want a refusal", err)
	}
}
