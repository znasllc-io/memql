package agents

// agent_turn.go -- runAgentTurn, the builtin a work run calls to invoke an
// agent (memql#5048).
//
// ===========================================================================
// WHY THIS EXISTS
// ===========================================================================
// `agent(name, prompt)` used to mint a v1:planner:plan and return its id. The
// Plan was not the work -- it was the planner loop's INPUT: the loop picked it
// up off a graph event and plan_execution.go built an AgentGenerateTurnMsg and
// forwarded it. So retiring the Plan (memql#5000) meant the work spine had to
// be able to run an agent turn without one, and that is this builtin.
//
// It has a caller now: the `invokeAgent` template in
// dsl/agents/automations.memql is one step, and that step is this. `agent()`
// opens a goal naming that template and the run dispatcher executes it
// (memql#5048).
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

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
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
	requireFile, _ := args["requireFile"].(bool)
	if requireFile {
		run, ok := common.RunFromContext(ctx)
		ac, _ := auth.AccessFromContext(ctx)
		if !ok || run.RunId == "" || run.GoalId == "" || run.OwnerUserId == "" || strings.TrimSpace(run.StepKey) == "" || ac == nil || memql.BareShortId(ac.UserId) != memql.BareShortId(run.OwnerUserId) || i.engine == nil {
			return nil, fmt.Errorf("runAgentTurn: a file receipt requires its owned work run, executing step, and engine")
		}
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
	if i.engine != nil {
		history, err := workTurnHistory(ctx, i.engine, prompt)
		if err != nil {
			return nil, err
		}
		if len(history) > 0 {
			msg.History = history
		}
	}
	reply, err := runner.RunTurn(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("runAgentTurn: %w", err)
	}

	envelope := map[string]any{
		"agentId":   agentId,
		"requestId": requestId,
		"reply":     reply,
	}
	if requireFile {
		fileId, err := i.workFileReceipt(ctx)
		if err != nil {
			return nil, err
		}
		envelope["outputFileId"] = fileId
	}
	payload, err := json.Marshal(envelope)
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

// A known file-producing template requires a durable file, not just the
// model's confirmation. The file row is synchronous; its Library index is
// promoted asynchronously on another replica and cannot serve as this receipt.
func (i *Integration) workFileReceipt(ctx context.Context) (string, error) {
	run, _ := common.RunFromContext(ctx)
	receiptRunId := run.RunId
	if run.Mode == common.RunModeReplay {
		// Strict replay serves the same step's completed source effect. It
		// cannot reattribute a file or borrow one from a different goal.
		if run.SourceRunId == "" || run.SourceGoalId == "" || memql.BareShortId(run.SourceGoalId) != memql.BareShortId(run.GoalId) {
			return "", fmt.Errorf("runAgentTurn: a replay file receipt requires its source run in the same goal")
		}
		receiptRunId = run.SourceRunId
	}
	res, err := i.engine.Execute(ctx, "query libraryFilesForOwner(runId: "+langparser.QuoteString(receiptRunId)+", stepKey: "+langparser.QuoteString(run.StepKey)+", status: \"ready\")")
	if err != nil {
		return "", fmt.Errorf("runAgentTurn: checking the saved file: %w", err)
	}
	for _, row := range memql.MaterializeRows(res) {
		if asString(row["status"]) == "ready" && asString(row["blobUrl"]) != "" &&
			memql.BareShortId(asString(row["ownerUserId"])) == memql.BareShortId(run.OwnerUserId) &&
			memql.BareShortId(asString(row["producedByRunId"])) == memql.BareShortId(receiptRunId) &&
			asString(row["producedByStepKey"]) == run.StepKey {
			if fileId := memql.BareShortId(asString(row["id"])); fileId != "" {
				return fileId, nil
			}
		}
	}
	return "", fmt.Errorf("runAgentTurn: the turn finished without saving a ready Library file for this work run and step")
}

// turnSeam is embedded on Integration; kept here beside its only users.
type turnSeam struct {
	turnMu     sync.RWMutex
	turnRunner AgentTurnRunner
}
