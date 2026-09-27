//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// owner_scope_test.go -- the worker builtins act for their caller
// (memql.CallOwner).
//
// Each handler that takes an ownerUserId is driven three ways: naming ANOTHER
// user, which is refused with nothing dispatched, listed, probed or written;
// naming the caller; and from a trusted server-side context naming another
// user -- the way a shipped automation reaches these builtins. The last two
// are the positive controls: a handler that refused everything would pass the
// first.

const (
	ownerA = "v1:identity:user:owner-a"
	ownerB = "v1:identity:user:owner-b"
)

// asPerson is a signed-in person's context.
func asPerson(userId string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userId)
}

// asAutomation is a shipped automation's context: the cluster's own synthetic
// actor under internal origin.
func asAutomation() context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:automation:ownerScopeTest", Role: auth.RoleReader, Synthetic: true, Unranked: true,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

func isOwnerRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), memql.OwnerNotCallerCode)
}

// ownedFleet is one person's machine, live in a real registry, behind a real
// dispatcher whose gates permit, with a dispatch that counts what reaches it.
type ownedFleet struct {
	integ      *Integration
	store      *fakeStore
	dispatched *int
}

func newOwnedFleet(t *testing.T, owner string) ownedFleet {
	t.Helper()
	reg := workerservice.NewRegistry(testLogger(), fleetNow)
	caps := []string{workerservice.CapabilityHeadless, workerservice.CapabilityComputerUse}
	calls := 0
	w := &workerservice.Worker{
		RegistrationId: "machine-1",
		OwnerUserId:    owner,
		Name:           "machine-1",
		Capabilities:   caps,
		Concurrency:    map[string]uint32{workerservice.CapabilityHeadless: 4, workerservice.CapabilityComputerUse: 4},
	}
	w.SetDispatchFunc(func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		calls++
		return okResult(d.GetCallId()), nil
	}, func() {})
	reg.Add(w)
	store := &fakeStore{fakeFleet: &fakeFleet{
		// Scoped to the one owner, as the owner-filtered read is: a fake that
		// answered every caller with this machine would let a call for the
		// wrong person look routed.
		owner:    owner,
		machines: []Candidate{machine("machine-1", func(c *Candidate) { c.Capabilities = caps })},
		policy:   &Policy{Id: "p", Strategy: StrategyFirstFit, Fallback: FallbackNextMatching},
	}}
	d := newTestDispatcher(t, store, reg, "", nil)
	return ownedFleet{integ: NewIntegration(d, reg, nil, nil), store: store, dispatched: &calls}
}

func dispatchArgs(owner, action string) map[string]any {
	return map[string]any{
		"action":      action,
		"args":        map[string]any{"path": "/tmp/x"},
		"agentId":     "agent-1",
		"ownerUserId": owner,
		// The per-task approval gate keys on the run.
		"runId": "v1:work:run:r1",
	}
}

func TestDispatchActsForItsCaller(t *testing.T) {
	for _, tc := range []struct{ tool, action string }{
		{"workerHost", "fs_read"},
		{"workerComputer", "screenshot"},
	} {
		call := func(f ownedFleet, ctx context.Context, owner string) error {
			var err error
			if tc.tool == "workerHost" {
				_, err = f.integ.handleDispatchHost(ctx, dispatchArgs(owner, tc.action), 0)
			} else {
				_, err = f.integ.handleDispatchComputer(ctx, dispatchArgs(owner, tc.action), 0)
			}
			return err
		}
		t.Run(tc.tool, func(t *testing.T) {
			// THE ASSERTION: a person naming another person's id reaches
			// nothing -- no dispatch, and no invocation record either,
			// because the refusal comes before the dispatcher runs.
			f := newOwnedFleet(t, ownerB)
			if err := call(f, asPerson(ownerA), ownerB); !isOwnerRefusal(err) {
				t.Fatalf("a caller naming another user's id was not refused: %v", err)
			}
			if *f.dispatched != 0 || len(f.store.invocations) != 0 {
				t.Fatalf("the refused call reached the dispatcher: %d dispatched, %d invocations", *f.dispatched, len(f.store.invocations))
			}

			for _, pc := range []struct {
				name  string
				ctx   context.Context
				owner string
			}{
				{"the caller's own id", asPerson(ownerA), ownerA},
				{"internal origin naming another user", asAutomation(), ownerB},
			} {
				f := newOwnedFleet(t, pc.owner)
				if err := call(f, pc.ctx, pc.owner); err != nil {
					t.Fatalf("%s: %v", pc.name, err)
				}
				if *f.dispatched != 1 {
					t.Fatalf("%s: dispatched %d times, want once to the owner's machine", pc.name, *f.dispatched)
				}
				if got := f.store.lastInvocation(t).OwnerUserId; got != pc.owner {
					t.Fatalf("%s: the invocation was recorded for %q, want %q", pc.name, got, pc.owner)
				}
			}
		})
	}
}

func TestListWorkersActsForItsCaller(t *testing.T) {
	list := func(ctx context.Context, owner string) ([]map[string]any, error) {
		reg := workerservice.NewRegistry(nil, fleetNow)
		reg.Add(&workerservice.Worker{RegistrationId: "machine-" + owner, OwnerUserId: owner, Name: "m",
			Capabilities: []string{workerservice.CapabilityHeadless}})
		nodes, err := NewIntegration(nil, reg, nil, nil).handleListWorkers(ctx, map[string]any{"ownerUserId": owner}, 0)
		if err != nil {
			return nil, err
		}
		var out []map[string]any
		if len(nodes) != 1 || json.Unmarshal(nodes[0].Payload, &out) != nil {
			t.Fatalf("unreadable list: %v", nodes)
		}
		return out, nil
	}
	if got, err := list(asPerson(ownerA), ownerB); !isOwnerRefusal(err) || got != nil {
		t.Fatalf("a caller naming another user's id was answered: %v, %v", got, err)
	}
	for name, pc := range map[string]struct {
		ctx   context.Context
		owner string
	}{
		"the caller's own id":                 {asPerson(ownerA), ownerA},
		"internal origin naming another user": {asAutomation(), ownerB},
	} {
		got, err := list(pc.ctx, pc.owner)
		if err != nil || len(got) != 1 || got[0]["registrationId"] != "machine-"+pc.owner {
			t.Fatalf("%s: got %v, %v; want the owner's one machine", name, got, err)
		}
	}
}

func TestWorkerStatusActsForItsCaller(t *testing.T) {
	status := func(ctx context.Context, owner string) (string, error) {
		reg := workerservice.NewRegistry(nil, fleetNow)
		reg.Add(&workerservice.Worker{RegistrationId: "machine-" + owner, OwnerUserId: owner, Name: "m",
			Capabilities: []string{workerservice.CapabilityHeadless}})
		nodes, err := NewIntegration(nil, reg, nil, nil).handleStatus(ctx, map[string]any{"ownerUserId": owner}, 0)
		if err != nil {
			return "", err
		}
		var out map[string]any
		if len(nodes) != 1 || json.Unmarshal(nodes[0].Payload, &out) != nil {
			t.Fatalf("unreadable status: %v", nodes)
		}
		return asString(out["status"]), nil
	}
	if got, err := status(asPerson(ownerA), ownerB); !isOwnerRefusal(err) || got != "" {
		t.Fatalf("a caller naming another user's id was answered: %q, %v", got, err)
	}
	for name, pc := range map[string]struct {
		ctx   context.Context
		owner string
	}{
		"the caller's own id":                 {asPerson(ownerA), ownerA},
		"internal origin naming another user": {asAutomation(), ownerB},
	} {
		if got, err := status(pc.ctx, pc.owner); err != nil || got != "connected" {
			t.Fatalf("%s: got %q, %v; want the owner's machine seen connected", name, got, err)
		}
	}
}

// The scope request writes a plan as its owner through the engine, which this
// package cannot stand up without a database. The owner rule is decided FIRST,
// so a call that gets past it reaches the next gate -- no engine on this
// integration -- and says so; a refused one never does.
func TestRequestScopeActsForItsCaller(t *testing.T) {
	args := func(owner string) map[string]any {
		return map[string]any{"intent": "read a file", "requestedScope": "observe", "summary": "s", "agentId": "agent-1", "ownerUserId": owner}
	}
	integ := NewIntegration(nil, nil, nil, nil)
	if _, err := integ.handleRequestScope(asPerson(ownerA), args(ownerB), 0); !isOwnerRefusal(err) {
		t.Fatalf("a caller naming another user's id was not refused: %v", err)
	}
	for name, pc := range map[string]struct {
		ctx   context.Context
		owner string
	}{
		"the caller's own id":                 {asPerson(ownerA), ownerA},
		"internal origin naming another user": {asAutomation(), ownerB},
	} {
		_, err := integ.handleRequestScope(pc.ctx, args(pc.owner), 0)
		if err == nil || isOwnerRefusal(err) || !strings.Contains(err.Error(), "engine not configured") {
			t.Fatalf("%s: got %v, want the call past the owner rule to the engine gate", name, err)
		}
	}
}
