// agent_loop_verify.go
//
// Result-verification structure (epic #836 / memql#841).
//
// Defines WHO checks a Task/phase result and HOW, explicitly, with a
// deterministic-FIRST policy:
//
//  1. Deterministic checks run first and cover the common cases for free:
//     - the Task errored                       -> fail (feeds retry)
//     - the Task produced a concrete artifact   -> pass (NO LLM)
//       (a Library/Document/artifact id, a written file, non-empty output)
//     - a tool invocation returned ok / not-ok  -> pass / fail (NO LLM)
//  2. An LLM verification is used ONLY when the result is inherently
//     SUBJECTIVE and a deterministic check can't speak to it (e.g. "is
//     this summary good?") -- a single, defined escalation branch on a
//     cheap model.
//  3. A failed verification feeds the bounded retry/replan path, never an
//     unbounded loop (the loop's existing caps bound retries).
//
// The decision is a PURE function over a projected result view so the
// policy is unit-testable without an engine and the happy path provably
// makes ZERO extra LLM verification calls.
package planner

// taskResultView is the decision-relevant projection of a completed Task.
type taskResultView struct {
	// Status is the Task's terminal status ("succeeded" / "failed" / ...).
	Status string
	// ErrorMessage is set when the Task failed.
	ErrorMessage string
	// Output is the Task's output payload (per-kind shape).
	Output map[string]any
	// ToolResult is the captured tool-call result for toolInvocation Tasks
	// (empty for semantic Tasks). A non-nil ToolResult with ok==false is a
	// deterministic failure.
	ToolResult map[string]any
	// Subjective marks a result whose correctness can't be settled by a
	// deterministic check (e.g. the quality of a written summary). Only
	// such results may escalate to an LLM verifier.
	Subjective bool
}

// verifyVerdict is the verification decision.
type verifyVerdict struct {
	// OK is the deterministic pass/fail when Deterministic is true.
	OK bool
	// Deterministic is true when a deterministic check settled the result
	// (no LLM needed). When false, NeedsLLM indicates whether to escalate.
	Deterministic bool
	// NeedsLLM is true only on the defined escalation branch: the result
	// is subjective and no deterministic signal applies.
	NeedsLLM bool
	// Reason is a short human-readable explanation.
	Reason string
}

// artifactOutputKeys are output fields whose presence means a concrete
// deliverable landed -- a deterministic PASS with no LLM check.
var artifactOutputKeys = []string{
	"documentId", "artifactId", "libraryItemId", "recordId",
	"filename", "filePath", "path", "writtenPath", "url",
}

// hasConcreteArtifact reports whether the output carries a concrete
// deliverable signal (a Library/Document id, a written file path, etc.).
func hasConcreteArtifact(output map[string]any) bool {
	if output == nil {
		return false
	}
	for _, k := range artifactOutputKeys {
		if v, ok := output[k]; ok {
			if s, isStr := v.(string); isStr {
				if s != "" {
					return true
				}
				continue
			}
			if v != nil {
				return true
			}
		}
	}
	return false
}

// toolReturnedOK inspects a captured tool result for an explicit
// ok / success / error signal. Returns (settled, ok): settled=false when
// the tool result carries no recognizable signal.
func toolReturnedOK(toolResult map[string]any) (settled bool, ok bool) {
	if toolResult == nil {
		return false, false
	}
	// Explicit error fields -> deterministic failure.
	for _, k := range []string{"error", "errorMessage", "err"} {
		if v, present := toolResult[k]; present {
			if s, isStr := v.(string); isStr && s != "" {
				return true, false
			}
		}
	}
	// Explicit ok / success boolean.
	for _, k := range []string{"ok", "success", "succeeded"} {
		if v, present := toolResult[k]; present {
			return true, asBool(v)
		}
	}
	// A "status" string of ok/success/done.
	if s, isStr := toolResult["status"].(string); isStr {
		switch s {
		case "ok", "success", "succeeded", "done", "completed":
			return true, true
		case "error", "failed", "failure":
			return true, false
		}
	}
	return false, false
}

// verifyTaskResult is the PURE deterministic-first verification decision.
// It NEVER calls an LLM; instead it reports whether a deterministic check
// settled the result and, only when none did and the result is
// subjective, that an LLM check is warranted (NeedsLLM). Callers run the
// cheap-model LLM verifier only on the NeedsLLM branch.
func verifyTaskResult(v taskResultView) verifyVerdict {
	// 1. An explicit failure is a deterministic fail (-> bounded retry).
	if v.Status == "failed" || v.ErrorMessage != "" {
		return verifyVerdict{Deterministic: true, OK: false, Reason: "task reported failure / error message"}
	}
	// 2. A tool invocation with an explicit ok/error signal is settled.
	if settled, ok := toolReturnedOK(v.ToolResult); settled {
		reason := "tool returned an explicit error"
		if ok {
			reason = "tool returned ok"
		}
		return verifyVerdict{Deterministic: true, OK: ok, Reason: reason}
	}
	// 3. A concrete artifact landed -> deterministic pass, no LLM. This is
	//    the happy path for produceArtifact (a file in the Library).
	if hasConcreteArtifact(v.Output) {
		return verifyVerdict{Deterministic: true, OK: true, Reason: "concrete artifact present in output"}
	}
	// 4. Subjective result with no deterministic signal -> the ONE defined
	//    escalation branch: a cheap-model LLM check.
	if v.Subjective {
		return verifyVerdict{Deterministic: false, NeedsLLM: true, Reason: "subjective result; escalate to cheap-model verification"}
	}
	// 5. Default: succeeded with no fault signal and nothing subjective to
	//    judge -> accept deterministically (don't burn an LLM call to
	//    rubber-stamp an objective success).
	return verifyVerdict{Deterministic: true, OK: true, Reason: "objective success; no LLM verification needed"}
}

// taskResultViewFromRow projects a (shape-flattened) Task row into a
// taskResultView. A Task kind of "semantic" (free-form reasoning output)
// is treated as subjective; "toolInvocation" is objective (its
// ToolResult carries the signal).
func taskResultViewFromRow(task map[string]any) taskResultView {
	view := taskResultView{
		Status:       getString(task, "status"),
		ErrorMessage: getString(task, "errorMessage"),
	}
	if out, ok := task["output"].(map[string]any); ok {
		view.Output = out
	}
	if tr, ok := task["toolResult"].(map[string]any); ok {
		view.ToolResult = tr
	}
	// Semantic tasks produce subjective prose; tool invocations are
	// objective (deterministic via toolResult / artifacts).
	if getString(task, "category") == "semantic" || getString(task, "kind") == "semantic" {
		// Only subjective if there's no concrete artifact to check
		// deterministically -- a semantic task that wrote a file is still
		// objectively verifiable.
		view.Subjective = !hasConcreteArtifact(view.Output)
	}
	return view
}
