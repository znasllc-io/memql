package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// resume.go -- executor-side resume-from-checkpoint for the background lane
// (memql#907, epic #902). The Session-A slice: when the planner re-admits a
// passed task (its slot now free), the background turn loads the taskState
// persisted at the pause (memql#906) and seeds itself with a "here's what
// you already did, continue from here" block so the agent doesn't redo
// completed work. The slot re-admission / re-queue-if-at-cap logic is
// Session B's (#902 controller + #905 queue); this file only restores the
// working context into the turn.

// resumeContextMaxToolEntries bounds how many prior tool calls we replay
// into the prompt so a long pre-pause history doesn't blow the context.
const resumeContextMaxToolEntries = 20

// resumeContextMaxResultChars truncates each replayed tool result so the
// resume block stays compact.
const resumeContextMaxResultChars = 240

// loadResumeContext reads the latest persisted taskState for the given
// task (keyed by planId per the #906 contract) and formats it into a prompt
// block. Returns ("", false) when there's no saved state, the lookup
// errors, or the state carries nothing useful -- the caller then just runs
// a fresh turn. Best-effort: a resume failure must never break the turn.
func (r *Replier) loadResumeContext(ctx context.Context, taskId string) (string, bool) {
	taskId = strings.TrimSpace(taskId)
	if taskId == "" || r.engine == nil {
		return "", false
	}
	q := fmt.Sprintf(`queryTaskStateById({taskId:%q})`, taskId)
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		r.logger.Debug("agent background: resume taskState lookup failed",
			"task_id", taskId, "error", err)
		return "", false
	}
	row := latestTaskStateRow(res)
	if row == nil {
		return "", false
	}
	block := formatResumeContext(row)
	if block == "" {
		return "", false
	}
	return block, true
}

// latestTaskStateRow unpacks queryTaskStateById's shape() result and returns
// the most recently-created row (taskState is versioned one-row-per-persist;
// the #906 writer uses a deterministic stateId so today there's exactly one,
// but pick the newest by createdAt defensively). Returns nil on no match.
func latestTaskStateRow(res any) map[string]any {
	resMap, ok := res.(map[string]any)
	if !ok {
		return nil
	}
	var rows []map[string]any
	switch data := resMap["data"].(type) {
	case map[string]any:
		rows = append(rows, data)
	case []any:
		for _, item := range data {
			if m, ok := item.(map[string]any); ok {
				rows = append(rows, m)
			}
		}
	case []map[string]any:
		rows = data
	}
	var latest map[string]any
	var latestAt string
	for _, row := range rows {
		createdAt, _ := row["createdAt"].(string)
		if latest == nil || createdAt > latestAt { // RFC3339 sorts lexically
			latest = row
			latestAt = createdAt
		}
	}
	return latest
}

// formatResumeContext renders a taskState row into the resume prompt block.
// Empty when the row carries neither reasoning nor tool history.
func formatResumeContext(row map[string]any) string {
	reasoning := strings.TrimSpace(stringFromAny(row["reasoningChain"]))
	history := toolHistoryEntries(row["toolCallHistory"])
	if reasoning == "" && len(history) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("RESUMING A PAUSED TASK. You were preempted at a checkpoint and are now continuing where you left off. Do NOT restart the work or repeat steps you already completed below.")
	if reasoning != "" {
		b.WriteString("\n\nWhat you had done so far:\n")
		b.WriteString(reasoning)
	}
	if len(history) > 0 {
		b.WriteString("\n\nTool calls already executed (do not repeat these):")
		for i, e := range history {
			if i >= resumeContextMaxToolEntries {
				fmt.Fprintf(&b, "\n- ...and %d earlier call(s) omitted", len(history)-resumeContextMaxToolEntries)
				break
			}
			b.WriteString("\n- ")
			b.WriteString(e)
		}
	}
	b.WriteString("\n\nContinue from this point and finish the task.")
	return b.String()
}

// toolHistoryEntries formats the persisted toolCallHistory ([]{toolName,
// args, result}) into compact one-line summaries.
func toolHistoryEntries(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		name := stringFromAny(m["toolName"])
		if name == "" {
			continue
		}
		line := name + "(" + truncateForLog(stringFromAny(m["args"]), 120) + ")"
		if res := strings.TrimSpace(stringFromAny(m["result"])); res != "" {
			line += " -> " + truncateForLog(res, resumeContextMaxResultChars)
		}
		out = append(out, line)
	}
	return out
}

// injectResumeContext inserts the resume block as a user-role message right
// after the system prompt (messages[0]), so the agent sees the prior
// progress up front while the rest of the history (the plan goal, etc.)
// follows. If there's no leading system message it prepends instead.
func injectResumeContext(messages []common.ChatMessage, block string) []common.ChatMessage {
	if strings.TrimSpace(block) == "" {
		return messages
	}
	resumeMsg := common.ChatMessage{Role: "user", Content: block}
	if len(messages) > 0 && messages[0].Role == "system" {
		out := make([]common.ChatMessage, 0, len(messages)+1)
		out = append(out, messages[0])
		out = append(out, resumeMsg)
		out = append(out, messages[1:]...)
		return out
	}
	return append([]common.ChatMessage{resumeMsg}, messages...)
}

// stringFromAny coerces a JSON-decoded value to a string for prompt
// formatting: strings pass through; everything else uses %v so a structured
// args/result object still renders something readable.
func stringFromAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
