package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// =============================================================================
// Context isolation / subagent boundary (#588, epic #590)
// =============================================================================
//
// The harness runs an orchestrator-worker pattern: a single ASSISTANT talks
// to the human, and zero or more SPECIALISTS execute scoped steps. This file
// enforces the boundary between the two roles as a set of PURE, table-testable
// helpers so the rules unit-test without a DB or an LLM:
//
//   1. Only the assistant may emit a human-facing turn. The sentinel
//      respondToUser tool is present ONLY in the assistant's tool set;
//      specialists never get it (RoleAllowsTool / ScopeToolsForRole).
//
//   2. Specialists run with a CLEAN, SCOPED context: role prompt + the step's
//      input + partition-scoped recall() for the step's topic. The raw human
//      conversation transcript NEVER enters a specialist's window
//      (BuildScopedSpecialistContext).
//
//   3. Specialists return RESULTS as observations (recorded via
//      mutationRecordHarnessObservation by the reconciler), not as messages to
//      the human. The assistant aggregates those observations into the single
//      human-facing answer (AggregateSpecialistObservations).
//
// Because each specialist's context is assembled from disjoint inputs (its own
// step + its own scoped recall), two specialists are parallel-safe: nothing in
// the build path is shared mutable state, so concurrent builds cannot
// cross-contaminate each other's windows.

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

// SpecialistStep is the minimal projection of a v1:harness:step a specialist
// needs to run: its identity, its role prompt, its task input, and the topic
// used to scope recall. It deliberately carries NO conversation transcript --
// the boundary is enforced by what this struct can express, not just by what
// the builder chooses to read.
type SpecialistStep struct {
	// StepID is the v1:harness:step.id (provenance for observations).
	StepID string
	// PlanID is the v1:harness:plan.id the step hangs off.
	PlanID string
	// RolePrompt is the specialist's persona / instructions -- the system
	// message for its window. This is the ONLY free-form instruction the
	// specialist sees; it does not include the human conversation.
	RolePrompt string
	// Title is a short human-readable label for the step (logging / the
	// recall topic fallback).
	Title string
	// Input is the step's structured input payload (v1:harness:step.input):
	// the request the specialist must act on. Rendered into the user message.
	Input map[string]any
	// RecallTopic is the free-text query used to pull partition-scoped
	// memories relevant to THIS step. Falls back to Title when empty.
	RecallTopic string
}

// recallTopicOrFallback returns the explicit recall topic, or the step title
// when none was supplied.
func (s SpecialistStep) recallTopicOrFallback() string {
	if t := strings.TrimSpace(s.RecallTopic); t != "" {
		return t
	}
	return strings.TrimSpace(s.Title)
}

// RecallFn is the partition-scoped recall hook (#585). It returns a slice of
// memory rows (each a flat map of the recalled payload) relevant to the topic.
// Injecting it as a func keeps BuildScopedSpecialistContext free of any DB /
// LLM dependency so the boundary is unit-testable: tests pass a stub that
// returns canned memories (or none).
type RecallFn func(ctx context.Context, topic string, k int) ([]map[string]any, error)

// ScopedContextOptions tunes the scoped builder.
type ScopedContextOptions struct {
	// RecallK is the top-k memories to pull for the step's topic. <=0 uses
	// defaultSpecialistRecallK.
	RecallK int
	// Recall is the partition-scoped recall hook. Nil disables the recall
	// section entirely (the context is then role + input only) -- a missing
	// memory substrate must never leak the transcript as a substitute.
	Recall RecallFn
}

// defaultSpecialistRecallK bounds how much memory a specialist pulls per step.
const defaultSpecialistRecallK = 8

// BuildScopedSpecialistContext assembles the message window for a specialist
// turn. The window is, in order:
//
//	[system] role prompt
//	[system] partition-scoped recall() block for the step's topic (if any)
//	[user]   the step's input payload
//
// CRITICAL BOUNDARY: the raw human conversation transcript is NEVER part of
// this window. The function has no parameter that could carry it, and it reads
// only the SpecialistStep + the scoped recall hook. The assertion test
// (subagent_test.go) proves a known transcript phrase is absent from every
// produced message.
//
// The build path touches no shared mutable state, so concurrent calls for two
// different steps are parallel-safe.
func BuildScopedSpecialistContext(ctx context.Context, step SpecialistStep, opts ScopedContextOptions) ([]common.ChatMessage, error) {
	rolePrompt := strings.TrimSpace(step.RolePrompt)
	if rolePrompt == "" {
		return nil, fmt.Errorf("scoped context: step %q has no role prompt", step.StepID)
	}

	messages := make([]common.ChatMessage, 0, 3)
	messages = append(messages, common.ChatMessage{Role: "system", Content: rolePrompt})

	// Partition-scoped recall block. Only added when a hook is wired AND it
	// returns memories. A nil hook or an empty result yields no recall
	// section -- and never a transcript fallback.
	if opts.Recall != nil {
		k := opts.RecallK
		if k <= 0 {
			k = defaultSpecialistRecallK
		}
		topic := step.recallTopicOrFallback()
		if topic != "" {
			memories, err := opts.Recall(ctx, topic, k)
			if err != nil {
				return nil, fmt.Errorf("scoped context: recall for step %q: %w", step.StepID, err)
			}
			if block := renderRecallBlock(memories); block != "" {
				messages = append(messages, common.ChatMessage{Role: "system", Content: block})
			}
		}
	}

	messages = append(messages, common.ChatMessage{
		Role:    "user",
		Content: renderStepInput(step),
	})
	return messages, nil
}

// renderStepInput renders a specialist's task input into the user message.
// It states the task plainly and serializes the structured input fields in a
// stable (key-sorted) order so two specialists with the same input produce
// byte-identical windows (parallel-safe, cacheable).
func renderStepInput(step SpecialistStep) string {
	var b strings.Builder
	b.WriteString("TASK")
	if t := strings.TrimSpace(step.Title); t != "" {
		b.WriteString(": ")
		b.WriteString(t)
	}
	b.WriteString("\n")
	if len(step.Input) > 0 {
		b.WriteString("\nINPUT\n")
		b.WriteString(renderSortedFields(step.Input))
	}
	b.WriteString("\nReturn your result; do not address the user directly.")
	return b.String()
}

// renderRecallBlock renders recalled memories into a single system block.
// Returns "" when there are no memories so the caller can skip the section.
func renderRecallBlock(memories []map[string]any) string {
	if len(memories) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("RELEVANT MEMORY (recalled for this step)\n")
	for _, m := range memories {
		content := recallMemoryContent(m)
		if content == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(content)
		b.WriteString("\n")
	}
	rendered := b.String()
	// If every memory was content-less, treat as empty.
	if strings.TrimSpace(strings.TrimPrefix(rendered, "RELEVANT MEMORY (recalled for this step)")) == "" {
		return ""
	}
	return strings.TrimRight(rendered, "\n")
}

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

// SpecialistObservation is the minimal projection of a v1:harness:observation
// the assistant reads to synthesize its human-facing answer. Specialists write
// these (via the reconciler's mutationRecordHarnessObservation); the assistant
// never reads a specialist's raw window -- only its recorded observations.
type SpecialistObservation struct {
	// StepID identifies which step produced the observation (provenance).
	StepID string
	// Kind is the observation kind: tool_result / error / note / decision.
	Kind string
	// Content is the text rendering of what happened (the embedding source).
	Content string
}

// AggregateSpecialistObservations folds a set of specialist observations into
// a single block the assistant injects into its own context before composing
// the human-facing reply. Observations are grouped by step (stable, sorted)
// so the assistant sees one coherent results section per specialist rather
// than an interleaved stream. Empty / content-less observations are skipped.
//
// This is the ONLY channel specialist results reach the assistant: results
// flow observation -> graph -> assistant, never specialist -> human.
func AggregateSpecialistObservations(obs []SpecialistObservation) string {
	byStep := make(map[string][]SpecialistObservation)
	order := make([]string, 0)
	for _, o := range obs {
		if strings.TrimSpace(o.Content) == "" {
			continue
		}
		if _, seen := byStep[o.StepID]; !seen {
			order = append(order, o.StepID)
		}
		byStep[o.StepID] = append(byStep[o.StepID], o)
	}
	if len(order) == 0 {
		return ""
	}
	sort.Strings(order)

	var b strings.Builder
	b.WriteString("SPECIALIST RESULTS\n")
	for _, step := range order {
		b.WriteString("\nStep ")
		b.WriteString(step)
		b.WriteString(":\n")
		for _, o := range byStep[step] {
			b.WriteString("- ")
			if k := strings.TrimSpace(o.Kind); k != "" {
				b.WriteString("[")
				b.WriteString(k)
				b.WriteString("] ")
			}
			b.WriteString(strings.TrimSpace(o.Content))
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
