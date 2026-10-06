package automations

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
)

func runTestAuthority(t *testing.T) auth.ForwardedAuthority {
	t.Helper()
	a, err := auth.ForwardedAuthorityForUser(&auth.AccessContext{UserId: "v1:identity:user:owner", Role: auth.RoleOwner}, auth.ForwardedClassBadge, auth.RoleOwner, time.Now().Add(time.Minute), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRelayedAutomationRejectsInvalidAuthorityBeforeExecution(t *testing.T) {
	for _, name := range []string{"missing", "expired", "unclamped", "writer", "unknown class"} {
		t.Run(name, func(t *testing.T) {
			authority := runTestAuthority(t)
			switch name {
			case "missing":
				authority = auth.ForwardedAuthority{}
			case "expired":
				authority.ExpiresAt = time.Now().Add(-time.Second)
			case "unclamped":
				authority.RoleCeiling = auth.RoleReader
			case "writer":
				authority.Role, authority.RoleCeiling = auth.RoleWriter, auth.RoleWriter
			case "unknown class":
				authority.CredentialClass = "unrecognized"
			}
			bus := events.NewBus()
			defer bus.Close()
			done := make(chan RunComplete, 1)
			unsub := bus.Subscribe(TopicRunTrace, func(evt events.Event) {
				var frame runFrame
				_ = json.Unmarshal([]byte(evt.Payload["frame"].(string)), &frame)
				if frame.Complete != nil {
					done <- *frame.Complete
				}
			})
			defer unsub()
			runner := &fakeRunner{registry: map[string]*Automation{"release": {Name: "release", Schedule: "0 * * * * *"}}}
			relay := newTestRelay(t, runner)
			relay.bus = bus
			relay.executeRelayed("run-1", "peer-1", relayedRequest{RunId: "run-1", Automation: "release", Authority: authority})
			select {
			case result := <-done:
				if result.Status != "refused" || result.ErrorCode != RunCodePermissionDenied || runner.gotName != "" {
					t.Fatalf("invalid authority executed: result=%+v automation=%q", result, runner.gotName)
				}
			case <-time.After(time.Second):
				t.Fatal("missing authority refusal")
			}
		})
	}
}

type runClaimProbe struct{ calls int }

func (p *runClaimProbe) Claim(context.Context, string, string) bool {
	p.calls++
	return false
}

func TestAutomationRelayRejectsApplicationEventForgeries(t *testing.T) {
	runner := &fakeRunner{registry: map[string]*Automation{}}
	relay := newTestRelay(t, runner)
	claim := &runClaimProbe{}
	relay.claimer = claim
	body, err := json.Marshal(relayedRequest{RunId: "r1", Automation: "release", Authority: runTestAuthority(t)})
	if err != nil {
		t.Fatal(err)
	}
	base := events.Event{Topic: TopicRunRequest, Kind: events.KindAutomationRunRequest, OriginNodeId: "peer-1", Payload: map[string]any{"runId": "r1", "origin": "peer-1", "targetType": "bff", "request": string(body)}}
	for _, name := range []string{"application event", "local forgery", "wrong origin", "wrong run id"} {
		evt := base
		evt.Payload = make(map[string]any)
		for k, v := range base.Payload {
			evt.Payload[k] = v
		}
		switch name {
		case "application event":
			evt.Kind = events.KindMessage
		case "local forgery":
			evt.OriginNodeId = ""
		case "wrong origin":
			evt.Payload["origin"] = "another-peer"
		case "wrong run id":
			evt.Payload["runId"] = "another-run"
		}
		relay.onRunRequest(evt)
		if claim.calls != 0 {
			t.Fatalf("%s reached the execution claim", name)
		}
	}
	// The probe itself is live: a correctly marked mesh request reaches it.
	relay.onRunRequest(base)
	if claim.calls != 1 {
		t.Fatal("valid transport request did not reach the claim")
	}
}

func TestAutomationRelayRefusesMissingOrMismatchedSenderAuthority(t *testing.T) {
	a := runTestAuthority(t)
	for _, ctx := range []context.Context{
		context.Background(),
		auth.ContextWithForwardedAuthority(context.Background(), a),
		auth.ContextWithAccess(auth.ContextWithForwardedAuthority(context.Background(), a), &auth.AccessContext{UserId: "another-owner", Role: auth.RoleOwner}),
	} {
		bus := events.NewBus()
		relay := newTestRelay(t, &fakeRunner{})
		relay.bus = bus
		sink := &captureSink{}
		relay.runRemote(ctx, "r1", "release", RunRequest{TargetNodeType: "planner"}, sink)
		bus.Close()
		if result := sink.onlyComplete(t); result.ErrorCode != RunCodePermissionDenied {
			t.Fatalf("sender without matching authority was admitted: %+v", result)
		}
	}
}

func TestAutomationRelayRejectsApplicationTraceForgery(t *testing.T) {
	relay := newTestRelay(t, &fakeRunner{})
	frames := make(chan runFrame, 1)
	relay.waiters["r1"] = frames
	body, err := json.Marshal(runFrame{Complete: &RunComplete{Status: "completed"}})
	if err != nil {
		t.Fatal(err)
	}
	evt := events.Event{Topic: TopicRunTrace, Kind: events.KindMessage, OriginNodeId: "peer-1", Payload: map[string]any{"runId": "r1", "origin": relay.nodeId, "frame": string(body)}}
	relay.onRunTrace(evt)
	if len(frames) != 0 {
		t.Fatal("application event forged a successful run result")
	}
	evt.Kind = events.KindAutomationRunTrace
	evt.OriginNodeId = ""
	relay.onRunTrace(evt)
	if len(frames) != 0 {
		t.Fatal("local event forged a mesh result")
	}
	evt.OriginNodeId = "peer-1"
	relay.onRunTrace(evt)
	if len(frames) != 1 {
		t.Fatal("valid mesh trace did not reach its waiter")
	}
}
