package planner

import "testing"

// Acceptance: the happy path (artifact produced) verifies deterministically
// with ZERO LLM calls; an LLM check is requested only on the defined
// subjective branch.

func TestVerifyTaskResult_FailureIsDeterministic(t *testing.T) {
	v := verifyTaskResult(taskResultView{Status: "failed", ErrorMessage: "boom"})
	if !v.Deterministic || v.OK || v.NeedsLLM {
		t.Fatalf("a failed task must deterministically fail (feeds retry), got %+v", v)
	}
	// errorMessage alone (status not set) is also a deterministic fail.
	v2 := verifyTaskResult(taskResultView{ErrorMessage: "disk full"})
	if !v2.Deterministic || v2.OK || v2.NeedsLLM {
		t.Fatalf("an errorMessage must deterministically fail, got %+v", v2)
	}
}

func TestVerifyTaskResult_ArtifactIsDeterministicPassNoLLM(t *testing.T) {
	// The produceArtifact happy path: a Library/Document id in output.
	for _, key := range []string{"documentId", "artifactId", "libraryItemId", "filename", "writtenPath", "url"} {
		v := verifyTaskResult(taskResultView{Status: "succeeded", Output: map[string]any{key: "x"}})
		if !v.Deterministic || !v.OK || v.NeedsLLM {
			t.Fatalf("artifact via %q must be a deterministic pass with NO LLM, got %+v", key, v)
		}
	}
	// Empty-string artifact value is NOT a signal.
	v := verifyTaskResult(taskResultView{Status: "succeeded", Output: map[string]any{"documentId": ""}})
	if v.NeedsLLM {
		t.Fatalf("an empty artifact id must not (by itself) trigger an LLM check; got %+v", v)
	}
}

func TestVerifyTaskResult_ToolResultSignalIsDeterministic(t *testing.T) {
	okCases := []map[string]any{
		{"ok": true}, {"success": true}, {"status": "ok"}, {"status": "completed"},
	}
	for _, tr := range okCases {
		v := verifyTaskResult(taskResultView{Status: "succeeded", ToolResult: tr})
		if !v.Deterministic || !v.OK || v.NeedsLLM {
			t.Fatalf("toolResult %v must deterministically pass, got %+v", tr, v)
		}
	}
	failCases := []map[string]any{
		{"ok": false}, {"error": "nope"}, {"status": "failed"},
	}
	for _, tr := range failCases {
		v := verifyTaskResult(taskResultView{Status: "succeeded", ToolResult: tr})
		if !v.Deterministic || v.OK || v.NeedsLLM {
			t.Fatalf("toolResult %v must deterministically fail, got %+v", tr, v)
		}
	}
}

func TestVerifyTaskResult_SubjectiveEscalatesToLLM(t *testing.T) {
	// A subjective result with no deterministic signal is the ONE branch
	// that requests an LLM verification.
	v := verifyTaskResult(taskResultView{Status: "succeeded", Subjective: true})
	if v.Deterministic || !v.NeedsLLM {
		t.Fatalf("a subjective result with no deterministic signal must escalate to LLM, got %+v", v)
	}
	// ...but a subjective task that ALSO produced a concrete artifact is
	// settled deterministically (artifact check wins) -- no LLM.
	v2 := verifyTaskResult(taskResultView{Status: "succeeded", Subjective: true, Output: map[string]any{"documentId": "d1"}})
	if v2.NeedsLLM {
		t.Fatalf("a subjective task with a concrete artifact must NOT need an LLM, got %+v", v2)
	}
}

func TestVerifyTaskResult_ObjectiveSuccessNoLLM(t *testing.T) {
	// Objective success, no fault, nothing subjective -> accept without
	// burning an LLM call to rubber-stamp it.
	v := verifyTaskResult(taskResultView{Status: "succeeded"})
	if !v.Deterministic || !v.OK || v.NeedsLLM {
		t.Fatalf("objective success must pass deterministically with no LLM, got %+v", v)
	}
}

func TestTaskResultViewFromRow_SemanticIsSubjectiveOnlyWithoutArtifact(t *testing.T) {
	// A semantic task with no artifact -> subjective (LLM-eligible).
	sem := taskResultViewFromRow(map[string]any{"category": "semantic", "status": "succeeded"})
	if !sem.Subjective {
		t.Fatalf("a semantic task with no artifact should be subjective")
	}
	// A semantic task that wrote a file -> objectively verifiable.
	semFile := taskResultViewFromRow(map[string]any{
		"category": "semantic", "status": "succeeded",
		"output": map[string]any{"writtenPath": "/x/out.md"},
	})
	if semFile.Subjective {
		t.Fatalf("a semantic task with a concrete artifact is objectively verifiable, not subjective")
	}
	// A toolInvocation task is never subjective.
	tool := taskResultViewFromRow(map[string]any{"category": "toolInvocation", "status": "succeeded"})
	if tool.Subjective {
		t.Fatalf("a toolInvocation task must not be subjective")
	}
}
