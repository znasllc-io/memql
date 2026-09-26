package agent

import (
	"context"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

const workExecutionDirective = `You are executing the current MemQL request now. Complete the requested work in this run. For a file or document, use composeFile to write it to the user's Library. Supply a descriptive name, the requested format (markdown by default), and a complete statement of the requested content; optionally provide your finished draft or source references. The call finishes the file before returning its outputFileId. Confirm success only after receiving that outputFileId. Do not use produceArtifact or open another goal to delegate this same work. A plain answer is appropriate only when the goal asks for an answer rather than a saved file.`

func isOwnedWorkExecution(ctx context.Context) bool {
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	return ok && run.IsRun() && run.GoalId != "" && run.OwnerUserId != "" && ac != nil && ac.UserId == run.OwnerUserId
}

// A background goal already owns the asynchronous work. Its turn needs the
// synchronous file capability, otherwise the chat delegation baseline starts
// a new goal and acknowledges work that this run never performed.
func (r *Replier) scopeWorkExecution(ctx context.Context, data map[string]any, names []string) []string {
	if !isOwnedWorkExecution(ctx) {
		return names
	}
	// Capacity is len(names); composeFile is appended only when present, so a
	// +1 here is what CodeQL flags as allocation-size overflow.
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name != produceArtifactToolName && name != "canvasPublish" && name != "composeFile" {
			out = append(out, name)
		}
	}
	if r.engine != nil && len(r.engine.ToolDefinitionsForNames([]string{"composeFile"})) > 0 {
		out = append(out, "composeFile")
	}
	for _, name := range []string{"discoverCapabilities", "executeCapability", "recallWorkHistory"} {
		if r.engine != nil && len(r.engine.ToolDefinitionsForNames([]string{name})) > 0 {
			out = append(out, name)
		}
	}
	data["productionDirective"] = workExecutionDirective
	if assistant, ok := data["assistant"].(map[string]any); ok {
		assistant["tools"] = out
	}
	return out
}
