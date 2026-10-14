package work

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/proving/goaleval"
	"github.com/znasllc-io/memql/core/common"
)

func TestEvaluationPublicationRejectsModelSelfCertification(t *testing.T) {
	i, eng := newTestIntegration(t)
	ctx := common.ContextWithRun(callerContext("u-alice"), common.RunContext{RunId: "run", OwnerUserId: "u-alice"})
	if _, err := i.handleRecordGoalEvaluation(ctx, map[string]any{}, 0); err == nil {
		t.Fatal("model published its own evaluation")
	}
	if eng.summary() != "(none)" {
		t.Fatal("read or wrote before authority check")
	}
	if _, err := i.handleRecordGoalEvaluation(context.Background(), map[string]any{}, 0); err == nil {
		t.Fatal("anonymous publication")
	}
}

func TestEvaluationPublicationRequiresEveryActualModelReceipt(t *testing.T) {
	rows := []map[string]any{{"id": "v1:work:modelCall:one", "served": "local", "provider": "fleet:qwen3.8:27b", "model": "qwen3.8:27b", "promptRef": "workAgentReply", "decision": map[string]any{"requestedLevel": "reasoning"}}}
	use := goaleval.ModelUse{Calls: 1, Evidence: []string{"modelCall:one"}, Source: "fleet", Model: "qwen3.8:27b", Role: "workAgentReply", Level: "reasoning"}
	r := goaleval.Result{ModelsUsed: []goaleval.ModelUse{use}}
	if err := verifyEvaluationModelReceipts(r, rows); err != nil {
		t.Fatal(err)
	}
	for name, uses := range map[string][]goaleval.ModelUse{"omitted": nil, "duplicate": {use, use}, "foreign": {{Calls: 1, Evidence: []string{"modelCall:other"}}}, "invented role": {{Calls: 1, Evidence: use.Evidence, Model: use.Model, Source: use.Source, Role: "invented", Level: use.Level}}} {
		t.Run(name, func(t *testing.T) {
			if verifyEvaluationModelReceipts(goaleval.Result{ModelsUsed: uses}, rows) == nil {
				t.Fatal("unverified model receipt accepted")
			}
		})
	}
}
