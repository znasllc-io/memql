//go:build agent

package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	permission "github.com/znasllc-io/memql/core/computerpermission"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agents"
)

// Only inference is replaced. The public installed builtin builds its actual
// history on the second engine, then the model adapter restores the durable
// exchange through the same engine method used by both real tool loops.
type permissionContextTurn struct {
	engine   *memql.MemQLEngine
	calls    int
	restored []common.ChatMessage
}

func (r *permissionContextTurn) ResolveOwnerAgent(context.Context, string) (string, error) {
	return "fixture-agent", nil
}

func (r *permissionContextTurn) RunTurn(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg) (string, error) {
	r.calls++
	messages := []common.ChatMessage{{Role: "system", Content: "Current instructions"}}
	for _, message := range msg.History {
		messages = append(messages, common.ChatMessage{Role: message.Role, Content: message.Content})
	}
	var err error
	r.restored, err = r.engine.RestoreWorkContinuation(ctx, messages)
	return "Continued from the exact saved exchange", err
}

func TestWorkDBNativeContinuationProjectsExactDecisionOnAnotherReplica(t *testing.T) {
	writer, reader := workTemplateDBEngine(t), workTemplateDBEngine(t)
	runner := &permissionContextTurn{engine: reader}
	ai := agents.New(reader.Agents(), reader)
	ai.SetAgentTurnRunner(runner)
	require.NoError(t, reader.RegisterIntegration(ai))
	for _, mode := range []string{"exact-command", "budget", "foreign-owner", "pending", "wrong-step", "wrong-run"} {
		t.Run(mode, func(t *testing.T) {
			runner.calls, runner.restored = 0, nil
			key := id.NewShortId()
			owner, runID, goalID, approvalID := key+"owner", key+"run", key+"goal", key+"approval"
			const stepKey = "inspect"
			person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
			run := common.RunContext{RunId: runID, GoalId: goalID, StepKey: stepKey, OwnerUserId: owner, Mode: common.RunModeLive}
			ctx := common.ContextWithRun(person, run)
			write := func(name string, args map[string]any) {
				templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
			}
			query := func(e *memql.MemQLEngine, name string, args map[string]any) []map[string]any {
				t.Helper()
				call, err := parser.RenderCall(name, args)
				require.NoError(t, err)
				result, err := e.Execute(person, "query "+call)
				require.NoError(t, err)
				return memql.MaterializeRows(result)
			}
			write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic saved progress", "origin": "user"})
			write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "fixture", "templateFingerprint": "unchanged-fixture", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
			write("updateWorkRun", map[string]any{"runId": runID, "humanResumeId": approvalID})
			const command = "git -C /fixture/repository status --short --branch"
			args := map[string]any{"cmd": command, "cwd": "/fixture/repository", "timeoutSec": 60}
			encodedArgs, err := json.Marshal(map[string]any{"operation": map[string]any{"tool": "workerHost", "action": "exec", "args": args}})
			require.NoError(t, err)
			saved := []common.ChatMessage{
				{Role: "system", Content: "Previous instructions"},
				{Role: "user", Content: "Inspect the already selected repository"},
				{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "completed", Name: "priorEffect", Arguments: `{}`}}},
				{Role: "tool", ToolCallId: "completed", Name: "priorEffect", Content: "Effect already completed; immutable receipt"},
				{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "permission", Name: "requestComputerUseScope", Arguments: string(encodedArgs)}}},
				{Role: "tool", ToolCallId: "permission", Name: "requestComputerUseScope", Content: "Not executed before the saved checkpoint. Inspect recorded results and any human answer before continuing."},
			}
			if mode == "budget" {
				saved = saved[:4] // A safe model boundary after the completed effect.
			}
			require.NoError(t, writer.SaveWorkContinuation(ctx, saved))
			checkpoints := query(reader, "workContinuationForOwnerRun", map[string]any{"runId": runID, "stepKey": stepKey})
			require.Len(t, checkpoints, 1)
			checkpointID := memql.BareShortId(checkpoints[0]["id"].(string))
			hash, err := permission.Hash("workerHost", "exec", args)
			require.NoError(t, err)
			scope := permission.Scope{Version: 1, Kind: "exact_command", RegistrationID: "fixture-worker", MachineID: "fixture-machine", MachineLabel: "Fixture computer", Tool: "workerHost", Action: "exec", Command: command, CWD: "/fixture/repository", TimeoutSeconds: 60, ArgsHash: hash, AllowedModes: []string{"once", "always"}}
			kind := "scopeElevation"
			subject := map[string]any{"computerPermission": scope.Map()}
			answer := map[string]any{"permissionMode": "once"}
			if mode == "budget" {
				kind = "budget"
				subject = map[string]any{"ceiling": "wallClock", "resumeKind": "checkpoint", "continuationId": checkpointID}
				answer = map[string]any{"newLimit": 3600000}
			}
			approvalRun, approvalStep := runID, stepKey
			approvalPerson := person
			if mode == "foreign-owner" {
				foreign := key + "foreign"
				approvalPerson = auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), foreign), &auth.AccessContext{UserId: foreign, Role: auth.RoleWriter})
			}
			if mode == "wrong-step" {
				approvalStep = "another-step"
			}
			if mode == "wrong-run" {
				approvalRun = key + "another-run"
			}
			templateMutation(t, writer, auth.ContextWithInternalOrigin(approvalPerson), "createWorkApproval", map[string]any{"approvalId": approvalID, "runId": approvalRun, "stepKey": approvalStep, "kind": kind, "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "question": "Continue this exact suspended operation?", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano)})
			// Prime a different replica before the answer. Neither an old cached
			// pending result nor the source replica's memory may supply the answer.
			prior := query(reader, "workApprovalForOwner", map[string]any{"approvalId": approvalID})
			if mode != "foreign-owner" {
				require.Len(t, prior, 1)
				require.Empty(t, prior[0]["decision"])
			}
			if mode != "pending" {
				templateMutation(t, writer, auth.ContextWithInternalOrigin(approvalPerson), "decideWorkApproval", map[string]any{"approvalId": approvalID, "decision": "approved", "decidedBy": owner, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": answer})
			}
			valid := mode == "exact-command" || mode == "budget"
			prepared, err := reader.PrepareWorkContinuation(ctx, memql.CheckpointResumeRequest{RunID: runID, StepKey: stepKey, ApprovalID: approvalID})
			if valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "the native continuation gate must refuse an unrelated or unanswered receipt")
				// A claimed proof must also fail before the runner at the installed
				// history seam, rather than leaking a foreign decision into context.
				run.Continuation = &common.StepContinuation{StepKey: stepKey, ObservationID: checkpointID, ApprovalID: approvalID}
				prepared = common.ContextWithRun(person, run)
			}
			_, err = reader.Execute(prepared, `builtin runAgentTurn(prompt: "Continue the unchanged section")`)
			if !valid {
				require.Error(t, err)
				require.Zero(t, runner.calls, "a refused decision must not reach the runtime adapter")
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, runner.calls)
			require.Len(t, runner.restored, len(saved)+1, "the fresh exact decision must survive checkpoint restoration")
			require.Equal(t, "Current instructions", runner.restored[0].Content)
			require.Equal(t, saved[1:], runner.restored[1:len(saved)], "completed tool effects and original arguments must remain verbatim, not be reconstructed or repeated")
			receipt := runner.restored[len(saved)]
			require.Equal(t, "user", receipt.Role)
			require.True(t, strings.HasPrefix(receipt.Content, "[Recorded answer for this "))
			var decision map[string]any
			require.NoError(t, json.Unmarshal([]byte(strings.SplitN(receipt.Content, "\n", 2)[1]), &decision))
			require.Equal(t, approvalID, decision["approvalId"])
			require.Equal(t, "approved", decision["decision"])
			require.Equal(t, kind, decision["kind"])
			if mode == "exact-command" {
				require.Equal(t, "once", decision["answer"].(map[string]any)["permissionMode"])
				require.Equal(t, command, decision["subject"].(map[string]any)["computerPermission"].(map[string]any)["command"])
			} else {
				require.Equal(t, float64(3600000), decision["answer"].(map[string]any)["newLimit"])
				require.Equal(t, checkpointID, decision["subject"].(map[string]any)["continuationId"])
			}
			// Reading a decision is not consuming permission or changing the
			// saved checkpoint. The actual worker admission remains separate.
			require.Equal(t, checkpoints, query(writer, "workContinuationForOwnerRun", map[string]any{"runId": runID, "stepKey": stepKey}))
			current := query(writer, "workApprovalForOwner", map[string]any{"approvalId": approvalID})
			require.Empty(t, current[0]["permissionUsedAt"])
		})
	}
}
