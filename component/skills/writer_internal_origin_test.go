package skills

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// writer_internal_origin_test.go -- the per-call guard behind this package's
// entry on the ContextWithInternalOrigin allowlist.
//
// ===========================================================================
// WHY THE TREE-WIDE GATE IS NOT ENOUGH, IN ITS OWN WORDS
// ===========================================================================
// `TestEveryGoCallerOfAServerOnlyConstructStampsInternalOrigin`
// (test/dslconformance) is FILE granularity, and its "what this does NOT
// catch" section says so:
//
//	Whether the stamp is on the RIGHT call. A file stamping for one query
//	and not another satisfies this.
//
// `writer.go` issues three `@serverOnly` calls from one file, so removing the
// stamp from ONE of them leaves the file still stamping and the gate still
// green. Measured, not assumed: deleting it from `SetScripts` and running that
// gate passes.
//
// The gate names the remedy in the same breath -- a per-store test -- and this
// is that test for this package, the sibling of
// component/workjournal/internal_origin_precondition_test.go.
//
// WHAT IT WOULD COST TO BE WRONG. Origin defaults to CLIENT, and the function
// validator refuses a `@serverOnly` write with a WARN and nothing else. So an
// unstamped writer here does not fail: a captured script is never recorded on
// its skill, and a skill edge a successful run committed silently is not
// committed at all -- selection then keeps answering from a graph that is
// missing every edge the platform has learned, and nothing anywhere says so.

// recordingExecutor answers every call and reports the origin and actor it was
// asked under.
type recordingExecutor struct {
	calls   []string
	origins []bool
}

func (e *recordingExecutor) Execute(ctx context.Context, query string) (any, error) {
	e.calls = append(e.calls, query)
	e.origins = append(e.origins, auth.OriginFromContext(ctx).IsInternal())
	return nil, nil
}

// EVERY write, exercised. A method added to Writer and not driven here is the
// hole this file exists to close, so the count is asserted too -- a test that
// silently stopped covering a method would look identical to one that passed.
func TestEveryWriteStampsInternalOrigin(t *testing.T) {
	engine := &recordingExecutor{}
	w := NewWriter(engine)

	if err := w.SetScripts(context.Background(), "skill-1", []byte(`[{"platform":"any","artifactId":"a-1"}]`)); err != nil {
		t.Fatalf("SetScripts: %v", err)
	}
	edges := []EdgeWrite{{
		EdgeID: "e-1", From: "skill-1", To: "skill-2", Type: EdgeDependsOn,
		Evidence: []Evidence{{RunID: "run-1", StepKey: "s"}},
	}}
	if err := w.Propose(context.Background(), edges); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if err := w.Commit(context.Background(), edges); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// SetScripts, Propose's createSkillEdge, Commit's createSkillEdge and its
	// commitSkillEdge.
	if len(engine.calls) != 4 {
		t.Fatalf("calls = %d (%v), want 4 -- a write added to Writer and not driven here is the hole this test exists to close",
			len(engine.calls), engine.calls)
	}
	for i, internal := range engine.origins {
		if !internal {
			t.Fatalf("call %d did not carry internal origin: %s", i, engine.calls[i])
		}
	}
}

// The three constructs by name, so a rename that silently stopped writing one
// of them is visible here rather than at runtime.
func TestTheWritesAreTheThreeServerOnlyConstructs(t *testing.T) {
	engine := &recordingExecutor{}
	w := NewWriter(engine)
	_ = w.SetScripts(context.Background(), "skill-1", []byte("[]"))
	_ = w.Commit(context.Background(), []EdgeWrite{{
		EdgeID: "e-1", From: "a", To: "b", Type: EdgeDependsOn,
	}})

	joined := strings.Join(engine.calls, "\n")
	for _, want := range []string{"setSkillScripts(", "createSkillEdge(", "commitSkillEdge("} {
		if !strings.Contains(joined, want) {
			t.Fatalf("no call to %s; calls were:\n%s", want, joined)
		}
	}
}

// A nil writer is the shape of no engine at all, and it REFUSES rather than
// silently succeeding -- a capture that reported success and recorded nothing
// would leave a skill pointing at a path on somebody's machine forever.
func TestANilWriterRefusesRatherThanSucceeding(t *testing.T) {
	var w *Writer
	if err := w.SetScripts(context.Background(), "skill-1", []byte("[]")); err == nil {
		t.Fatal("SetScripts on a nil writer reported success")
	}
	if err := w.Commit(context.Background(), []EdgeWrite{{EdgeID: "e", From: "a", To: "b"}}); err == nil {
		t.Fatal("Commit on a nil writer reported success")
	}
	if NewWriter(nil) != nil {
		t.Fatal("NewWriter returned a writer with no engine to write through")
	}
}

// An edge that names the same skill at both ends, or names nothing, is SKIPPED
// rather than written -- it is not a relation, and the row would be one
// selection could never act on.
func TestADegenerateEdgeIsNotWritten(t *testing.T) {
	engine := &recordingExecutor{}
	w := NewWriter(engine)
	if err := w.Commit(context.Background(), []EdgeWrite{
		{EdgeID: "e-1", From: "same", To: "same", Type: EdgeDependsOn},
		{EdgeID: "e-2", From: "", To: "b", Type: EdgeDependsOn},
		{EdgeID: "", From: "a", To: "b", Type: EdgeDependsOn},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(engine.calls) != 0 {
		t.Fatalf("calls = %v, want none", engine.calls)
	}
}
