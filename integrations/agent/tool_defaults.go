package agent

import (
	"context"

	"github.com/znasllc-io/memql/core/common"
)

// tool_defaults.go -- the defaults-delivery seam on the agent path (memql#3237).
//
// # The fork this closes
//
// `applyToolDefaults` (component/memql/tool_args.go, called from ExecuteTool)
// DELETES every field a tool declares `@autoInjected`, whether or not a server
// default exists to replace it. That is deliberate and correct -- it is the
// anti-forgery rule, pinned by
// TestApplyToolDefaults_AutoInjectedStripsEvenWhenNoDefaults:
//
//	Edge case: tool has @autoInjected fields but the runtime supplied no
//	defaults [...] we must still consult AutoInjectedFields and strip --
//	otherwise the LLM's forge survives.
//
// The problem was who supplies the defaults. `common.ContextWithToolDefaults`
// had exactly two production callers -- the voice CallTool hop
// (component/grpc/voice_agent_handlers.go) and cognition's engine tool loop
// (integrations/cognition/cognition_handler.go). This package was not among
// them, so on the agent path a server-stamped `@autoInjected` field was
// stripped at dispatch with NOTHING put back.
//
// That put any server-stamped argument on this path in a state where neither
// option was safe:
//
//   - WITHOUT `@autoInjected`, the field is caller-supplied, so an LLM-supplied
//     value survives -- forgeable.
//   - WITH `@autoInjected`, the field is deleted at dispatch with no default to
//     restore it -- fails closed 100% of the time.
//
// injectAgentContext stamps the values onto the args map, correctly and
// authoritatively. applyToolDefaults then deleted them a layer later, because
// a stamped arg and a forged arg are the same bytes by the time ExecuteTool
// sees them. The context is what distinguishes them, and the context carried
// nothing.
//
// # Why per-tool rather than one map for the turn
//
// The two existing producers stamp ONE defaults map for a whole session/turn,
// which is fine for them: the voice hop stamps three fields on a
// CallTool it already knows the shape of, and cognition stamps partitionId /
// participantId, which mean the same thing for every tool it dispatches.
//
// This path dispatches the agent's whole tool surface, where the same field
// name does NOT mean the same thing everywhere -- `space` vs `partitionId`, a
// flat `planId` vs one nested in a card's `data`, an `agentId` that is the
// calling agent vs one that is an argument naming some other agent. And
// applyToolDefaults' pass 2 fills ANY declared field a default names, not only
// the auto-injected ones, so a turn-wide map would quietly populate optional
// arguments on unrelated tools that the model deliberately left out.
//
// So the defaults are derived per call from agentContextStamps -- the same
// table injectAgentContext consults, so the two cannot disagree about what the
// runtime believes, which is the only failure mode a second table would add.

// agentToolDefaults returns the server-resolved defaults for one tool call:
// exactly the fields agentContextStamps says the runtime owns for that tool,
// with the values injectAgentContext would stamp.
//
// Empty when the tool takes no runtime context, or when the turn resolved none
// -- an empty turnContext must produce an empty map rather than a map of empty
// strings, because a default of "" would be RESTORED over the strip and reach
// the handler as a real (wrong) value, which is worse than the field being
// absent and the handler saying so.
//
// Deliberately flat: only the fields applyToolDefaults can act on. The nested
// and derived stamps (`actor`, `data.producedByPlanId`, the visibility pair)
// stay with injectAgentContext, which is also what keeps the overwrite
// semantics for fields NOT marked @autoInjected -- applyToolDefaults' pass 2 is
// fill-if-missing, so an LLM-supplied value would win there.
func agentToolDefaults(toolName string, turnCtx turnContext) map[string]any {
	stamp, ok := agentContextStamps[toolName]
	if !ok {
		return nil
	}
	defaults := map[string]any{}
	if stamp.StampAgentId && turnCtx.AgentId != "" {
		defaults["agentId"] = turnCtx.AgentId
	}
	if stamp.StampOwnerUserId && turnCtx.OwnerUserId != "" {
		defaults["ownerUserId"] = turnCtx.OwnerUserId
	}
	if stamp.SpaceField != "" && turnCtx.PartitionId != "" {
		defaults[stamp.SpaceField] = turnCtx.PartitionId
	}
	if stamp.StampPlanId && turnCtx.PlanId != "" {
		defaults["planId"] = turnCtx.PlanId
	}
	if stamp.StampProducedByPlanId && turnCtx.PlanId != "" {
		defaults["producedByPlanId"] = turnCtx.PlanId
	}
	if len(defaults) == 0 {
		return nil
	}
	return defaults
}

// agentToolCallContext returns the context one tool dispatch runs under.
//
// It is the delivery half of the pair: injectAgentContext puts the runtime's
// values on the args map, and this puts the same values where ExecuteTool's
// applyToolDefaults will look for them after it strips the auto-injected
// fields. Both are needed, and they do different jobs:
//
//   - injectAgentContext OVERWRITES, so a value the model forged for a field
//     that is not marked @autoInjected does not survive.
//   - the ctx defaults RESTORE, so a value the runtime stamped for a field that
//     IS marked @autoInjected is not lost to the anti-forgery strip.
//
// Returns ctx unchanged when the tool takes no runtime context, so a dispatch
// that never needed defaults does not acquire an empty map on its context.
func agentToolCallContext(ctx context.Context, toolName string, turnCtx turnContext) context.Context {
	defaults := agentToolDefaults(toolName, turnCtx)
	if len(defaults) == 0 {
		return ctx
	}
	return common.ContextWithToolDefaults(ctx, defaults)
}
