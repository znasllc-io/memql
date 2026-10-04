package agent

import (
	"context"
	"fmt"
	"testing"
	"text/template"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/common"
)

// sessionDoorApps is one signed-in app on a machine whose stream this replica
// holds: the only arrangement in which the router can take the app door as a
// session.
type sessionDoorApps struct{}

func (sessionDoorApps) Doors(context.Context, string) ([]memql.AppDoor, error) {
	return []memql.AppDoor{{AppId: "claude-code", Machines: []memql.AppMachine{{
		RegistrationId: "laptop", Name: "laptop", Online: true, LocalStream: true,
		StructuredResult: true, FollowUps: true,
	}}}}, nil
}

func (sessionDoorApps) AppOrder(context.Context, string) ([]string, error) { return nil, nil }

// Call is the app's CHAT door. A tool turn must never reach it: the session
// door takes the whole step instead, and a tool turn answered here would be
// two agents in one conversation.
func (sessionDoorApps) Call(context.Context, memql.AppCallRequest) (memql.AppCallResult, error) {
	return memql.AppCallResult{}, fmt.Errorf("a tool turn reached the app's chat door")
}

// handoverDelegate records the step the router handed to the app.
type handoverDelegate struct {
	got  memql.AppSessionHandover
	runs int
}

func (d *handoverDelegate) RunStep(_ context.Context, h memql.AppSessionHandover) (memql.AppSessionOutcome, error) {
	d.runs++
	d.got = h
	return memql.AppSessionOutcome{Content: "Hello from Claude Code", ExecutionSurface: "cockpit-app:" + h.AppId}, nil
}

// sessionDoorRouter is the Claude Code door test's policy in miniature: the
// app first, with nothing behind it that a refusal could quietly fall to.
func sessionDoorRouter(t *testing.T) (*router.Router, *handoverDelegate) {
	t.Helper()
	providers := memql.NewProviderRegistryForTest()
	providers.SetAppInference(sessionDoorApps{})
	delegate := &handoverDelegate{}
	providers.SetAppSessionDelegate(delegate)
	rules := memql.NewRuleRegistry()
	if err := rules.Register(&memql.RuleConfig{Name: memql.DefaultRuleName, When: memql.RuleWhen{Present: map[string]bool{}}, Policy: "localFirst", Locked: true, OnUnavailable: memql.OnUnavailableDegrade, SourceFile: "dsl/rules/rules.memql"}); err != nil {
		t.Fatal(err)
	}
	if err := rules.Finalize(); err != nil {
		t.Fatal(err)
	}
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"localFirst": {memql.AppReferencePrefix + "claude-code"}})
	return router.New(providers, policies, rules, nil, nil), delegate
}

// askTurnContext is the context an Ask's `reason := builtin runAgentTurn` step
// runs under: an owned work execution whose run context names the step by
// KEY, with no override because nobody re-ran it.
func askTurnContext(t *testing.T, owner string) context.Context {
	t.Helper()
	ctx, err := auth.ContextWithPersistedOwner(context.Background(), owner, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return common.ContextWithRun(ctx, common.RunContext{RunId: "v1:work:run:ask", GoalId: "goal", StepKey: "reason", OwnerUserId: owner})
}

// AN ASK TURN NAMES ITS STEP FOR THE SESSION DOOR, on both lanes, without
// anybody pinning a model (the 2026-09-28 Claude Code door test).
//
// The replier builds its own router request and calls ResolveFor directly, so
// the engine seam that copies the run and step onto a prompt call never runs
// for it. Before this was fixed the step was named only when a person had
// pinned a model for a re-run, so the ordinary Ask turn -- policy routing to
// app:claude-code -- reached the session door with nothing to hand over and
// was refused at resolution. The refusal is correct (design D7 will not fall
// through to the source behind the door); the missing step was the bug.
func TestAnAskTurnNamesItsStepForTheSessionDoor(t *testing.T) {
	prompts := memql.NewPromptRegistry()
	if _, err := memql.LoadUnifiedPrompts(nil, prompts, template.New("partials")); err != nil {
		t.Fatal(err)
	}
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("background=%v", background), func(t *testing.T) {
			owner := "v1:identity:user:ask-session-owner"
			ctx, cancel := context.WithTimeout(askTurnContext(t, owner), time.Minute)
			defer cancel()
			rtr, delegate := sessionDoorRouter(t)
			engine := &workPromptEngine{registryEngine: registryEngine{registered: map[string]bool{"composeFile": true}}, prompts: prompts}
			r := newTestReplier(engine)
			r.router = rtr
			msg := &memqlv1.AgentGenerateTurnMsg{RequestId: "ask-hello", AgentId: "assistant", ActingAgent: &memqlv1.ActingAgentIdentity{Id: "assistant", Name: "Ada", Role: "assistant"}, History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "hello"}}}
			if background {
				msg.Hints = map[string]string{ExecutionLaneHintKey: ExecutionLaneBackground}
			}

			result, err := r.Handle(ctx, msg, &captureSink{})
			if err != nil {
				t.Fatalf("the Ask turn was refused at the session door: %v", err)
			}
			if delegate.runs != 1 {
				t.Fatalf("the delegate ran %d times, want exactly one session", delegate.runs)
			}
			// THE STEP KEY, not the step row's id: the delegate derives the row
			// from the run and the key, and a row id here would name a step
			// that does not exist.
			if delegate.got.RunId != "v1:work:run:ask" || delegate.got.StepId != "reason" {
				t.Fatalf("the handover names run=%q step=%q, want the Ask run and its step key %q",
					delegate.got.RunId, delegate.got.StepId, "reason")
			}
			if delegate.got.ActingUserId != owner {
				t.Fatalf("the handover acts for %q, want the run's owner %q", delegate.got.ActingUserId, owner)
			}
			if result.FinalText != "Hello from Claude Code" {
				t.Fatalf("the session's answer did not become the reply: %+v", result)
			}
		})
	}
}

// sessionHandoverOf resolves a prepared turn's request the way the lanes do
// and runs it through the session door, returning what the app was handed.
func sessionHandoverOf(t *testing.T, ctx context.Context, req router.ResolveRequest) memql.AppSessionHandover {
	t.Helper()
	rtr, delegate := sessionDoorRouter(t)
	selection, err := rtr.ResolveFor(ctx, req)
	if err != nil {
		t.Fatalf("the turn was refused at resolution: %v", err)
	}
	streamer, ok := selection.Client.(common.ChatStreamWithToolsProvider)
	if !ok {
		t.Fatalf("the turn resolved to %T, which cannot stream a tool turn", selection.Client)
	}
	ch, err := streamer.CallChatStreamWithTools(ctx, []common.ChatMessage{{Role: "user", Content: "go"}}, nil)
	if err != nil {
		t.Fatalf("CallChatStreamWithTools: %v", err)
	}
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("chunk error: %v", chunk.Error)
		}
	}
	if delegate.runs != 1 {
		t.Fatalf("the delegate ran %d times, want exactly one session", delegate.runs)
	}
	return delegate.got
}
