//go:build agent

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	permission "github.com/znasllc-io/memql/core/computerpermission"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agents"
	work "github.com/znasllc-io/memql/integrations/work"
)

const permissionCorrection = "Correct the command and request its exact approval. This feedback grants no permission."

// Only inference is replaced: the installed runAgentTurn, nested execution,
// checkpoint, typed question, public decision, grant reader and peer resume run.
type permissionGuidanceTurn struct {
	engine   *memql.MemQLEngine
	work     *work.Integration
	effects  map[string]int
	requests [][]common.ChatMessage
	paused   func(string)
}

func (*permissionGuidanceTurn) ResolveOwnerAgent(context.Context, string) (string, error) {
	return "fixture-agent", nil
}
func (f *permissionGuidanceTurn) RunTurn(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg) (string, error) {
	run, _ := common.RunFromContext(ctx)
	messages := []common.ChatMessage{{Role: "system", Content: "Current instructions"}}
	for _, m := range msg.History {
		messages = append(messages, common.ChatMessage{Role: m.Role, Content: m.Content})
	}
	if run.Continuation == nil {
		f.effects["completed-tool"]++
		messages = append(messages, common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "saved", Name: "effect", Arguments: `{}`}}}, common.ChatMessage{Role: "tool", ToolCallId: "saved", Name: "effect", Content: "effect already performed"})
	} else {
		var err error
		messages, err = f.engine.RestoreWorkContinuation(ctx, messages)
		if err != nil {
			return "", err
		}
	}
	f.requests = append(f.requests, messages)
	command := "printf original"
	if run.Continuation != nil {
		guided := false
		for _, m := range messages {
			if m.Role == "user" && strings.HasPrefix(m.Content, "[Recorded answer for this ") && strings.Contains(m.Content, permissionCorrection) {
				guided = true
			}
		}
		if !guided {
			return "", fmt.Errorf("recorded correction missing")
		}
		command = "printf corrected"
	}
	args := map[string]any{"cmd": command, "cwd": "/fixture/repository", "timeoutSec": 60}
	req := permission.Request{OwnerID: run.OwnerUserId, AgentID: "fixture-agent", RunID: run.RunId, Tool: "workerHost", Action: "exec", Args: args}
	grant, err := f.work.Find(ctx, req)
	if err != nil {
		return "", err
	}
	if grant != nil {
		if command != "printf corrected" {
			return "", fmt.Errorf("feedback approved original command")
		}
		if _, err := f.work.Claim(ctx, grant.ID, req, "fixture-worker", "fixture-machine"); err != nil {
			return "", err
		}
		f.effects["corrected-command"]++
		return "completed corrected operation", nil
	}
	hash, err := permission.Hash(req.Tool, req.Action, args)
	if err != nil {
		return "", err
	}
	scope := permission.Scope{Version: 1, Kind: "exact_command", RegistrationID: "fixture-worker", MachineID: "fixture-machine", MachineLabel: "Test computer", Tool: req.Tool, Action: req.Action, Command: command, CWD: "/fixture/repository", TimeoutSeconds: 60, ArgsHash: hash, AllowedModes: []string{"once", "always"}}
	encoded, _ := json.Marshal(map[string]any{"operation": map[string]any{"tool": req.Tool, "action": req.Action, "args": args}})
	messages = append(messages, common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: hash, Name: "requestComputerUseScope", Arguments: string(encoded)}}}, common.ChatMessage{Role: "tool", ToolCallId: hash, Name: "requestComputerUseScope", Content: "Not executed before the saved checkpoint. Inspect recorded results and any human answer before continuing."})
	if err := f.engine.SaveWorkContinuation(ctx, messages); err != nil {
		return "", err
	}
	approval, err := f.work.AskComputerScope(ctx, run.OwnerUserId, run.RunId, map[string]any{"agentId": "fixture-agent", "computerPermission": scope.Map(), "summary": "Review the exact test command"})
	if err != nil {
		return "", err
	}
	if f.paused != nil {
		f.paused(approval)
	}
	return "", &workstate.HumanWait{ApprovalID: approval}
}

func TestWorkDBTypedPermissionGuidanceKeepsExactNestedProgress(t *testing.T) {
	for _, mode := range []string{"ordinary", "early-answer", "refusals", "fresh-rerun"} {
		t.Run(mode, func(t *testing.T) {
			writer, db := workTemplateDBEngineAndDB(t)
			reader := workTemplateDBEngine(t)
			gates, err := database.NewGatePool(dbtest.DSN(), 0)
			require.NoError(t, err)
			defer gates.Close()
			key := id.NewShortId()
			owner, runID, goalID := key+"owner", key+"run", key+"goal"
			person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
			ctx := common.ContextWithRun(person, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: owner, Mode: common.RunModeLive})
			effects := map[string]int{}
			var runners []*permissionGuidanceTurn
			var triggers []*workTemplateTrigger
			for _, e := range []*memql.MemQLEngine{writer, reader} {
				var wi *work.Integration
				for _, plugin := range memql.RegisteredPlugins() {
					if plugin.Name != "work" {
						continue
					}
					provider, err := plugin.Factory(memql.PluginContext{Engine: e, Logger: e.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, ConceptDataIsStaged: e.ConceptDataIsStaged, AdmitSourceRow: memql.AdmitSourceRow})
					require.NoError(t, err)
					wi = provider.(*work.Integration)
				}
				require.NotNil(t, wi)
				require.NoError(t, e.RegisterIntegration(wi))
				runner := &permissionGuidanceTurn{engine: e, work: wi, effects: effects}
				runners = append(runners, runner)
				ai := agents.New(e.Agents(), e)
				ai.SetAgentTurnRunner(runner)
				require.NoError(t, e.RegisterIntegration(ai))
				require.NoError(t, e.RegisterIntegration(&childPauseFixture{engine: e, t: t, effects: effects}))
				require.NoError(t, e.Functions().Upsert(&memql.Function{Name: "fixtureeffect", Type: memql.FunctionTypeBuiltin, Enabled: true, BuiltinArgs: &memql.BuiltinArgContract{Profile: memql.BuiltinArgProfileOptionalObject, Properties: map[string]string{"label": "string"}}, ArgsSchema: &memql.ArgsSchemaConfig{Fields: []*memql.FunctionArgsField{{Name: "label", Type: "string"}}}, FunctionKind: "builtin", Executor: "integration.childPauseFixture.effect"}))
				loader := automations.NewLoader(automations.LoaderOptions{Functions: e.Functions()})
				tpl := &workTemplate{members: map[string]*automations.Automation{}}
				for _, src := range []string{
					`@template automation section { builtin fixtureeffect(label: "child-prefix"); answer := builtin runAgentTurn(prompt: "Complete the synthetic section"); builtin fixtureeffect(label: "child-tail"); return answer }`,
					`@template automation parent { builtin fixtureeffect(label: "completed-section"); answer := automation section(); builtin fixtureeffect(label: "later-section"); return answer }`,
				} {
					a, err := loader.CompileSource(strings.ReplaceAll(src, "; ", "\n"), "permission-guidance.memql")
					require.NoError(t, err)
					tpl.members[a.Name] = a
				}
				triggers = append(triggers, &workTemplateTrigger{app: &App{engine: e, stepRegistry: steps.NewRegistry(), automationLoader: loader, Logger: e.Logger}, template: tpl})
			}
			write := func(name string, args map[string]any) {
				templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
			}
			query := func(e *memql.MemQLEngine, name string, args map[string]any) []map[string]any {
				t.Helper()
				call, err := parser.RenderCall(name, args)
				require.NoError(t, err)
				r, err := e.Execute(memql.ContextWithFreshRead(person), "query "+call)
				require.NoError(t, err)
				return memql.MaterializeRows(r)
			}
			write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic scoped command correction", "origin": "user"})
			write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "parent", "templateFingerprint": triggers[0].template.members["parent"].DefinitionFingerprint(id.New()), "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
			write("updateWorkRun", map[string]any{"runId": runID, "spent": map[string]any{"modelCalls": 3, "tokensLocal": 700, "retries": 2}})
			decide := func(id, decision string, answer map[string]any) {
				t.Helper()
				call, err := parser.RenderCall("decideApproval", map[string]any{"approvalId": id, "decision": decision, "answer": answer})
				require.NoError(t, err)
				r, err := reader.Execute(person, "builtin "+call)
				require.NoError(t, err)
				require.Equal(t, true, memql.MaterializeRows(r)[0]["runResumed"])
			}
			if mode == "early-answer" {
				runners[0].paused = func(id string) {
					decide(id, "answered", map[string]any{"feedback": permissionCorrection})
					_, ready := (&workRunDispatcher{app: triggers[1].app}).PrepareDispatch(person, work.DispatchRequest{RunId: runID, HumanResumeId: id})
					require.False(t, ready)
				}
			}
			ex := triggers[0].executor()
			defer ex.Close()
			_, err = ex.ExecuteAdopted(ctx, triggers[0].template.members["parent"], automations.RunAdoption{RunId: runID})
			var first *workstate.HumanWait
			require.ErrorAs(t, err, &first)
			load := func() *automations.RunJournal {
				t.Helper()
				j, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
				require.NoError(t, err)
				return j
			}
			journal := load()
			childID := journal.StepStates[journal.FailedStep].ChildRunId
			require.NotEmpty(t, childID)
			checkpoint := query(reader, "workContinuationForOwnerRun", map[string]any{"runId": runID, "stepKey": query(reader, "workApprovalForOwner", map[string]any{"approvalId": first.ApprovalID})[0]["stepKey"]})
			require.Len(t, checkpoint, 1)
			if mode == "fresh-rerun" {
				// A public rejection closes the root but retains its selected
				// section's waiting receipt. Run again authorizes a NEW version;
				// it must not resume that older child's checkpoint.
				decide(first.ApprovalID, "rejected", nil)
				require.Equal(t, "failed", load().Status)
				target := journal.FailedStep
				call, err := parser.RenderCall("rerunStep", map[string]any{"runId": runID, "stepKey": target})
				require.NoError(t, err)
				_, err = writer.Execute(person, "builtin "+call)
				require.NoError(t, err)
				journal = load() // The execution peer knows only durable rows.
				require.Empty(t, journal.HumanResumeId)
				require.NotNil(t, journal.Rerun)
				require.True(t, journal.Rerun.Override.Empty(), "requestedBy alone is an empty override")
				require.Equal(t, 2, journal.Rerun.Versions[target])
				require.Equal(t, 1, journal.StepStates[target].Version)
				require.Equal(t, "waiting", journal.StepStates[target].Status)
				peerCtx, err := workExecutionContext(person, journal, nil, nil)
				require.NoError(t, err)
				resume, opts, code, err := workRerunResumption(journal, nil, triggers[1].template.members["parent"])
				require.NoError(t, err, code)
				peer := triggers[1].executor()
				defer peer.Close()
				_, err = peer.ResumeFrom(peerCtx, resume, triggers[1].template.members["parent"], opts)
				var next *workstate.HumanWait
				require.ErrorAs(t, err, &next, "a fresh public rerun must start its new version")
				require.NotEqual(t, first.ApprovalID, next.ApprovalID)
				current := load()
				require.Equal(t, 2, current.StepStates[target].Version)
				require.NotEqual(t, childID, current.StepStates[target].ChildRunId)
				require.Equal(t, journal.Rerun.RequestId, current.Rerun.RequestId)
				require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 2, "completed-tool": 2}, effects)
				oldChild, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, childID)
				require.NoError(t, err)
				require.Equal(t, "waiting", oldChild.Status, "the old checkpoint is never borrowed")
				require.Len(t, runners[1].requests, 1)
				// The new version has now stopped at its OWN unanswered wait.
				// Re-delivering the rerun must not repeat any of its effects.
				resume, opts, code, err = workRerunResumption(current, nil, triggers[1].template.members["parent"])
				require.NoError(t, err, code)
				_, err = peer.ResumeFrom(peerCtx, resume, triggers[1].template.members["parent"], opts)
				require.ErrorIs(t, err, automations.ErrNonRetryableStep)
				require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 2, "completed-tool": 2}, effects)
				require.Len(t, runners[1].requests, 1)
				finalRun := query(writer, "workRunForOwner", map[string]any{"runId": runID})[0]
				require.Equal(t, float64(3), finalRun["spent"].(map[string]any)["modelCalls"])
				require.Equal(t, float64(2), finalRun["spent"].(map[string]any)["retries"])
				return
			}
			if mode != "early-answer" {
				decide(first.ApprovalID, "answered", map[string]any{"feedback": permissionCorrection})
			}
			journal = load()
			require.Equal(t, "running", journal.Status)
			require.Nil(t, journal.Rerun)
			require.Equal(t, childID, journal.StepStates[journal.FailedStep].ChildRunId)
			require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 1, "completed-tool": 1}, effects)
			originalRequest := permission.Request{OwnerID: owner, AgentID: "fixture-agent", RunID: runID, Tool: "workerHost", Action: "exec", Args: map[string]any{"cmd": "printf original", "cwd": "/fixture/repository", "timeoutSec": 60}}
			grant, err := runners[1].work.Find(ctx, originalRequest)
			require.NoError(t, err)
			require.Nil(t, grant)
			_, err = runners[1].work.Claim(ctx, first.ApprovalID, originalRequest, "fixture-worker", "fixture-machine")
			require.Error(t, err)
			if mode == "refusals" {
				approval := query(reader, "workApprovalForOwner", map[string]any{"approvalId": first.ApprovalID})[0]
				var originalJSON string
				require.NoError(t, db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:approval:"+memql.BareShortId(first.ApprovalID)).Scan(person, &originalJSON))
				var original map[string]any
				require.NoError(t, json.Unmarshal([]byte(originalJSON), &original))
				for _, negative := range []string{"foreign-owner", "pending", "wrong-step", "changed-scope", "legacy-scope", "mixed-answer", "empty-feedback", "wrong-decider"} {
					var changed map[string]any
					require.NoError(t, json.Unmarshal([]byte(originalJSON), &changed))
					switch negative {
					case "foreign-owner":
						changed["ownerUserId"] = "v1:identity:user:other"
					case "pending":
						delete(changed, "decision")
					case "wrong-step":
						changed["stepKey"] = "other"
					case "changed-scope":
						changed["subject"].(map[string]any)["computerPermission"].(map[string]any)["command"] = "changed"
					case "legacy-scope":
						delete(changed["subject"].(map[string]any), "computerPermission")
					case "mixed-answer":
						changed["answer"] = map[string]any{"feedback": permissionCorrection, "permissionMode": "always"}
					case "empty-feedback":
						changed["answer"] = map[string]any{"feedback": " "}
					case "wrong-decider":
						changed["decidedBy"] = "v1:identity:user:other"
					}
					insertWorkRow(t, db, "v1:work:approval:"+memql.BareShortId(first.ApprovalID), "v1:work:approval", time.Now().UTC(), changed)
					refused := triggers[1].executor()
					_, err := refused.ResumeFrom(ctx, journal, triggers[1].template.members["parent"], nil)
					refused.Close()
					require.Error(t, err, negative)
					require.Empty(t, runners[1].requests, negative)
					insertWorkRow(t, db, "v1:work:approval:"+memql.BareShortId(first.ApprovalID), "v1:work:approval", time.Now().UTC(), original)
				}
				require.Equal(t, first.ApprovalID, memql.BareShortId(approval["id"].(string)))
				// Mutate only synthetic rows after the peer has read them. Admission
				// must revalidate the current lineage and immutable checkpoint.
				for _, negative := range []string{"cancelled", "moved-goal", "changed-rerun", "newer-answer", "checkpoint-missing", "checkpoint-content", "checkpoint-rerun"} {
					rowID, concept := "v1:work:run:"+runID, "v1:work:run"
					if strings.HasPrefix(negative, "checkpoint-") {
						rowID, concept = "v1:work:observation:"+memql.BareShortId(checkpoint[0]["id"].(string)), "v1:work:observation"
					}
					var raw string
					require.NoError(t, db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, rowID).Scan(person, &raw))
					var original, changed map[string]any
					require.NoError(t, json.Unmarshal([]byte(raw), &original))
					require.NoError(t, json.Unmarshal([]byte(raw), &changed))
					switch negative {
					case "cancelled":
						changed["cancelRequested"] = true
					case "moved-goal":
						changed["goalId"] = "v1:work:goal:other"
					case "changed-rerun":
						changed["rerun"] = map[string]any{"requestId": "new-execution"}
					case "newer-answer":
						changed["humanResumeId"] = "another-answer"
					case "checkpoint-missing":
						changed["data"].(map[string]any)["continuation"] = false
					case "checkpoint-content":
						changed["data"].(map[string]any)["messages"] = "invalid"
					case "checkpoint-rerun":
						changed["data"].(map[string]any)["rerunRequestId"] = "older-execution"
					}
					insertWorkRow(t, db, rowID, concept, time.Now().UTC(), changed)
					refused := triggers[1].executor()
					_, err := refused.ResumeFrom(ctx, journal, triggers[1].template.members["parent"], nil)
					refused.Close()
					require.Error(t, err, negative)
					require.Empty(t, runners[1].requests, negative)
					insertWorkRow(t, db, rowID, concept, time.Now().UTC(), original)
				}
			}
			resumed := triggers[1].executor()
			defer resumed.Close()
			_, err = resumed.ResumeFrom(ctx, load(), triggers[1].template.members["parent"], nil)
			var corrected *workstate.HumanWait
			require.ErrorAs(t, err, &corrected)
			require.NotEqual(t, first.ApprovalID, corrected.ApprovalID, "a corrected command needs a new exact receipt")
			require.Len(t, runners[1].requests, 1)
			require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 1, "completed-tool": 1}, effects)
			old := query(reader, "workApprovalForOwner", map[string]any{"approvalId": first.ApprovalID})[0]
			require.Equal(t, "answered", old["decision"])
			require.Empty(t, old["permissionUsedAt"])
			decide(corrected.ApprovalID, "approved", map[string]any{"permissionMode": "once"})
			done, err := resumed.ResumeFrom(ctx, load(), triggers[1].template.members["parent"], nil)
			require.NoError(t, err)
			require.Equal(t, "completed", done.Status)
			require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 1, "completed-tool": 1, "corrected-command": 1, "child-tail": 1, "later-section": 1}, effects)
			newReceipt := query(writer, "workApprovalForOwner", map[string]any{"approvalId": corrected.ApprovalID})[0]
			require.NotEmpty(t, newReceipt["permissionUsedAt"])
			finalChild, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), writer, childID)
			require.NoError(t, err)
			require.Equal(t, "succeeded", finalChild.Status)
			finalRun := query(writer, "workRunForOwner", map[string]any{"runId": runID})[0]
			require.Equal(t, float64(3), finalRun["spent"].(map[string]any)["modelCalls"])
			require.Equal(t, float64(2), finalRun["spent"].(map[string]any)["retries"])
		})
	}
}
