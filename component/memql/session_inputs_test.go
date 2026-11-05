package memql

import (
	"context"
	"slices"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

// TestAppSessionHandoverCarriesSessionInputs: a tool-using step handed to an
// app carries the Library artifacts its caller put on the context, and the
// cockpit pulls them into the session's workspace before the app starts. The
// inputs ride only a handover that offers tools, which is the handover the
// owned-run and acting-agent precondition guards.
func TestAppSessionHandoverCarriesSessionInputs(t *testing.T) {
	inputs := []string{"artifact-material-1", "artifact-material-2"}
	tools := []common.ToolDefinition{{Name: "emailComposeContext"}, {Name: "emailComposeRender"}, {Name: "emailComposeSubmit"}}
	messages := []common.ChatMessage{{Role: "system", Content: "write the email"}}

	d := &fakeDelegate{out: AppSessionOutcome{Content: "submitted"}}
	p := sessionRegistry(d).SessionProvider("claude-code", "", SessionRequest{ActingUserId: "u1", RunId: "run-1", StepId: "composeEmail"})
	ctx := WithSessionInputs(delegatedProviderTestContext("u1", "run-1", "composeEmail"), inputs)
	if _, err := p.(common.ToolCallingChatAIProvider).CallChatWithTools(ctx, messages, tools); err != nil {
		t.Fatalf("CallChatWithTools: %v", err)
	}
	if !slices.Equal(d.got.Inputs, inputs) {
		t.Fatalf("handover inputs = %v, want %v", d.got.Inputs, inputs)
	}
	if len(d.got.Tools) != len(tools) || d.got.ToolScope == nil || d.got.ToolScope.AgentID == "" {
		t.Fatalf("the handover lost its tool scope: %+v", d.got)
	}

	// No tools offered: nothing the precondition guards, and no inputs.
	d.got = AppSessionHandover{}
	if _, err := p.(common.ToolCallingChatAIProvider).CallChatWithTools(ctx, messages, nil); err != nil {
		t.Fatalf("CallChatWithTools without tools: %v", err)
	}
	if len(d.got.Inputs) != 0 {
		t.Fatalf("a handover that offers no tools carried inputs %v", d.got.Inputs)
	}

	// Tools offered but the precondition fails (another run): refused before
	// the delegate, so nothing is pulled anywhere.
	d.got = AppSessionHandover{}
	other := WithSessionInputs(delegatedProviderTestContext("u1", "run-2", "composeEmail"), inputs)
	if _, err := p.(common.ToolCallingChatAIProvider).CallChatWithTools(other, messages, tools); err == nil {
		t.Fatal("a handover for another run was not refused")
	}
	if d.got.RunId != "" || len(d.got.Inputs) != 0 {
		t.Fatalf("the delegate was reached for another run: %+v", d.got)
	}
}

// TestSessionInputsCarrierKeepsItsOwnCopy: the carrier answers what was put on
// the context, once each, in order, and a caller's later change to either
// slice reaches neither the context nor the next reader.
func TestSessionInputsCarrierKeepsItsOwnCopy(t *testing.T) {
	if got := SessionInputsFromContext(context.Background()); got != nil {
		t.Fatalf("an empty context carries inputs %v", got)
	}
	given := []string{" a-1 ", "a-2", "", "a-1"}
	ctx := WithSessionInputs(context.Background(), given)
	given[1] = "changed"
	got := SessionInputsFromContext(ctx)
	if !slices.Equal(got, []string{"a-1", "a-2"}) {
		t.Fatalf("inputs = %v, want [a-1 a-2]", got)
	}
	got[0] = "changed"
	if again := SessionInputsFromContext(ctx); !slices.Equal(again, []string{"a-1", "a-2"}) {
		t.Fatalf("a reader's change reached the context: %v", again)
	}
	// An inner frame that names none clears an outer frame's.
	if inner := SessionInputsFromContext(WithSessionInputs(ctx, nil)); inner != nil {
		t.Fatalf("an inner frame with no inputs still carries %v", inner)
	}
}
