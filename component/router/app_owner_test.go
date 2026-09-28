package router

// A call that acts for NO PERSON never selects an app door (owner decision,
// 2026-09-28): the chain walk skips it with a recorded reason and goes on down
// the chain, and the call is never failed because of it.
//
// Scheduled work runs as `system:automation:<name>` (component/automations'
// system actor), which is a non-empty UserId. Until this rule the app door was
// shut to it only incidentally -- the agent's Doors found no machine
// registered to that id -- so a stub that answers for anyone opened it, and
// the reason a reader got was about machines rather than about the owner.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// everyCloud is a vendor entry that serves every text surface a chain walk can
// ask for, so the tests below prove the app door was PASSED rather than that
// the vendor could not have served anyway.
type everyCloud struct{ calls int }

func (c *everyCloud) Call(context.Context, string) (any, error) { return "cloud", nil }
func (c *everyCloud) CallChat(context.Context, []common.ChatMessage) (string, error) {
	c.calls++
	return "cloud answer", nil
}
func (c *everyCloud) CallChatStructured(context.Context, []common.ChatMessage, common.StructuredSchema) (string, error) {
	c.calls++
	return `{"ok":true}`, nil
}
func (c *everyCloud) CallChatWithTools(context.Context, []common.ChatMessage, []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	c.calls++
	return &common.ToolCallingChatResult{AssistantText: "cloud answer"}, nil
}
func (c *everyCloud) CallChatStreamWithTools(context.Context, []common.ChatMessage, []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	c.calls++
	ch := make(chan common.StreamToolChunk, 1)
	ch <- common.StreamToolChunk{Content: "cloud answer", Done: true}
	close(ch)
	return ch, nil
}

// appFirstRouter is the owner's test customization on the live cluster: an OPEN
// Claude Code door first, a vendor behind it. The app stub answers for anyone,
// so only the owner rule can keep a sweep away from it.
func appFirstRouter(t *testing.T, chain ...string) (*Router, *everyCloud, *stubAppInference, *recordingDelegate) {
	t.Helper()
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{})
	apps := &stubAppInference{doors: []memql.AppDoor{openApp("claude-code")}}
	providers.SetAppInference(apps)
	delegate := &recordingDelegate{}
	providers.SetAppSessionDelegate(delegate)
	cloud := &everyCloud{}
	providers.RegisterWithParamsForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet",
		map[string]any{"contextWindow": 200000}, cloud)
	if len(chain) == 0 {
		chain = []string{memql.AppReferencePrefix + "claude-code", "streamClaudeSonnet"}
	}
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"appFirst": chain})
	return New(providers, policies, testRules(t, defaultRule("appFirst")), nil, nil), cloud, apps, delegate
}

// sweepContext is a scheduled automation's execution context: its synthetic
// system actor and the run context the executor stamps on every execution.
func sweepContext() context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:automation:workerAppSessionStaleSweep", Role: auth.RoleReader, Unranked: true, Synthetic: true,
	})
	return common.ContextWithRun(ctx, common.RunContext{RunId: "v1:work:run:sweep", StepKey: "sweep"})
}

func consideredReason(considered []airoute.ConsideredEntry, entry string) (string, bool) {
	for _, c := range considered {
		if c.Entry == entry {
			return c.Reason, true
		}
	}
	return "", false
}

func TestAnOwnerlessCallSkipsTheAppDoorAndIsServedByTheNextSource(t *testing.T) {
	for _, mod := range []airoute.Modality{airoute.ModalityChat, airoute.ModalityStructured, airoute.ModalityTools} {
		t.Run(string(mod), func(t *testing.T) {
			r, cloud, apps, delegate := appFirstRouter(t)
			ctx := sweepContext()

			resolved, err := r.ResolveFor(ctx, ResolveRequest{
				Level: airoute.LevelFast, Modality: mod,
				Needs: airoute.Needs{MinContextTokens: 8000, Structured: mod == airoute.ModalityStructured, Tools: mod == airoute.ModalityTools},
			})
			if err != nil {
				t.Fatalf("an ownerless call was failed at the app door instead of moving on: %v", err)
			}
			if resolved.Resolution.ProviderName != "streamClaudeSonnet" {
				t.Fatalf("served by %q, want the next source after the app door", resolved.Resolution.ProviderName)
			}
			reason, ok := consideredReason(resolved.Resolution.Decision.Considered, "app:claude-code")
			if !ok {
				t.Fatalf("the decision does not say it passed the app door: %+v", resolved.Resolution.Decision.Considered)
			}
			if !strings.Contains(reason, memql.AppNoOwnerReason) {
				t.Errorf("the recorded reason is %q, want it to name %q", reason, memql.AppNoOwnerReason)
			}

			switch mod {
			case airoute.ModalityChat:
				_, err = resolved.Client.(common.ChatAIProvider).CallChat(ctx, []common.ChatMessage{{Role: "user", Content: "sweep"}})
			case airoute.ModalityStructured:
				_, err = resolved.Client.(common.ChatStructuredProvider).CallChatStructured(ctx,
					[]common.ChatMessage{{Role: "user", Content: "sweep"}}, common.StructuredSchema{Name: "s"})
			case airoute.ModalityTools:
				_, err = resolved.Client.(common.ToolCallingChatAIProvider).CallChatWithTools(ctx,
					[]common.ChatMessage{{Role: "user", Content: "sweep"}}, []common.ToolDefinition{{Name: "t"}})
			}
			if err != nil {
				t.Fatalf("the next source did not serve the call: %v", err)
			}
			if apps.calls != 0 || delegate.runs != 0 {
				t.Errorf("an app session was opened for nobody: %d app call(s), %d handed-over step(s)", apps.calls, delegate.runs)
			}
			if cloud.calls != 1 {
				t.Errorf("the next source served %d call(s), want 1", cloud.calls)
			}
		})
	}
}

// With nothing behind the app door the call has nowhere to go, and the refusal
// -- the park card, the run's error -- names the owner as the reason rather
// than a machine to wake.
func TestAnOwnerlessCallWithOnlyAnAppDoorSaysWhyItWasShut(t *testing.T) {
	r, _, apps, _ := appFirstRouter(t, memql.AppReferencePrefix+"claude-code")
	_, err := r.ResolveFor(sweepContext(), ResolveRequest{
		Level: airoute.LevelFast, Modality: airoute.ModalityStructured,
		Needs: airoute.Needs{MinContextTokens: 8000, Structured: true},
	})
	if err == nil {
		t.Fatal("an ownerless call with only an app door resolved")
	}
	if !strings.Contains(err.Error(), memql.AppNoOwnerReason) {
		t.Errorf("refusal = %v, want it to name %q", err, memql.AppNoOwnerReason)
	}
	if apps.calls != 0 {
		t.Errorf("the app was called %d time(s)", apps.calls)
	}
}

// A PERSON'S CALL IS UNCHANGED: the same chain, the same stub, and the app door
// is the winner -- the rule is about who is acting, nothing else. (Resolved,
// not called: a local call spends the process-wide local rate ceiling every
// test in this package shares, and the winner is the fact under test.)
func TestAPersonsCallStillTakesTheAppDoorFirst(t *testing.T) {
	r, _, _, _ := appFirstRouter(t)
	ctx := auth.ContextWithUserActor(context.Background(), "alice")
	resolved, err := r.ResolveFor(ctx, ResolveRequest{
		Level: airoute.LevelFast, Modality: airoute.ModalityStructured,
		Needs: airoute.Needs{MinContextTokens: 8000, Structured: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Resolution.ProviderName != "app:claude-code" || resolved.Resolution.Decision.Door != DoorApp {
		t.Fatalf("served by %q through %q, want the app door the owner's route puts first",
			resolved.Resolution.ProviderName, resolved.Resolution.Decision.Door)
	}
}
