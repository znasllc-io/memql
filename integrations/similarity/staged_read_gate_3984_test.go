package similarity

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// staged_read_gate_3984_test.go -- epic memql#3974, task memql#3984.
//
// `similarTo()` is the general RAG surface and the ONE hand-rolled read whose
// target concept is a caller ARGUMENT rather than a fixed engine concept. Every
// other gated site in the tree is pinned to something like
// v1:cognition:utterance and can only be reached by a staged concept if that
// concept ever becomes author-promotable; this one is reachable by a newly
// promoted, still-staged, vector-indexed concept on the first call, and what it
// returns goes straight into a prompt.
//
// No database and no embedding provider are wired, so the gate is the only
// thing that can produce a clean empty answer: an ungated handler reaches the
// "database not configured" error immediately afterwards.

func quietIntegration(staged func(string) bool) *Integration {
	i := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	i.SetStagedConceptPredicate(staged)
	return i
}

// TestSimilarToWithholdsStagedConcepts: a staged concept answers EMPTY, and
// empty rather than an error.
//
// The empty/error distinction is the assertion that matters. A staged concept
// has to be indistinguishable from one nothing has been written to yet; an
// error saying "that concept is staged" discloses that it exists and is being
// withheld, which is a smaller leak than the rows and still a leak.
func TestSimilarToWithholdsStagedConcepts(t *testing.T) {
	i := quietIntegration(func(conceptId string) bool { return conceptId == "v1:trained:widget" })

	nodes, err := i.similarToHandler(context.Background(), map[string]any{
		"text":    "anything",
		"concept": "v1:trained:widget",
	}, 5)
	if err != nil {
		t.Fatalf("similarTo over a staged concept errored (%v); it must answer empty, exactly as a "+
			"concept with no rows does", err)
	}
	if len(nodes) != 0 {
		t.Errorf("similarTo returned %d rows for a staged concept", len(nodes))
	}
}

// TestSimilarToProceedsForUnstagedConcepts is the control. A handler that
// returned empty for everything would pass the test above; this asserts an
// unstaged concept gets PAST the gate, observable as the next check in the
// function failing.
func TestSimilarToProceedsForUnstagedConcepts(t *testing.T) {
	i := quietIntegration(func(string) bool { return false })

	_, err := i.similarToHandler(context.Background(), map[string]any{
		"text":    "anything",
		"concept": "v1:knowledge:documentChunk",
	}, 5)
	if err == nil || !strings.Contains(err.Error(), "database not configured") {
		t.Errorf("similarTo over an unstaged concept = %v, want it to reach the database check -- a "+
			"gate that withholds unconditionally is an outage, not a gate", err)
	}
}

// TestSimilarToRefusesWhenTheStagedPredicateIsMissing: an unwired predicate is
// refused, never resolved to "nothing is staged".
func TestSimilarToRefusesWhenTheStagedPredicateIsMissing(t *testing.T) {
	i := New(slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := i.similarToHandler(context.Background(), map[string]any{
		"text":    "anything",
		"concept": "v1:trained:widget",
	}, 5)
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Errorf("similarTo with no predicate = %v, want a refusal naming the unwired predicate", err)
	}
}
