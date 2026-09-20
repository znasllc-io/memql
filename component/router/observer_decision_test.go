package router

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// Actual fleet calls pass through a process-wide guard. Give each regression
// group its own test process so these additional calls neither exhaust the
// budget of unrelated tests nor depend on calls another test already made.
// All production guard settings remain enabled and unchanged.
func observerDecisionChild(t *testing.T) bool {
	t.Helper()
	if os.Getenv("MEMQL_OBSERVER_DECISION_TEST") == t.Name() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), "MEMQL_OBSERVER_DECISION_TEST="+t.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated observer regression: %v\n%s", err, out)
	}
	return true
}

// Capture the real detached ledger write, not the resolution returned before
// the fallback wrapper looks up its provider again. Parse the actual mutation.
type decisionLedger struct{ writes chan string }

func (e *decisionLedger) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	e.writes <- query
	return &memql.ExecuteResult{}, nil
}
func (e *decisionLedger) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case query := <-e.writes:
		expr, err := langparser.ParseExpression(query)
		if err != nil {
			t.Fatal(err)
		}
		call, ok := expr.(*langparser.FunctionCallExpr)
		if !ok || call.Name != "recordRouterCall" {
			t.Fatalf("unexpected ledger invocation: %s", query)
		}
		return call.Args
	case <-time.After(3 * time.Second):
		t.Fatal("observer never wrote its call record")
		return nil
	}
}
func invokeDecisionClient(ctx context.Context, client any, modality airoute.Modality) error {
	messages := []common.ChatMessage{{Role: "user", Content: "hello"}}
	switch modality {
	case airoute.ModalityChat:
		_, err := client.(common.ChatAIProvider).CallChat(ctx, messages)
		return err
	case airoute.ModalityTools:
		_, err := client.(common.ToolCallingChatAIProvider).CallChatWithTools(ctx, messages, nil)
		return err
	case airoute.ModalityStreamingChat:
		ch, err := client.(common.ChatStreamProvider).CallChatStream(ctx, messages)
		if err != nil {
			return err
		}
		for chunk := range ch {
			if chunk.Error != nil {
				return chunk.Error
			}
		}
		return nil
	case airoute.ModalityStreamingTools:
		ch, err := client.(common.ChatStreamWithToolsProvider).CallChatStreamWithTools(ctx, messages, nil)
		if err != nil {
			return err
		}
		for chunk := range ch {
			if chunk.Error != nil {
				return chunk.Error
			}
		}
		return nil
	}
	return fmt.Errorf("unexpected test modality %s", modality)
}
func assertRecordedDecision(t *testing.T, args map[string]any, decision airoute.Decision) {
	t.Helper()
	for key, want := range map[string]any{
		"level": string(decision.Level), "requestedLevel": string(decision.RequestedLevel), "servedLevel": string(decision.ServedLevel),
		"degraded": decision.Degraded, "rule": decision.Rule, "policy": decision.Policy, "door": decision.Door,
		"machineOwnerUserId": decision.MachineOwnerUserId,
	} {
		if !reflect.DeepEqual(args[key], want) {
			t.Errorf("persisted %s=%#v, want %#v", key, args[key], want)
		}
	}
	for _, key := range []string{"level", "requestedLevel", "servedLevel"} {
		value, _ := args[key].(string)
		if !airoute.Level(value).Valid() {
			t.Errorf("%s=%q would fail the router:call enum", key, value)
		}
	}
	if args["door"] != DoorLocal && args["door"] != DoorApp && args["door"] != DoorFederation && args["door"] != DoorSession {
		t.Errorf("door=%q would fail the router:call enum", args["door"])
	}
}
func TestObservedExplicitFleetCallKeepsResolutionDecision(t *testing.T) {
	if observerDecisionChild(t) {
		return
	}
	for _, mod := range []airoute.Modality{airoute.ModalityChat, airoute.ModalityTools, airoute.ModalityStreamingChat, airoute.ModalityStreamingTools} {
		t.Run(string(mod), func(t *testing.T) {
			r := authenticatedRouter(t, &authenticatedFleet{})
			ledger := &decisionLedger{writes: make(chan string, 4)}
			r.engine = ledger
			ctx := common.ContextWithFleetRegistration(auth.ContextWithUserActor(context.Background(), "alice"), "v1:worker:registration:my-machine")
			resolved, err := r.ResolveFor(ctx, ResolveRequest{Level: airoute.LevelStrong, Modality: mod, ExplicitProvider: "fleet:qwen3.8:27b", PromptName: "ask", RequestId: "explicit-fleet", Touches: []string{"v1:library:file"}, Needs: airoute.Needs{MinContextTokens: 8192}})
			if err != nil {
				t.Fatal(err)
			}
			if err = invokeDecisionClient(ctx, resolved.Client, mod); err != nil {
				t.Fatal(err)
			}
			args := ledger.next(t)
			assertRecordedDecision(t, args, resolved.Resolution.Decision)
			for key, want := range map[string]any{"userId": "alice", "promptName": "ask", "providerName": "fleet:qwen3.8:27b", "outcome": "ok", "requestId": "explicit-fleet"} {
				if args[key] != want {
					t.Errorf("%s=%v, want %v", key, args[key], want)
				}
			}
			// JSON-shaped arguments survive the real renderer/parser in their native
			// numeric/list forms; their textual rendering also proves they were copied.
			expected, _ := langparser.RenderCall("check", map[string]any{"considered": consideredArgs(resolved.Resolution.Decision.Considered), "touches": resolved.Resolution.Decision.Touches, "minContextTokens": 8192})
			actual, _ := langparser.RenderCall("check", map[string]any{"considered": args["considered"], "touches": args["touches"], "minContextTokens": args["minContextTokens"]})
			if actual != expected {
				t.Errorf("decision report lost:\n%s\nwant %s", actual, expected)
			}
		})
	}
}
func TestObservedCallKeepsRuleOverrideAndDegradation(t *testing.T) {
	if observerDecisionChild(t) {
		return
	}
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprint("override=", override), func(t *testing.T) {
			level := airoute.LevelStrong
			var rule *memql.RuleConfig
			if override {
				level = airoute.LevelFast
				rule = &memql.RuleConfig{Name: "raiseForDesign", When: when("level", "fast"), Level: airoute.LevelReasoning, Policy: "liveChain", Precedence: 100, Locked: true}
			}
			r, _ := levelRouter(t, level, rule)
			ledger := &decisionLedger{writes: make(chan string, 4)}
			r.engine = ledger
			client, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice", Level: level})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.CallChat(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			args := ledger.next(t)
			assertRecordedDecision(t, args, resolved.Decision)
			if args["policyName"] != resolved.PolicyName {
				t.Errorf("lost policy attribution: %v", args["policyName"])
			}
		})
	}
}

type decisionRefusingFleet struct{ authenticatedFleet }

func (*decisionRefusingFleet) Call(context.Context, memql.FleetCallRequest) (memql.FleetCallResult, error) {
	return memql.FleetCallResult{}, &memql.FleetUnavailable{ModelId: "qwen3.8:27b"}
}

type decisionCloud struct{}

func (*decisionCloud) Call(context.Context, string) (any, error) { return "cloud", nil }
func (*decisionCloud) CallChat(context.Context, []common.ChatMessage) (string, error) {
	return "cloud", nil
}
func (*decisionCloud) CallChatWithTools(context.Context, []common.ChatMessage, []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	return &common.ToolCallingChatResult{AssistantText: "cloud"}, nil
}
func (*decisionCloud) CallChatStream(context.Context, []common.ChatMessage) (<-chan common.StreamChunk, error) {
	ch := make(chan common.StreamChunk, 1)
	ch <- common.StreamChunk{Content: "cloud", Done: true}
	close(ch)
	return ch, nil
}
func (*decisionCloud) CallChatStreamWithTools(context.Context, []common.ChatMessage, []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	ch := make(chan common.StreamToolChunk, 1)
	ch <- common.StreamToolChunk{Content: "cloud", Done: true}
	close(ch)
	return ch, nil
}
func TestObservedFallbackKeepsDecisionAndActualAttemptDoor(t *testing.T) {
	if observerDecisionChild(t) {
		return
	}
	for _, mod := range []airoute.Modality{airoute.ModalityChat, airoute.ModalityTools, airoute.ModalityStreamingChat, airoute.ModalityStreamingTools} {
		t.Run(string(mod), func(t *testing.T) {
			providers := memql.NewProviderRegistryForTest()
			providers.SetFleetInference(&decisionRefusingFleet{})
			providers.RegisterWithParamsForTest("cloud", "OpenAI", "cloud-model", map[string]any{"contextWindow": 131072}, &decisionCloud{})
			ledger := &decisionLedger{writes: make(chan string, 4)}
			r := New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"p": {"fleet:strongest", "cloud"}}), testRules(t, defaultRule("p")), ledger, nil)
			ctx := auth.ContextWithUserActor(context.Background(), "alice")
			resolved, err := r.ResolveFor(ctx, ResolveRequest{Level: airoute.LevelStrong, Modality: mod, RequestId: "fallback-turn"})
			if err != nil {
				t.Fatal(err)
			}
			if err = invokeDecisionClient(ctx, resolved.Client, mod); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for i := 0; i < 3; i++ {
				args := ledger.next(t)
				decision := resolved.Resolution.Decision
				provider := args["providerName"].(string)
				if provider == "cloud" {
					decision.Door = DoorFederation
				}
				assertRecordedDecision(t, args, decision)
				if args["policyName"] != "p" || args["requestId"] != "fallback-turn" {
					t.Errorf("lost fallback attribution: %v", args)
				}
				seen[provider+"/"+args["outcome"].(string)] = true
			}
			for _, key := range []string{"fleet:qwen3.8:27b/error", "fleet:qwen3.8:27b/fallback_used", "cloud/ok"} {
				if !seen[key] {
					t.Errorf("missing attempt %s: %v", key, seen)
				}
			}
		})
	}
}
