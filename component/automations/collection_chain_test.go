package automations

import (
	"context"
	"fmt"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// collection_chain_test.go -- collection chains over statement values and
// args (#2317 / #2318), evaluated over the run the way a statement, a return,
// a `for` source and a `for` filter are.

// runChainLogic compiles a statement-form logic and runs it on the sequence
// runner, each construct call answered from answers.
func runChainLogic(t *testing.T, src string, answers map[string]any) any {
	t.Helper()
	r, probe, _ := newLogicRig(nil)
	for callee, answer := range answers {
		probe.answers[callee] = answer
	}
	name, body := compiledLogic(t, src)
	out, err := r.RunLogicBody(context.Background(), name, body, nil)
	if err != nil {
		t.Fatalf("run %s: %v", name, err)
	}
	return out
}

// TestLogicCollectionChain_IntermediateStepThenReturn pins gap 2 (#2317): a
// multi-statement logic body whose intermediate `:=` statement is a
// collection chain over a prior statement's rows, followed by a `return
// active.count()` over that bound collection.
func TestLogicCollectionChain_IntermediateStepThenReturn(t *testing.T) {
	got := runChainLogic(t, `logic countActive {
  rows := query someActiveQuery()
  active := rows.where(r => r.status == "active")
  return active.count()
}`, map[string]any{"someActiveQuery": rowsResult(
		map[string]any{"id": "a", "payload": map[string]any{"status": "active"}},
		map[string]any{"id": "b", "payload": map[string]any{"status": "inactive"}},
		map[string]any{"id": "c", "payload": map[string]any{"status": "active"}},
	)})
	if fmt.Sprint(got) != "2" {
		t.Errorf("active.count() = %#v, want 2", got)
	}
}

// TestLogicCollectionChain_FullChainReturn pins gap 2 (#2317): a `return`
// expression that is a full collection chain (`rows.where(...).count()`).
func TestLogicCollectionChain_FullChainReturn(t *testing.T) {
	got := runChainLogic(t, `logic countAdmins {
  rows := query members()
  return rows.where(r => r.role == "admin").count()
}`, map[string]any{"members": rowsResult(
		map[string]any{"id": "a", "payload": map[string]any{"role": "admin"}},
		map[string]any{"id": "b", "payload": map[string]any{"role": "member"}},
		map[string]any{"id": "c", "payload": map[string]any{"role": "admin"}},
	)})
	if fmt.Sprint(got) != "2" {
		t.Errorf("admin count = %#v, want 2", got)
	}
}

// TestLogicCollectionChain_CountOverBundle: a query whose answer is the
// engine's bundle -- the `return X.count()` / `.empty()` / `.first()` family
// -- reads the bundle's rows.
func TestLogicCollectionChain_CountOverBundle(t *testing.T) {
	node := func(id string) *memqlv1.MemoryNode {
		return &memqlv1.MemoryNode{Id: id, Concept: "v1:identity:delegation", Payload: &structpb.Struct{}}
	}
	got := runChainLogic(t, `logic countExpired {
  expiredDelegations := query expired()
  return expiredDelegations.count()
}`, map[string]any{"expired": &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{node("d1"), node("d2"), node("d3")},
	}}})
	if fmt.Sprint(got) != "3" {
		t.Errorf("expiredDelegations.count() = %#v, want 3", got)
	}
}

// TestCollectionChain_SourceOuterArgSubstitution pins gap 3b (#2318): a
// collection chain whose lambda body references an outer `args.X` (not the
// bound element) resolves it against the run's args -- the `for`-source
// position.
func TestCollectionChain_SourceOuterArgSubstitution(t *testing.T) {
	members := []any{
		map[string]any{"name": "alice", "score": 5.0},
		map[string]any{"name": "bob", "score": 1.0},
		map[string]any{"name": "carol", "score": 9.0},
	}
	for threshold, want := range map[float64]int{3.0: 2, 6.0: 1} {
		e := NewEvaluator()
		e.SetCustom("args", map[string]any{"members": members, "threshold": threshold})
		val, err := evalV1(e, `args.members.where(m => m.score > args.threshold)`)
		if err != nil {
			t.Fatalf("outer-arg chain (threshold %v): %v", threshold, err)
		}
		items, ok := val.([]any)
		if !ok || len(items) != want {
			t.Errorf("members over threshold %v = %#v, want %d", threshold, val, want)
		}
	}
}

// TestForEachFilter_ChainAndComparison pins gap 3a (#2318): a `for` filter
// that is a collection chain over the loop's row, and one that is a plain
// comparison, both decide over the row.
func TestForEachFilter_ChainAndComparison(t *testing.T) {
	e := NewEvaluator()
	e.enterStatements()
	loop := e.ChildFrame()
	loop.Bind("row", map[string]any{
		"id":      "x",
		"payload": map[string]any{"tags": []any{"vip", "beta"}},
	})

	for cond, want := range map[string]bool{
		`row.tags.any(t => t == "vip")`:  true,
		`row.tags.any(t => t == "gold")`: false,
		`row.id == "x"`:                  true,
	} {
		got, err := evalV1Cond(t, loop, cond)
		if err != nil {
			t.Fatalf("%s: %v", cond, err)
		}
		if got != want {
			t.Errorf("%s = %v, want %v", cond, got, want)
		}
	}
}
