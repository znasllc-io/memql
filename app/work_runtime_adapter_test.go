package app

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/agent"
)

func TestAgentRuntimeAdapterPreservesTheEnginesOwnershipBarrier(t *testing.T) {
	var runtime agent.WorkRuntime = &CognitionEngineAdapter{Engine: &memql.MemQLEngine{}}
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
	if err := runtime.PrepareWorkTool(ctx); err == nil || !strings.Contains(err.Error(), "owned work") {
		t.Fatalf("adapter lost workload ownership gate: %v", err)
	}
	if err := runtime.SaveWorkContinuation(ctx, nil); err == nil || !strings.Contains(err.Error(), "owned work") {
		t.Fatalf("adapter lost continuation ownership gate: %v", err)
	}
	bounded, cancel, err := runtime.ContextWithWorkCallDeadline(context.Background())
	defer cancel()
	if err != nil || bounded.Err() != nil {
		t.Fatalf("unowned call deadline: %v", err)
	}
}
