//go:build agent

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// The agent tool loop reaches the run owner's machine under the owner rule the
// worker builtins now hold every caller to (memql.CallOwner), and it does so
// WITHOUT the escape that rule leaves trusted code: the owner the runtime
// stamps onto workerHost's @autoInjected ownerUserId is the actor the run
// restored on the very context the call runs under. So the context here is
// built the way a work run's is on the agent node -- the persisted owner, no
// captured grant -- and pinned to CLIENT origin, so the call is admitted on the
// owner being the caller and on nothing else.
//
// The other half runs through the same handler on the same context: the
// owner a model would supply, had the runtime not stamped over it, is refused
// with nothing dispatched.
func TestTheToolLoopDispatchesToTheRunOwnersMachine(t *testing.T) {
	const owner = "v1:identity:user:loop-owner"
	ctx, err := auth.ContextWithPersistedOwner(context.Background(), owner, nil, nil)
	if err != nil {
		t.Fatalf("restore the run owner: %v", err)
	}
	ctx = auth.ContextWithClientOrigin(ctx)

	// The turn's owner, resolved from that actor as the replier resolves it.
	turn := turnContext{AgentId: "v1:agents:agent:assistant-1", OwnerUserId: turnOwnerFromActor(ctx), RunId: "v1:work:run:r1"}
	if turn.OwnerUserId != owner {
		t.Fatalf("the turn resolved owner %q from the run's actor, want %q", turn.OwnerUserId, owner)
	}

	machine := newLoopMachine(t, owner)
	handler := dispatchHostHandler(t, machine.integ)

	// What a model sends: another person's id. The runtime overwrites it, and
	// the per-call defaults put the same value back after ExecuteTool strips
	// the @autoInjected field.
	args := map[string]any{
		"action":      "fs_read",
		"args":        map[string]any{"path": "/tmp/notes.txt"},
		"ownerUserId": "v1:identity:user:somebody-else",
		"agentId":     "forged",
	}
	injectAgentContext("workerHost", args, turn)
	callCtx := agentToolCallContext(ctx, "workerHost", turn)
	defaults := common.ToolDefaultsFromContext(callCtx)
	if args["ownerUserId"] != owner || defaults["ownerUserId"] != owner {
		t.Fatalf("the runtime stamped %v (default %v), want the run owner %q", args["ownerUserId"], defaults["ownerUserId"], owner)
	}
	// ExecuteTool's applyToolDefaults (component/memql) then puts the per-call
	// defaults over what arrived -- the server's value wins for every
	// @autoInjected field -- and the builtin hands the handler the result.
	for k, v := range defaults {
		args[k] = v
	}

	nodes, err := handler(callCtx, args, 0)
	if err != nil {
		t.Fatalf("the tool loop's dispatch was refused: %v", err)
	}
	var result map[string]any
	if len(nodes) != 1 || json.Unmarshal(nodes[0].Payload, &result) != nil || result["ok"] != true {
		t.Fatalf("the tool loop's dispatch did not run: %v", nodes)
	}
	if *machine.dispatched != 1 || machine.store.invocationOwner != owner {
		t.Fatalf("dispatched %d times, recorded for %q; want once, for %q", *machine.dispatched, machine.store.invocationOwner, owner)
	}

	// Unstamped, the same call on the same context is refused.
	forged := map[string]any{
		"action":      "fs_read",
		"args":        map[string]any{"path": "/tmp/notes.txt"},
		"ownerUserId": "v1:identity:user:somebody-else",
		"agentId":     turn.AgentId,
		"runId":       turn.RunId,
	}
	if _, err := handler(callCtx, forged, 0); err == nil || !strings.Contains(err.Error(), memql.OwnerNotCallerCode) {
		t.Fatalf("a call naming somebody else's id on the run's context was not refused: %v", err)
	}
	if *machine.dispatched != 1 {
		t.Fatalf("the refused call reached the machine: dispatched %d times", *machine.dispatched)
	}
}

// loopMachine is one owner's machine behind a real worker dispatcher whose
// gates permit.
type loopMachine struct {
	integ      *agentworker.Integration
	store      *loopStore
	dispatched *int
}

func newLoopMachine(t *testing.T, owner string) loopMachine {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := workerservice.NewRegistry(logger, time.Now)
	calls := 0
	w := &workerservice.Worker{
		RegistrationId: "machine-1",
		OwnerUserId:    owner,
		Name:           "machine-1",
		Capabilities:   []string{workerservice.CapabilityHeadless},
		Concurrency:    map[string]uint32{workerservice.CapabilityHeadless: 4},
	}
	w.SetDispatchFunc(func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		calls++
		return &memqlv1.ToolResult{CallId: d.GetCallId(), Payload: &memqlv1.ToolResult_Success{
			Success: &memqlv1.Success{ResultJson: []byte(`{"ok":true}`)},
		}}, nil
	}, func() {})
	reg.Add(w)
	store := &loopStore{owner: owner}
	d, err := agentworker.NewDispatcher(agentworker.Options{Logger: logger, Registry: reg, Store: store})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return loopMachine{integ: agentworker.NewIntegration(d, reg, nil, logger), store: store, dispatched: &calls}
}

// dispatchHostHandler is the capability a workerHost tool's builtin reaches.
func dispatchHostHandler(t *testing.T, integ *agentworker.Integration) func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	t.Helper()
	for _, c := range integ.Capabilities() {
		if c.Name == "dispatchHost" {
			return c.Handler
		}
	}
	t.Fatal("the worker integration publishes no dispatchHost capability")
	return nil
}

// loopStore permits every gate, and answers the fleet read for one owner only,
// as the owner-filtered read does.
type loopStore struct {
	owner           string
	invocationOwner string
}

func (s *loopStore) WorkersForOwner(_ context.Context, ownerUserId string) ([]agentworker.Candidate, error) {
	if ownerUserId != s.owner {
		return nil, nil
	}
	return []agentworker.Candidate{{
		RegistrationId:  "machine-1",
		Name:            "machine-1",
		OwnerUserId:     s.owner,
		Capabilities:    []string{workerservice.CapabilityHeadless},
		ConnectedNodeId: "agent-test",
		LastSeenAt:      time.Now(),
		Labels:          map[string]string{},
	}}, nil
}

func (s *loopStore) RoutingPolicyForOwner(context.Context, string) (*agentworker.Policy, error) {
	return nil, nil
}

func (s *loopStore) TouchWorkerSelected(context.Context, string, string) error { return nil }

func (s *loopStore) UserPreferences(context.Context, string) (agentworker.Preferences, error) {
	return agentworker.Preferences{}, nil
}

func (s *loopStore) AgentAuthorization(context.Context, string, string) (*agentworker.Authorization, error) {
	return &agentworker.Authorization{ComputerUseScope: "full"}, nil
}

func (s *loopStore) WriteInvocation(_ context.Context, row workerservice.InvocationRow) error {
	s.invocationOwner = row.OwnerUserId
	return nil
}
