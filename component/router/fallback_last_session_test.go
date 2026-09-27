package router

// The tools wrapper reports the session its call ran as (epic memql#5408).
//
// A step handed to an app becomes a SUBRUN, and the caller that handed it over
// needs that subrun's id: a replay's fallback records the app's repair as it.
// The session client that actually ran is built INSIDE the wrapper's call --
// the chain is re-resolved by name every time -- so without LastSession on the
// wrapper the id existed only on an object nobody outside the router could
// reach. These tests go through ResolveFor, the seam the engine hands every
// caller, because that is the only client a caller ever holds.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type lastSessionReporter interface {
	LastSession() (memql.AppSessionOutcome, bool)
}

func TestTheToolsWrapperReportsTheSessionItsCallRanAs(t *testing.T) {
	r, cloud, delegate := sessionRouter(t, []memql.AppDoor{openApp("claude-code")})

	got, err := r.ResolveFor(context.Background(), toolRequest())
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	client, ok := got.Client.(common.ToolCallingChatAIProvider)
	if !ok {
		t.Fatalf("a tools resolution must hand back a tool-calling client, got %T", got.Client)
	}
	reporter, ok := got.Client.(lastSessionReporter)
	if !ok {
		t.Fatalf("the tools wrapper %T does not report the session it ran as", got.Client)
	}
	if _, set := reporter.LastSession(); set {
		t.Fatal("LastSession answered before any call ran, so it cannot be telling the truth after one")
	}

	if _, err := client.CallChatWithTools(context.Background(),
		[]common.ChatMessage{{Role: "user", Content: "repair it"}}, nil); err != nil {
		t.Fatalf("CallChatWithTools: %v", err)
	}
	session, set := reporter.LastSession()
	if !set {
		t.Fatal("a call served by a session door must report that session")
	}
	if session.ChildRunId != "v1:work:run:child" || session.SessionId != "v1:worker:appSession:s1" {
		t.Fatalf("LastSession = %+v, want the delegate's own child run and session", session)
	}
	if delegate.runs != 1 || cloud.calls != 0 {
		t.Fatalf("delegate runs=%d cloud calls=%d", delegate.runs, cloud.calls)
	}
}

// The negative control: a vendor that served the turn took no step, and the
// wrapper must say so rather than invent a session.
func TestTheToolsWrapperReportsNoSessionForAProviderThatTookNoStep(t *testing.T) {
	r, cloud, delegate := sessionRouter(t, []memql.AppDoor{shutApp("claude-code")})

	got, err := r.ResolveFor(context.Background(), toolRequest())
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	client := got.Client.(common.ToolCallingChatAIProvider)
	if _, err := client.CallChatWithTools(context.Background(),
		[]common.ChatMessage{{Role: "user", Content: "repair it"}}, nil); err != nil {
		t.Fatalf("CallChatWithTools: %v", err)
	}
	if cloud.calls != 1 || delegate.runs != 0 {
		t.Fatalf("the fixture must have been served by the vendor: cloud=%d delegate=%d", cloud.calls, delegate.runs)
	}
	if session, set := got.Client.(lastSessionReporter).LastSession(); set {
		t.Fatalf("a vendor turn reported a session: %+v", session)
	}
}
