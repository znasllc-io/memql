package memql

// The Cluster > Automations reads (epic memql#5380, D-L; task memql#5384).
//
// Two builtins behind one MemQL OS section, and neither is a row read with a
// tier to gate it:
//
//   - automationGraph projects the static loop graph over the automations
//     THIS NODE'S SCHEDULER REGISTERED, one v1:platform:automationNode row per
//     automation. The scheduler builds and caches the rows
//     (component/automations/loop_graph_rows.go); the engine holds no
//     automation registry of its own, so it reads them through
//     AutomationGraphSource, the SetAutomationCataloger shape.
//   - automationLoopStops projects the latest runs the loop protection
//     stopped -- v1:work:run rows at errorCode loop_depth_exceeded -- into
//     v1:platform:automationLoopStop rows carrying the chain and the bound
//     and no payload.
//
// THE CAPABILITY IS THE WALL. Both declare
// @requiresCapability("read", "app:cluster/automations"), and
// evaluateBuiltinFunctionExpression refuses a caller without it BEFORE either
// executor runs. There is no second gate to fall back on: the graph is a
// projection of the scheduler, never persisted, so no @rowAuthz tier can see
// it, and the stops are read as the engine (below), so the caller's tier
// never meets them.
//
// ===========================================================================
// WHY THE STOPS ARE READ AS THE CLUSTER
// ===========================================================================
// A refused run is a system run: the journal writes it under a synthetic
// cluster actor, and rowauthz_nonprincipal_owner.go blanks the owner such an
// actor would stamp, so its owner is present and empty. On the composite
// owner tier v1:work:run declares, that row answers ZERO ROWS AND NO ERROR to
// every reader who is not a cluster owner -- and the capability is seeded for
// developer and admin as well as owner. Read under the caller, the band would
// say "no loop has been stopped" to exactly the people the capability admits.
//
// So the read runs under loopStopsReadContext, the arrangement
// readinessEvaluateContext already uses: a named synthetic cluster owner,
// unranked, stamped internal (the query is @serverOnly), which REPLACES the
// caller rather than adding to it. The query takes no argument, so nothing a
// caller supplies reaches it; what the caller can do is ask. And the reply is
// a projection under a concept of its own -- a v1:work:run node would be
// filtered out of a builtin's reply by the row gate for the same readers,
// and would carry the run's input, variables and triggering event besides.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/num"
)

// The canonical ids of the two virtual projections.
const (
	AutomationNodeConcept     = "v1:platform:automationNode"
	AutomationLoopStopConcept = "v1:platform:automationLoopStop"
)

// loopStopsQuery is the one read the stops builtin makes. It names the query
// and passes nothing, which is half of what makes the internal-origin stamp
// below safe on a request-derived path.
const loopStopsQuery = "query workRunsStoppedByLoops()"

// AutomationGraphSource supplies the static loop graph over the automations a
// node's scheduler registered, one row per automation
// (component/automations.LoopGraphRows documents the row).
//
// An ERROR is part of the contract, not a formality: a scheduler that has not
// registered its automations yet, or whose loader cannot see what they write,
// has no graph -- and an empty list would be an answer, "this node loaded no
// automations", which it is not.
//
// The rows may be shared between callers and must not be mutated.
type AutomationGraphSource interface {
	AutomationGraphRows() ([]map[string]any, error)
}

// SetAutomationGraphSource wires the scheduler the automationGraph builtin
// reads. Unset -- a binary that loads no scheduler -- the builtin refuses.
func (e *MemQLEngine) SetAutomationGraphSource(s AutomationGraphSource) {
	e.automationGraphSource = s
}

// evaluateAutomationGraphExpression is the automationGraph builtin: the
// source's rows as v1:platform:automationNode nodes, keyed and sorted by the
// automation's name. The capability was checked before this ran.
func (e *MemQLEngine) evaluateAutomationGraphExpression(_ context.Context) ([]memorynodes.MemoryNode, error) {
	if e == nil || e.automationGraphSource == nil {
		return nil, fmt.Errorf("automationGraph: this node runs no automation scheduler, so it has no automation graph to report")
	}
	rows, err := e.automationGraphSource.AutomationGraphRows()
	if err != nil {
		return nil, err
	}
	nodes := make([]memorynodes.MemoryNode, 0, len(rows))
	for _, row := range rows {
		// The row id IS the name: there is one row per registered automation,
		// and a builtin's reply is one id-keyed map, so a row with no name
		// would collapse into the next one that has none.
		name, _ := row["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return nil, fmt.Errorf("automationGraph: %s: %w", name, err)
		}
		nodes = append(nodes, memorynodes.MemoryNode{
			ID:      name,
			Concept: AutomationNodeConcept,
			Type:    memorynodes.NodeTypeObject,
			Payload: raw,
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes, nil
}

// evaluateAutomationLoopStopsExpression is the automationLoopStops builtin:
// the latest 50 stopped runs, projected. The capability was checked before
// this ran; the read below does not run as the caller.
func (e *MemQLEngine) evaluateAutomationLoopStopsExpression(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	read := e.loopStopRows
	if read == nil {
		read = e.readLoopStopRows
	}
	rows, err := read(ctx)
	if err != nil {
		return nil, err
	}
	nodes := make([]memorynodes.MemoryNode, 0, len(rows))
	for _, row := range rows {
		if node, ok := loopStopNode(row); ok {
			nodes = append(nodes, node)
		}
	}
	return nodes, nil
}

// readLoopStopRows runs workRunsStoppedByLoops as the cluster. The stamped
// context is a local here and is never returned, so it dies with this one
// Execute.
func (e *MemQLEngine) readLoopStopRows(ctx context.Context) ([]map[string]any, error) {
	result, err := e.Execute(loopStopsReadContext(ctx), loopStopsQuery)
	if err != nil {
		return nil, fmt.Errorf("automationLoopStops: %w", err)
	}
	return MaterializeRows(result), nil
}

// loopStopsReadContext is the context the stops read runs under: a named
// synthetic cluster owner, unranked and synthetic, stamped internal. It
// REPLACES the caller's actor, which is the property that makes stamping
// internal origin on a request-derived path safe -- no caller authority
// flows past this line, and the query it reaches takes no argument.
// Asserted by TestTheLoopStopsReadAsTheClusterNotTheCaller and
// TestTheStopsQueryIsServerOnlyAndTakesNoCallerArgument.
//
// Not auth.MaintenanceActor, for readinessEvaluateContext's reason: that
// constructor is keyed on a compiled-in list of AUTOMATION names, and a
// builtin is not one.
func loopStopsReadContext(ctx context.Context) context.Context {
	const actorId = "system:maintenance:automationLoopStops"
	claims := map[string]any{"sub": actorId, "email": actorId, "role": "system"}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: actorId,
		// RoleOwner buys the composite tier's cluster-owner escape, and the
		// query's `actor.isClusterOwner == true` conjunct.
		Role: auth.RoleOwner,
		// Not a principal: the rank rules do not govern it (D4, epic
		// memql#4832), and it can never be a row's owner.
		Unranked:  true,
		Synthetic: true,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

// loopStopNode projects one stopped run's stored row into a
// v1:platform:automationLoopStop node carrying exactly eight fields: runId,
// automationName, finishedAt, and outcome.loop's reason, depth, cap,
// correlationId and chain -- each chain link cut to {automation, runId}. The
// row's input, variables, triggering event and message go nowhere.
//
// A run that failed because a sub-automation it called was refused carries
// the code and no loop record of its own: its reason, depth, cap and
// correlation are ABSENT, never zero, and the page draws an em dash.
//
// A row with no id is dropped: the reply is one id-keyed map, and it would
// collapse with the next such row.
func loopStopNode(row map[string]any) (memorynodes.MemoryNode, bool) {
	runId := strings.TrimSpace(BareShortId(stringField(row, "id")))
	if runId == "" {
		return memorynodes.MemoryNode{}, false
	}
	payload := map[string]any{
		"runId":          runId,
		"automationName": stringField(row, "automationName"),
	}
	finished := strings.TrimSpace(stringField(row, "finishedAt"))
	if finished != "" {
		payload["finishedAt"] = finished
	}
	chain := []any{}
	outcome, _ := row["outcome"].(map[string]any)
	if loop, ok := outcome["loop"].(map[string]any); ok {
		if reason := strings.TrimSpace(stringField(loop, "reason")); reason != "" {
			payload["reason"] = reason
		}
		if depth, ok := loopFigure(loop["depth"]); ok {
			payload["depth"] = depth
		}
		if bound, ok := loopFigure(loop["cap"]); ok {
			payload["cap"] = bound
		}
		if correlation := strings.TrimSpace(stringField(loop, "correlationId")); correlation != "" {
			payload["correlationId"] = correlation
		}
		chain = loopChain(loop["chain"])
	}
	payload["chain"] = chain

	raw, err := json.Marshal(payload)
	if err != nil {
		return memorynodes.MemoryNode{}, false
	}
	return memorynodes.MemoryNode{
		ID:      runId,
		Concept: AutomationLoopStopConcept,
		Type:    memorynodes.NodeTypeObject,
		Payload: raw,
		// The SDK orders a builtin's nodes by createdAt, newest first, so a
		// stop is stamped with WHEN IT HAPPENED: its finishedAt, falling back
		// to when its row was written.
		CreatedAt: stopInstant(finished, stringField(row, "createdAt")),
	}, true
}

// loopChain is a stored chain cut to {automation, runId} per link, in order.
// A link that is not an object is dropped; nothing else a link might carry is
// passed on.
func loopChain(v any) []any {
	links, _ := v.([]any)
	out := make([]any, 0, len(links))
	for _, l := range links {
		link, ok := l.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"automation": stringField(link, "automation"),
			"runId":      stringField(link, "runId"),
		})
	}
	return out
}

// loopFigure reads a stored depth or bound, and whether one was stored at all:
// an absent or non-numeric value is (0, false), which the caller leaves out
// rather than writing a zero.
//
// narrowing: SATURATE -- a depth and a cap are orderings against each other,
// so a value too large for an int must still compare past the cap rather than
// wrap.
func loopFigure(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return num.ClampFloat64(n), true
	case int:
		return n, true
	case int64:
		return num.ClampInt64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return num.ClampInt64(i), true
		}
	}
	return 0, false
}

// stopInstant is the first of the given RFC 3339 instants that parses, or the
// zero time.
func stopInstant(values ...string) time.Time {
	for _, v := range values {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(v)); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
