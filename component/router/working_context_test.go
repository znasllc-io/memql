package router

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

type contextFleet struct {
	models    []memql.FleetModel
	requests  []memql.FleetCallRequest
	failModel string
}

func (f *contextFleet) Catalog(context.Context, string) ([]memql.FleetModel, error) {
	return advertisedOnEveryMachine(f.models), nil
}
func (f *contextFleet) ModelPreference(context.Context, string) ([]string, error) { return nil, nil }
func (f *contextFleet) Call(_ context.Context, req memql.FleetCallRequest) (memql.FleetCallResult, error) {
	f.requests = append(f.requests, req)
	if req.ModelId == f.failModel {
		return memql.FleetCallResult{}, fmt.Errorf("machine went offline")
	}
	return memql.FleetCallResult{Content: "{}"}, nil
}
func contextRouter(t *testing.T, chain ...string) (*Router, *contextFleet) {
	t.Helper()
	f := &contextFleet{}
	for _, name := range chain {
		m := sizedModel(strings.TrimPrefix(name, "fleet:"), 8000000000, 131072)
		m.Vision = true
		f.models = append(f.models, m)
	}
	p := memql.NewProviderRegistryForTest()
	p.SetFleetInference(f)
	return New(p, memql.NewPolicyRegistryForTest(map[string][]string{"p": chain}), testRules(t, defaultRule("p")), nil, nil), f
}

func TestWorkingContextSurvivesResolutionAndFallback(t *testing.T) {
	r, f := contextRouter(t, "fleet:first", "fleet:second")
	f.failModel = "first"
	p, _, err := r.ResolveChat(ResolveRequest{UserId: "alice", Needs: airoute.Needs{MinContextTokens: 32768}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CallChat(context.Background(), []common.ChatMessage{{Role: "user", Content: "hello"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 2 {
		t.Fatalf("calls=%d", len(f.requests))
	}
	for _, req := range f.requests {
		if req.ContextTokens != 32768 {
			t.Errorf("%s context=%d, want resolved floor 32768", req.ModelId, req.ContextTokens)
		}
	}
}

func TestWorkingContextRecomputedForToolHistoryAndSchemas(t *testing.T) {
	r, f := contextRouter(t, "fleet:local")
	p, _, err := r.ResolveWithTools(ResolveRequest{UserId: "alice", Needs: airoute.Needs{MinContextTokens: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	tools := []common.ToolDefinition{{Name: "search", Description: strings.Repeat("d", 20000), InputSchema: map[string]any{"type": "object", "description": strings.Repeat("s", 20000)}}}
	history := []common.ChatMessage{{Role: "user", Content: "hello"}}
	if _, err := p.CallChatWithTools(context.Background(), history, tools); err != nil {
		t.Fatal(err)
	}
	history = append(history, common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "call", Name: "search", Arguments: strings.Repeat("a", 20000)}}}, common.ChatMessage{Role: "tool", ToolCallId: "call", Content: strings.Repeat("r", 40000)})
	if _, err := p.CallChatWithTools(context.Background(), history, tools); err != nil {
		t.Fatal(err)
	}
	first, second := f.requests[0].ContextTokens, f.requests[1].ContextTokens
	if first < 14096 {
		t.Errorf("tools and schemas were not counted: %d", first)
	}
	if second < first+15000 {
		t.Errorf("growing tool history did not expand context: %d -> %d", first, second)
	}
	if second >= 131072 {
		t.Errorf("allocated full model window rather than working need: %d", second)
	}
}

func TestWorkingContextDirectSurfacesAreIsolatedPerResolution(t *testing.T) {
	r, f := contextRouter(t, "fleet:local")
	req := ResolveRequest{UserId: "alice", Needs: airoute.Needs{MinContextTokens: 32768}}
	high, _, err := r.ResolveStructured(req)
	if err != nil {
		t.Fatal(err)
	}
	req.Needs.MinContextTokens = 8192
	low, _, err := r.ResolveStructured(req)
	if err != nil {
		t.Fatal(err)
	}
	schema := common.StructuredSchema{Name: "large", Schema: json.RawMessage(`{"description":"` + strings.Repeat("s", 50000) + `","type":"object"}`)}
	if _, err := low.CallChatStructured(context.Background(), []common.ChatMessage{{Role: "user", Content: "classify"}}, schema); err != nil {
		t.Fatal(err)
	}
	if got := f.requests[0].ContextTokens; got < 16596 || got >= 32768 {
		t.Errorf("schema working window=%d", got)
	}
	if _, err := high.CallChatStructured(context.Background(), nil, common.StructuredSchema{Schema: json.RawMessage(`{"type":"object"}`)}); err != nil {
		t.Fatal(err)
	}
	if got := f.requests[1].ContextTokens; got != 32768 {
		t.Errorf("independent high floor lost: %d", got)
	}
	vision, _, err := r.ResolveVision(ResolveRequest{UserId: "alice", Needs: airoute.Needs{MinContextTokens: 32768}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vision.CallVision(context.Background(), "describe", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.requests[2].ContextTokens; got != 32768 {
		t.Errorf("vision floor lost: %d", got)
	}
}
