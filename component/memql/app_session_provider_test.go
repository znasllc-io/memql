package memql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

type fakeDelegate struct {
	got AppSessionHandover
	out AppSessionOutcome
	err error
}

func (f *fakeDelegate) RunStep(_ context.Context, h AppSessionHandover) (AppSessionOutcome, error) {
	f.got = h
	return f.out, f.err
}

func sessionRegistry(d AppSessionDelegate) *ProviderRegistry {
	r := &ProviderRegistry{}
	r.SetAppSessionDelegate(d)
	return r
}

// The whole step goes over, and the answer comes back as ONE assistant turn
// with NO tool calls -- which is what terminates MemQL's loop. The app drove
// its own; a second turn would be MemQL asking an agent that has already
// finished to carry on.
func TestSessionProviderHandsTheStepOverAndEndsTheLoop(t *testing.T) {
	d := &fakeDelegate{out: AppSessionOutcome{
		Content:     "done",
		ChildRunId:  "v1:work:run:child",
		SessionId:   "v1:worker:appSession:s1",
		ArtifactIds: []string{"v1:library:artifact:a1"},
		Model:       "claude-sonnet-4-6",
		Effort:      "high",
		Billing:     "subscription",
	}}
	p := sessionRegistry(d).SessionProvider("claude-code", "claude-sonnet-4-6", SessionRequest{
		ActingUserId: "u1", AgentId: "ag1", Level: "reasoning",
		RunId: "v1:work:run:r1", StepId: "v1:work:step:s1",
	})
	caller, ok := p.(common.ToolCallingChatAIProvider)
	if !ok {
		t.Fatalf("the session provider must satisfy ToolCallingChatAIProvider; got %T", p)
	}
	res, err := caller.CallChatWithTools(context.Background(),
		[]common.ChatMessage{{Role: "system", Content: "be useful"}, {Role: "user", Content: "ship it"}},
		[]common.ToolDefinition{{Name: "workbenchHost"}})
	if err != nil {
		t.Fatalf("CallChatWithTools: %v", err)
	}
	if len(res.ToolCalls) != 0 {
		t.Fatalf("a session answer carries no tool calls, got %d", len(res.ToolCalls))
	}
	if res.AssistantText != "done" {
		t.Fatalf("AssistantText = %q, want the session's answer", res.AssistantText)
	}
	if d.got.StepId != "v1:work:step:s1" || d.got.RunId != "v1:work:run:r1" {
		t.Fatalf("the handover must carry the step: %+v", d.got)
	}
	if d.got.Level != "reasoning" {
		t.Fatalf("the handover must carry the level, got %q", d.got.Level)
	}
	if d.got.Model != "claude-sonnet-4-6" {
		t.Fatalf("the handover must carry the policy's model pin, got %q", d.got.Model)
	}
	if d.got.AppId != "claude-code" {
		t.Fatalf("the handover must carry the app the door resolved, got %q", d.got.AppId)
	}
	if !strings.Contains(d.got.Prompt, "ship it") || !strings.Contains(d.got.Prompt, "be useful") {
		t.Fatalf("the handover's prompt must carry the conversation, got %q", d.got.Prompt)
	}
	if len(d.got.Tools) != 1 || d.got.Tools[0].Name != "workbenchHost" {
		t.Fatalf("the handover must carry the tools the turn offered, got %+v", d.got.Tools)
	}
}

// The streaming surface runs the SAME handover and emits the answer as one
// chunk plus a done. A session's live output is its transcript, which the
// cockpit streams onto the session row; re-emitting it as model deltas would
// make a record of ACTIONS look like a model thinking.
func TestSessionProviderStreamsOneChunkAndDone(t *testing.T) {
	d := &fakeDelegate{out: AppSessionOutcome{Content: "shipped"}}
	p := sessionRegistry(d).SessionProvider("claude-code", "", SessionRequest{
		ActingUserId: "u1", RunId: "r", StepId: "s",
	})
	streamer, ok := p.(common.ChatStreamWithToolsProvider)
	if !ok {
		t.Fatalf("the session provider must satisfy ChatStreamWithToolsProvider; got %T", p)
	}
	ch, err := streamer.CallChatStreamWithTools(context.Background(),
		[]common.ChatMessage{{Role: "user", Content: "go"}}, nil)
	if err != nil {
		t.Fatalf("CallChatStreamWithTools: %v", err)
	}
	var text string
	var sawDone bool
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("chunk error: %v", chunk.Error)
		}
		if len(chunk.ToolCalls) != 0 {
			t.Fatalf("a session stream carries no tool calls")
		}
		text += chunk.Content
		sawDone = sawDone || chunk.Done
	}
	if text != "shipped" || !sawDone {
		t.Fatalf("stream = %q done=%v, want the answer then a done", text, sawDone)
	}
}

// The reporters are how the decision row learns what actually served it. A
// servedModel copied from the request would record as measured something
// nobody measured (design D9).
func TestSessionProviderReportsWhatServedIt(t *testing.T) {
	d := &fakeDelegate{out: AppSessionOutcome{
		Content: "ok", Model: "claude-opus-5", Effort: "", SessionId: "v1:worker:appSession:s2",
		ExecutionSurface: "cockpit-app:claude-code",
	}}
	// The PIN is a different model from the report, deliberately: that gap is
	// what the two fields exist to make visible.
	p := sessionRegistry(d).SessionProvider("claude-code", "claude-sonnet-4-6", SessionRequest{
		ActingUserId: "u1", RunId: "r", StepId: "s",
	})

	model, effort := p.(interface{ ServedModel() (string, string) }).ServedModel()
	if model != "" || effort != "" {
		t.Fatalf("before the first call nothing has been reported, got (%q, %q)", model, effort)
	}

	if _, err := p.(common.ToolCallingChatAIProvider).CallChatWithTools(
		context.Background(), []common.ChatMessage{{Role: "user", Content: "go"}}, nil); err != nil {
		t.Fatalf("CallChatWithTools: %v", err)
	}
	model, effort = p.(interface{ ServedModel() (string, string) }).ServedModel()
	if model != "claude-opus-5" {
		t.Fatalf("ServedModel model = %q, want the app's report and not the pin", model)
	}
	if effort != "" {
		t.Fatalf("ServedModel effort = %q; an app that stated none leaves it EMPTY, never the requested value", effort)
	}
	if surface := p.(interface{ ExecutionSurface() string }).ExecutionSurface(); surface != "cockpit-app:claude-code" {
		t.Fatalf("ExecutionSurface = %q", surface)
	}
}

// A call with no step cannot be delegated, and the refusal says so before
// anything is spent -- and before the delegate is even consulted.
func TestSessionProviderRefusesWithNoStep(t *testing.T) {
	d := &fakeDelegate{}
	p := sessionRegistry(d).SessionProvider("claude-code", "", SessionRequest{ActingUserId: "u1"})
	_, err := p.(common.ToolCallingChatAIProvider).CallChatWithTools(
		context.Background(), []common.ChatMessage{{Role: "user", Content: "go"}}, nil)
	if !errors.Is(err, ErrAppSessionNoStep) {
		t.Fatalf("CallChatWithTools with no step = %v, want ErrAppSessionNoStep", err)
	}
	if d.got.AppId != "" {
		t.Fatalf("the delegate must not be reached for a stepless call")
	}
}

// An engine with no delegate refuses as a WIRING fault rather than as an
// unavailable door: the two need different fixes and the sentences must not be
// confusable. This is ai_resolver.go's rule, applied to the second seam.
func TestSessionProviderRefusesWhenUnwired(t *testing.T) {
	r := &ProviderRegistry{}
	if r.AppSessionDelegateInstalled() {
		t.Fatalf("AppSessionDelegateInstalled() = true on a bare registry")
	}
	p := r.SessionProvider("claude-code", "", SessionRequest{ActingUserId: "u1", RunId: "r", StepId: "s"})
	_, err := p.(common.ToolCallingChatAIProvider).CallChatWithTools(
		context.Background(), []common.ChatMessage{{Role: "user", Content: "go"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "no app-session delegate") {
		t.Fatalf("an unwired delegate must refuse as a wiring fault, got %v", err)
	}
	if strings.Contains(err.Error(), "no machine") {
		t.Fatalf("the wiring fault must not read as a shut door: %v", err)
	}
}

// A delegate that fails does NOT record an outcome. The reporters would
// otherwise answer with a previous call's model on a call that never ran.
func TestSessionProviderRecordsNothingOnFailure(t *testing.T) {
	d := &fakeDelegate{err: errors.New("the machine went to sleep")}
	p := sessionRegistry(d).SessionProvider("claude-code", "", SessionRequest{
		ActingUserId: "u1", RunId: "r", StepId: "s",
	})
	if _, err := p.(common.ToolCallingChatAIProvider).CallChatWithTools(
		context.Background(), []common.ChatMessage{{Role: "user", Content: "go"}}, nil); err == nil {
		t.Fatalf("a failing delegate must surface its error")
	}
	if model, effort := p.(interface{ ServedModel() (string, string) }).ServedModel(); model != "" || effort != "" {
		t.Fatalf("a failed session reports nothing, got (%q, %q)", model, effort)
	}
}
