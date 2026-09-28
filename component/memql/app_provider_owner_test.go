package memql

// A call that acts for NO PERSON never reaches an app door.
//
// An app session runs on one person's machine under a credential whose subject
// is that person. A scheduled sweep, a connector, an anonymous caller and a
// bare Go call with no actor act for nobody, so there is no machine to open a
// session on -- and until this rule the door stayed shut for them only
// incidentally: the agent's Doors happened to find no machine registered to
// `system:automation:<name>`, a non-agent node said "this node has no app
// sessions installed", and neither sentence named the actual reason.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

// countingApps is stubApps with a count of every question put to it, so a
// test can say the door was shut BEFORE anything asked a machine.
type countingApps struct {
	stubApps
	doorsCalls int
	callCalls  int
}

func (c *countingApps) Doors(ctx context.Context, actingUserId string) ([]AppDoor, error) {
	c.doorsCalls++
	return c.stubApps.Doors(ctx, actingUserId)
}

func (c *countingApps) Call(ctx context.Context, req AppCallRequest) (AppCallResult, error) {
	c.callCalls++
	return c.stubApps.Call(ctx, req)
}

func ownerlessCallers() []struct {
	name  string
	actor string
	ctx   context.Context
} {
	sweep := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:automation:workerAppSessionStaleSweep", Role: auth.RoleReader, Unranked: true, Synthetic: true,
	})
	return []struct {
		name  string
		actor string
		ctx   context.Context
	}{
		{"no actor at all", "", context.Background()},
		{"a scheduled automation", "system:automation:workerAppSessionStaleSweep", sweep},
		{"the maintenance principal", auth.MaintenanceActor("auditEventRetentionSweep").UserId,
			auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("auditEventRetentionSweep"))},
		{"a connector", auth.ConnectorActor("shopify").UserId,
			auth.ContextWithConnectorActor(context.Background(), "shopify")},
		{"the anonymous actor", auth.AnonymousUserId, context.Background()},
	}
}

func TestAnOwnerlessCallFindsEveryAppDoorShutAndSaysWhy(t *testing.T) {
	for _, tc := range ownerlessCallers() {
		t.Run(tc.name, func(t *testing.T) {
			apps := &countingApps{stubApps: stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}}}
			r := newProviderRegistry()
			r.SetAppInference(apps)

			for _, ref := range []string{AppReferencePrefix + appIdClaudeCode, AppWildcard, AppReferencePrefix + appIdClaudeCode + ":sonnet"} {
				entry, ok := r.EntryForUser(tc.ctx, tc.actor, ref)
				if !ok {
					t.Fatalf("%s must still resolve to an entry, so the chain walk can say why it passed it", ref)
				}
				if entry.Available {
					t.Fatalf("%s is available to a call acting for nobody", ref)
				}
				if err := entry.Err(); err == nil || !strings.Contains(err.Error(), AppNoOwnerReason) {
					t.Errorf("%s: reason = %v, want %q", ref, err, AppNoOwnerReason)
				}
			}
			if apps.doorsCalls != 0 {
				t.Errorf("the machines were asked %d time(s) about a call that acts for nobody", apps.doorsCalls)
			}

			// The refusal the walk attaches as detail says the same, and names
			// no machine and no app that "is not reported" -- neither is why.
			refusal := r.AppRefusal(tc.ctx, tc.actor, appIdClaudeCode)
			if refusal == nil || !strings.Contains(refusal.Error(), AppNoOwnerReason) {
				t.Errorf("AppRefusal = %v, want it to name %q", refusal, AppNoOwnerReason)
			}
			if len(refusal.Considered) != 0 {
				t.Errorf("AppRefusal.Considered = %v, want no per-app line: the owner, not an app, is what is missing", refusal.Considered)
			}
			if apps.doorsCalls != 0 {
				t.Errorf("AppRefusal asked the machines about a call that acts for nobody")
			}
		})
	}
}

// A person's call is untouched: the rule is about who is acting, and a
// borrowed owner (the validator, a dispatched run) IS a person.
func TestAPersonsCallStillReachesTheAppDoor(t *testing.T) {
	for _, actor := range []string{"alice", "v1:identity:user:alice"} {
		apps := &countingApps{stubApps: stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}}}
		r := newProviderRegistry()
		r.SetAppInference(apps)
		ctx := auth.ContextWithUserActor(context.Background(), actor)
		entry, _ := r.EntryForUser(ctx, actor, AppReferencePrefix+appIdClaudeCode)
		if !entry.Available {
			t.Fatalf("%s: a person's runnable app door must be open: %v", actor, entry.Err())
		}
	}
}

// DEFENCE IN DEPTH: a client obtained for an ownerless call and used anyway is
// refused before any machine is asked, with the same reason.
func TestAnAppClientCalledForNobodyIsRefusedBeforeAnyMachine(t *testing.T) {
	for _, tc := range ownerlessCallers() {
		t.Run(tc.name, func(t *testing.T) {
			apps := &countingApps{stubApps: stubApps{doors: []AppDoor{runnableDoor(appIdClaudeCode)}, answer: "hi"}}
			r := newProviderRegistry()
			r.SetAppInference(apps)
			entry, _ := r.EntryForUser(tc.ctx, tc.actor, AppReferencePrefix+appIdClaudeCode)

			_, err := entry.Client.(common.ChatStructuredProvider).CallChatStructured(tc.ctx,
				[]common.ChatMessage{{Role: "user", Content: "classify"}}, common.StructuredSchema{Name: "s"})
			var refusal *AppUnavailable
			if !errors.As(err, &refusal) || !strings.Contains(err.Error(), AppNoOwnerReason) {
				t.Fatalf("err = %v, want the typed refusal naming %q", err, AppNoOwnerReason)
			}
			if !errors.Is(err, ErrAppUnavailable) {
				t.Error("the refusal must read as a shut door, so a chain moves on")
			}
			if apps.callCalls != 0 {
				t.Errorf("a machine was asked to run a session for nobody (%d call(s))", apps.callCalls)
			}
		})
	}
}

// The SESSION door (design D7) is the same act -- a whole step handed to the
// app -- and refuses the same caller before the delegate is reached.
func TestASessionDoorForNobodyNeverReachesTheDelegate(t *testing.T) {
	for _, tc := range ownerlessCallers() {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDelegate{}
			p := sessionRegistry(d).SessionProvider(appIdClaudeCode, "", SessionRequest{
				ActingUserId: tc.actor, RunId: "v1:work:run:r1", StepId: "reason",
			})
			_, err := p.(common.ToolCallingChatAIProvider).CallChatWithTools(tc.ctx,
				[]common.ChatMessage{{Role: "user", Content: "go"}}, nil)
			if err == nil || !strings.Contains(err.Error(), AppNoOwnerReason) {
				t.Fatalf("err = %v, want a refusal naming %q", err, AppNoOwnerReason)
			}
			if d.got.AppId != "" {
				t.Fatalf("the delegate was handed a step for nobody: %+v", d.got)
			}
		})
	}
}
