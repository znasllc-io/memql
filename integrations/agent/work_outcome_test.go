package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

type outcomeProvider struct {
	steps    [][]common.ToolCall
	calls    int
	messages []common.ChatMessage
}

func (p *outcomeProvider) CallChatWithTools(_ context.Context, messages []common.ChatMessage, _ []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	p.messages = append([]common.ChatMessage(nil), messages...)
	if p.calls >= len(p.steps) {
		return nil, fmt.Errorf("unexpected model call")
	}
	calls := p.steps[p.calls]
	p.calls++
	return &common.ToolCallingChatResult{ToolCalls: calls}, nil
}

func (p *outcomeProvider) CallChatStreamWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	result, err := p.CallChatWithTools(ctx, messages, tools)
	if err != nil {
		return nil, err
	}
	ch := make(chan common.StreamToolChunk, 2)
	var deltas []common.ToolCallDelta
	for i, call := range result.ToolCalls {
		deltas = append(deltas, common.ToolCallDelta{Index: i, ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	ch <- common.StreamToolChunk{ToolCalls: deltas}
	ch <- common.StreamToolChunk{Done: true}
	close(ch)
	return ch, nil
}

type questionExecutor struct {
	calls int
	args  map[string]any
}

func (*questionExecutor) Execute(context.Context, string) (any, error) { return nil, nil }
func (e *questionExecutor) ExecuteToolByName(_ context.Context, name string, args map[string]any) (string, error) {
	e.calls++
	e.args = args
	if name != "requestUserFeedback" {
		return "", fmt.Errorf("unexpected side effect: %s", name)
	}
	return `{"data":[{"receipt":{"id":"receipt","payload":{"status":"awaiting_user","approvalId":"question-on-another-replica"}}}]}`, nil
}

func outcomeCall(args string) []common.ToolCall {
	return []common.ToolCall{{ID: "outcome", Name: RespondToUserToolName, Arguments: args}}
}

// The execution replica starts with persisted owner/run authority, without the
// originating Ask stream. Both transport lanes must create the same durable
// question and stop before another model call or side effect.
func TestOwnedWorkOutcomePausesBothLanes(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, question := range []string{
			`{"text":"Which supplier should this report cover?","kind":"text"}`,
			`{"text":"Which region?","kind":"choice","options":[{"label":"West","value":"west"},{"label":"East","value":"east"}]}`,
			`{"text":"Which formats?","kind":"multi","options":[{"label":"PDF","value":"pdf"}]}`,
		} {
			t.Run(fmt.Sprintf("stream=%v/%s", streaming, question), func(t *testing.T) {
				ctx, err := auth.ContextWithPersistedOwner(context.Background(), "owner", nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "adopted-run", GoalId: "goal", OwnerUserId: "owner"})
				p := &outcomeProvider{steps: [][]common.ToolCall{outcomeCall(`{"status":"needs_input","question":` + question + `,"ownerUserId":"attacker","runId":"other"}`)}}
				executor := &questionExecutor{}
				saved := &workPromptEngine{}
				r := testReplier()
				r.engine = saved
				r.stamper = newToolRecorder(executor, r.logger)
				tools := []common.ToolDefinition{workResponseToolDefinition()}
				turn := turnContext{AgentId: "assistant", RunId: "adopted-run", OwnerUserId: "owner", IsWorkExecution: true}
				if streaming {
					_, err = r.runStreamingToolLoop(ctx, p, nil, tools, &captureSink{}, time.Now(), "outcome", turn)
				} else {
					_, err = r.runNonStreamingToolLoop(ctx, p, nil, nil, tools, &captureSink{}, time.Now(), "outcome", turn)
				}
				var wait *work.HumanWait
				if !errors.As(err, &wait) || wait.ApprovalID != "question-on-another-replica" {
					t.Fatalf("did not suspend: %v", err)
				}
				if p.calls != 1 || executor.calls != 1 || executor.args["ownerUserId"] != "owner" || executor.args["runId"] != "adopted-run" {
					t.Fatalf("invalid dispatch: calls=%d args=%v", p.calls, executor.args)
				}
				if len(saved.savedContinuation) != 2 || saved.savedContinuation[0].ToolCalls[0].Name != "requestUserFeedback" || saved.savedContinuation[1].ToolCallId != "outcome" {
					t.Fatalf("unresumable checkpoint: %+v", saved.savedContinuation)
				}
			})
		}
	}
}

func TestOwnedWorkOutcomeRejectsBeforeEffectsAndRepairsBoundedly(t *testing.T) {
	bad := []struct {
		name  string
		calls []common.ToolCall
	}{
		{"missing outcome", outcomeCall(`{"response":"tell me later"}`)},
		{"malformed", outcomeCall(`{"status":`)},
		{"empty response", outcomeCall(`{"status":"complete","response":" "}`)},
		{"question marked complete", outcomeCall(`{"status":"complete","response":"done","question":{"text":"Which?","kind":"text"}}`)},
		{"empty question", outcomeCall(`{"status":"needs_input","question":{"text":" ","kind":"text"}}`)},
		{"missing choices", outcomeCall(`{"status":"needs_input","question":{"text":"Which?","kind":"choice"}}`)},
		{"duplicate choices", outcomeCall(`{"status":"needs_input","question":{"text":"Which?","kind":"choice","options":[{"label":"A","value":"x"},{"label":"B","value":"x"}]}}`)},
		{"text only", nil},
		{"sibling effect", append(outcomeCall(`{"status":"complete","response":"done"}`), common.ToolCall{ID: "effect", Name: "composeFile", Arguments: `{}`})},
	}
	for _, streaming := range []bool{false, true} {
		for _, tt := range bad {
			t.Run(fmt.Sprintf("stream=%v/%s", streaming, tt.name), func(t *testing.T) {
				ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
				for _, repair := range []bool{false, true} {
					second := tt.calls
					if repair {
						second = outcomeCall(`{"status":"complete","response":"The supplier is Example, from the provided invoice."}`)
					}
					p := &outcomeProvider{steps: [][]common.ToolCall{tt.calls, second}}
					r := testReplier()
					executor := &questionExecutor{}
					r.stamper = newToolRecorder(executor, r.logger)
					var result *TurnResult
					var err error
					tools := []common.ToolDefinition{workResponseToolDefinition()}
					if streaming {
						result, err = r.runStreamingToolLoop(ctx, p, nil, tools, &captureSink{}, time.Now(), "repair", turnContext{})
					} else {
						result, err = r.runNonStreamingToolLoop(ctx, p, nil, nil, tools, &captureSink{}, time.Now(), "repair", turnContext{})
					}
					if p.calls != 2 || executor.calls != 0 {
						t.Fatalf("unbounded repair or side effect: calls=%d effects=%d", p.calls, executor.calls)
					}
					if repair && (err != nil || result == nil || !strings.Contains(result.FinalText, "invoice")) {
						t.Fatalf("repair failed: result=%+v err=%v", result, err)
					}
					if !repair && (err == nil || !strings.Contains(err.Error(), "bounded repair")) {
						t.Fatalf("invalid output accepted: %v", err)
					}
					last := p.messages[len(p.messages)-1]
					if !strings.Contains(last.Content, `"executed":false`) {
						t.Fatalf("model did not receive repair feedback: %+v", last)
					}
				}
			})
		}
	}
}

func TestWorkOutcomeDoesNotGrantAuthorityOrConstrainSpecialists(t *testing.T) {
	tools := []common.ToolDefinition{workResponseToolDefinition()}
	for _, ctx := range []context.Context{context.Background(), common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "stranger"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})} {
		if requiresWorkOutcome(ctx, tools) {
			t.Fatal("unowned turn acquired work contract")
		}
	}
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
	if requiresWorkOutcome(ctx, ScopeToolDefinitionsForRole(RoleSpecialist, tools)) {
		t.Fatal("specialist required to use forbidden human response tool")
	}
}
