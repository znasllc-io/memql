package memql

// The app door (epic memql#5096, task memql#5100, design D3 / D10).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// The app ids are LITERALS here, not imports. component/memql cannot import
// component/worker -- the dependency runs the other way -- and this package
// deliberately knows nothing about which ids exist: it resolves any `app:<id>`
// name and the closed set is component/worker's. The test that pins the two
// together lives THERE, in apps_test.go, which can see both
// (TestAppProviderReferencesMatchTheClosedSet).
const (
	appIdClaudeCode = "claude-code"
	appIdCodex      = "codex"
)

// stubApps is an in-process AppInference.
type stubApps struct {
	doors     []AppDoor
	order     []string
	orderErr  error
	lastReq   AppCallRequest
	lastActor string
	answer    string
	billing   string
	err       error
}

func (s *stubApps) Doors(_ context.Context, actingUserId string) ([]AppDoor, error) {
	s.lastActor = actingUserId
	return s.doors, nil
}

func (s *stubApps) AppOrder(_ context.Context, _ string) ([]string, error) {
	return s.order, s.orderErr
}

func (s *stubApps) Call(_ context.Context, req AppCallRequest) (AppCallResult, error) {
	s.lastReq = req
	if s.err != nil {
		return AppCallResult{}, s.err
	}
	billing := s.billing
	if billing == "" {
		billing = "subscription"
	}
	return AppCallResult{
		Content:          s.answer,
		Usage:            AppUsage{InputTokens: 11, OutputTokens: 22, Known: true},
		ExecutionSurface: "app:" + req.AppId + "@laptop",
		MachineLabel:     "Laptop",
		Billing:          billing,
	}, nil
}

func runnableDoor(appId string) AppDoor {
	return AppDoor{AppId: appId, Machines: []AppMachine{{
		RegistrationId:   "laptop",
		Name:             "laptop",
		Online:           true,
		LocalStream:      true,
		StructuredResult: true,
		FollowUps:        true,
		Harness:          "claude-headless",
		Subscription:     "present",
	}}}
}

func TestOnlyAnAppPrefixResolvesToADoor(t *testing.T) {
	for _, tt := range []struct {
		name string
		id   string
		ok   bool
	}{
		{"app:claude-code", "claude-code", true},
		{"app:*", "*", true},
		{"app:", "", false},
		{"fleet:llama3.1:8b", "", false},
		{"chat54Mini", "", false},
		{"", "", false},
	} {
		id, ok := IsAppReference(tt.name)
		if ok != tt.ok || id != tt.id {
			t.Errorf("IsAppReference(%q) = (%q, %v), want (%q, %v)", tt.name, id, ok, tt.id, tt.ok)
		}
	}
	if !IsAppWildcard(AppWildcard) || IsAppWildcard("app:claude-code") {
		t.Error("the wildcard must be recognised and a named app must not be")
	}
}

func TestAnAppEntryIsAvailableOnlyWhenAMachineCanRunIt(t *testing.T) {
	r := newProviderRegistry()
	r.SetAppInference(&stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}})

	entry, ok := r.EntryForUser(userCtx("alice"), "alice", AppReferencePrefix+appIdClaudeCode)
	if !ok || !entry.Available {
		t.Fatalf("a runnable app must resolve as available: %v", entry.Err())
	}

	// Signed in but the machine is asleep.
	asleep := runnableDoor(appIdClaudeCode)
	asleep.Machines[0].Online = false
	r2 := newProviderRegistry()
	r2.SetAppInference(&stubApps{doors: []AppDoor{asleep}})
	entry2, _ := r2.EntryForUser(userCtx("alice"), "alice", AppReferencePrefix+appIdClaudeCode)
	if entry2.Available {
		t.Error("an offline machine must leave the door shut")
	}

	// Online, but its stream is held by a sibling replica this node cannot
	// forward to (no AppCallForward wired). Skipped rather than failed: a
	// door this node cannot walk through is a shut door. The forwardable case
	// is TestAForwardableMachineOpensTheChatDoorButNotTheSessionDoor.
	elsewhere := runnableDoor(appIdClaudeCode)
	elsewhere.Machines[0].LocalStream = false
	r3 := newProviderRegistry()
	r3.SetAppInference(&stubApps{doors: []AppDoor{elsewhere}})
	entry3, _ := r3.EntryForUser(userCtx("alice"), "alice", AppReferencePrefix+appIdClaudeCode)
	if entry3.Available {
		t.Error("a machine on a sibling replica must not open the door on this one")
	}
}

// A node with no worker service has an UNAVAILABLE door, not a broken one --
// the same state as "nobody is signed in", flowing through the same path.
//
// THE REASON NAMES WHERE APP SOURCES DO RUN, and which node this is (the
// planner/app-source design, section 3a). A node with no app door wired --
// neither sessions of its own nor a forward to the agent holding the machine
// -- reads this line on every decision record whose route puts an app source
// first; "this node has no app sessions installed" read like a fault to fix
// on that node.
func TestANodeWithNoAppSessionsHasAnUnavailableDoor(t *testing.T) {
	t.Setenv("MEMQL_NODE_TYPE", "planner")
	const want = "app sources run on the agent holding the machine; this planner node cannot open one"
	r := newProviderRegistry()
	entry, ok := r.EntryForUser(userCtx("alice"), "alice", AppReferencePrefix+appIdCodex)
	if !ok {
		t.Fatal("the reference must still resolve to an entry")
	}
	if entry.Available {
		t.Error("no app inference installed must mean unavailable")
	}
	if entry.Err() == nil || entry.Err().Error() != want {
		t.Errorf("reason = %v, want %q", entry.Err(), want)
	}
	// The refusal the router inlines beside that line adds nothing to it:
	// the one sentence already says everything a reader can act on.
	if refusal := r.AppRefusal(userCtx("alice"), "alice", appIdCodex); len(refusal.Considered) != 0 || refusal.LastError != want {
		t.Errorf("refusal = %+v, want no per-app lines and the one sentence", refusal)
	}
	// And a call that reaches the client anyway refuses in the same words.
	_, err := entry.Client.(common.ChatAIProvider).CallChat(userCtx("alice"), []common.ChatMessage{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrAppUnavailable) || !strings.Contains(err.Error(), want) {
		t.Errorf("call err = %v, want ErrAppUnavailable naming %q", err, want)
	}
}

// The app answers a prompt, and the ledger learns where it ran and who paid.
func TestAnAppDoorAnswersAChatTurn(t *testing.T) {
	apps := &stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}, answer: "done"}
	r := newProviderRegistry()
	r.SetAppInference(apps)
	entry, _ := r.EntryForUser(userCtx("alice"), "alice", AppReferencePrefix+appIdClaudeCode)

	got, err := entry.Client.(common.ChatAIProvider).CallChat(
		userCtx("alice"), []common.ChatMessage{{Role: "user", Content: "summarise"}})
	if err != nil {
		t.Fatalf("chat through the app door: %v", err)
	}
	if got != "done" {
		t.Fatalf("answer = %q", got)
	}
	surface, usage, billing := entry.Client.(*appProvider).LastCall()
	if surface != "app:claude-code@laptop" {
		t.Errorf("executionSurface = %q, want app:<appId>@<registrationId>", surface)
	}
	if !usage.Known || usage.InputTokens != 11 {
		t.Errorf("usage = %+v, want what the app reported", usage)
	}
	if billing != "subscription" {
		t.Errorf("billing = %q, want subscription", billing)
	}
}

// A structured turn reaches the harness's own structured-output option, and a
// harness that cannot honour one is not selected for it.
func TestAStructuredTurnNeedsAHarnessThatCanAnswerOne(t *testing.T) {
	apps := &stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}, answer: `{"ok":true}`}
	r := newProviderRegistry()
	r.SetAppInference(apps)
	entry, _ := r.EntryForUser(userCtx("alice"), "alice", AppReferencePrefix+appIdClaudeCode)

	if _, err := entry.Client.(common.ChatStructuredProvider).CallChatStructured(
		userCtx("alice"),
		[]common.ChatMessage{{Role: "user", Content: "decide"}},
		common.StructuredSchema{Name: "decision", Schema: []byte(`{"type":"object"}`)},
	); err != nil {
		t.Fatalf("structured turn: %v", err)
	}
	if apps.lastReq.Schema == nil {
		t.Error("the schema must reach the harness rather than being described in the prompt")
	}

	// A door whose harness cannot return a structured answer is skipped by
	// the WILDCARD, which is where the choice between apps is made.
	prose := runnableDoor(appIdCodex)
	prose.Machines[0].StructuredResult = false
	prose.Machines[0].Harness = "codex-mcp"
	r2 := newProviderRegistry()
	r2.SetAppInference(&stubApps{doors: []AppDoor{prose}})
	entry2, _ := r2.EntryForUser(userCtx("alice"), "alice", AppWildcard)
	_, err := entry2.Client.(common.ChatStructuredProvider).CallChatStructured(
		userCtx("alice"),
		[]common.ChatMessage{{Role: "user", Content: "decide"}},
		common.StructuredSchema{Name: "decision", Schema: []byte(`{"type":"object"}`)},
	)
	var refusal *AppUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("want the typed refusal, got %v", err)
	}
	if !strings.Contains(refusal.Considered[appIdCodex], "structured") {
		t.Errorf("the refusal must name the harness limitation: %v", refusal.Considered)
	}
}

// THE INVERSION, asserted (design D3). An app door does NOT serve MemQL's own
// tool-calling turns: the app is the agent there and MemQL is its tool
// provider over MCP. The router skips a chain entry that lacks the modality,
// so the absence is what makes a tool turn walk past every app door.
func TestAnAppDoorDoesNotServeMemqlsOwnToolCallingTurns(t *testing.T) {
	r := newProviderRegistry()
	r.SetAppInference(&stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}})
	entry, _ := r.EntryForUser(userCtx("alice"), "alice", AppReferencePrefix+appIdClaudeCode)

	if _, ok := entry.Client.(common.ToolCallingChatAIProvider); ok {
		t.Error("an app door must not implement the tool-calling surface: driving an app that is " +
			"itself driving produces two agents fighting over one conversation. MemQL reaches an " +
			"app's tools through MCP, in the other direction.")
	}
	if _, ok := entry.Client.(common.ChatStreamWithToolsProvider); ok {
		t.Error("nor the streaming tool surface, for the same reason")
	}
}

func TestTheWildcardPicksTheOwnersPreferredApp(t *testing.T) {
	apps := &stubApps{
		doors:  []AppDoor{runnableDoor(appIdClaudeCode), runnableDoor(appIdCodex)},
		order:  []string{appIdCodex},
		answer: "ok",
	}
	r := newProviderRegistry()
	r.SetAppInference(apps)
	entry, _ := r.EntryForUser(userCtx("alice"), "alice", AppWildcard)

	if _, err := entry.Client.(common.ChatAIProvider).CallChat(
		userCtx("alice"), []common.ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if apps.lastReq.AppId != appIdCodex {
		t.Errorf("ran on %q, want the owner's preferred app", apps.lastReq.AppId)
	}

	// With no preference, the engine's own closed order decides -- stable, so
	// every replica agrees with no shared state.
	apps.order = nil
	if _, err := entry.Client.(common.ChatAIProvider).CallChat(
		userCtx("alice"), []common.ChatMessage{{Role: "user", Content: "hi again"}}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if apps.lastReq.AppId != appIdClaudeCode {
		t.Errorf("ran on %q, want the first of the engine's own order", apps.lastReq.AppId)
	}
}

func TestAnAppMissNamesEveryDoorConsidered(t *testing.T) {
	shut := AppDoor{AppId: appIdCodex}
	r := newProviderRegistry()
	r.SetAppInference(&stubApps{doors: []AppDoor{shut}})
	entry, _ := r.EntryForUser(userCtx("alice"), "alice", AppWildcard)

	_, err := entry.Client.(common.ChatAIProvider).CallChat(
		userCtx("alice"), []common.ChatMessage{{Role: "user", Content: "hi"}})
	var refusal *AppUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("want the typed refusal, got %v", err)
	}
	if refusal.Code() != AppRefusalCode {
		t.Errorf("code = %q, want %q", refusal.Code(), AppRefusalCode)
	}
	if refusal.Considered[appIdCodex] == "" {
		t.Errorf("the refusal must name the door and why: %v", refusal.Considered)
	}
	if !errors.Is(err, ErrAppUnavailable) {
		t.Error("the refusal must read as unavailable")
	}
}

// An app door is NOT a cloud provider. HasCloudProviderConfigured decides
// whether the park card offers "approve cloud", and counting a subscription
// app as cloud would offer a button that spends nothing and fixes nothing.
func TestAnAppDoorIsNotACloudProvider(t *testing.T) {
	r := newProviderRegistry()
	r.SetAppInference(&stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}})
	if r.HasCloudProviderConfigured() {
		t.Error("an app door must not count as a configured cloud provider")
	}
}

// The loop caps see an app call; the dollar ceiling does not. A runaway loop
// on somebody's subscription is still a runaway loop.
func TestAnAppCallIsFingerprintedForTheLoopBreaker(t *testing.T) {
	a := AppCallRequest{AppId: "claude-code", Messages: []common.ChatMessage{{Role: "user", Content: "x"}}}
	b := AppCallRequest{AppId: "claude-code", Messages: []common.ChatMessage{{Role: "user", Content: "x"}}}
	c := AppCallRequest{AppId: "codex", Messages: []common.ChatMessage{{Role: "user", Content: "x"}}}
	d := AppCallRequest{AppId: "claude-code", Messages: []common.ChatMessage{{Role: "user", Content: "y"}}}

	if AppCallFingerprint(a) != AppCallFingerprint(b) {
		t.Error("two identical calls must fingerprint identically or the loop breaker never trips")
	}
	if AppCallFingerprint(a) == AppCallFingerprint(c) {
		t.Error("a different app is a different call")
	}
	if AppCallFingerprint(a) == AppCallFingerprint(d) {
		t.Error("a different prompt is a different call")
	}
	// The run id deliberately does NOT enter the fingerprint: it varies
	// across a genuine loop and would make every repetition look novel.
	withRun := a
	withRun.RunId = "v1:work:run:abc"
	if AppCallFingerprint(a) != AppCallFingerprint(withRun) {
		t.Error("the run id must not change the fingerprint")
	}
}

func TestTheAppProviderHasNoStaticPerAppChildren(t *testing.T) {
	_, err := newAIProvider(ProviderConfig{Name: "claudeCode", Type: AppProviderType, Model: "claude-code"})
	if err == nil {
		t.Fatal("a static per-app child must be refused: a door exists while somebody is signed in, " +
			"so an entry written at load is a claim the tree cannot keep")
	}
	if !strings.Contains(err.Error(), AppReferencePrefix+"claude-code") {
		t.Errorf("the refusal must name the alternative that works: %v", err)
	}
}

// The `app` door appears in inferenceStatus once a machine can run one, and
// the row says which app -- so a surface can name it rather than only counting
// doors.
func TestInferenceStatusReportsTheAppDoor(t *testing.T) {
	r := newProviderRegistry()
	r.SetAppInference(&stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}})
	e := &MemQLEngine{providers: r}

	nodes, err := e.evaluateInferenceStatusExpression(userCtx("alice"))
	if err != nil {
		t.Fatalf("inferenceStatus: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want one row, got %d", len(nodes))
	}
	var row map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	doors, _ := row["doorsOpen"].([]any)
	if len(doors) != 1 || doors[0] != InferenceDoorApp {
		t.Fatalf("doorsOpen = %v, want exactly [app] -- with an app runnable and nothing else, "+
			"the gate must say so rather than reporting the cluster ineligible", doors)
	}
	if row["eligible"] != true {
		t.Error("an open app door makes the cluster eligible")
	}
	if apps, _ := row["runnableApps"].([]any); len(apps) != 1 || apps[0] != appIdClaudeCode {
		t.Errorf("runnableApps = %v, want the app named", row["runnableApps"])
	}
	if row["appSessionsInstalled"] != true {
		t.Error("appSessionsInstalled must distinguish a signed-out user from a node with no worker service")
	}
}

// A node with no app sessions installed reports the door SHUT and says why it
// could not have been open. Reporting "eligible" here would send somebody to a
// console whose features then refuse.
func TestInferenceStatusSeparatesNoAppFromNoWorkerService(t *testing.T) {
	e := &MemQLEngine{providers: newProviderRegistry()}
	nodes, err := e.evaluateInferenceStatusExpression(userCtx("alice"))
	if err != nil {
		t.Fatalf("inferenceStatus: %v", err)
	}
	var row map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if row["appEligible"] != false || row["appSessionsInstalled"] != false {
		t.Errorf("want both false on a node with no worker service, got %v / %v",
			row["appEligible"], row["appSessionsInstalled"])
	}
	if doors, _ := row["doorsOpen"].([]any); len(doors) != 0 {
		t.Errorf("doorsOpen = %v, want none", doors)
	}
}

// THE READINESS VERDICT AND THE inferenceStatus ROW READ ONE IMPLEMENTATION.
//
// `inferenceDoors` was extracted for exactly this (epic memql#5077): a second
// reading would let a person be told inference is configured on one surface
// and not on the other, with both readings defensible. This epic added a door,
// and it went INTO that function rather than beside its one caller -- so the
// `ai` readiness module picks it up without knowing the door exists.
func TestTheAppDoorReachesTheSharedInferenceReading(t *testing.T) {
	r := newProviderRegistry()
	r.SetAppInference(&stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}})
	e := &MemQLEngine{providers: r}

	d := e.inferenceDoors(userCtx("alice"))
	if !d.AppEligible {
		t.Fatal("a runnable app must make the shared reading eligible")
	}
	if len(d.Doors) != 1 || d.Doors[0] != InferenceDoorApp {
		t.Fatalf("doors = %v, want exactly [app]", d.Doors)
	}
	// The readiness module branches on `len(Doors) > 0` (readiness_eval.go).
	// With only an app signed in, that has to be true -- a cluster whose only
	// route to a model is somebody's Claude Code IS able to do work, and
	// reporting it as not ready would send them to add a key they do not need.
	if len(d.Doors) == 0 {
		t.Error("readiness would report this cluster unable to reach a model")
	}
	if !d.AppSessionsInstalled {
		t.Error("appSessionsInstalled must come off the same reading, or the row and the " +
			"readiness verdict can disagree about why the door is shut")
	}

	// THE ORDER IS PART OF THE ANSWER. Every client renders this list rather
	// than re-deriving one, so a door inserted in the wrong place changes what
	// four surfaces say the chain tries first.
	full := newProviderRegistry()
	full.SetAppInference(&stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}})
	full.SetFleetInference(&stubFleet{models: []FleetModel{eligibleForGate()}})
	e2 := &MemQLEngine{providers: full}
	got := e2.inferenceDoors(userCtx("alice")).Doors
	if len(got) != 2 || got[0] != InferenceDoorLocal || got[1] != InferenceDoorApp {
		t.Fatalf("doors = %v, want [local app] -- own hardware, then a subscription already paid for", got)
	}
}

// eligibleForGate is a model that meets the first-run gate's minimum profile:
// online, structured output, and over the context floor.
func eligibleForGate() FleetModel {
	m := onlineModel("llama3.1:8b", true)
	m.ContextWindow = MinimumContextWindow
	return m
}

// TestAppProviderCallCarriesRunContext. A model call answered by an app is
// made INSIDE a step, and the run context says which one -- but the door never
// read it, so every session it opened had runId and stepId empty and could not
// be traced back to the Ask run that caused it, and its recording opened as a
// free-floating goal of its own. What the door carries is the step KEY, the
// same contract the session door's handover uses; the recorder decides what a
// model call may and may not claim on the calling step.
func TestAppProviderCallCarriesRunContext(t *testing.T) {
	apps := &stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}, answer: `{"ok":true}`}
	r := newProviderRegistry()
	r.SetAppInference(apps)
	entry, _ := r.EntryForUser(userCtx("alice"), "alice", AppReferencePrefix+appIdClaudeCode)

	inRun := common.ContextWithRun(userCtx("alice"), common.RunContext{RunId: "v1:work:run:ask", StepKey: "reason"})
	if _, err := entry.Client.(common.ChatStructuredProvider).CallChatStructured(inRun,
		[]common.ChatMessage{{Role: "user", Content: "classify"}},
		common.StructuredSchema{Name: "verdict", Schema: json.RawMessage(`{"type":"object"}`)}); err != nil {
		t.Fatalf("structured call through the app door: %v", err)
	}
	if apps.lastReq.RunId != "v1:work:run:ask" || apps.lastReq.StepId != "reason" {
		t.Fatalf("the app was called with run %q step %q, want the calling run and step key", apps.lastReq.RunId, apps.lastReq.StepId)
	}

	// The control: outside a run there is nothing to carry, and nothing is
	// invented.
	if _, err := entry.Client.(common.ChatAIProvider).CallChat(userCtx("alice"),
		[]common.ChatMessage{{Role: "user", Content: "hello"}}); err != nil {
		t.Fatalf("chat through the app door: %v", err)
	}
	if apps.lastReq.RunId != "" || apps.lastReq.StepId != "" {
		t.Errorf("a call outside any run named run %q step %q", apps.lastReq.RunId, apps.lastReq.StepId)
	}
}

// A MACHINE HELD BY ANOTHER AGENT OPENS THE CHAT DOOR AND NOT THE SESSION DOOR
// (the planner/app-source design, section 3a).
//
// A chat or structured turn crosses to the holding agent over AppCallForward,
// so a node that can forward sees the door open -- which is the whole of how a
// planner's triage, compile and compose reach a signed-in Claude Code. A STEP
// HANDOVER does not cross: it runs as a session subrun on the replica whose
// session delegate opens it, so for a tool turn the door stays shut on every
// node that does not hold the stream, and says where it would open.
func TestAForwardableMachineOpensTheChatDoorButNotTheSessionDoor(t *testing.T) {
	t.Setenv("MEMQL_NODE_TYPE", "planner")
	held := runnableDoor(appIdClaudeCode)
	held.Machines[0].LocalStream = false
	held.Machines[0].Forwardable = true
	r := newProviderRegistry()
	r.SetAppInference(&stubApps{doors: []AppDoor{held}, answer: `{"complexity":"trivial"}`})

	entry, _ := r.EntryForUser(userCtx("alice"), "alice", AppReferencePrefix+appIdClaudeCode)
	if !entry.Available {
		t.Fatalf("a machine this node can forward to must open the chat door: %v", entry.Err())
	}
	if _, err := entry.Client.(common.ChatStructuredProvider).CallChatStructured(userCtx("alice"),
		[]common.ChatMessage{{Role: "user", Content: "triage"}}, common.StructuredSchema{Name: "t", Schema: []byte(`{"type":"object"}`)}); err != nil {
		t.Fatalf("a structured turn through a forwardable door: %v", err)
	}

	why := r.AppSessionRefusalHere(userCtx("alice"), "alice", appIdClaudeCode)
	if why == "" {
		t.Fatal("a step handover opened on a node that does not hold the machine's stream: the session " +
			"would have nowhere to run and, as a session winner, no remaining chain")
	}
	for _, want := range []string{"agent holding", "planner"} {
		if !strings.Contains(why, want) {
			t.Errorf("the session door's reason %q does not say %q -- it must say where a step handover runs", why, want)
		}
	}

	// The holding replica opens both.
	local := runnableDoor(appIdClaudeCode)
	r2 := newProviderRegistry()
	r2.SetAppInference(&stubApps{doors: []AppDoor{local}})
	if why := r2.AppSessionRefusalHere(userCtx("alice"), "alice", appIdClaudeCode); why != "" {
		t.Errorf("the replica holding the stream refused the session door: %s", why)
	}
	if why := r2.AppSessionRefusalHere(userCtx("alice"), "alice", AppWildcardId); why != "" {
		t.Errorf("the wildcard refused the session door on the holding replica: %s", why)
	}
	// And a node with no app inference at all says what it always said.
	if why := newProviderRegistry().AppSessionRefusalHere(userCtx("alice"), "alice", appIdClaudeCode); why != appSourcesNotOnThisNode() {
		t.Errorf("reason = %q, want %q", why, appSourcesNotOnThisNode())
	}
}

// The wildcard's choice between apps is a CHAT-door choice, so it counts a
// door this node can forward to as open.
func TestTheWildcardTakesAnAppHeldByAnotherAgent(t *testing.T) {
	// This test makes a guarded call, and the package's earlier tests fill the
	// process-wide local rate ceiling; the guard is not what it asserts.
	offTheSharedRateCeiling(t)
	held := runnableDoor(appIdCodex)
	held.Machines[0].LocalStream = false
	held.Machines[0].Forwardable = true
	apps := &stubApps{doors: []AppDoor{held}, answer: "ok"}
	r := newProviderRegistry()
	r.SetAppInference(apps)
	entry, _ := r.EntryForUser(userCtx("alice"), "alice", AppWildcard)
	if !entry.Available {
		t.Fatalf("the wildcard door is shut over an app this node can forward to: %v", entry.Err())
	}
	if _, err := entry.Client.(common.ChatStructuredProvider).CallChatStructured(userCtx("alice"),
		[]common.ChatMessage{{Role: "user", Content: "decide"}}, common.StructuredSchema{Name: "d", Schema: []byte(`{"type":"object"}`)}); err != nil {
		t.Fatalf("the wildcard refused an app held by another agent: %v", err)
	}
	if apps.lastReq.AppId != appIdCodex {
		t.Errorf("the wildcard chose %q, want %q", apps.lastReq.AppId, appIdCodex)
	}
}

// THE READINESS ROW ASKS THE CHAT DOOR'S QUESTION. Inference through an app is
// one chat, structured or vision turn, and that crosses to the agent holding
// the machine -- so a node that can forward there CAN get inference through
// it, and the row that says whether this caller can get inference must say
// so. Reading the local-only session predicate here had an agent replica
// report "no app door" while every chat and structured call through
// app:claude-code on it succeeded.
func TestTheReadinessRowCountsAnAppThisNodeCanForwardTo(t *testing.T) {
	held := runnableDoor(appIdClaudeCode)
	held.Machines[0].LocalStream = false
	held.Machines[0].Forwardable = true
	r := newProviderRegistry()
	r.SetAppInference(&stubApps{doors: []AppDoor{held}})
	e := &MemQLEngine{providers: r}

	d := e.inferenceDoors(userCtx("alice"))
	if !d.AppEligible || len(d.RunnableApps) != 1 || d.RunnableApps[0] != appIdClaudeCode {
		t.Fatalf("appEligible=%v runnableApps=%v, want the forwardable app counted", d.AppEligible, d.RunnableApps)
	}
	if len(d.Doors) != 1 || d.Doors[0] != InferenceDoorApp {
		t.Fatalf("doors = %v, want [app]", d.Doors)
	}

	// The control: a machine that is offline opens nothing, forwardable or not.
	asleep := held
	asleep.Machines = []AppMachine{held.Machines[0]}
	asleep.Machines[0].Online = false
	r2 := newProviderRegistry()
	r2.SetAppInference(&stubApps{doors: []AppDoor{asleep}})
	if d := (&MemQLEngine{providers: r2}).inferenceDoors(userCtx("alice")); d.AppEligible {
		t.Fatal("an offline machine made the app door eligible")
	}
}
