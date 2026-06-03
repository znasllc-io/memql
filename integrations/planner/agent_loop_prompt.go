// agent_loop_prompt.go
//
// Lean prompt inputs for the plannerAgent call (memql#820).
//
// The decompose loop used to hand the plannerAgent prompt the RAW rows
// for plan / tasks / specialists. The specialist rows
// (agentMinimalForDedupe) each carry a `roleEmbedding` -- a full
// float vector the LLM cannot read and that serializes to thousands of
// tokens -- plus `lineage` provenance and other dedupe-only fields. The
// plan + task rows carry large input/output/refinementContext blobs.
// With N agents and 5 iterations per cycle, that bloat is what blew
// Anthropic's 800k-input-tokens/min limit.
//
// These helpers project each input down to ONLY the fields the planner
// reasons over (names, roles, skills/tools, descriptions, statuses,
// goal, phases). The structured-output schema still sees `object` /
// `[]object`, so nothing downstream changes -- the calls just get cheap.
package planner

const (
	// maxDescriptionChars bounds a specialist's description in the
	// prompt; full descriptions can run long and add up across a
	// candidate set.
	maxDescriptionChars = 320
	// maxGoalChars bounds the plan goal echoed into the prompt.
	maxGoalChars = 1200
	// maxErrorChars bounds a task's errorMessage echoed into the prompt.
	maxErrorChars = 600
)

// specialistPromptFields is the whitelist of specialist fields the
// planner needs to pick / dedupe an agent. Everything else (notably
// roleEmbedding + lineage) is dropped.
var specialistPromptFields = []string{
	"id", "name", "role", "roleSlug", "kind", "capabilities",
}

// planPromptFields is the whitelist of plan fields the planner reasons
// over. Large per-kind blobs (input / output / refinementContext /
// estimate / metrics) are intentionally omitted -- they are not part of
// the next-decision reasoning and dominate the payload.
var planPromptFields = []string{
	"id", "kind", "status", "ownerAgentId", "retryThreshold",
	"phases", "feedbackResponse", "tokenBudget", "tokenSpent",
}

// taskPromptFields is the whitelist of task fields the planner needs to
// see what has been attempted. The (potentially large) input/output
// blobs are dropped; errorMessage is kept (truncated) because the
// planner branches on failures.
var taskPromptFields = []string{
	"id", "kind", "status", "logicalStepId", "phase", "seq", "attemptNumber",
}

// compactSpecialistsForPrompt projects each specialist down to the
// decision-relevant fields, dropping the roleEmbedding vector + lineage
// + any other heavy fields. Description is kept but truncated.
func compactSpecialistsForPrompt(specialists []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(specialists))
	for _, s := range specialists {
		c := pickFields(s, specialistPromptFields)
		if desc, ok := s["description"].(string); ok && desc != "" {
			c["description"] = truncate(desc, maxDescriptionChars)
		}
		out = append(out, c)
	}
	return out
}

// compactPlanForPrompt projects the plan down to the next-decision
// fields and truncates the goal.
func compactPlanForPrompt(plan map[string]any) map[string]any {
	c := pickFields(plan, planPromptFields)
	if goal, ok := plan["goal"].(string); ok && goal != "" {
		c["goal"] = truncate(goal, maxGoalChars)
	}
	return c
}

// compactTasksForPrompt projects each task down to status-relevant
// fields, dropping input/output blobs and truncating errorMessage.
func compactTasksForPrompt(tasks []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		c := pickFields(t, taskPromptFields)
		if em, ok := t["errorMessage"].(string); ok && em != "" {
			c["errorMessage"] = truncate(em, maxErrorChars)
		}
		out = append(out, c)
	}
	return out
}

// pickFields returns a new map containing only the named keys that are
// present (and non-nil) on src.
func pickFields(src map[string]any, keys []string) map[string]any {
	c := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := src[k]; ok && v != nil {
			c[k] = v
		}
	}
	return c
}
