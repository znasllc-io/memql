package actionsearch

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// staged_read_gate_3984_test.go -- epic memql#3974, task memql#3984.
//
// searchActions feeds the planner's REUSE/ADAPT/SYNTHESIZE library decision, so
// a staged action is one the planner could select and dispatch.
//
// The gate is a Go pre-check rather than a conjunct in searchSQL, and this site
// is the clearest illustration of why: that statement's `latest` CTE projects
// four expressions and `concept` is not one of them, so an outer conjunct would
// not compile -- and would be wrong even if it did, because a term outside the
// collapse drops the id rather than falling through.

func quietSearch(staged func(string) bool) *Integration {
	i := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if staged != nil {
		i.SetStagedConceptPredicate(staged)
	}
	return i
}

func TestSearchActionsWithholdsStagedActions(t *testing.T) {
	i := quietSearch(func(conceptId string) bool {
		return conceptId == memorynodes.ConceptActionsAction
	})

	nodes, err := i.searchHandler(context.Background(), map[string]any{"text": "anything"}, 5)
	if err != nil {
		t.Fatalf("searchActions with the action concept staged errored (%v); it must answer empty", err)
	}
	if len(nodes) != 0 {
		t.Errorf("searchActions returned %d rows while the action concept was staged", len(nodes))
	}
}

// The control: nothing staged means the read must get past the gate.
func TestSearchActionsProceedsWhenNothingIsStaged(t *testing.T) {
	i := quietSearch(func(string) bool { return false })

	_, err := i.searchHandler(context.Background(), map[string]any{"text": "anything"}, 5)
	if err == nil || !strings.Contains(err.Error(), "database not configured") {
		t.Errorf("searchActions with nothing staged = %v, want it to reach the database check", err)
	}
}

func TestSearchActionsRefusesWhenTheStagedPredicateIsMissing(t *testing.T) {
	i := quietSearch(nil)

	_, err := i.searchHandler(context.Background(), map[string]any{"text": "anything"}, 5)
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Errorf("searchActions with no predicate = %v, want a refusal", err)
	}
}
