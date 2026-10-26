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

Ordinary generated documents go to the user's Library unless another destination is requested. For a Library document deliverable, use composeFile to write it. Supply a descriptive name, the requested format (markdown by default), and a complete statement of the requested content; optionally provide your finished draft or source references. The call finishes the Library file before returning its outputFileId. Compound formats automatically include a reproducible source ZIP, returned as sourceFileId; link both files when present. A default document style is provided when no template was selected, so ordinary Library document requests do not need a template search. Confirm that Library file is saved only after receiving its outputFileId.

For other artifacts, discover the appropriate capability and its exact contract. Images use an image-generation operation, never composeFile with PNG. Discover current Fleet model capabilities as well as installed operations: image understanding (vision) is distinct from image generation. A busy or offline generator is temporarily unavailable, not a permanent platform limitation. Execution rechecks model routing, sharing and policy. Read referenced saved files before resolving phrases such as "the first one"; a previous answer's summary does not prove the file's contents. Return the actual saved file reference, never invent a path or claim success before storage confirms it.

Capability claims must come from current registered contracts and live evidence. A failed search, a partial catalog or this model's training does not prove absence. Never invent system instructions or policy prohibitions. If evidence is unavailable, say what could not be checked; when corrected, check rather than doubling down.

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
	// Repository requests use the installed bounded tools and their existing
	// role/authority gates. Function discovery does not enumerate tool aliases.
	for _, name := range []string{"discoverCapabilities", "executeCapability", "consultWorkKnowledge", "artifactSearch", "recallMemory", "recallWorkHistory", "navigateOS", "requestUserFeedback", "workerStatus", "requestComputerUseScope", "workerHost", "workerComputer", "workbenchHost", "inspectRepositoryAuthority", "requestRepositoryChange", "repositoryChangeRequestStatus", "listRepositoryChangeRequests"} {
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
