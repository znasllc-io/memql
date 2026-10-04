package router

// How an app door was reached travels with it (memql.AppDoorPin), because the
// app gate on the agent node needs it and only the router knows it: a routing
// rule's chain named the app, or an explicit pin skipped every rule -- and if
// it was a pin, whose.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// pinApps records the chat door's request.
type pinApps struct {
	stubAppInference
	got memql.AppCallRequest
}

func (a *pinApps) Call(ctx context.Context, req memql.AppCallRequest) (memql.AppCallResult, error) {
	a.got = req
	return a.stubAppInference.Call(ctx, req)
}

func TestAnAppDoorCarriesHowItWasReached(t *testing.T) {
	apps := &pinApps{stubAppInference: stubAppInference{doors: []memql.AppDoor{openApp("claude-code")}}}
	delegate := &recordingDelegate{}
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{})
	providers.SetAppInference(apps)
	providers.SetAppSessionDelegate(delegate)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"appFirst": {memql.AppReferencePrefix + "claude-code"},
	})
	r := New(providers, policies, testRules(t, defaultRule("appFirst")), nil, nil)
	messages := []common.ChatMessage{{Role: "user", Content: "hello"}}
	// The app door passes the process-wide LLM guard; the background lane's
	// own bucket keeps these calls from exhausting the interactive one.
	callCtx := memql.ContextWithBackgroundLane(context.Background())
	app := memql.AppReferencePrefix + "claude-code"

	// through reports what each door was handed for req.
	through := func(t *testing.T, req ResolveRequest) (chat, session memql.AppDoorPin) {
		t.Helper()
		chatReq := req
		chatReq.Modality = airoute.ModalityChat
		client, _, err := r.ResolveChat(chatReq)
		if err != nil {
			t.Fatalf("resolve chat: %v", err)
		}
		if _, err := client.CallChat(callCtx, messages); err != nil {
			t.Fatalf("CallChat: %v", err)
		}
		tools, resolved, err := r.resolveWithTools(context.Background(), req)
		if err != nil {
			t.Fatalf("resolve tools: %v", err)
		}
		if resolved.Decision.Door != airoute.DoorSession {
			t.Fatalf("tools door = %q, want the session door", resolved.Decision.Door)
		}
		if _, err := tools.CallChatWithTools(callCtx, messages, nil); err != nil {
			t.Fatalf("CallChatWithTools: %v", err)
		}
		return apps.got.Pin, delegate.got.Pin
	}

	// A RULE'S CHAIN: the routing configuration named the app. Not a pin,
	// whatever PinnedBy a caller left on a request that pinned nothing.
	req := toolRequest()
	req.PinnedBy = "alice"
	if chat, session := through(t, req); chat != (memql.AppDoorPin{}) || session != (memql.AppDoorPin{}) {
		t.Fatalf("a rule's chain reached the doors as chat=%+v session=%+v, want no pin", chat, session)
	}

	// A PERSON'S PIN: pinned, and by whom.
	req = toolRequest()
	req.ExplicitProvider = app
	req.PinnedBy = "alice"
	want := memql.AppDoorPin{Pinned: true, By: "alice"}
	if chat, session := through(t, req); chat != want || session != want {
		t.Fatalf("a person's pin reached the doors as chat=%+v session=%+v, want %+v", chat, session, want)
	}

	// A PIN NOBODY MADE -- a prompt author's default, a deploy env var: pinned,
	// by no person.
	req = toolRequest()
	req.ExplicitProvider = app
	want = memql.AppDoorPin{Pinned: true}
	if chat, session := through(t, req); chat != want || session != want {
		t.Fatalf("an author's pin reached the doors as chat=%+v session=%+v, want %+v", chat, session, want)
	}

	// A STEP OVERRIDE is the person who asked for the re-run, whatever pin
	// the request carried before it.
	req = toolRequest()
	req.ExplicitProvider = "someVendor"
	req.PinnedBy = "alice"
	req, err := memql.ApplyStepOverride(overriddenStep(&common.StepOverride{Model: app, RequestedBy: "bob"}), req)
	if err != nil {
		t.Fatalf("ApplyStepOverride: %v", err)
	}
	if req.ExplicitProvider != app || req.PinnedBy != "bob" {
		t.Fatalf("the override pinned %q by %q, want %q by the person who asked for the re-run", req.ExplicitProvider, req.PinnedBy, app)
	}
	want = memql.AppDoorPin{Pinned: true, By: "bob"}
	if chat, session := through(t, req); chat != want || session != want {
		t.Fatalf("an override's pin reached the doors as chat=%+v session=%+v, want %+v", chat, session, want)
	}
}
