package agents

// agent_turn.go -- runAgentTurn, the builtin a work run calls to invoke an
// agent (memql#5048).
//
// ===========================================================================
// WHY THIS EXISTS
// ===========================================================================
// `agent(name, prompt)` mints a v1:planner:plan and returns its id. The Plan
// is not the work -- it is the planner loop's INPUT: the loop picks it up off
// a graph event and plan_execution.go builds an AgentGenerateTurnMsg and
// forwards it. So retiring the Plan (memql#5000) means the work spine has to
// be able to run an agent turn without one, and that is this builtin.
//
// ===========================================================================
// WHY IT DISPATCHES LOCALLY RATHER THAN FORWARDING
// ===========================================================================
// Not a preference -- the topology settles it three times over.
//
//   - The design record's section H puts step execution on the AGENT node:
//     "the planner node keeps compile, the reactive loop and the sweeps; the
//     agent node runs steps." A work step calling this is already there.
//   - The AiForwardRouter is constructed for the BFF and PLANNER node types
//     only (app/cluster.go). There is none on the agent node to forward WITH,
//     and adding one would be building a network hop from a node to itself.
//   - agent.Replier.Handle is the same entry point the gRPC server already
//     dispatches AgentGenerateTurnMsg through, so this reaches the identical
//     runtime by the identical call.
//
// ===========================================================================
// THE SEAM IS NIL ON EVERY NODE THAT IS NOT AN AGENT, AND SAYS SO
// ===========================================================================
// integrations/agents is a CORE plug-in: it loads on identity, edge, mcp and
// the rest, none of which has an agent runtime. A nil runner refuses with a
// sentence naming what is missing rather than returning an empty reply, which
// would present as an agent that had nothing to say.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/core/id"
)

// AgentTurnRunner is the agent runtime, as this package needs it.
//
// It mirrors agent.Replier.Handle exactly, so app/ can satisfy it with the
// Replier itself and this package needs no import of the agent-tagged tree --
// which it could not have, because it loads on nodes that do not build it.
type AgentTurnRunner interface {
	// RunTurn executes one agent turn and returns the reply text. A caller
	// that wants deltas uses the gRPC path; this one waits.
	RunTurn(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg) (string, error)
}

// SetAgentTurnRunner installs the runtime. Called from app/ on an
// agent-tagged build; nil everywhere else, which is a working configuration
// for every node that never runs a turn.
func (i *Integration) SetAgentTurnRunner(r AgentTurnRunner) {
	if i == nil {
		return
	}
	i.turnMu.Lock()
	defer i.turnMu.Unlock()
	i.turnRunner = r
}

func (i *Integration) agentTurnRunner() AgentTurnRunner {
	if i == nil {
		return nil
	}
	i.turnMu.RLock()
	defer i.turnMu.RUnlock()
	return i.turnRunner
}

// handleRunAgentTurn is the builtin.
//
// SYNCHRONOUS, unlike `agent()`. The asynchrony `agent()` promises is the WORK
// RUN's: createGoal returns a goal id and the run carries on detached, so the
// step that calls this is already the background. A second layer of
// asynchrony here would be a run waiting on something it could not journal.
func (i *Integration) handleRunAgentTurn(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil {
		return nil, fmt.Errorf("runAgentTurn: agents integration not initialized")
	}
	agentId := strings.TrimSpace(asString(args["agentId"]))
	prompt := strings.TrimSpace(asString(args["prompt"]))
	if agentId == "" || prompt == "" {
		return nil, fmt.Errorf("runAgentTurn: needs an agentId and a prompt")
	}

	runner := i.agentTurnRunner()
	if runner == nil {
		// NAMED, not empty. An empty reply is indistinguishable from an agent
		// that had nothing to say, and this is a deployment fact rather than
		// an answer.
		return nil, fmt.Errorf("runAgentTurn: no agent runtime on this node -- a turn runs on an agent node, and the step that called this did not land on one")
	}

	requestId := id.NewShortId()
	msg := &memqlv1.AgentGenerateTurnMsg{
		RequestId: requestId,
		AgentId:   agentId,
		ScopeId:   strings.TrimSpace(asString(args["scopeId"])),
		History: []*memqlv1.AgentTurnMessage{
			{Role: "user", Content: prompt},
		},
	}
	reply, err := runner.RunTurn(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("runAgentTurn: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"agentId":   agentId,
		"requestId": requestId,
		"reply":     reply,
	})
	if err != nil {
		return nil, fmt.Errorf("runAgentTurn: marshal envelope: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "runAgentTurn:" + requestId,
		Concept:   envelopeConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: systemActorId,
		Payload:   payload,
	}}, nil
}

// turnSeam is embedded on Integration; kept here beside its only users.
type turnSeam struct {
	turnMu     sync.RWMutex
	turnRunner AgentTurnRunner
}
