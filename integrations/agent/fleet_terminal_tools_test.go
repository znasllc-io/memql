package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

type terminalToolFleet struct{ workContextFleet }

func (f *terminalToolFleet) Call(_ context.Context, req memql.FleetCallRequest) (memql.FleetCallResult, error) {
	f.calls++
	if f.calls == 1 {
		return memql.FleetCallResult{ToolCalls: []common.ToolCall{{ID: "save", Name: "composeFile", Arguments: `{"name":"Heroes","format":"markdown","statement":"Rank ten heroes"}`}}}, nil
	}
	if f.calls != 2 || len(req.Messages) == 0 {
		return memql.FleetCallResult{}, fmt.Errorf("unexpected fleet continuation")
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "tool" || last.Name != "composeFile" || !strings.Contains(last.Content, "saved-file") {
		return memql.FleetCallResult{}, fmt.Errorf("continuation lost the saved file receipt: %+v", last)
	}
	req.OnDelta("Saved the file")
	return memql.FleetCallResult{Content: "Saved the file"}, nil
}

type fileReceiptExecutor struct{ calls int }

func (e *fileReceiptExecutor) ExecuteToolByName(_ context.Context, name string, args map[string]any) (string, error) {
	if name != "composeFile" || args["name"] != "Heroes" {
		return "", fmt.Errorf("unexpected file request")
	}
	e.calls++
	return `{"outputFileId":"saved-file"}`, nil
}

func (*fileReceiptExecutor) Execute(context.Context, string) (any, error) { return nil, nil }

// Fleet providers deliver their assembled tool calls on the terminal chunk.
// Exercise the real provider adapter and agent loop together: a clean stream
// ending must execute the file request before reporting a completed answer.
func TestFleetTerminalToolCallsExecuteBeforeCompletion(t *testing.T) {
	owner := "v1:identity:user:terminal-tool-owner"
	ctx, err := auth.ContextWithPersistedOwner(context.Background(), owner, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "adopted-run", GoalId: "goal", OwnerUserId: owner})
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	fleet := &terminalToolFleet{workContextFleet: workContextFleet{owner: owner}}
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(fleet)
	rules := memql.NewRuleRegistry()
	if err := rules.Register(&memql.RuleConfig{Name: memql.DefaultRuleName, When: memql.RuleWhen{Present: map[string]bool{}}, Policy: "local", Locked: true, OnUnavailable: memql.OnUnavailableDegrade, SourceFile: "dsl/rules/rules.memql"}); err != nil {
		t.Fatal(err)
	}
	if err := rules.Finalize(); err != nil {
		t.Fatal(err)
	}
	rtr := router.New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"local": {"fleet:strongest"}}), rules, nil, nil)
	selection, err := rtr.ResolveFor(ctx, router.ResolveRequest{Modality: airoute.ModalityStreamingTools, Needs: airoute.Needs{Tools: true}})
	if err != nil {
		t.Fatal(err)
	}
	executor := &fileReceiptExecutor{}
	r := testReplier()
	r.stamper = newToolRecorder(executor, r.logger)
	sink := &captureSink{}
	result, err := r.runStreamingToolLoop(ctx, selection.Client.(common.ChatStreamWithToolsProvider), []common.ChatMessage{{Role: "user", Content: "Save the heroes file"}}, []common.ToolDefinition{{Name: "composeFile"}}, sink, time.Now(), "terminal-tools", turnContext{IsWorkExecution: true, OwnerUserId: owner})
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls != 1 || fleet.calls != 2 || len(result.ToolCalls) != 1 || result.FinalText != "Saved the file" || sink.toolResults != 1 {
		t.Fatalf("terminal file call was lost: executions=%d fleetCalls=%d result=%+v sink=%+v", executor.calls, fleet.calls, result, sink)
	}
}
