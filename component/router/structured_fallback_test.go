package router

// structured_fallback_test.go -- a structured call walks the route and writes
// its decision record (synthesis2 fix 8; the planner/app-source design's
// precondition 2).
//
// Before this, a structured call resolved the route and then called the WINNER
// RAW: no fallback wrapper and no observer. So a preferred source that failed
// at call time failed the whole call even when the route named another source
// behind it, and a structured call -- every classifier, the planner's triage,
// the compile pass -- wrote no v1:router:call row at all, which is why the
// sources it passed over (an app source this node cannot open, say) were
// never readable anywhere.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// servedReporter is what the engine reads back off a routed client: which
// source actually answered, after any fallback, and which ledger row says so.
type servedReporter interface {
	LastServed() (airoute.Served, bool)
}

// scriptedSource answers or fails on every surface these tests call, and
// COUNTS, for the reason countingCloud does: "no vendor was reached" is a
// counter that stayed at zero.
//
// Registered under a `fleet:` name it is a LOCAL source to the router -- the
// door is the name's -- without a real fleet call. That matters rather than
// being convenience: a fleet call passes the process-wide LLM guard, whose rate
// ceiling every test in this package shares, and a handful of extra ones is
// enough to trip it under the tests that need real fleet requests.
type scriptedSource struct {
	answer string
	fail   error
	calls  int
}

func (s *scriptedSource) reply() (string, error) {
	s.calls++
	if s.fail != nil {
		return "", s.fail
	}
	return s.answer, nil
}
func (s *scriptedSource) Call(context.Context, string) (any, error) {
	text, err := s.reply()
	if err != nil {
		return nil, err
	}
	return text, nil
}
func (s *scriptedSource) CallChat(context.Context, []common.ChatMessage) (string, error) {
	return s.reply()
}
func (s *scriptedSource) CallChatStructured(context.Context, []common.ChatMessage, common.StructuredSchema) (string, error) {
	return s.reply()
}

// scriptedRouter builds a router whose one rule names `chain`, over these
// sources and nothing else.
func scriptedRouter(t *testing.T, chain []string, sources map[string]*scriptedSource) *Router {
	t.Helper()
	providers := memql.NewProviderRegistryForTest()
	for name, source := range sources {
		providers.RegisterForTest(name, "Scripted", strings.TrimPrefix(name, memql.FleetReferencePrefix), source)
	}
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"route": chain})
	return New(providers, policies, testRules(t, defaultRule("route")), nil, nil)
}

var errMachineWentOffline = errors.New("machine went offline")

var probeSchema = common.StructuredSchema{Name: "probe", Schema: json.RawMessage(`{"type":"object"}`), Strict: true}

func structuredRequest() ResolveRequest {
	return ResolveRequest{UserId: "alice", Level: airoute.LevelFast, Modality: airoute.ModalityStructured, PromptName: "goalComplexityTriage"}
}

// A STRUCTURED CALL WHOSE PREFERRED SOURCE FAILS IS SERVED BY THE NEXT ONE.
// The route names two local models; the first goes offline between the
// resolution and the call. Before the wrapper, the call failed with the first
// model's error and the second was never asked.
func TestAStructuredCallWhosePreferredSourceFailsIsServedByTheNextSource(t *testing.T) {
	first := &scriptedSource{fail: errMachineWentOffline}
	second := &scriptedSource{answer: `{"complexity":"trivial"}`}
	r := scriptedRouter(t, []string{"fleet:first", "fleet:second"},
		map[string]*scriptedSource{"fleet:first": first, "fleet:second": second})

	resolved, err := r.ResolveFor(context.Background(), structuredRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Resolution.ProviderName != "fleet:first" {
		t.Fatalf("resolved %q, want the preferred source", resolved.Resolution.ProviderName)
	}
	client, ok := resolved.Client.(common.ChatStructuredProvider)
	if !ok {
		t.Fatalf("the structured client %T does not serve structured output", resolved.Client)
	}
	text, err := client.CallChatStructured(context.Background(), []common.ChatMessage{{Role: "user", Content: "classify"}}, probeSchema)
	if err != nil {
		t.Fatalf("the preferred source failed and the route named another; the call must be served by it: %v", err)
	}
	if text != `{"complexity":"trivial"}` {
		t.Fatalf("text=%q, want the second source's answer", text)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("the route was not walked in order: first=%d second=%d", first.calls, second.calls)
	}

	reporter, ok := resolved.Client.(servedReporter)
	if !ok {
		t.Fatalf("the structured client %T cannot say which source served", resolved.Client)
	}
	served, ok := reporter.LastServed()
	if !ok {
		t.Fatal("a served call reported no source")
	}
	if served.ProviderName != "fleet:second" || served.Decision.Door != airoute.DoorLocal {
		t.Fatalf("served=%+v, want fleet:second through the local door", served)
	}
	if !strings.HasPrefix(served.RouterCallId, RouterCallConcept+":") {
		t.Fatalf("routerCallId=%q, want a v1:router:call row id", served.RouterCallId)
	}
	last := served.Decision.Considered[len(served.Decision.Considered)-1]
	if last.Entry != "fleet:second" || last.Reason != "selected from fallback chain" {
		t.Fatalf("the served decision does not say the fallback took it: %+v", served.Decision.Considered)
	}
}

// THE STRUCTURED ROW IS WRITTEN, and it carries the decision -- including the
// app source a planner node passed over, in words that say why.
func TestAStructuredCallWritesALedgerRowNamingTheSourceThatServed(t *testing.T) {
	t.Setenv("MEMQL_NODE_TYPE", "planner")
	// NO SetAppInference: this is a planner node. App sessions ride the
	// WorkerService stream, which terminates on the agent.
	r := scriptedRouter(t, []string{"app:claude-code", "fleet:qwen3.5:4b"},
		map[string]*scriptedSource{"fleet:qwen3.5:4b": {answer: `{"complexity":"trivial"}`}})
	ledger := &countingLedger{writes: make(chan string, 8)}
	r.engine = ledger

	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "alice", Role: auth.RoleWriter})
	resolved, err := r.ResolveFor(ctx, structuredRequest())
	if err != nil {
		t.Fatal(err)
	}
	client := resolved.Client.(common.ChatStructuredProvider)
	if _, err := client.CallChatStructured(ctx, []common.ChatMessage{{Role: "user", Content: "classify"}}, probeSchema); err != nil {
		t.Fatal(err)
	}

	args := ledger.args(t)
	if args["providerName"] != "fleet:qwen3.5:4b" || args["door"] != airoute.DoorLocal || args["outcome"] != "ok" {
		t.Fatalf("row = provider %v door %v outcome %v, want the local model serving", args["providerName"], args["door"], args["outcome"])
	}
	if args["promptName"] != "goalComplexityTriage" || args["level"] != string(airoute.LevelFast) {
		t.Fatalf("the row lost the call's prompt or level: %v / %v", args["promptName"], args["level"])
	}
	if args["streaming"] != false {
		t.Fatalf("streaming=%v on a structured row", args["streaming"])
	}
	considered, _ := args["considered"].([]any)
	var sawApp, sawFleet bool
	for _, raw := range considered {
		entry, _ := raw.(map[string]any)
		switch entry["entry"] {
		case "app:claude-code":
			sawApp = true
			reason := fmt.Sprint(entry["reason"])
			if entry["door"] != airoute.DoorApp || !strings.Contains(reason, "app sources run on the agent holding the machine; this planner node cannot open one") {
				t.Errorf("app source line = %v, want it skipped with the node-type reason", entry)
			}
		case "fleet:qwen3.5:4b":
			sawFleet = true
			if entry["reason"] != "selected" {
				t.Errorf("fleet line = %v, want selected", entry)
			}
		}
	}
	if !sawApp || !sawFleet {
		t.Fatalf("considered=%v, want the skipped app source and the serving local model", considered)
	}

	served, ok := resolved.Client.(servedReporter).LastServed()
	if !ok {
		t.Fatal("no served report")
	}
	if served.RouterCallId != RouterCallConcept+":"+fmt.Sprint(args["callId"]) {
		t.Fatalf("served routerCallId %q does not name the row the ledger wrote (callId %v)", served.RouterCallId, args["callId"])
	}
}

// THE COST CEILING IS ASKED AGAIN BEFORE THE WRAPPER HOPS TO A VENDOR. The
// resolution asked it once, when a local source won; that source then failed at
// call time, and falling back to the vendor behind it is exactly the paid hop
// the ceiling governs.
func TestAStructuredFallbackAsksTheCeilingBeforeItReachesAVendor(t *testing.T) {
	build := func(ceilingReached bool) (*Router, *scriptedSource) {
		cloud := &scriptedSource{answer: `{"from":"cloud"}`}
		r := scriptedRouter(t, []string{"fleet:qwen3.5:4b", "chat54Mini"}, map[string]*scriptedSource{
			"fleet:qwen3.5:4b": {fail: errMachineWentOffline},
			"chat54Mini":       cloud,
		})
		r.ceilingCheck = func(context.Context) (string, bool) {
			if ceilingReached {
				return "MEMQL_LLM_MAX_TOTAL_CALLS ceiling reached", true
			}
			return "", false
		}
		return r, cloud
	}

	// Control: under the ceiling the hop happens and the vendor serves.
	r, cloud := build(false)
	resolved, err := r.ResolveFor(context.Background(), structuredRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolved.Client.(common.ChatStructuredProvider).CallChatStructured(context.Background(), nil, probeSchema); err != nil {
		t.Fatalf("under the ceiling the vendor behind a failed local source must serve: %v", err)
	}
	if cloud.calls != 1 {
		t.Fatalf("cloud calls = %d, want 1", cloud.calls)
	}

	r, cloud = build(true)
	resolved, err = r.ResolveFor(context.Background(), structuredRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolved.Client.(common.ChatStructuredProvider).CallChatStructured(context.Background(), nil, probeSchema)
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) || refusal.Code != work.RefusalCeilingReached {
		t.Fatalf("err = %v, want the typed ceiling refusal", err)
	}
	if !strings.Contains(refusal.CeilingReason, "MEMQL_LLM_MAX_TOTAL_CALLS") {
		t.Errorf("the refusal lost the guard's own sentence: %q", refusal.CeilingReason)
	}
	if cloud.calls != 0 {
		t.Fatalf("the vendor was called %d times past the ceiling", cloud.calls)
	}
}

// THE PROMPT FORM WALKS THE ROUTE TOO. The DSL's ai(...) calls a provider's
// bare prompt surface (a string in, any value out), and it called the
// winner's registry client directly -- so it neither fell back nor wrote a row.
func TestThePromptFormFallsBackAndReportsTheSourceThatServed(t *testing.T) {
	r := scriptedRouter(t, []string{"fleet:first", "fleet:second"}, map[string]*scriptedSource{
		"fleet:first":  {fail: errMachineWentOffline},
		"fleet:second": {answer: "hello"},
	})
	ledger := &countingLedger{writes: make(chan string, 8)}
	r.engine = ledger

	resolved, err := r.ResolveFor(context.Background(), ResolveRequest{UserId: "alice", Level: airoute.LevelFast, Modality: airoute.ModalityChat})
	if err != nil {
		t.Fatal(err)
	}
	prompt, ok := resolved.Client.(memql.AIProvider)
	if !ok {
		t.Fatalf("the chat client %T has no prompt surface", resolved.Client)
	}
	got, err := prompt.Call(context.Background(), "say hello")
	if err != nil {
		t.Fatalf("the prompt form must be served by the second source: %v", err)
	}
	if got != "hello" {
		t.Fatalf("got %v, want the second source's answer", got)
	}
	served, ok := resolved.Client.(servedReporter).LastServed()
	if !ok || served.ProviderName != "fleet:second" {
		t.Fatalf("served=%+v, want fleet:second", served)
	}
	// Three rows: the first source's error, its fallback_used, and the serve.
	outcomes := map[string]string{}
	for i := 0; i < 3; i++ {
		args := ledger.args(t)
		outcomes[fmt.Sprint(args["outcome"])] = fmt.Sprint(args["providerName"])
	}
	if outcomes["ok"] != "fleet:second" || outcomes["error"] != "fleet:first" || outcomes["fallback_used"] != "fleet:first" {
		t.Fatalf("ledger outcomes = %v", outcomes)
	}
}
