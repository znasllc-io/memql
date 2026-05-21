// Package taskstamp owns the engine-side auto-stamping of v1:planner:task
// rows on every agent tool call (Q5 + Q6 in the planner-redesign brainstorm).
//
// The package is structured around two ideas:
//
//  1. PlanContext on the Go context carries enough state for the stamper
//     to attribute a tool call to its semantic parent: the active Plan id,
//     the executing semantic Task id (the toolInvocation's parentTaskId),
//     and the calling agent / owner / space coordinates needed to write
//     a synthetic Plan if no Plan exists yet (the ad-hoc-chat-tool-call
//     case).
//
//  2. The Stamper interface wraps the engine's existing tool-dispatch
//     primitive (engine.ExecuteToolByName). Callers that opt in get
//     Task rows emitted on every tool invocation; callers that don't
//     opt in see no behavior change. This lets Phase 2 ship without
//     touching every call site in the codebase.
//
// Use:
//
//	ctx = taskstamp.WithPlanContext(ctx, taskstamp.PlanContext{
//	    AgentId: "...", OwnerUserId: "...", SpaceId: "...",
//	    // PlanId + SemanticTaskId set by the caller when a real Plan
//	    // is being executed; left empty for ad-hoc chat tool calls
//	    // (the stamper materializes synthetic rows on first call).
//	})
//	result, err := stamper.ExecuteToolByName(ctx, toolName, args)
//
// What the stamper does on each call (when PlanContext is present):
//
//   - First call in this PlanContext where SemanticTaskId is empty:
//     materializes a synthetic Plan (kind='adHocAction') + semantic
//     Task wrapper. Updates the PlanContext on the returned context so
//     subsequent calls in the same turn reuse them.
//   - Every call: inserts a v1:planner:task row with category='toolInvocation',
//     parentTaskId = the semantic Task. Dispatches the actual tool call
//     through the underlying executor. Updates the row with the result
//     (status='succeeded' + toolResult) or error (status='failed' +
//     errorMessage) before returning.
//
// What the stamper does NOT do:
//
//   - It does not enforce policy. Authorization, scope checks, retry
//     thresholds -- all out of scope. The stamper is a passive recorder.
//   - It does not block on the row write. Best-effort: if the engine
//     mutation fails, the stamper logs and continues with the dispatch
//     so observability problems never break a user-facing tool call.
//   - It does not work for non-agent callers yet. Engine cron jobs,
//     server handlers calling tools synchronously, etc. don't pass
//     through PlanContext. Future phases migrate them.
package taskstamp

import (
	"context"
)

// PlanContext is the per-call orchestration metadata the stamper needs.
// Threaded through Go context.Context using WithPlanContext.
//
// Zero-value semantics: a PlanContext with empty PlanId AND empty
// SemanticTaskId is treated as "ad-hoc chat tool call from this agent;
// please materialize a synthetic Plan + semantic Task." A PlanContext
// with PlanId set but SemanticTaskId empty is treated as "real Plan is
// running but no semantic Task wraps this call yet; please materialize
// just the semantic Task" -- rare, but valid for the path where an
// agent is dispatched into a Plan with no decomposition yet.
type PlanContext struct {
	// PlanId is the v1:planner:plan.id this tool call executes under.
	// Empty when the call is ad-hoc (chat-driven, no user-initiated Plan).
	// The stamper materializes a synthetic kind='adHocAction' Plan on
	// first call when empty, then writes the new id back into the
	// PlanContext on the returned context for downstream reuse.
	PlanId string

	// SemanticTaskId is the v1:planner:task.id whose execution prompted
	// this tool call. Tool-invocation Task rows attach to it via their
	// parentTaskId field. Empty until the stamper (or the Planner Agent
	// in a future phase) materializes a semantic Task wrapper. Like
	// PlanId, the stamper backfills this on the returned context.
	SemanticTaskId string

	// AgentId is the v1:agents:agent.id of the agent making the tool
	// call. Required for synthetic Plan creation and for cross-row
	// attribution (the toolInvocation Task row carries no explicit
	// agentId field today; the audit trail keys on PlanId / parent
	// semantic Task / agent attribution lives one hop away).
	AgentId string

	// OwnerUserId is the v1:identity:user.id of the agent's owner --
	// used for synthetic Plan's requestedBy + ownerUserId fields when
	// the ad-hoc path fires.
	OwnerUserId string

	// SpaceId is the v1:cognition:space.id the agent is operating in.
	// Required for synthetic Plan attribution. The stamper does NOT
	// rewrite this for child Tasks -- they inherit via the Plan link.
	SpaceId string
}

// ctxKey is the Go context-key type for PlanContext lookup. Unexported
// so external packages can't collide with us by accident.
type ctxKey struct{}

// WithPlanContext returns a new context.Context carrying the provided
// PlanContext. The stamper reads this on dispatch; absent context = no
// stamping (fallthrough to the underlying executor).
func WithPlanContext(ctx context.Context, pc PlanContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxKey{}, pc)
}

// PlanContextFrom extracts the PlanContext, returning (zero, false) when
// the context carries none. Stamper implementations and tests both use
// this.
func PlanContextFrom(ctx context.Context) (PlanContext, bool) {
	if ctx == nil {
		return PlanContext{}, false
	}
	pc, ok := ctx.Value(ctxKey{}).(PlanContext)
	return pc, ok
}

// updatePlanContext is the package-internal helper that returns a new
// context with a mutated PlanContext. Used after synthetic Plan / Task
// materialization to backfill the ids so subsequent tool calls in the
// same turn reuse them.
func updatePlanContext(ctx context.Context, pc PlanContext) context.Context {
	return context.WithValue(ctx, ctxKey{}, pc)
}
