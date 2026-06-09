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
		AgentId: "v1:agents:agent:a", OwnerUserId: "u", SpaceId: "v1:cognition:space:s",
	})
	out, err := s.ExecuteToolByName(ctx, "produceArtifact", map[string]any{"x": 1})
	if err != nil || out != "ran produceArtifact" {
		t.Fatalf("tool passthrough broken: out=%q err=%v", out, err)
	}

	// The synthetic wrapper was created...
	if !fe.has("mutationCreateAdHocPlan") || !fe.has("mutationCreateSemanticTask") {
		t.Fatalf("ad-hoc path must create the synthetic Plan + semantic task; queries=%v", fe.queries)
	}
	// ...and FINALIZED (both to succeeded) so it can't sit running.
	if !fe.has("mutationUpdatePlanStatus", `"status": "succeeded"`) {
		t.Errorf("synthetic adHocAction Plan must be finalized succeeded; queries=%v", fe.queries)
	}
	if !fe.has("mutationUpdateTaskStatus", `"status": "succeeded"`) {
		t.Errorf("synthetic callTool task must be finalized succeeded; queries=%v", fe.queries)
	}
}

// TestStamper_AdHoc_ToolError_FinalizesFailed: when the wrapped tool errors,
// the synthetic wrapper is finalized 'failed', not left running.
func TestStamper_AdHoc_ToolError_FinalizesFailed(t *testing.T) {
	fe := &fakeExec{toolErr: errors.New("boom")}
	s := New(fe, nil)
	ctx := WithPlanContext(context.Background(), PlanContext{AgentId: "a", OwnerUserId: "u", SpaceId: "s"})

	_, err := s.ExecuteToolByName(ctx, "someTool", nil)
	if err == nil {
		t.Fatal("tool error must propagate")
	}
	if !fe.has("mutationUpdatePlanStatus", `"status": "failed"`) {
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
		AgentId: "a", OwnerUserId: "u", SpaceId: "s",
	})

	if _, err := s.ExecuteToolByName(ctx, "someTool", nil); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if fe.count("mutationUpdatePlanStatus") != 0 {
		t.Errorf("a real caller-supplied Plan must NOT be status-finalized by the stamper; queries=%v", fe.queries)
	}
	if fe.count("mutationUpdateTaskStatus") != 0 {
		t.Errorf("a real Plan's semantic task must NOT be finalized by the stamper; queries=%v", fe.queries)
	}
	// It still records the tool invocation.
	if !fe.has("mutationCreateToolInvocationTask") {
		t.Errorf("real-plan path should still stamp the toolInvocation; queries=%v", fe.queries)
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
