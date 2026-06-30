package taskstamp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeExec records every Execute query and returns a canned tool result.
type fakeExec struct {
	mu      sync.Mutex
	queries []string
	toolErr error
}

func (f *fakeExec) ExecuteToolByName(_ context.Context, name string, _ map[string]any) (string, error) {
	return "ran " + name, f.toolErr
}

func (f *fakeExec) Execute(_ context.Context, query string) (any, error) {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.mu.Unlock()
	return nil, nil
}

func (f *fakeExec) count(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, q := range f.queries {
		if strings.Contains(q, sub) {
			n++
		}
	}
	return n
}

func (f *fakeExec) has(subs ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.queries {
		all := true
		for _, s := range subs {
			if !strings.Contains(q, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// TestStamper_AdHoc_FinalizesWrapper: an ad-hoc chat tool call (empty
// PlanContext) materializes a synthetic adHocAction Plan + callTool task AND
// drives BOTH to succeeded after the call -- no more stuck "running" (#1186).
func TestStamper_AdHoc_FinalizesWrapper(t *testing.T) {
	fe := &fakeExec{}
	s := New(fe, nil)

	ctx := WithPlanContext(context.Background(), PlanContext{
		AgentId: "v1:agents:agent:a", OwnerUserId: "u", PartitionId: "v1:cognition:space:s",
	})
	// A normal (non-self-planning) ad-hoc tool: it IS wrapped + finalized.
	out, err := s.ExecuteToolByName(ctx, "webSearch", map[string]any{"x": 1})
	if err != nil || out != "ran webSearch" {
		t.Fatalf("tool passthrough broken: out=%q err=%v", out, err)
	}

	// The synthetic wrapper was created...
	if !fe.has("createAdHocPlan") || !fe.has("createSemanticTask") {
		t.Fatalf("ad-hoc path must create the synthetic Plan + semantic task; queries=%v", fe.queries)
	}
	// ...and FINALIZED (both to succeeded) so it can't sit running.
	if !fe.has("updatePlanStatus", `status: "succeeded"`) {
		t.Errorf("synthetic adHocAction Plan must be finalized succeeded; queries=%v", fe.queries)
	}
	if !fe.has("updateTaskStatus", `status: "succeeded"`) {
		t.Errorf("synthetic callTool task must be finalized succeeded; queries=%v", fe.queries)
	}
}

// TestStamper_AdHoc_ToolError_FinalizesFailed: when the wrapped tool errors,
// the synthetic wrapper is finalized 'failed', not left running.
func TestStamper_AdHoc_ToolError_FinalizesFailed(t *testing.T) {
	fe := &fakeExec{toolErr: errors.New("boom")}
	s := New(fe, nil)
	ctx := WithPlanContext(context.Background(), PlanContext{AgentId: "a", OwnerUserId: "u", PartitionId: "s"})

	_, err := s.ExecuteToolByName(ctx, "someTool", nil)
	if err == nil {
		t.Fatal("tool error must propagate")
	}
	if !fe.has("updatePlanStatus", `status: "failed"`) {
		t.Errorf("tool error must finalize the wrapper failed; queries=%v", fe.queries)
	}
}

// TestStamper_RealPlan_NotFinalized: a caller-supplied REAL Plan (PlanId set)
// is the planner's to finalize -- the stamper must NEVER touch its status.
func TestStamper_RealPlan_NotFinalized(t *testing.T) {
	fe := &fakeExec{}
	s := New(fe, nil)
	ctx := WithPlanContext(context.Background(), PlanContext{
		PlanId: "v1:planner:plan:real", SemanticTaskId: "v1:planner:task:real",
		AgentId: "a", OwnerUserId: "u", PartitionId: "s",
	})

	if _, err := s.ExecuteToolByName(ctx, "someTool", nil); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if fe.count("updatePlanStatus") != 0 {
		t.Errorf("a real caller-supplied Plan must NOT be status-finalized by the stamper; queries=%v", fe.queries)
	}
	if fe.count("updateTaskStatus") != 0 {
		t.Errorf("a real Plan's semantic task must NOT be finalized by the stamper; queries=%v", fe.queries)
	}
	// It still records the tool invocation.
	if !fe.has("createToolInvocationTask") {
		t.Errorf("real-plan path should still stamp the toolInvocation; queries=%v", fe.queries)
	}
}

// TestStamper_AdHoc_SelfPlanningTool_NotWrapped: an ad-hoc call to a
// self-planning tool (produceArtifact) is NOT wrapped -- it spawns its own
// Plan, so a synthetic adHocAction wrapper would be a duplicate task (#1192).
// Zero rows stamped.
func TestStamper_AdHoc_SelfPlanningTool_NotWrapped(t *testing.T) {
	fe := &fakeExec{}
	s := New(fe, nil)
	ctx := WithPlanContext(context.Background(), PlanContext{AgentId: "a", OwnerUserId: "u", PartitionId: "s"})

	out, err := s.ExecuteToolByName(ctx, "produceArtifact", map[string]any{"goal": "x"})
	if err != nil || out != "ran produceArtifact" {
		t.Fatalf("tool must still run: out=%q err=%v", out, err)
	}
	if len(fe.queries) != 0 {
		t.Fatalf("self-planning ad-hoc tool must NOT be stamped/wrapped; got %v", fe.queries)
	}
}

// TestStamper_RealPlan_SelfPlanningTool_StillStamped: under a REAL caller Plan,
// even a self-planning tool is stamped (it's a genuine sub-step) -- the skip is
// only for the ad-hoc duplicate-wrapper case.
func TestStamper_RealPlan_SelfPlanningTool_StillStamped(t *testing.T) {
	fe := &fakeExec{}
	s := New(fe, nil)
	ctx := WithPlanContext(context.Background(), PlanContext{
		PlanId: "v1:planner:plan:real", SemanticTaskId: "v1:planner:task:real",
		AgentId: "a", OwnerUserId: "u", PartitionId: "s",
	})
	if _, err := s.ExecuteToolByName(ctx, "produceArtifact", nil); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !fe.has("createToolInvocationTask") {
		t.Errorf("a self-planning tool under a real Plan should still stamp its toolInvocation; queries=%v", fe.queries)
	}
}

// TestIsSelfPlanningTool: produceArtifact is always self-planning; env adds
// more; unknown tools are not.
func TestIsSelfPlanningTool(t *testing.T) {
	if !isSelfPlanningTool("produceArtifact") {
		t.Error("produceArtifact must be self-planning by default")
	}
	if isSelfPlanningTool("workbenchHost") {
		t.Error("workbenchHost is not self-planning")
	}
	t.Setenv("MEMQL_TASKSTAMP_SELF_PLANNING_TOOLS", "fooTool, barTool")
	if !isSelfPlanningTool("barTool") {
		t.Error("env-added tool must count as self-planning")
	}
	if isSelfPlanningTool("") {
		t.Error("empty tool name must never be self-planning")
	}
}

// TestStamper_NoPlanContext_Passthrough: without a PlanContext the stamper is a
// pure passthrough -- zero rows written.
func TestStamper_NoPlanContext_Passthrough(t *testing.T) {
	fe := &fakeExec{}
	s := New(fe, nil)
	out, err := s.ExecuteToolByName(context.Background(), "t", nil)
	if err != nil || out != "ran t" {
		t.Fatalf("passthrough broken: %q %v", out, err)
	}
	if len(fe.queries) != 0 {
		t.Errorf("no PlanContext => no stamping; got %v", fe.queries)
	}
}
