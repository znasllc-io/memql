package router

// The `session` door, end to end through the chain walk (epic memql#5391,
// task memql#5392, design D7).
//
// The problem this fixes has one sentence: on a cluster whose only open door
// is a signed-in Claude Code, a tool-needing call walked past the app door and
// Ask stayed dark. These tests are what say it no longer does, and what say the
// three ways the fix could be wrong -- falling through to a vendor anyway,
// swallowing a stepless call, and quietly admitting embeddings.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// recordingDelegate stands in for the agent node's app-session delegate.
type recordingDelegate struct {
	got  memql.AppSessionHandover
	runs int
}

func (d *recordingDelegate) RunStep(_ context.Context, h memql.AppSessionHandover) (memql.AppSessionOutcome, error) {
	d.runs++
	d.got = h
	return memql.AppSessionOutcome{
		Content:          "the app did the work",
		SessionId:        "v1:worker:appSession:s1",
		ChildRunId:       "v1:work:run:child",
		Model:            "claude-opus-5",
		Effort:           "high",
		ExecutionSurface: "cockpit-app:" + h.AppId,
	}, nil
}

// toolCloud is a vendor entry that CAN serve a tool turn.
//
// countingCloud cannot, and using it here would make every one of these tests
// pass for the wrong reason: "the chain did not reach the vendor" and "the
// vendor could not have served it anyway" are different facts, and only the
// first is what the session door is for.
type toolCloud struct{ calls int }

func (c *toolCloud) Call(context.Context, string) (any, error) { return "cloud", nil }
func (c *toolCloud) CallChat(context.Context, []common.ChatMessage) (string, error) {
	c.calls++
	return "cloud answer", nil
}
func (c *toolCloud) CallChatWithTools(context.Context, []common.ChatMessage, []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	c.calls++
	return &common.ToolCallingChatResult{AssistantText: "cloud answer"}, nil
}
func (c *toolCloud) CallChatStreamWithTools(context.Context, []common.ChatMessage, []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	c.calls++
	ch := make(chan common.StreamToolChunk, 1)
	ch <- common.StreamToolChunk{Content: "cloud answer", Done: true}
	close(ch)
	return ch, nil
}

// sessionRouter is the three-step default shape with an OPEN app door and a
// vendor entry behind it -- the arrangement in which "passed over" used to mean
// "spent money".
func sessionRouter(t *testing.T, appDoors []memql.AppDoor) (*Router, *toolCloud, *recordingDelegate) {
	t.Helper()
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{})
	providers.SetAppInference(&stubAppInference{doors: appDoors})
	delegate := &recordingDelegate{}
	providers.SetAppSessionDelegate(delegate)
	cloud := &toolCloud{}
	// WITH a declared context window: this fixture's whole job is to be the
	// paid entry the session door must not fall through to, and an entry the
	// context floor rejects for its own reasons could not prove that.
	providers.RegisterWithParamsForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet",
		map[string]any{"contextWindow": 200000}, cloud)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"defaultChain": {memql.AppReferencePrefix + "claude-code", "streamClaudeSonnet"},
	})
	return New(providers, policies, testRules(t, defaultRule("defaultChain")), nil, nil), cloud, delegate
}

func toolRequest() ResolveRequest {
	return ResolveRequest{
		Level:    airoute.LevelStrong,
		Modality: airoute.ModalityTools,
		UserId:   "alice",
		RunId:    "v1:work:run:r1",
		StepId:   "v1:work:step:s1",
		Needs:    airoute.Needs{Tools: true, MinContextTokens: 8000},
	}
}

// Issue memql#5392's first acceptance, in its own words: a tool-needing
// request resolving to app:claude-code yields a SESSION winner and never a
// federation entry when one is behind it in the chain.
func TestToolNeedingCallOnAnAppDoorYieldsASessionWinner(t *testing.T) {
	r, cloud, delegate := sessionRouter(t, []memql.AppDoor{openApp("claude-code")})

	client, resolved, err := r.resolveWithTools(context.Background(), toolRequest())
	if err != nil {
		t.Fatalf("resolveWithTools: %v", err)
	}
	if resolved.Decision.Door != airoute.DoorSession {
		t.Fatalf("Door = %q, want %q", resolved.Decision.Door, airoute.DoorSession)
	}
	if resolved.ProviderName != "app:claude-code" {
		t.Fatalf("ProviderName = %q, want the app entry the policy named", resolved.ProviderName)
	}
	if resolved.Client == nil {
		t.Fatalf("a session winner must carry the session client")
	}

	// NEVER A FEDERATION ENTRY BEHIND IT. The whole point of the session door
	// is that a signed-in app is not passed over on a tool turn; a chain that
	// still fell through to a vendor would spend money on exactly the cluster
	// this epic exists for. The chain is what the fallback wrapper walks, so
	// its contents ARE the reachable set.
	for _, name := range resolved.Chain {
		if doorFor(name) == DoorFederation {
			t.Fatalf("the session winner's chain carries a federation entry %q: %v", name, resolved.Chain)
		}
	}

	// And the call itself goes to the app, not to the vendor.
	res, err := client.CallChatWithTools(context.Background(),
		[]common.ChatMessage{{Role: "user", Content: "ship it"}},
		[]common.ToolDefinition{{Name: "workbenchHost"}})
	if err != nil {
		t.Fatalf("CallChatWithTools: %v", err)
	}
	if res.AssistantText != "the app did the work" {
		t.Fatalf("AssistantText = %q", res.AssistantText)
	}
	if delegate.runs != 1 {
		t.Fatalf("the delegate ran %d times, want exactly one session", delegate.runs)
	}
	if delegate.got.StepId != "v1:work:step:s1" {
		t.Fatalf("the handover must carry the step, got %+v", delegate.got)
	}
	if cloud.calls != 0 {
		t.Fatalf("the federation hop must stay untouched while an app door is open, got %d", cloud.calls)
	}
}

// The STREAMING tool modality takes the same door. The two spellings of "MemQL
// is driving a tool loop" must not disagree about whether an app can be handed
// the step, or the interactive lane and the background lane would route the
// same cluster differently.
func TestStreamingToolCallOnAnAppDoorAlsoYieldsASessionWinner(t *testing.T) {
	r, cloud, delegate := sessionRouter(t, []memql.AppDoor{openApp("claude-code")})
	req := toolRequest()
	req.Modality = airoute.ModalityStreamingTools

	client, resolved, err := r.resolveStreamWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("resolveStreamWithTools: %v", err)
	}
	if resolved.Decision.Door != airoute.DoorSession {
		t.Fatalf("Door = %q, want %q", resolved.Decision.Door, airoute.DoorSession)
	}
	ch, err := client.CallChatStreamWithTools(context.Background(),
		[]common.ChatMessage{{Role: "user", Content: "ship it"}}, nil)
	if err != nil {
		t.Fatalf("CallChatStreamWithTools: %v", err)
	}
	var text string
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("chunk error: %v", chunk.Error)
		}
		text += chunk.Content
	}
	if text != "the app did the work" {
		t.Fatalf("stream = %q", text)
	}
	if delegate.runs != 1 || cloud.calls != 0 {
		t.Fatalf("delegate runs=%d cloud calls=%d", delegate.runs, cloud.calls)
	}
}

// Issue memql#5392's second acceptance: a bare Go model call with tools and no
// step refuses, NAMING THE MODALITY. There is nothing to hand over -- and
// reporting it as an unavailable door would send somebody to look at their
// laptop for a fact about the call site.
func TestToolNeedingCallWithNoStepRefusesAtResolution(t *testing.T) {
	r, cloud, delegate := sessionRouter(t, []memql.AppDoor{openApp("claude-code")})
	req := toolRequest()
	req.RunId = ""
	req.StepId = ""

	_, _, err := r.resolveWithTools(context.Background(), req)
	if err == nil {
		t.Fatalf("resolveWithTools with no step = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), modalityName(modalityTools)) {
		t.Fatalf("the refusal must name the modality, got %q", err)
	}
	if !strings.Contains(err.Error(), "app:claude-code") {
		t.Fatalf("the refusal must name the entry it resolved, got %q", err)
	}
	// IT REFUSES RATHER THAN FALLING THROUGH. Skipping the door would have
	// reached the vendor behind it, which is a paid call for a call site that
	// simply forgot its step.
	if cloud.calls != 0 || delegate.runs != 0 {
		t.Fatalf("a stepless refusal must spend nothing (cloud=%d delegate=%d)", cloud.calls, delegate.runs)
	}
}

// A SHUT app door is still a shut door. The session door changes which turns an
// OPEN app can take; it does not make an asleep laptop selectable, and the
// chain continues past it as it always did.
func TestAShutAppDoorStillFallsThroughOnAToolTurn(t *testing.T) {
	r, cloud, delegate := sessionRouter(t, []memql.AppDoor{shutApp("claude-code")})

	_, resolved, err := r.resolveWithTools(context.Background(), toolRequest())
	if err != nil {
		t.Fatalf("resolveWithTools: %v", err)
	}
	if resolved.Decision.Door == airoute.DoorSession {
		t.Fatalf("a shut app door must not yield a session winner")
	}
	if resolved.ProviderName != "streamClaudeSonnet" {
		t.Fatalf("ProviderName = %q, want the entry behind the shut door", resolved.ProviderName)
	}
	_ = cloud
	if delegate.runs != 0 {
		t.Fatalf("no session may be opened on a shut door, got %d", delegate.runs)
	}
}

// A CHAT turn through the same door is unchanged: the app serves it as a
// provider and the door stays `app`. The session door is about tool turns and
// nothing else.
func TestChatOnAnAppDoorIsStillTheAppDoor(t *testing.T) {
	r, _, delegate := sessionRouter(t, []memql.AppDoor{openApp("claude-code")})

	_, resolved, err := r.resolveChat(context.Background(), ResolveRequest{
		Level:    airoute.LevelStrong,
		Modality: airoute.ModalityChat,
		UserId:   "alice",
		Needs:    airoute.Needs{MinContextTokens: 8000},
	})
	if err != nil {
		t.Fatalf("resolveChat: %v", err)
	}
	if resolved.Decision.Door != DoorApp {
		t.Fatalf("Door = %q, want %q", resolved.Decision.Door, DoorApp)
	}
	if resolved.Client != nil {
		t.Fatalf("a chat winner carries no session client")
	}
	if delegate.runs != 0 {
		t.Fatalf("a chat turn opens no delegated session, got %d", delegate.runs)
	}
}

// EMBEDDINGS NEVER GO THROUGH AN APP (design D10), and the session door does
// not quietly become the exception. A degraded or substituted embedder answers
// in a DIFFERENT VECTOR SPACE, so the vector would not belong in the index it
// was about to be written to -- and every later similarity read would come back
// plausible and wrong.
func TestEmbeddingsNeverBecomeASession(t *testing.T) {
	for _, mod := range []providerModality{modalityChat, modalityStreamChat, modalityStructured, modalityVision, modalityEmbedding} {
		if toolNeeding(mod) {
			t.Fatalf("modality %s must not be tool-needing", modalityName(mod))
		}
	}
	if !toolNeeding(modalityTools) || !toolNeeding(modalityStreamTools) {
		t.Fatalf("the two tool modalities must be tool-needing")
	}
}

// The model PIN rides the door. A policy that wrote `app:claude-code:<model>`
// resolved a chain step naming that model, and the handover has to carry it or
// the pin is a decision the record claims was made and nothing acted on.
func TestTheModelPinRidesTheSessionHandover(t *testing.T) {
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{})
	providers.SetAppInference(&stubAppInference{doors: []memql.AppDoor{openApp("claude-code")}})
	delegate := &recordingDelegate{}
	providers.SetAppSessionDelegate(delegate)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"pinned": {"app:claude-code:claude-opus-5"},
	})
	r := New(providers, policies, testRules(t, defaultRule("pinned")), nil, nil)

	client, resolved, err := r.resolveWithTools(context.Background(), toolRequest())
	if err != nil {
		t.Fatalf("resolveWithTools: %v", err)
	}
	// The decision names what the AUTHOR WROTE, pin included: a record that
	// said `app:claude-code` would hide the pin from the one reader who needs
	// to see whether the app honoured it.
	if resolved.ProviderName != "app:claude-code:claude-opus-5" {
		t.Fatalf("ProviderName = %q, want the pinned reference", resolved.ProviderName)
	}
	if _, err := client.CallChatWithTools(context.Background(),
		[]common.ChatMessage{{Role: "user", Content: "go"}}, nil); err != nil {
		t.Fatalf("CallChatWithTools: %v", err)
	}
	if delegate.got.Model != "claude-opus-5" {
		t.Fatalf("the handover's model = %q, want the pin", delegate.got.Model)
	}
	if delegate.got.AppId != "claude-code" {
		t.Fatalf("the handover's app = %q, want the door's app id", delegate.got.AppId)
	}
}

// The LEVEL rides the door too (design D8). It is what the cockpit translates
// into the app's own knobs, and a session opened with no level runs at the
// app's defaults -- which is not what a `reasoning` call asked for.
func TestTheLevelRidesTheSessionHandover(t *testing.T) {
	r, _, delegate := sessionRouter(t, []memql.AppDoor{openApp("claude-code")})
	req := toolRequest()
	req.Level = airoute.LevelReasoning

	client, _, err := r.resolveWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("resolveWithTools: %v", err)
	}
	if _, err := client.CallChatWithTools(context.Background(),
		[]common.ChatMessage{{Role: "user", Content: "go"}}, nil); err != nil {
		t.Fatalf("CallChatWithTools: %v", err)
	}
	if delegate.got.Level != string(airoute.LevelReasoning) {
		t.Fatalf("the handover's level = %q, want %q", delegate.got.Level, airoute.LevelReasoning)
	}
}

// A SESSION CALL WRITES ITS LEDGER ROW, and this test exists because the way
// it would not is invisible.
//
// The chain walk builds a session client and hands it back on
// Resolved.Client. Handing THAT to the caller from ResolveFor is the obvious
// move and it is wrong: every client the resolve* paths return is already
// wrapped in a fallback wrapper which wraps again in the OBSERVER, and the
// observer is what writes v1:router:call. Substituting the bare session client
// would leave a run that spent somebody's entire subscription with no ledger
// row -- and the call itself would work perfectly, so nothing would report it.
//
// The row also has to carry the SESSION door and the app's own report, because
// those are the two facts that make the run readable afterwards.
func TestASessionCallWritesItsLedgerRow(t *testing.T) {
	r, _, delegate := sessionRouter(t, []memql.AppDoor{openApp("claude-code")})
	ledger := &decisionLedger{writes: make(chan string, 4)}
	r.engine = ledger

	resolved, err := r.ResolveFor(context.Background(), toolRequest())
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	if _, err := resolved.Client.(common.ToolCallingChatAIProvider).CallChatWithTools(
		context.Background(), []common.ChatMessage{{Role: "user", Content: "ship it"}}, nil); err != nil {
		t.Fatalf("CallChatWithTools: %v", err)
	}
	if delegate.runs != 1 {
		t.Fatalf("the delegate ran %d times, want one session", delegate.runs)
	}

	args := ledger.next(t)
	if args["door"] != DoorSession {
		t.Fatalf("the ledger row's door = %v, want %q", args["door"], DoorSession)
	}
	if args["providerName"] != "app:claude-code" {
		t.Fatalf("providerName = %v, want the entry the policy named", args["providerName"])
	}
	// `model` is what the CHAIN resolved -- the app id. `servedModel` is what
	// the app said it ran. The gap between them is the whole point.
	if args["model"] != "claude-code" {
		t.Fatalf("model = %v, want the app id the chain resolved", args["model"])
	}
	if args["servedModel"] != "claude-opus-5" || args["servedEffort"] != "high" {
		t.Fatalf("the app's report did not reach the row: servedModel=%v servedEffort=%v",
			args["servedModel"], args["servedEffort"])
	}
}
