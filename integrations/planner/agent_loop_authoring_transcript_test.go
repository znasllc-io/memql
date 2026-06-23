package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// TestRenderTranscriptAutomation_VerbatimCalls: the render emits one step per
// recorded call, in order, invoking the tool with its exact args.
func TestRenderTranscriptAutomation_VerbatimCalls(t *testing.T) {
	calls := []toolCall{
		{Name: "produceArtifact", Args: `{"goal":"A markdown file titled X"}`, Seq: 0},
		{Name: "workbenchHost", Args: `{"args":{"path":"x.md","content":"# X"}}`, Seq: 1},
	}
	src := renderTranscriptAutomation("reproduceP1", "Make a list", calls)

	if !strings.Contains(src, "automation reproduceP1 {") {
		t.Fatalf("missing automation header:\n%s", src)
	}
	if !strings.Contains(src, `produceArtifact({"goal":"A markdown file titled X"})`) {
		t.Errorf("first call not rendered verbatim:\n%s", src)
	}
	if !strings.Contains(src, `workbenchHost({"args":{"path":"x.md","content":"# X"}})`) {
		t.Errorf("second call not rendered verbatim:\n%s", src)
	}
	// Order preserved: call0 before call1.
	if strings.Index(src, "step call0") > strings.Index(src, "step call1") {
		t.Errorf("steps out of order:\n%s", src)
	}
	if !strings.Contains(src, "@description(") {
		t.Errorf("missing description:\n%s", src)
	}
}

func TestTranscriptAutomationName_ValidCamelCase(t *testing.T) {
	got := transcriptAutomationName("v1:planner:plan:65857485-3e51-4839-bc4f-891d3a40c9d3")
	if !strings.HasPrefix(got, "reproduce") {
		t.Fatalf("want reproduce-prefixed name, got %q", got)
	}
	// No separators -- a valid identifier (letters/digits only after the prefix).
	for _, r := range got {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			t.Fatalf("name %q has a non-identifier char %q", got, string(r))
		}
	}
}

// TestRunCaptureTranscript_PersistsLiteralCalls: the orchestration reads a
// plan's toolInvocation rows and persists a bundle + one automation construct
// whose source is the verbatim transcript -- no LLM (no InvokeAI).
func TestRunCaptureTranscript_PersistsLiteralCalls(t *testing.T) {
	plan := capturePlanRow("p-tr", "user-9", "Make a list of birds")
	tasks := []any{
		map[string]any{"id": "t1", "category": "toolInvocation", "status": "succeeded", "toolName": "workbenchHost", "toolArgs": map[string]any{"args": map[string]any{"path": "birds.md", "content": "# Birds"}}, "seq": float64(1)},
		map[string]any{"id": "t0", "category": "semantic", "status": "succeeded", "kind": "callTool", "seq": float64(0)},             // not a tool call -> excluded
		map[string]any{"id": "tfail", "category": "toolInvocation", "status": "failed", "toolName": "brokenTool", "seq": float64(2)}, // failed -> excluded
	}
	var aiCalls int
	fe := &fakeEngine{
		aiResponder: func(string, map[string]any) (any, error) { aiCalls++; return nil, nil },
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "authoringBundleForPlan"):
				return map[string]any{"output": []any{}}, nil
			case strings.Contains(query, "planById"):
				return map[string]any{"output": []any{plan}}, nil
			case strings.Contains(query, "tasksForPlan"):
				return map[string]any{"output": tasks}, nil
			}
			return nil, nil
		},
	}
	d := NewAuthoringCaptureDispatcher(&PlannerAgentLoop{engine: fe, logger: authoringTestLogger()}, fe, authoringTestLogger())

	if err := d.runCaptureTranscript(context.Background(), "p-tr", "produceArtifact"); err != nil {
		t.Fatalf("runCaptureTranscript: %v", err)
	}
	if aiCalls != 0 {
		t.Fatalf("transcription must make ZERO LLM calls, got %d", aiCalls)
	}

	_, _, _ = fe.snapshot()
	exec, _, _ := fe.snapshot()
	if !anyCallContainsAll(exec, "createAuthoringBundle", `"sourcePlanId":"p-tr"`) {
		t.Errorf("must persist a bundle stamped with the source plan; exec=%v", exec)
	}
	// One automation construct, source = the verbatim workbenchHost call.
	if !anyCallContainsAll(exec, "createAuthoringConstruct", `"kind":"automation"`, "workbenchHost") {
		t.Errorf("construct must carry the verbatim workbenchHost call; exec=%v", exec)
	}
	// The failed tool + the semantic row must NOT appear in the transcript.
	if anyCallContainsAll(exec, "createAuthoringConstruct", "brokenTool") {
		t.Errorf("a failed tool call must not be transcribed; exec=%v", exec)
	}
	if !anyCallContainsAll(exec, "recordBundleValidation", `"status":"validated"`, `"transcript":true`) {
		t.Errorf("transcript bundle must be marked validated+transcript; exec=%v", exec)
	}
}

// TestRunCaptureTranscript_NoCallsSkips: a plan with no tool-call rows produces
// no bundle (nothing concrete ran to transcribe).
func TestRunCaptureTranscript_NoCallsSkips(t *testing.T) {
	plan := capturePlanRow("p-empty", "user-9", "Did nothing")
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "authoringBundleForPlan"):
				return map[string]any{"output": []any{}}, nil
			case strings.Contains(query, "planById"):
				return map[string]any{"output": []any{plan}}, nil
			case strings.Contains(query, "tasksForPlan"):
				return map[string]any{"output": []any{}}, nil
			}
			return nil, nil
		},
	}
	d := NewAuthoringCaptureDispatcher(&PlannerAgentLoop{engine: fe, logger: authoringTestLogger()}, fe, authoringTestLogger())
	if err := d.runCaptureTranscript(context.Background(), "p-empty", "produceArtifact"); err != nil {
		t.Fatalf("runCaptureTranscript: %v", err)
	}
	exec, _, _ := fe.snapshot()
	if anyCallContainsAll(exec, "createAuthoringBundle") {
		t.Errorf("no tool calls => no bundle; exec=%v", exec)
	}
}

// TestRunCaptureTranscript_Gate1ReRunnable: when the Gate-1 sandbox is linked,
// the transcript runs the rendered automation through real compile+bind and
// records the verdict -- reRunnable:true on a clean compile, false otherwise --
// while still storing the transcript as a validated RECORD either way. (#1195)
func TestRunCaptureTranscript_Gate1ReRunnable(t *testing.T) {
	cases := []struct {
		name      string
		report    memql.SandboxReport
		wantReRun string
	}{
		{"compiles", memql.SandboxReport{OK: true}, `"reRunnable":true`},
		{"doesNotCompile", memql.SandboxReport{OK: false}, `"reRunnable":false`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := capturePlanRow("p-g1", "user-9", "Make a list")
			tasks := []any{
				map[string]any{"id": "t1", "category": "toolInvocation", "status": "succeeded", "toolName": "workbenchHost", "toolArgs": map[string]any{"args": map[string]any{"path": "x.md", "content": "# X"}}, "seq": float64(0)},
			}
			fe := &fakeEngine{
				execResponder: func(query string) (any, error) {
					switch {
					case strings.Contains(query, "authoringBundleForPlan"):
						return map[string]any{"output": []any{}}, nil
					case strings.Contains(query, "planById"):
						return map[string]any{"output": []any{plan}}, nil
					case strings.Contains(query, "tasksForPlan"):
						return map[string]any{"output": tasks}, nil
					}
					return nil, nil
				},
			}
			ce := &fakeCaptureEngine{fakeEngine: fe, sandbox: &fakeSandbox{reports: []memql.SandboxReport{tc.report}}}
			d := NewAuthoringCaptureDispatcher(&PlannerAgentLoop{engine: ce, logger: authoringTestLogger()}, ce, authoringTestLogger())

			if err := d.runCaptureTranscript(context.Background(), "p-g1", "produceArtifact"); err != nil {
				t.Fatalf("runCaptureTranscript: %v", err)
			}
			if len(ce.sandbox.calls) == 0 {
				t.Fatalf("Gate-1 compile was not invoked on the rendered transcript")
			}
			exec, _, _ := ce.snapshot()
			if !anyCallContainsAll(exec, "recordBundleValidation", `"status":"validated"`, `"transcript":true`, `"gate1":"ran"`, tc.wantReRun) {
				t.Errorf("validation must record the gate1 verdict %s; exec=%v", tc.wantReRun, exec)
			}
		})
	}
}

// TestCaptureMode_DefaultTranscript: default is transcript; only an explicit
// "author" flips to the legacy LLM path.
func TestCaptureMode_DefaultTranscript(t *testing.T) {
	t.Setenv("MEMQL_AUTHORING_CAPTURE_MODE", "")
	if captureMode() != "transcript" {
		t.Errorf("default capture mode must be transcript")
	}
	t.Setenv("MEMQL_AUTHORING_CAPTURE_MODE", "author")
	if captureMode() != "author" {
		t.Errorf("explicit author mode not honored")
	}
}
