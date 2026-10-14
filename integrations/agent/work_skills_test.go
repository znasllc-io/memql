package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func TestWorkPromptConsumesExactSavedSkillContentAndFailsWithoutReceipt(t *testing.T) {
	registry := memql.NewPromptRegistry()
	if _, err := memql.LoadUnifiedPrompts(nil, registry, template.New("partials")); err != nil {
		t.Fatal(err)
	}
	engine := &workPromptEngine{prompts: registry, skills: []map[string]any{{"receiptId": "saved-on-another-replica", "selection": map[string]any{"selected": []map[string]any{{"id": "software", "name": "Software", "version": 7, "digest": "exact-content-digest", "text": "Preserve unrelated edits from the original task."}, {"id": "testing", "name": "Testing", "version": 2, "digest": "test-digest", "text": "Demonstrate the behavioral regression before changing production code."}}}}}}
	owner := "v1:identity:user:skill-context-owner"
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), owner), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: owner, StepKey: "implement", Mode: common.RunModeLive})
	msg := &memqlv1.AgentGenerateTurnMsg{AgentId: "assistant", ActingAgent: &memqlv1.ActingAgentIdentity{Id: "assistant", Role: "assistant"}, History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "Fix the software defect"}}}
	result, err := newTestReplier(engine).prepareTurn(ctx, msg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Preserve unrelated edits from the original task.", "Demonstrate the behavioral regression before changing production code.", "Software v7", "Testing v2", "saved-on-another-replica", "exact-content-digest"} {
		if !strings.Contains(result.messages[0].Content, want) {
			t.Fatalf("prompt lost %q", want)
		}
	}
	rc, _ := common.RunFromContext(ctx)
	rc.Mode, rc.SourceRunId = common.RunModeReplay, "source-run"
	replayed, err := newTestReplier(engine).prepareTurn(common.ContextWithRun(ctx, rc), msg, time.Now())
	if err != nil || replayed.messages[0].Content != result.messages[0].Content {
		t.Fatalf("replay changed exact skill context: %v", err)
	}
	engine.skillError = errors.New("journal unavailable")
	if _, err := newTestReplier(engine).prepareTurn(ctx, msg, time.Now()); err == nil {
		t.Fatal("provider could run without the required skill receipt")
	}
}
