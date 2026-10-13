package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

type discussionResolver struct{ model *discussionModel }

func (r discussionResolver) ResolveFor(context.Context, airoute.ResolveRequest) (memql.ResolvedProvider, error) {
	return memql.ResolvedProvider{Client: r.model, Entry: &memql.ProviderConfigEntry{Config: memql.ProviderConfig{Name: "fleet:discussion-test", Model: "discussion-test"}}, Resolution: airoute.Resolution{ProviderName: "fleet:discussion-test", Model: "discussion-test"}}, nil
}

type discussionModel struct {
	entered chan string
	release chan struct{}
	calls   int
	failure error
}

func (m *discussionModel) Call(ctx context.Context, prompt string) (any, error) {
	m.calls++
	if m.failure != nil {
		return nil, m.failure
	}
	if m.entered != nil {
		m.entered <- prompt
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return "The recorded failure is in context checkpointing. Inspect that boundary before changing the host.", nil
}

func discussionExecute(t *testing.T, e *memql.MemQLEngine, ctx context.Context, name string, args map[string]any) map[string]any {
	t.Helper()
	result, err := e.Execute(ctx, "builtin "+call(name, args))
	require.NoError(t, err)
	rows := memql.MaterializeRows(result.OutputPayload())
	require.Len(t, rows, 1)
	return rows[0]
}

func TestApprovalDiscussionAdmissionAndAnalysisSurviveReplicaChanges(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	for _, peer := range []actsDB{a, b} {
		require.NoError(t, peer.eng.RegisterIntegration(peer.i))
	}
	ctx := actorCtx(a.owner)
	approvalID := id.NewShortId()
	parent := a.openRun(t, map[string]any{"status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": approvalID}, "errorMessage": "context checkpoint did not free space; originals were retained"})
	a.write(t, "createWorkApproval", map[string]any{"approvalId": approvalID, "runId": parent, "kind": "planReview", "stepKey": "reason", "subject": map[string]any{"symptom": "plan", "errorMessage": "context checkpoint did not free space; originals were retained", "computerPermission": map[string]any{"kind": "directory_read", "machineLabel": "Synthetic laptop", "directory": "/fixture/repository", "argsHash": "private-correlation", "registrationId": "private-id", "unrelatedSecret": "never selected"}}, "artifactHash": "test", "question": "How should this continue?", "requestedAt": rfc(time.Now())})
	a.writeVersion(t, parent, parent, "reason", 0, 1, nil, "", map[string]any{"status": "failed", "errorMessage": "Exact blocked step evidence"})
	first := discussionExecute(t, a.eng, ctx, "askApprovalDiscussion", map[string]any{"approvalId": approvalID})
	second := discussionExecute(t, b.eng, ctx, "askApprovalDiscussion", map[string]any{"approvalId": approvalID})
	require.Contains(t, fmt.Sprint(rowMap(first, "approval")["summary"]), "context checkpoint")
	require.Equal(t, first, second, "opening on another node must recover the same conversation without a model")
	conversation := fmt.Sprint(first["conversationId"])
	args := map[string]any{"approvalId": approvalID, "conversationId": conversation, "requestId": "second-opinion", "prompt": "Explain the recorded failure without changing anything.", "advisor": true}
	var replies [2]map[string]any
	var errs [2]error
	var wg sync.WaitGroup
	for n, peer := range []actsDB{a, b} {
		wg.Add(1)
		go func(n int, peer actsDB) {
			defer wg.Done()
			nodes, err := peer.i.handleOpenApprovalDiscussion(ctx, args, 0)
			errs[n] = err
			if err == nil {
				if len(nodes) != 1 {
					errs[n] = fmt.Errorf("wanted one receipt, got %d", len(nodes))
				} else {
					errs[n] = json.Unmarshal(nodes[0].Payload, &replies[n])
				}
			}
		}(n, peer)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, replies[0], replies[1])
	receipt := replies[0]
	runID := fmt.Sprint(receipt["runId"])
	child := a.runRow(t, runID)
	require.Equal(t, memql.BareShortId(parent), memql.BareShortId(rowString(child, "parentRunId")))
	require.Equal(t, memql.ApprovalDiscussionTemplate, child["automationName"])
	goal, err := b.i.store().goalForOwner(memql.ContextWithFreshRead(ctx), rowString(child, "goalId"))
	require.NoError(t, err)
	require.EqualValues(t, 1, rowMap(goal, "ceilings")["maxModelCalls"])
	model := &discussionModel{entered: make(chan string, 1), release: make(chan struct{})}
	b.eng.SetAIResolver(discussionResolver{model})
	b.eng.SetRunCeilingGuard(NewRunCeilings(b.eng, testLogger()))
	b.eng.SetModelCallJournal(NewModelJournal(b.eng))
	runCtx := common.ContextWithRun(ctx, common.RunContext{RunId: runID, GoalId: rowString(child, "goalId"), OwnerUserId: a.owner, StepKey: "reply", Mode: common.RunModeLive})
	type answerResult struct {
		answer map[string]any
		err    error
	}
	done := make(chan answerResult, 1)
	go func() {
		r, err := b.eng.Execute(runCtx, "builtin askRunApprovalMessage(templateId: \"askApprovalSecondOpinion\")")
		var value map[string]any
		if err == nil {
			rows := memql.MaterializeRows(r.OutputPayload())
			if len(rows) == 1 {
				value = rows[0]
			}
		}
		done <- answerResult{value, err}
	}()
	select {
	case prompt := <-model.entered:
		require.Contains(t, prompt, "context checkpoint")
		require.Contains(t, prompt, "independent second opinion")
	case result := <-done:
		t.Fatalf("analysis failed before model: %v", result.err)
	case <-time.After(10 * time.Second):
		t.Fatal("analysis did not reach model")
	}
	// While the model is held on replica B, replica A records another human
	// message immediately. There is no conversation-wide model lock.
	noteArgs := map[string]any{"approvalId": approvalID, "conversationId": conversation, "requestId": "new-information", "prompt": "I verified that the host has enough memory.", "advisor": false}
	note := discussionExecute(t, a.eng, ctx, "askApprovalMessage", noteArgs)
	require.NotEqual(t, receipt["runId"], note["runId"])
	close(model.release)
	answer := <-done
	require.NoError(t, answer.err)
	require.Equal(t, 1, model.calls)
	// The model capability cannot spend a second call on this child's budget.
	_, err = b.eng.Execute(runCtx, "builtin askRunApprovalMessage(templateId: \"askApprovalSecondOpinion\")")
	require.ErrorContains(t, err, "ceiling")
	require.Equal(t, 1, model.calls)
	a.writeVersion(t, runID, runID, "reply", 1, 1, nil, "advice", map[string]any{"result": map[string]any{"value": answer.answer}})
	a.finishRun(t, runID, []string{"reply"}, nil)
	snapshot := discussionExecute(t, b.eng, ctx, "askConversationSnapshot", map[string]any{"conversationId": conversation})
	text := fmt.Sprint(snapshot)
	require.Contains(t, text, "I verified that the host has enough memory.")
	require.Contains(t, text, "The recorded failure is in context checkpointing.")
	require.Contains(t, text, "advisor:true")
	parentAfter := a.runRow(t, parent)
	require.Equal(t, "waiting", parentAfter["status"])
	approval := a.query(t, a.owner, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))[0]
	require.Equal(t, "", approval["decision"], "advice must never become human approval")
	// The next analysis sees the new human information and the earlier advice.
	next := a.runRow(t, fmt.Sprint(note["runId"]))
	nextCtx := common.ContextWithRun(ctx, common.RunContext{RunId: rowString(next, "id"), GoalId: rowString(next, "goalId"), OwnerUserId: a.owner, StepKey: "reply", Mode: common.RunModeLive})
	evidence := discussionExecute(t, b.eng, nextCtx, "askApprovalEvidence", map[string]any{})
	require.Contains(t, fmt.Sprint(evidence), "Exact blocked step evidence")
	require.Contains(t, fmt.Sprint(evidence), "Synthetic laptop")
	require.Contains(t, fmt.Sprint(evidence), "/fixture/repository")
	require.NotContains(t, fmt.Sprint(evidence), "private-correlation")
	require.NotContains(t, fmt.Sprint(evidence), "private-id")
	require.NotContains(t, fmt.Sprint(evidence), "never selected")
	require.Contains(t, fmt.Sprint(evidence), noteArgs["prompt"])
	require.Contains(t, fmt.Sprint(evidence), answer.answer["reply"])
	for name, deniedCtx := range map[string]context.Context{"another owner": actorCtx("unrelated-discussion-owner"), "model delegation": runCtx} {
		t.Run(name, func(t *testing.T) {
			_, err := a.eng.Execute(deniedCtx, "builtin "+call("askApprovalMessage", noteArgs))
			require.Error(t, err)
		})
	}
	for _, name := range []string{"askRunApprovalMessage(templateId: \"askApprovalReply\")", "askApprovalEvidence()"} {
		_, err := b.eng.Execute(ctx, "builtin "+name)
		require.Error(t, err, "a direct client has no child scope")
	}
	_, err = b.eng.Execute(nextCtx, "builtin askRunApprovalMessage(templateId: \"workAgentReply\")")
	require.Error(t, err)
	changed := map[string]any{}
	for k, v := range args {
		changed[k] = v
	}
	changed["prompt"] = "Different instructions"
	_, err = a.i.handleOpenApprovalDiscussion(ctx, changed, 0)
	require.ErrorContains(t, err, "different proposal")
	a.write(t, "decideWorkApproval", map[string]any{"approvalId": approvalID, "decision": "rejected", "decidedBy": a.owner, "decidedAt": rfc(time.Now())})
	_, err = b.eng.Execute(nextCtx, "builtin askApprovalEvidence()")
	require.ErrorContains(t, err, "no longer pending")
	_, err = a.i.handleOpenApprovalDiscussion(ctx, map[string]any{"approvalId": approvalID, "conversationId": conversation, "requestId": "after-decision", "prompt": "Do it anyway"}, 0)
	require.ErrorContains(t, err, "no longer pending")
	// A retry of an already admitted message remains the same receipt.
	replayed, err := a.i.handleOpenApprovalDiscussion(ctx, args, 0)
	require.NoError(t, err)
	require.Equal(t, runID, decodeReply(t, replayed)["runId"])
	require.False(t, strings.Contains(text, "approved:true"))
}

// Execute the real installed DSL on a replica that did not admit the message.
// Both outcomes close the analysis; an error must not become a new permission
// request, classification call, or executable repair plan.
func TestApprovalDiscussionRunsInstalledTemplateAndEndsWithoutRecovery(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			a, b := openActsDB(t), openActsDB(t)
			previous := auth.InstalledCapabilityCatalog()
			auth.SetCapabilityCatalog(nil)
			t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
			for _, peer := range []actsDB{a, b} {
				require.NoError(t, peer.eng.RegisterIntegration(peer.i))
			}
			ctx := actorCtx(a.owner)
			approvalID := id.NewShortId()
			parent := a.openRun(t, map[string]any{"status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": approvalID}})
			a.write(t, "createWorkApproval", map[string]any{"approvalId": approvalID, "runId": parent, "kind": "sideEffect", "subject": map[string]any{"stepKey": "reason"}, "artifactHash": "fixture", "question": "Should this action run?", "requestedAt": rfc(time.Now())})
			opened := discussionExecute(t, a.eng, ctx, "askApprovalDiscussion", map[string]any{"approvalId": approvalID})
			receipt := discussionExecute(t, a.eng, ctx, "askApprovalMessage", map[string]any{"approvalId": approvalID, "conversationId": opened["conversationId"], "requestId": "message", "prompt": "Explain what this request would allow.", "advisor": false})
			runID := fmt.Sprint(receipt["runId"])
			model := &discussionModel{}
			if failed {
				model.failure = errors.New("connection reset while reading provider response")
			}
			b.eng.SetAIResolver(discussionResolver{model})
			b.eng.SetRunCeilingGuard(NewRunCeilings(b.eng, testLogger()))
			b.eng.SetModelCallJournal(NewModelJournal(b.eng))
			loader := automations.NewLoader(automations.LoaderOptions{Functions: b.eng.Functions(), Logger: b.eng.Logger})
			auto, err := loader.LoadByName(memql.ApprovalDiscussionTemplate)
			require.NoError(t, err)
			journal, err := automations.LoadRunJournal(ctx, b.eng, runID)
			require.NoError(t, err)
			executor := automations.NewExecutor(automations.ExecutorOptions{Engine: b.eng, Logger: b.eng.Logger, StepRegistry: steps.NewRegistry(), TerminalFailures: true})
			defer executor.Close()
			_, err = executor.ExecuteAdopted(common.ContextWithRun(ctx, common.RunContext{RunId: journal.RunId, GoalId: journal.GoalId, OwnerUserId: journal.OwnerUserId, Mode: common.RunModeLive, Spine: journal.Spine}), auto, automations.RunAdoption{RunId: runID, TriggeredBy: "compiled", Variables: journal.Variables, Journal: journal})
			if failed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 1, model.calls)
			row := a.runRow(t, runID)
			want := "succeeded"
			if failed {
				want = "failed"
			}
			require.Equal(t, want, row["status"])
			approvals := a.query(t, a.owner, "query "+call("workApprovalsForOwnedRun", map[string]any{"runId": runID}))
			require.Empty(t, approvals)
			require.Equal(t, "waiting", a.runRow(t, parent)["status"])
			snapshot := discussionExecute(t, a.eng, ctx, "askConversationSnapshot", map[string]any{"conversationId": opened["conversationId"]})
			if !failed {
				require.Contains(t, fmt.Sprint(snapshot), "The recorded failure is in context checkpointing.")
			}
		})
	}
}
