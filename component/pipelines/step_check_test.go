package pipelines

import (
	"strings"
	"testing"
)

func TestStepCheckName(t *testing.T) {
	cases := []struct {
		runCheck string
		step     StepReport
		want     string
	}{
		{"MemQL / memql", StepReport{Key: "checks.build", Stage: "checks", Name: "build"}, "MemQL / memql / checks / build"},
		{"MemQL / memql", StepReport{Key: "tests.go-unit#2", Stage: "tests", Name: "go-unit"}, "MemQL / memql / tests / go-unit #2"},
		{"MemQL / memql / push", StepReport{Key: "checks.build", Stage: "checks", Name: "build"}, "MemQL / memql / push / checks / build"},
		// A name with no stage or name of its own still names the step.
		{"MemQL / shop", StepReport{Key: "lint"}, "MemQL / shop / lint"},
	}
	for _, c := range cases {
		if got := StepCheckName(c.runCheck, c.step); got != c.want {
			t.Errorf("StepCheckName(%q, %+v) = %q, want %q", c.runCheck, c.step, got, c.want)
		}
	}
	// A step check can never be the run's own context, nor the reserved
	// review check, whatever the manifest names it.
	for _, s := range []StepReport{{Key: "x", Stage: "", Name: ""}, {Key: "review", Stage: "review", Name: "review"}} {
		name := StepCheckName("MemQL / memql", s)
		if name == "MemQL / memql" || strings.EqualFold(name, IndependentReviewCheckName) {
			t.Errorf("step %+v named %q, which is a reserved context", s, name)
		}
	}
}

func TestStepCheckState(t *testing.T) {
	cases := map[string][2]string{
		"pending":   {"queued", ""},
		"running":   {"in_progress", ""},
		"succeeded": {"completed", "success"},
		"failed":    {"completed", "failure"},
		"refused":   {"completed", "failure"},
		"cancelled": {"completed", "cancelled"},
		"skipped":   {"completed", "skipped"},
		"":          {"queued", ""},
	}
	for status, want := range cases {
		gotStatus, gotConclusion := StepCheckState(status)
		if gotStatus != want[0] || gotConclusion != want[1] {
			t.Errorf("StepCheckState(%q) = %q, %q; want %q, %q", status, gotStatus, gotConclusion, want[0], want[1])
		}
	}
}

func TestStepCheckOutput(t *testing.T) {
	title, summary, _ := StepCheckOutput(StepReport{Key: "checks.build", Status: "succeeded", DurationMs: 52000})
	if title != "Passed in 52s" || !strings.Contains(summary, "`checks.build`") {
		t.Errorf("a passed step reads %q / %q", title, summary)
	}
	title, summary, text := StepCheckOutput(StepReport{Key: "tests.go-unit#2", Status: "failed", DurationMs: 460000,
		Code: "pipeline_step_failed", Message: "go test exited 1 in component/memql", LogTail: "--- FAIL: TestX\nFAIL\n"})
	if !strings.HasPrefix(title, "Failed: go test exited 1") || !strings.Contains(summary, "`pipeline_step_failed`") {
		t.Errorf("a failed step reads %q / %q", title, summary)
	}
	if !strings.Contains(text, "--- FAIL: TestX") || !strings.Contains(text, "```") {
		t.Errorf("a failed step's text does not hold its output in a code block: %q", text)
	}
	title, _, _ = StepCheckOutput(StepReport{Key: "tests.x", Status: "skipped", Code: CodeStageBlocked, Message: "Not run: stage checks failed."})
	if title != "Skipped: Not run: stage checks failed." {
		t.Errorf("a skipped step reads %q", title)
	}
	for status, want := range map[string]string{"running": "Running", "pending": "Queued", "cancelled": "Cancelled"} {
		if title, _, _ := StepCheckOutput(StepReport{Key: "k", Status: status}); title != want {
			t.Errorf("a %s step reads %q, want %q", status, title, want)
		}
	}
	// Reported text cannot become Markdown in the summary.
	_, summary, _ = StepCheckOutput(StepReport{Key: "k", Status: "failed", Message: "**bold** <img src=x>"})
	if strings.Contains(summary, "**bold**") || !strings.Contains(summary, "\\<img") {
		t.Errorf("a message reached the summary as Markdown: %q", summary)
	}
}

func TestStepCheckExternalID(t *testing.T) {
	id := StepCheckExternalID("8228700fd1edb0e39510f4375e76df11", "tests.go-unit#2")
	if id != "8228700fd1edb0e39510f4375e76df11:tests.go-unit#2" {
		t.Fatalf("StepCheckExternalID = %q", id)
	}
	run, key, ok := StepFromExternalID(id)
	if !ok || run != "8228700fd1edb0e39510f4375e76df11" || key != "tests.go-unit#2" {
		t.Errorf("StepFromExternalID(%q) = %q, %q, %v", id, run, key, ok)
	}
	if _, _, ok := StepFromExternalID("8228700fd1edb0e39510f4375e76df11"); ok {
		t.Error("a run's own external id read as a step's")
	}
}
