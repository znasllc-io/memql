package knowledge

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// staged_read_gate_3984_test.go -- epic memql#3974, task memql#3984.
//
// queryChunksForDomain produces the chunks an agent grounds and CITES from, and
// it is the only statement in the tree that scans TWO concepts -- the chunks
// themselves, and the parent documents whose `rejected` status suppresses them
// through a correlated NOT EXISTS. Both halves are ruled on; see the function's
// own comment for why the suppression half was decided rather than defaulted.
//
// No database is wired, so a clean empty answer can only come from the gate: an
// ungated call reaches i.db() and panics on the nil handle.

func quietKnowledge(staged func(string) bool) *Integration {
	i := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if staged != nil {
		i.SetStagedConceptPredicate(staged)
	}
	return i
}

// TestQueryChunksForDomainWithholdsStagedChunks: the primary half. A staged
// documentChunk concept yields no chunks at all, so nothing reaches a prompt.
func TestQueryChunksForDomainWithholdsStagedChunks(t *testing.T) {
	i := quietKnowledge(func(conceptId string) bool {
		return conceptId == conceptKnowledgeDocumentChunk
	})

	chunks, err := i.queryChunksForDomain(context.Background(), "some-domain", "")
	if err != nil {
		t.Fatalf("queryChunksForDomain with staged chunks errored (%v); it must answer empty", err)
	}
	if len(chunks) != 0 {
		t.Errorf("returned %d chunks while the chunk concept was staged", len(chunks))
	}
}

// TestQueryChunksForDomainRefusesWhenTheStagedPredicateIsMissing: unwired is
// refused. This one matters more than most, because the rows in question are
// quoted into an agent's answer with a citation attached.
func TestQueryChunksForDomainRefusesWhenTheStagedPredicateIsMissing(t *testing.T) {
	i := quietKnowledge(nil)

	_, err := i.queryChunksForDomain(context.Background(), "some-domain", "")
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Errorf("queryChunksForDomain with no predicate = %v, want a refusal", err)
	}
}

// TestQueryChunksForDomainProceedsWhenNothingIsStaged is the control: with
// nothing staged the function must get PAST the gate, observable here as the
// nil database handle it then dereferences.
func TestQueryChunksForDomainProceedsWhenNothingIsStaged(t *testing.T) {
	i := quietKnowledge(func(string) bool { return false })
	reached := false
	func() {
		defer func() {
			if recover() != nil {
				reached = true
			}
		}()
		_, _ = i.queryChunksForDomain(context.Background(), "some-domain", "")
	}()
	if !reached {
		t.Error("queryChunksForDomain returned without reaching the database when nothing is staged " +
			"-- a gate that withholds unconditionally is an outage, not a gate")
	}
}
