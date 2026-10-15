package agent

import (
	"context"
	"slices"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

const workExecutionDirective = `You are executing the current MemQL request now. Complete this section's declared work and outputs in this run, using the requested destination and authorized capabilities.

A research-only section returns evidence, source references and any requested draft for its next step; it does not create a final deliverable assigned to a later section. A section assigned repository, source-code or test-file changes must perform those changes through the authorized computer or repository tools and verify the required results. Being a section does not make it research-only. Do not substitute a Library document for a requested repository change. All writes and commands remain subject to existing permissions and run limits.

Ordinary generated documents go to the user's Library unless another destination is requested. For a Library deliverable, use composeFile to write it. Supply a descriptive name, the requested format (markdown by default), and a complete statement of the requested content; optionally provide your finished draft or source references. The call finishes the Library file before returning its outputFileId. Compound formats automatically include a reproducible source ZIP, returned as sourceFileId; link both files when present. A default document style is provided when no template was selected, so ordinary Library document requests do not need a template search. Confirm that Library file is saved only after receiving its outputFileId.

Do not use produceArtifact or open another goal to delegate this same work. An answer-only goal needs no file; deliver its answer using the supplied response contract.`

func isOwnedWorkExecution(ctx context.Context) bool {
	run, ok := common.RunFromContext(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	return ok && run.IsRun() && run.GoalId != "" && run.OwnerUserId != "" && ac != nil && memql.BareShortId(ac.UserId) == memql.BareShortId(run.OwnerUserId)
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
	for _, name := range []string{"discoverCapabilities", "executeCapability", "recallMemory", "recallWorkHistory", "navigateOS", "requestUserFeedback", "workerStatus", "requestComputerUseScope", "workerHost", "workerComputer", "workbenchHost"} {
		if r.engine != nil && !slices.Contains(out, name) && len(r.engine.ToolDefinitionsForNames([]string{name})) > 0 {
			out = append(out, name)
		}
	}
	data["productionDirective"] = workExecutionDirective
	if assistant, ok := data["assistant"].(map[string]any); ok {
		assistant["tools"] = out
	}
	return out
}
