package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// =============================================================================
// The assistant / specialist tool boundary (#588)
// =============================================================================
//
// The chat path runs an orchestrator-worker pattern: a single ASSISTANT talks
// to the human, and zero or more SPECIALISTS do scoped work. This file is the
// TOOL half of that boundary, kept as PURE, table-testable helpers so the
// rules unit-test without a DB or an LLM:
//
//   - Only the assistant may emit a human-facing turn. The sentinel
//     respondToUser tool is present ONLY in the assistant's tool set;
//     specialists never get it (RoleAllowsTool / ScopeToolsForRole /
//     ScopeToolDefinitionsForRole).
//   - A deliverable surface narrows the set further
//     (ScopeToolsForDeliverableSurface), and a produceArtifact execution turn
//     cannot re-delegate to itself (IsProduceArtifactExecutionTurn).
//
// THE CONTEXT half of the boundary lived here too and is retired with the
// harness reconciler (work spine A1): BuildScopedSpecialistContext assembled a
// specialist's window from a v1:harness:step plus scoped recall, and
// AggregateSpecialistObservations folded v1:harness:observation rows back into
// one answer. Both concepts are gone. The property they enforced -- a
// specialist never sees the human transcript -- returns in epic A2 as a subrun
// step, where the child run's own journal is the scope by construction rather
// than by a builder that had to be remembered.

// HarnessRole names which side of the orchestrator-worker boundary an agent
// turn runs on. The zero value is RoleUnknown so a turn that forgot to
// declare its role is treated conservatively (no human-facing tool).
type HarnessRole string

const (
	// RoleUnknown is the zero value: a turn whose role was not declared.
	// Treated as a specialist for tool-gating purposes (no respondToUser),
	// so a misconfigured turn fails closed rather than leaking a
	// human-facing surface to a worker.
	RoleUnknown HarnessRole = ""
	// RoleAssistant is the orchestrator -- the ONLY role permitted to emit
	// a human-facing turn (respondToUser).
	RoleAssistant HarnessRole = "assistant"
	// RoleSpecialist is a worker executing a scoped step. It never talks to
	// the human; its results flow back as observations.
	RoleSpecialist HarnessRole = "specialist"
)

// HarnessRoleHintKey is the hint that carries the per-turn role across the
// wire (msg.Hints[HarnessRoleHintKey] = "assistant" | "specialist"). Absent
// or unrecognized values resolve to RoleAssistant for backwards compatibility
// with the existing single-agent chat path -- every legacy turn is an
// assistant turn talking to the human, so omitting the hint must NOT silently
// strip respondToUser from those turns. The boundary only tightens when a
// turn EXPLICITLY declares itself a specialist.
const HarnessRoleHintKey = "harness_role"

// ExecutionLaneHintKey is the hint that selects the AI execution lane for a
// turn (msg.Hints[ExecutionLaneHintKey]). It is orthogonal to the harness
// role: the role decides WHAT the turn may do (emit a human-facing reply or
// not), the lane decides HOW the model is driven (interactive streaming vs
// non-streaming request/response).
//
//   - "" / "interactive" -> the streaming tool loop + idle watchdog. A human
//     is watching tokens arrive (chat, voice). This is the default for every
//     legacy turn, so omitting the hint keeps the existing behavior.
//   - "background" -> the non-streaming tool loop (one request/response per
//     step, one overall timeout, no idle watchdog). Planner-dispatched
//     plan/task execution turns set this -- nobody is watching token-by-token,
//     and running background work through the interactive idle watchdog is
//     what false-killed slow produceArtifact turns (memql#893). (memql#896)
const ExecutionLaneHintKey = "execution_lane"

// ExecutionLaneBackground is the ExecutionLaneHintKey value that routes a
// turn onto the non-streaming background executor.
const ExecutionLaneBackground = "background"

// backgroundExecutionPolicy is the AI Router policy the background lane
// resolves against (memql#897). It gives batch/plan execution its own
// provider chain, tuned independently of the interactive chat policies, so
// background model selection (and, per memql#898, its model tier) can
// change without touching live chat. Defined in dsl/policies/policies.memql.
// memql#898 retuned it to a CHEAP default tier.
const backgroundExecutionPolicy = "backgroundExecution"

// backgroundEscalationPolicy is the strong/expensive AI Router policy the
// background executor swaps to mid-turn when the cheap backgroundExecution
// tier gets stuck (memql#898). Cheap-by-default, strong-on-demand: most
// routine deliverables finish on the cheap tier; only a turn that trips the
// stuck signal pays for the stronger model. Defined in
// dsl/policies/policies.memql.
const backgroundEscalationPolicy = "backgroundEscalation"

// ResumeHintKey marks a background dispatch as a RESUME of a previously
// passed/paused task (memql#907). When the planner re-admits a task whose
// slot freed up, it re-dispatches with hints[ResumeHintKey]="true"; the
// executor then loads the taskState persisted at the pause (memql#906) and
// seeds the turn with a resume-context block so the agent continues from
// the checkpoint instead of redoing completed work. Absent / "false" runs
// a normal fresh turn.
const ResumeHintKey = "resume"

// IsResume reports whether a turn's hints flag it as a resume dispatch.
func IsResume(hints map[string]string) bool {
	if hints == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(hints[ResumeHintKey]), "true")
}

// DeliverableSurfaceHintKey marks a turn whose deliverable must land on a
// specific surface. produceArtifact sets it to DeliverableSurfaceWorkbench so
// the executor delivers ONLY by writing the file to its workbench (promoted to
// the Library) -- the file body must never be dumped onto the canvas as a
// content card. The canvas is for events/notifications; a produceArtifact plan
// gets its short "ready in your Library" card from the carrier automation, not
// from the agent. (memql#950)
const DeliverableSurfaceHintKey = "deliverable_surface"

// DeliverableSurfaceWorkbench is the DeliverableSurfaceHintKey value that
// restricts a turn's deliverable surface to the workbench/Library and scopes
// canvas-publishing OUT of the turn's tool set.
const DeliverableSurfaceWorkbench = "workbench"

// canvasContentToolSlugs are the tools that render free-form CONTENT onto the
// canvas. They are scoped out when DeliverableSurfaceHintKey=workbench so a
// file deliverable's body can't leak onto the canvas (memql#950).
var canvasContentToolSlugs = map[string]bool{
	"canvasPublish": true,
}

// ScopeToolsForDeliverableSurface drops canvas content-publishing tools when
// the turn's deliverable is workbench-only. No-op for any other surface, so
// ordinary turns keep canvasPublish. (memql#950)
func ScopeToolsForDeliverableSurface(hints map[string]string, toolNames []string) []string {
	if hints == nil {
		return toolNames
	}
	if !strings.EqualFold(strings.TrimSpace(hints[DeliverableSurfaceHintKey]), DeliverableSurfaceWorkbench) {
		return toolNames
	}
	out := toolNames[:0:0]
	for _, name := range toolNames {
		if canvasContentToolSlugs[name] {
			continue
		}
		out = append(out, name)
	}
	return out
}

// produceArtifactToolName is the delegation tool the Assistant calls to mint a
// new produceArtifact plan (integrations/agents/integration.go,
// produceArtifactKind). A produceArtifact EXECUTOR turn must never call it again
// (that re-delegates and produces nothing -- memql#1133).
const produceArtifactToolName = "produceArtifact"

// IsProduceArtifactExecutionTurn reports whether THIS turn is itself the
// produceArtifact deliverable-execution turn -- i.e. the planner dispatched it
// to write the file directly to the workbench (hints[deliverable_surface]=
// workbench). On such a turn the acting plan IS already a kind=produceArtifact
// plan, so calling the produceArtifact delegation tool would spawn ANOTHER
// produceArtifact plan -> another executor turn -> the plan-level re-delegation
// loop that burned ~$13 in failed agentReply turns (memql#1133). This is the
// lineage signal the per-turn breaker (memql#1128) can't see: each iteration is
// a fresh plan/turn, so the breaker never observes a repeated failure within one
// turn. The same workbench-surface hint that scopes canvasPublish out (memql#950)
// is the authoritative "I am the executor" marker.
func IsProduceArtifactExecutionTurn(hints map[string]string) bool {
	if hints == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(hints[DeliverableSurfaceHintKey]), DeliverableSurfaceWorkbench)
}

// produceArtifactRedelegationError is the typed message fed back to the model in
// place of dispatching a produceArtifact tool call on a produceArtifact executor
// turn. It tells the model to do the work directly (write the file to the
// workbench) rather than re-delegate. No plan is minted. (memql#1133)
const produceArtifactRedelegationError = "you are already executing this produceArtifact deliverable -- " +
	"write the file directly to your workbench with the workbenchHost tool (action \"fs_write\") " +
	"and end your turn; do NOT call produceArtifact (re-delegating spawns another plan and produces nothing)"

// IsBackgroundLane reports whether a turn's hints select the background
// (non-streaming) execution lane.
func IsBackgroundLane(hints map[string]string) bool {
	if hints == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(hints[ExecutionLaneHintKey]), ExecutionLaneBackground)
}

// ResolveHarnessRole maps a hint value to a HarnessRole. Unknown / empty
// values resolve to RoleAssistant (the legacy human-facing chat path).
// Only an explicit "specialist" enters the constrained worker path.
func ResolveHarnessRole(hint string) HarnessRole {
	switch strings.ToLower(strings.TrimSpace(hint)) {
	case string(RoleSpecialist):
		return RoleSpecialist
	case string(RoleAssistant):
		return RoleAssistant
	default:
		// Legacy / undeclared turns are assistant turns: the single-agent
		// chat path predates the harness and must keep its human-facing
		// surface.
		return RoleAssistant
	}
}

// IsSpecialist reports whether the role is a constrained worker.
func (r HarnessRole) IsSpecialist() bool { return r == RoleSpecialist }

// RoleAllowsTool reports whether an agent in the given role may hold the
// named tool. The only role-gated tool today is the human-facing reply
// sentinel: respondToUser is assistant-only. Every other tool is allowed for
// both roles (per-specialist scoping narrows the set further via the planner's
// ScopedTools, but that is a relevance decision, not a boundary one).
//
// This is the single source of truth the boundary tests assert against.
func RoleAllowsTool(role HarnessRole, toolName string) bool {
	if strings.TrimSpace(toolName) == RespondToUserToolName {
		return role == RoleAssistant
	}
	return true
}

// ScopeToolsForRole filters a tool-name list to those the role may hold.
// Order is preserved; the only thing it can remove today is respondToUser
// from a non-assistant role. A specialist's tool set is therefore GUARANTEED
// to exclude the human-facing reply tool, regardless of what the caller
// passed in.
func ScopeToolsForRole(role HarnessRole, toolNames []string) []string {
	out := make([]string, 0, len(toolNames))
	for _, n := range toolNames {
		if RoleAllowsTool(role, n) {
			out = append(out, n)
		}
	}
	return out
}

// ScopeToolDefinitionsForRole is the ToolDefinition counterpart of
// ScopeToolsForRole: it drops any definition the role may not hold. Used at
// the point the replier builds the concrete tool schemas passed to the
// provider, so a specialist's wire tool set physically cannot carry
// respondToUser even if it was appended unconditionally upstream.
func ScopeToolDefinitionsForRole(role HarnessRole, tools []common.ToolDefinition) []common.ToolDefinition {
	out := make([]common.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if RoleAllowsTool(role, t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// =============================================================================
// Scoped context builder
// =============================================================================

// recallMemoryContent extracts the human-readable line from a recalled row.
// Observations + semantic memories both carry their embedding source on
// `content`; fall back to `text` then `goal` for other recall sources.
func recallMemoryContent(m map[string]any) string {
	for _, key := range []string{"content", "text", "goal"} {
		if v, ok := m[key].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// renderSortedFields serializes a flat map in key-sorted order as "key: value"
// lines. Nested values are rendered with %v -- the specialist sees the shape,
// not a pretty-printed tree. Deterministic ordering keeps parallel windows
// identical for identical inputs.
func renderSortedFields(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(": ")
		fmt.Fprintf(&b, "%v", m[k])
		b.WriteString("\n")
	}
	return b.String()
}

// =============================================================================
// Results aggregation (specialist observations -> assistant answer)
// =============================================================================
