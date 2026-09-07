package planner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// mustJSON renders a test argument object the way the recorder does.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// observationRow builds one v1:work:observation the transcript reader sees --
// the shape integrations/work.RecordToolInvocation writes (memql#5050).
func observationRow(id, tool string, seq int, isError bool, args map[string]any) map[string]any {
	data := map[string]any{
		"tool":    tool,
		"seq":     float64(seq),
		"isError": isError,
	}
	if args != nil {
		data["args"] = mustJSON(args)
	}
	return map[string]any{"id": id, "kind": "tool_result", "data": data}
}

// TestRenderTranscriptAutomation_VerbatimCalls: the render emits one step per
// recorded call, in order, invoking the tool with its exact args.
func TestRenderTranscriptAutomation_VerbatimCalls(t *testing.T) {
	calls := []toolCall{
		{Name: "produceArtifact", Args: `{"goal":"A markdown file titled X"}`, Seq: 0},
		{Name: "workbenchHost", Args: `{"args":{"path":"x.md","content":"# X"}}`, Seq: 1},
	}
	src := renderTranscriptAutomation("reproduceR1", "Make a list", calls)

	if !strings.Contains(src, "automation reproduceR1 {") {
		t.Fatalf("missing automation header:\n%s", src)
	}
	// Story 9 (#2335): rendered in the named-args invocation form
	// `name(k: v, ...)`, not the legacy object-literal wrapper. Nested object
	// VALUES keep their JSON braces (keys sorted by json.Marshal).
	if !strings.Contains(src, `produceArtifact(goal: "A markdown file titled X")`) {
		t.Errorf("first call not rendered as named args:\n%s", src)
	}
	if !strings.Contains(src, `workbenchHost(args: {"content":"# X","path":"x.md"})`) {
		t.Errorf("second call not rendered as named args:\n%s", src)
	}
	if strings.Index(src, "step call0") > strings.Index(src, "step call1") {
		t.Errorf("steps out of order:\n%s", src)
	}
	if !strings.Contains(src, "@description(") {
		t.Errorf("missing description:\n%s", src)
	}
}

func TestTranscriptAutomationName_ValidCamelCase(t *testing.T) {
	got := transcriptAutomationName("v1:work:run:65857485-3e51-4839-bc4f-891d3a40c9d3")
	if !strings.HasPrefix(got, "reproduce") {
		t.Fatalf("want reproduce-prefixed name, got %q", got)
	}
	for _, r := range got {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			t.Fatalf("name %q has a non-identifier char %q", got, string(r))
		}
	}
}

// TestRunCaptureTranscript_PersistsLiteralCalls: the orchestration reads a
// RUN's tool_result observations and persists a bundle + one automation
// construct whose source is the verbatim transcript -- no LLM (no InvokeAI).
func TestRunCaptureTranscript_PersistsLiteralCalls(t *testing.T) {
	obs := []any{
		observationRow("o1", "workbenchHost", 1, false, map[string]any{"args": map[string]any{"path": "birds.md", "content": "# Birds"}}),
		// A non-tool observation -> excluded.
		map[string]any{"id": "o0", "kind": "note", "data": map[string]any{"seq": float64(0)}},
		// A FAILED call -> excluded: it is not part of the reproducible path.
		observationRow("ofail", "brokenTool", 2, true, nil),
	}
	var aiCalls int
	fe := &fakeEngine{
		aiResponder: func(string, map[string]any) (any, error) { aiCalls++; return nil, nil },
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "authoringBundleForRun"):
				return map[string]any{"output": []any{}}, nil
			case strings.Contains(query, "workObservationsForOwnerRun"):
				return map[string]any{"output": obs}, nil
			}
			return nil, nil
		},
	}
	d := NewAuthoringCaptureDispatcher(&PlannerAgentLoop{engine: fe, logger: authoringTestLogger()}, fe, authoringTestLogger())

	if err := d.runCaptureTranscript(context.Background(), "r-tr", "user-9", "analyzeFile"); err != nil {
		t.Fatalf("runCaptureTranscript: %v", err)
	}
	if aiCalls != 0 {
		t.Fatalf("transcription must make ZERO LLM calls, got %d", aiCalls)
	}

	exec, _, _ := fe.snapshot()
	if !anyCallContainsAll(exec, "createAuthoringBundle", `sourceRunId: "r-tr"`) {
		t.Errorf("must persist a bundle stamped with the source RUN; exec=%v", exec)
	}
	if !anyCallContainsAll(exec, "createAuthoringConstruct", `kind: "automation"`, "workbenchHost") {
		t.Errorf("construct must carry the verbatim workbenchHost call; exec=%v", exec)
	}
	if anyCallContainsAll(exec, "createAuthoringConstruct", "brokenTool") {
		t.Errorf("a failed tool call must not be transcribed; exec=%v", exec)
	}
	if !anyCallContainsAll(exec, "recordBundleValidation", `status: "validated"`, `"transcript":true`) {
		t.Errorf("transcript bundle must be marked validated+transcript; exec=%v", exec)
	}
}

// TestRunCaptureTranscript_ReadsUnderTheOwner pins the ACTOR the observations
// are read under.
//
// workObservationsForOwnerRun filters on ownerUserId==actor.userId, and its
// cluster-owner twin (workObservationsForRun) exists for the sweeps. Reading
// through the wrong one would let capture transcribe a run that is not the
// caller's; reading under no actor at all returns zero rows and no error,
// which presents as "this run made no tool calls".
func TestRunCaptureTranscript_ReadsUnderTheOwner(t *testing.T) {
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			if strings.Contains(query, "workObservationsForRun(") {
				t.Errorf("capture used the CLUSTER-OWNER observations query; it must use the owner-scoped one")
			}
			return map[string]any{"output": []any{}}, nil
		},
	}
	d := NewAuthoringCaptureDispatcher(&PlannerAgentLoop{engine: fe, logger: authoringTestLogger()}, fe, authoringTestLogger())
	if err := d.runCaptureTranscript(context.Background(), "r-actor", "user-9", "analyzeFile"); err != nil {
		t.Fatalf("runCaptureTranscript: %v", err)
	}
	exec, _, _ := fe.snapshot()
	if !anyCallContainsAll(exec, "workObservationsForOwnerRun") {
		t.Errorf("capture did not read the owner-scoped observations query; exec=%v", exec)
	}
}

// TestRunCaptureTranscript_UnownedRunSkips: a run with a present-and-empty
// owner is the deployment's own. Capturing it would write a bundle owned by
// nobody and readable by nobody.
func TestRunCaptureTranscript_UnownedRunSkips(t *testing.T) {
	fe := &fakeEngine{execResponder: func(string) (any, error) { return map[string]any{"output": []any{}}, nil }}
	d := NewAuthoringCaptureDispatcher(&PlannerAgentLoop{engine: fe, logger: authoringTestLogger()}, fe, authoringTestLogger())
	if err := d.runCaptureTranscript(context.Background(), "r-sys", "", "someSweep"); err != nil {
		t.Fatalf("runCaptureTranscript: %v", err)
	}
	exec, _, _ := fe.snapshot()
	if len(exec) != 0 {
		t.Errorf("an unowned run must read and write nothing; exec=%v", exec)
	}
}

// TestRunCaptureTranscript_TruncatedArgsAreNotRendered: a call whose arguments
// were cut at the byte bound renders `{}` rather than broken JSON.
//
// Cut JSON does not parse, so rendering it verbatim produces a bundle that
// cannot compile -- and Gate 1 would then report an AUTHORING failure for what
// is really missing evidence.
func TestRunCaptureTranscript_TruncatedArgsAreNotRendered(t *testing.T) {
	row := observationRow("o1", "workbenchHost", 0, false, map[string]any{"content": "x"})
	data := row["data"].(map[string]any)
	data["args"] = `{"content":"xxxx` // deliberately cut
	data["argsTruncated"] = true

	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "authoringBundleForRun"):
				return map[string]any{"output": []any{}}, nil
			case strings.Contains(query, "workObservationsForOwnerRun"):
				return map[string]any{"output": []any{row}}, nil
			}
			return nil, nil
		},
	}
	d := NewAuthoringCaptureDispatcher(&PlannerAgentLoop{engine: fe, logger: authoringTestLogger()}, fe, authoringTestLogger())
	if err := d.runCaptureTranscript(context.Background(), "r-cut", "user-9", "analyzeFile"); err != nil {
		t.Fatalf("runCaptureTranscript: %v", err)
	}
	exec, _, _ := fe.snapshot()
	if anyCallContainsAll(exec, "createAuthoringConstruct", `{"content":"xxxx`) {
		t.Errorf("a truncated argument set was rendered verbatim, producing unparseable MemQL; exec=%v", exec)
	}
	if !anyCallContainsAll(exec, "createAuthoringConstruct", "workbenchHost") {
		t.Errorf("the call itself must still be transcribed, with empty args; exec=%v", exec)
	}
}

// TestRunCaptureTranscript_NoCallsSkips: a run with no recorded tool calls
// produces no bundle (nothing concrete ran to transcribe).
func TestRunCaptureTranscript_NoCallsSkips(t *testing.T) {
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "authoringBundleForRun"):
				return map[string]any{"output": []any{}}, nil
			case strings.Contains(query, "workObservationsForOwnerRun"):
				return map[string]any{"output": []any{}}, nil
			}
			return nil, nil
		},
	}
	d := NewAuthoringCaptureDispatcher(&PlannerAgentLoop{engine: fe, logger: authoringTestLogger()}, fe, authoringTestLogger())
	if err := d.runCaptureTranscript(context.Background(), "r-empty", "user-9", "analyzeFile"); err != nil {
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
			obs := []any{observationRow("o1", "workbenchHost", 0, false, map[string]any{"args": map[string]any{"path": "x.md", "content": "# X"}})}
			fe := &fakeEngine{
				execResponder: func(query string) (any, error) {
					switch {
					case strings.Contains(query, "authoringBundleForRun"):
						return map[string]any{"output": []any{}}, nil
					case strings.Contains(query, "workObservationsForOwnerRun"):
						return map[string]any{"output": obs}, nil
					}
					return nil, nil
				},
			}
			ce := &fakeCaptureEngine{fakeEngine: fe, sandbox: &fakeSandbox{reports: []memql.SandboxReport{tc.report}}}
			d := NewAuthoringCaptureDispatcher(&PlannerAgentLoop{engine: ce, logger: authoringTestLogger()}, ce, authoringTestLogger())

			if err := d.runCaptureTranscript(context.Background(), "r-g1", "user-9", "analyzeFile"); err != nil {
				t.Fatalf("runCaptureTranscript: %v", err)
			}
			if len(ce.sandbox.calls) == 0 {
				t.Fatalf("Gate-1 compile was not invoked on the rendered transcript")
			}
			exec, _, _ := ce.snapshot()
			if !anyCallContainsAll(exec, "recordBundleValidation", `status: "validated"`, `"transcript":true`, `"gate1":"ran"`, tc.wantReRun) {
				t.Errorf("validation must record the gate1 verdict %s; exec=%v", tc.wantReRun, exec)
			}
		})
	}
}
