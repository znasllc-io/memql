package planner

import (
	"context"
	"reflect"
	"sync"
	"testing"
)

// TestBuildTaskWaves_FallsBackToPhasesWithoutDependsOn: with no dependsOn,
// buildTaskWaves must produce exactly the phase+seq grouping (back-compat).
func TestBuildTaskWaves_FallsBackToPhasesWithoutDependsOn(t *testing.T) {
	phaseOrder := []string{"gather", "summarize"}
	tasks := []fanOutTaskRow{
		{ID: "t1", Phase: "gather", Seq: 0, Category: "semantic"},
		{ID: "t2", Phase: "gather", Seq: 1, Category: "semantic"},
		{ID: "t3", Phase: "summarize", Seq: 2, Category: "semantic"},
	}
	got := buildTaskWaves(phaseOrder, tasks)
	want := groupTasksIntoPhases(phaseOrder, tasks)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("no-dependsOn waves should equal phase grouping\n got=%v\nwant=%v", got, want)
	}
	// Sanity: gather (t1,t2) is one concurrent wave, summarize (t3) the next.
	if len(got) != 2 || len(got[0]) != 2 || len(got[1]) != 1 {
		t.Fatalf("expected [[t1 t2] [t3]], got %v", got)
	}
}

// TestGroupTasksByDependsOn_DAGLayers: a diamond DAG (a -> {b,c} -> d) layers
// into [a] [b c] [d]; b and c are independent (same layer, run concurrently).
func TestGroupTasksByDependsOn_DAGLayers(t *testing.T) {
	tasks := []fanOutTaskRow{
		{ID: "ta", LogicalStepId: "a", Seq: 0, Category: "semantic"},
		{ID: "tb", LogicalStepId: "b", DependsOn: []string{"a"}, Seq: 1, Category: "semantic"},
		{ID: "tc", LogicalStepId: "c", DependsOn: []string{"a"}, Seq: 2, Category: "semantic"},
		{ID: "td", LogicalStepId: "d", DependsOn: []string{"b", "c"}, Seq: 3, Category: "semantic"},
	}
	got := buildTaskWaves(nil, tasks)
	want := [][]string{{"ta"}, {"tb", "tc"}, {"td"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diamond DAG layering wrong\n got=%v\nwant=%v", got, want)
	}
}

// TestGroupTasksByDependsOn_DropsToolInvocationAndUnknownEdges: toolInvocation
// rows are excluded; a dependsOn on an unknown step is treated as satisfied.
func TestGroupTasksByDependsOn_DropsToolInvocationAndUnknownEdges(t *testing.T) {
	tasks := []fanOutTaskRow{
		{ID: "ta", LogicalStepId: "a", Seq: 0, Category: "semantic"},
		{ID: "tb", LogicalStepId: "b", DependsOn: []string{"a", "ghost"}, Seq: 1, Category: "semantic"},
		{ID: "tool", LogicalStepId: "x", DependsOn: []string{"a"}, Seq: 2, Category: "toolInvocation"},
	}
	got := groupTasksByDependsOn(tasks)
	want := [][]string{{"ta"}, {"tb"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown-edge/toolInvocation handling wrong\n got=%v\nwant=%v", got, want)
	}
}

// TestGroupTasksByDependsOn_CycleTerminates: a dependency cycle must not
// deadlock -- every task is still emitted exactly once.
func TestGroupTasksByDependsOn_CycleTerminates(t *testing.T) {
	tasks := []fanOutTaskRow{
		{ID: "tx", LogicalStepId: "x", DependsOn: []string{"y"}, Seq: 0, Category: "semantic"},
		{ID: "ty", LogicalStepId: "y", DependsOn: []string{"x"}, Seq: 1, Category: "semantic"},
	}
	got := groupTasksByDependsOn(tasks)
	total := 0
	seen := map[string]bool{}
	for _, layer := range got {
		for _, id := range layer {
			seen[id] = true
			total++
		}
	}
	if total != 2 || !seen["tx"] || !seen["ty"] {
		t.Fatalf("cycle must still emit both tasks exactly once, got %v", got)
	}
}

// TestRunPhasedFanOut_OverDAG: the DAG waves drive real concurrency -- the two
// independent middle tasks dispatch concurrently, and the dependent final task
// only runs after them.
func TestRunPhasedFanOut_OverDAG(t *testing.T) {
	tasks := []fanOutTaskRow{
		{ID: "ta", LogicalStepId: "a", Seq: 0, Category: "semantic"},
		{ID: "tb", LogicalStepId: "b", DependsOn: []string{"a"}, Seq: 1, Category: "semantic"},
		{ID: "tc", LogicalStepId: "c", DependsOn: []string{"a"}, Seq: 2, Category: "semantic"},
		{ID: "td", LogicalStepId: "d", DependsOn: []string{"b", "c"}, Seq: 3, Category: "semantic"},
	}
	waves := buildTaskWaves(nil, tasks)

	var (
		mu    sync.Mutex
		order []string
	)
	dispatch := func(_ context.Context, id string) error {
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
		return nil
	}
	// Gate of 2 so the b/c layer can actually overlap; serialized here only for
	// deterministic assertion of completion, not ordering within the layer.
	res := runPhasedFanOut(context.Background(), waves, NewSemaphoreGate(2), dispatch)
	if !res.AllSucceeded() {
		t.Fatalf("all DAG tasks should succeed; res=%+v", res)
	}
	// 'a' before 'b'/'c'; 'd' last.
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if !(pos["ta"] < pos["tb"] && pos["ta"] < pos["tc"]) {
		t.Errorf("root must run before its dependents: %v", order)
	}
	if !(pos["tb"] < pos["td"] && pos["tc"] < pos["td"]) {
		t.Errorf("final task must run after both its deps: %v", order)
	}
}
