package compose

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	pure "github.com/znasllc-io/memql/component/compose"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/agents"
	"github.com/znasllc-io/memql/integrations/work"
)

type workReceiptRunner func(context.Context, *memqlv1.AgentGenerateTurnMsg) (string, error)

func (f workReceiptRunner) RunTurn(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg) (string, error) {
	return f(ctx, msg)
}

func TestProduceArtifactDBRequiresAnActualFileReceipt(t *testing.T) {
	e := materializeDBEngine(t)
	materializer := New(e, e.Logger)
	uploader := &materializeUploader{}
	materializer.SetUploader(uploader, "files")
	materializer.SetComposer(materializeComposerFunc(func(context.Context, ComposeRequest) (ComposeReply, error) {
		return ComposeReply{Draft: pure.Draft{Body: "Saved report"}}, nil
	}))
	agentIntegration := agents.New(memql.NewAgentRegistry(), e)
	if err := e.RegisterIntegration(agentIntegration); err != nil {
		t.Fatal(err)
	}
	loader := automations.NewLoader(automations.LoaderOptions{Logger: e.Logger})
	auto, err := loader.LoadByName("produceArtifact")
	if err != nil || auto == nil {
		t.Fatalf("load produceArtifact: %v", err)
	}
	executor := automations.NewExecutor(automations.ExecutorOptions{Engine: e, Logger: e.Logger, StepRegistry: steps.NewRegistry()})
	defer executor.Close()
	for _, tc := range []struct {
		name              string
		save, earlierFile bool
	}{
		{name: "no file"},
		{name: "earlier branch file", earlierFile: true},
		{name: "current step file", save: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := fmt.Sprintf("work-receipt-%d", time.Now().UnixNano())
			ctx := auth.ContextWithUserActor(context.Background(), owner)
			agentIntegration.SetAgentTurnRunner(workReceiptRunner(func(ctx context.Context, _ *memqlv1.AgentGenerateTurnMsg) (string, error) {
				if tc.save {
					ac, _ := auth.AccessFromContext(ctx)
					if _, err := materializer.materialize(ctx, ac.UserId, "", materializeDraft()); err != nil {
						return "", err
					}
				}
				return "Done", nil
			}))
			_, runId, err := work.New(e, e.Logger).OpenDirectGoal(ctx, work.DirectGoal{OwnerUserId: owner, Statement: "Save report", AutomationName: "produceArtifact", Input: map[string]any{"agentId": "agent", "goal": "Save report"}})
			if err != nil {
				t.Fatal(err)
			}
			journal, err := automations.LoadRunJournal(ctx, e, runId)
			if err != nil {
				t.Fatal(err)
			}
			ctx, err = auth.ContextWithPersistedOwner(context.Background(), journal.OwnerUserId, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx = common.ContextWithRun(ctx, common.RunContext{RunId: journal.RunId, GoalId: journal.GoalId, OwnerUserId: journal.OwnerUserId, Mode: journal.Mode})
			if tc.earlierFile {
				sectionCtx := common.ContextWithRun(ctx, common.RunContext{RunId: journal.RunId, GoalId: journal.GoalId, OwnerUserId: journal.OwnerUserId, Mode: journal.Mode, StepKey: "sections.partial"})
				if _, err := materializer.materialize(sectionCtx, journal.OwnerUserId, "", materializeDraft()); err != nil {
					t.Fatal(err)
				}
			}
			result, runErr := executor.ExecuteAdopted(ctx, auto, automations.RunAdoption{RunId: journal.RunId, Variables: journal.Variables, Journal: journal})
			if !tc.save {
				if runErr == nil || result.Status != "failed" || !strings.Contains(runErr.Error(), "without saving a ready Library file") {
					t.Fatalf("plain confirmation counted as a produced file: result=%+v err=%v", result, runErr)
				}
				return
			}
			if runErr != nil || result.Status != "completed" {
				t.Fatalf("real saved file did not satisfy receipt: result=%+v err=%v", result, runErr)
			}
			// A recovered turn in this step, and strict replay of that same
			// effect, must keep using the completed composition's file.
			for _, replay := range []bool{false, true} {
				rc := common.RunContext{RunId: journal.RunId, GoalId: journal.GoalId, OwnerUserId: journal.OwnerUserId, Mode: journal.Mode, StepKey: "produce"}
				if replay {
					rc.RunId, rc.Mode = "receipt-replay", common.RunModeReplay
					rc.SourceRunId, rc.SourceGoalId = journal.RunId, journal.GoalId
				}
				before := uploader.calls
				for _, capability := range agentIntegration.Capabilities() {
					if capability.Name != "runAgentTurn" {
						continue
					}
					_, err := capability.Handler(common.ContextWithRun(ctx, rc), map[string]any{"agentId": "agent", "prompt": "Save report", "requireFile": true}, 0)
					if err != nil || uploader.calls != before {
						t.Fatalf("same-step effect was lost or duplicated (replay=%v): err=%v uploads=%d, before=%d", replay, err, uploader.calls, before)
					}
				}
			}
		})
	}
}
