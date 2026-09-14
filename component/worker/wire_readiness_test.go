package worker

// The wire is ready before anything reads it (epic memql#5096, task
// memql#5097).
//
// This file exists because the engine and the cockpit land the two halves of
// one protocol change in separate repositories, days apart, and the cockpit
// reaches this proto through a PIN: whatever these fields mean on the day the
// pin is bumped is what the far side builds against. A field that survives a
// round trip is not much of a claim on its own -- but a field that DOES NOT
// (a schema string re-encoded through a Struct, an arguments object that
// arrives as an object on one runtime and a string on the other) is a claim
// the far side cannot discover except by shipping.
//
// So each test below pins one thing the cockpit told us it depends on:
//
//   - a tool's JSON Schema survives VERBATIM, because it is forwarded
//     unchanged into two runtimes' request bodies;
//   - a tool call carries an INDEX, because an OpenAI-compatible stream sends
//     arguments in fragments keyed by nothing else;
//   - a follow-up prompt has its own field, because `reason` already means
//     something on the cancel path;
//   - a structured result is separate from the error and from the exit code,
//     because a harness can produce one and still exit non-zero;
//   - an unknown harness word leaves the descriptor unused rather than
//     refusing the registration, which is the same rule the closed app-id set
//     already follows;
//   - a level arrives as the engine's word, and the model and effort the app
//     REPORTED come back as their own fields whose absence reads as unknown,
//     because the served-model record must never be filled from the request
//     (memql#5393).

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/proto"
)

// roundTrip marshals and unmarshals through the wire, so a test asserts on
// bytes that actually travelled rather than on a struct it just built.
func roundTrip[T proto.Message](t *testing.T, in T, out T) T {
	t.Helper()
	raw, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := proto.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestModelCallParamsCarriesWorkingContext(t *testing.T) {
	params := roundTrip(t, &memqlv1.ModelCallParams{ContextTokens: 32768}, &memqlv1.ModelCallParams{})
	if params.GetContextTokens() != 32768 {
		t.Fatalf("working context lost over the wire: %+v", params)
	}
}

func TestModelCallStartCarriesToolsWithTheirSchemaIntact(t *testing.T) {
	// A schema with an integer bound and a deliberate key order. Both are
	// what a Struct round trip would destroy: the bound becomes a float and
	// the order becomes whatever the map iterator says.
	schema := `{"type":"object","properties":{"limit":{"type":"integer","maximum":50},"q":{"type":"string"}},"required":["q"]}`

	got := roundTrip(t, &memqlv1.ModelCallStart{
		RequestId: "req-1",
		Model:     "llama3.1:8b",
		Kind:      "chat",
		Tools: []*memqlv1.ModelCallTool{
			{Name: "searchLibrary", Description: "Search the Library", ParametersJson: schema},
			{Name: "readFile", Description: "Read one file", ParametersJson: `{"type":"object"}`},
		},
	}, &memqlv1.ModelCallStart{})

	if len(got.GetTools()) != 2 {
		t.Fatalf("tools: want 2, got %d", len(got.GetTools()))
	}
	if got.GetTools()[0].GetParametersJson() != schema {
		t.Errorf("the tool schema did not survive the wire verbatim.\n want %s\n  got %s",
			schema, got.GetTools()[0].GetParametersJson())
	}
	if got.GetTools()[1].GetName() != "readFile" {
		t.Errorf("tool order or name lost: %+v", got.GetTools()[1])
	}
}

func TestModelCallMessageCarriesAToolResultBackToTheModel(t *testing.T) {
	got := roundTrip(t, &memqlv1.ModelCallMessage{
		Role:       "tool",
		Content:    `{"rows":3}`,
		ToolCallId: "call_abc",
		Name:       "searchLibrary",
	}, &memqlv1.ModelCallMessage{})

	if got.GetToolCallId() != "call_abc" {
		t.Errorf("tool_call_id: want call_abc, got %q -- both runtimes need the id to round-trip "+
			"or the result cannot be matched to its request", got.GetToolCallId())
	}
	if got.GetName() != "searchLibrary" {
		t.Errorf("name: want searchLibrary, got %q", got.GetName())
	}
}

func TestModelCallMessageReplaysTheAssistantsOwnToolCalls(t *testing.T) {
	got := roundTrip(t, &memqlv1.ModelCallMessage{
		Role: "assistant",
		ToolCalls: []*memqlv1.ModelCallToolCall{
			{Id: "call_a", Name: "searchLibrary", ArgumentsJson: `{"q":"invoice"}`, Index: 0},
			{Id: "call_b", Name: "readFile", ArgumentsJson: `{"path":"/tmp/x"}`, Index: 1},
		},
	}, &memqlv1.ModelCallMessage{})

	if len(got.GetToolCalls()) != 2 {
		t.Fatalf("tool_calls: want 2, got %d", len(got.GetToolCalls()))
	}
	if got.GetToolCalls()[1].GetIndex() != 1 {
		t.Errorf("index lost on the replayed assistant turn: %+v", got.GetToolCalls()[1])
	}
}

func TestModelCallDeltaCarriesFragmentedToolArgumentsByIndex(t *testing.T) {
	// The OpenAI-compatible shape: the first fragment names the call, the
	// rest carry only more of its arguments. Without `index` the second
	// fragment is indistinguishable from a second call with no name.
	first := roundTrip(t, &memqlv1.ModelCallDelta{
		RequestId: "req-1", Seq: 1,
		ToolCalls: []*memqlv1.ModelCallToolCall{{Index: 0, Id: "call_a", Name: "searchLibrary", ArgumentsJson: `{"q":`}},
	}, &memqlv1.ModelCallDelta{})
	second := roundTrip(t, &memqlv1.ModelCallDelta{
		RequestId: "req-1", Seq: 2,
		ToolCalls: []*memqlv1.ModelCallToolCall{{Index: 0, ArgumentsJson: `"invoice"}`}},
	}, &memqlv1.ModelCallDelta{})

	if first.GetToolCalls()[0].GetIndex() != 0 || second.GetToolCalls()[0].GetIndex() != 0 {
		t.Fatalf("both fragments must name index 0; got %d and %d",
			first.GetToolCalls()[0].GetIndex(), second.GetToolCalls()[0].GetIndex())
	}
	if second.GetToolCalls()[0].GetName() != "" {
		t.Errorf("a continuation fragment carries no name; got %q", second.GetToolCalls()[0].GetName())
	}
}

func TestModelCallEndCarriesTheCompleteToolCallList(t *testing.T) {
	got := roundTrip(t, &memqlv1.ModelCallEnd{
		RequestId:    "req-1",
		FinishReason: "stop",
		ToolCalls: []*memqlv1.ModelCallToolCall{
			{Id: "call_a", Name: "searchLibrary", ArgumentsJson: `{"q":"invoice"}`, Index: 0},
			{Id: "call_b", Name: "readFile", ArgumentsJson: `{"path":"/tmp/x"}`, Index: 1},
		},
	}, &memqlv1.ModelCallEnd{})

	if len(got.GetToolCalls()) != 2 {
		t.Fatalf("end tool_calls: want 2, got %d", len(got.GetToolCalls()))
	}
	if got.GetToolCalls()[0].GetArgumentsJson() != `{"q":"invoice"}` {
		t.Errorf("arguments must arrive as the STRING the worker normalised to; got %q",
			got.GetToolCalls()[0].GetArgumentsJson())
	}
}

func TestAppSessionControlCarriesAFollowUpPromptOfItsOwn(t *testing.T) {
	got := roundTrip(t, &memqlv1.AppSessionControl{
		SessionId: "s1",
		Action:    AppSessionActionMessage,
		Prompt:    "now write the tests",
	}, &memqlv1.AppSessionControl{})

	if got.GetPrompt() != "now write the tests" {
		t.Errorf("prompt: want the follow-up, got %q", got.GetPrompt())
	}
	if got.GetReason() != "" {
		t.Error("a follow-up must not ride `reason`: that field is transcript free-text on cancel, " +
			"and one field meaning two things cannot be read without knowing which branch wrote it")
	}
}

func TestAppSessionEndSeparatesAStructuredResultFromFailure(t *testing.T) {
	// Claude Code can emit a structured result AND exit non-zero. Folding
	// the two would make an answer we actually have unreadable.
	got := roundTrip(t, &memqlv1.AppSessionEnd{
		SessionId:  "s1",
		ExitCode:   2,
		ResultJson: `{"summary":"partially done","files":2}`,
	}, &memqlv1.AppSessionEnd{})

	if got.GetResultJson() != `{"summary":"partially done","files":2}` {
		t.Errorf("result_json: want the structured answer, got %q", got.GetResultJson())
	}
	if got.GetError() != "" {
		t.Error("a result must not imply an error, nor an error a result")
	}
	if got.GetExitCode() != 2 {
		t.Errorf("exit code: want 2, got %d", got.GetExitCode())
	}
}

func TestAppSessionStartCarriesAResponseSchema(t *testing.T) {
	got := roundTrip(t, &memqlv1.AppSessionStart{
		SessionId:          "s1",
		App:                AppIdClaudeCode,
		Kind:               AppSessionKindRun,
		ResponseSchemaJson: `{"type":"object","required":["summary"]}`,
	}, &memqlv1.AppSessionStart{})

	if got.GetResponseSchemaJson() != `{"type":"object","required":["summary"]}` {
		t.Errorf("response_schema_json: got %q", got.GetResponseSchemaJson())
	}
}

// The LEVEL travels as the engine's own word (design D8 of the 2026-09-13
// recording-and-learning record, memql#5393). The cockpit translates it into
// the app's knobs from a table that lives on the machine, so what crosses the
// wire is the level and never a model name the engine picked for an app it
// cannot see.
func TestAppSessionStartCarriesTheLevel(t *testing.T) {
	got := roundTrip(t, &memqlv1.AppSessionStart{
		SessionId: "s1",
		App:       AppIdClaudeCode,
		Kind:      AppSessionKindRun,
		Level:     "reasoning",
	}, &memqlv1.AppSessionStart{})

	if got.GetLevel() != "reasoning" {
		t.Errorf("level: want the engine's word verbatim, got %q", got.GetLevel())
	}
}

// What the app SERVED comes back beside the answer (design D9), and a cockpit
// that predates the fields sends neither. Silence has to decode as the empty
// string -- "the app did not say" -- rather than as anything the engine could
// mistake for a report, which is the whole of this task's acceptance.
func TestAppSessionEndCarriesTheReportedModelAndEffort(t *testing.T) {
	got := roundTrip(t, &memqlv1.AppSessionEnd{
		SessionId: "s1",
		Model:     "claude-sonnet-5",
		Effort:    "high",
	}, &memqlv1.AppSessionEnd{})
	if got.GetModel() != "claude-sonnet-5" || got.GetEffort() != "high" {
		t.Errorf("model/effort: want the app's report, got %q / %q", got.GetModel(), got.GetEffort())
	}

	older := roundTrip(t, &memqlv1.AppSessionEnd{SessionId: "s1", ExitCode: 0}, &memqlv1.AppSessionEnd{})
	if older.GetModel() != "" || older.GetEffort() != "" {
		t.Errorf("an older cockpit's silence must read as unknown, got %q / %q",
			older.GetModel(), older.GetEffort())
	}
}

// A model call carries the level too. The router has already chosen the model
// from the machine's advertisement, so for a local runtime the level is a
// ledger and log fact -- which is exactly why it must arrive intact rather
// than be reconstructed on the far side.
func TestModelCallStartCarriesTheLevel(t *testing.T) {
	got := roundTrip(t, &memqlv1.ModelCallStart{
		RequestId: "r1",
		Model:     "qwen3.5:9b",
		Kind:      "chat",
		Level:     "strong",
	}, &memqlv1.ModelCallStart{})

	if got.GetLevel() != "strong" {
		t.Errorf("level: got %q", got.GetLevel())
	}
	if got.GetModel() != "qwen3.5:9b" {
		t.Error("the level must not displace the model the router selected on")
	}
}

// A model call's served-model report already lives on its usage, so the
// effort sits BESIDE it there rather than in a second place on the end
// message. A runtime that states no effort leaves it empty, and that silence
// survives the wire as silence.
func TestModelCallUsageCarriesTheReportedEffortBesideTheModel(t *testing.T) {
	got := roundTrip(t, &memqlv1.ModelCallEnd{
		RequestId: "r1",
		Usage:     &memqlv1.ModelCallUsage{Known: true, Model: "gpt-oss:20b", Effort: "low"},
	}, &memqlv1.ModelCallEnd{})
	if got.GetUsage().GetModel() != "gpt-oss:20b" || got.GetUsage().GetEffort() != "low" {
		t.Errorf("usage: want the runtime's report, got %+v", got.GetUsage())
	}

	silent := roundTrip(t, &memqlv1.ModelCallEnd{
		RequestId: "r1",
		Usage:     &memqlv1.ModelCallUsage{Known: true, Model: "qwen3.5:9b"},
	}, &memqlv1.ModelCallEnd{})
	if silent.GetUsage().GetEffort() != "" {
		t.Errorf("a runtime that stated no effort must read as unknown, got %q", silent.GetUsage().GetEffort())
	}
}

func TestRegisterCarriesAppDescriptors(t *testing.T) {
	got := roundTrip(t, &memqlv1.Register{
		Name: "laptop",
		Apps: []*memqlv1.AppInfo{
			{Id: AppIdClaudeCode, Version: "2.1.4", SignedIn: true, Allowed: true, Subscription: "present"},
			{Id: AppIdCodex, Version: "0.9.0", SignedIn: true, Allowed: true, Subscription: "unknown"},
		},
		AppDescriptors: []*memqlv1.AppDescriptor{
			{Id: AppIdClaudeCode, Harness: HarnessClaudeHeadless, StructuredResult: true, FollowUps: true},
			{Id: AppIdCodex, Harness: HarnessCodexMCP, StructuredResult: false, FollowUps: true},
		},
	}, &memqlv1.Register{})

	descs := AppDescriptorsFromProto(got.GetAppDescriptors())
	if len(descs) != 2 {
		t.Fatalf("descriptors: want 2, got %d", len(descs))
	}
	byId := map[string]AppDescriptor{}
	for _, d := range descs {
		byId[d.Id] = d
	}
	if !byId[AppIdClaudeCode].StructuredResult {
		t.Error("claude-code descriptor lost structuredResult")
	}
	if byId[AppIdCodex].Harness != HarnessCodexMCP {
		t.Errorf("codex harness: want %s, got %s", HarnessCodexMCP, byId[AppIdCodex].Harness)
	}
}

// THE NEGATIVE CONTROL. An id or a harness word the engine does not know is
// DROPPED, not refused -- the same rule the closed app-id set follows, so a
// newer cockpit never makes the engine attempt a protocol it lacks and never
// fails a registration over a word it has not learned yet.
func TestAnUnknownHarnessOrAppIdLeavesNoDescriptor(t *testing.T) {
	descs := AppDescriptorsFromProto([]*memqlv1.AppDescriptor{
		{Id: AppIdClaudeCode, Harness: "telepathy", StructuredResult: true},
		{Id: "gemini-cli", Harness: HarnessClaudeHeadless},
		{Id: AppIdCodex, Harness: HarnessCodexAppServer, StructuredResult: true, FollowUps: true},
	})
	if len(descs) != 1 {
		t.Fatalf("want only the recognised descriptor, got %d: %+v", len(descs), descs)
	}
	if descs[0].Id != AppIdCodex {
		t.Errorf("kept the wrong one: %+v", descs[0])
	}

	if IsKnownHarness("telepathy") {
		t.Error("IsKnownHarness admitted a word outside the closed set")
	}
	for _, h := range KnownHarnesses() {
		if !IsKnownHarness(h) {
			t.Errorf("KnownHarnesses lists %q, which IsKnownHarness rejects", h)
		}
	}
}

// The descriptor set is looked up by app id, and an app that reported none
// answers "not known" rather than a zero descriptor that would read as "this
// harness does neither".
func TestDescriptorLookupDistinguishesAbsentFromCapableOfNothing(t *testing.T) {
	descs := []AppDescriptor{
		{Id: AppIdCodex, Harness: HarnessCodexAppServer, StructuredResult: true, FollowUps: true},
	}
	if _, ok := DescriptorFor(descs, AppIdClaudeCode); ok {
		t.Error("a lookup for an app with no descriptor must answer not-found")
	}
	d, ok := DescriptorFor(descs, AppIdCodex)
	if !ok || !d.FollowUps {
		t.Errorf("codex descriptor: %+v ok=%v", d, ok)
	}
}
