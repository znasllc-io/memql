package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// recording_test.go -- an app's calls back into MemQL are part of its
// recording (epic memql#5396, task memql#5399).
//
// TO CONFIRM THESE ARE LOAD-BEARING: drop the isAppSessionTool guard and the
// exclusion test fails; drop the appSessionId check and the browser test does.

type captureRecorder struct {
	mu    sync.Mutex
	calls []AppSessionToolCall
}

func (c *captureRecorder) RecordToolCall(_ context.Context, call AppSessionToolCall) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
	return nil
}

func (c *captureRecorder) recorded() []AppSessionToolCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]AppSessionToolCall, len(c.calls))
	copy(out, c.calls)
	return out
}

func recordingCtx(r AppSessionRecorder) context.Context {
	return withMCPAppSessionRecorder(context.Background(), r)
}

// TestAToolCallUnderAnAppSessionBearerWritesAnMCPStep -- issue #5399's first
// acceptance. The harness sees an opaque MCP result and the replica running
// the session never sees the call at all, so the node that DOES see it is the
// only one that can record it.
func TestAToolCallUnderAnAppSessionBearerWritesAnMCPStep(t *testing.T) {
	rec := &captureRecorder{}
	eng := newFakeEngine()
	args := map[string]any{"name": "librarySearch", "args": map[string]any{"q": "budget"}}

	callMCPTool(recordingCtx(rec), eng, "assistant", TierAuthoring, "v1:worker:appSession:s1", toolRunQuery, args)

	calls := rec.recorded()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls, want 1 (calls: %+v)", len(calls), calls)
	}
	got := calls[0]
	if got.SessionId != "v1:worker:appSession:s1" {
		t.Errorf("SessionId = %q, want the one the credential named", got.SessionId)
	}
	if got.Tool != toolRunQuery {
		t.Errorf("Tool = %q, want %q", got.Tool, toolRunQuery)
	}
	// D5: an action is only reproducible from its arguments.
	inner, _ := got.Args["args"].(map[string]any)
	if got.Args["name"] != "librarySearch" || inner["q"] != "budget" {
		t.Errorf("Args = %v, want the call verbatim", got.Args)
	}
	if got.ResultDigest == "" || got.ResultType == "" {
		t.Errorf("the result must be recorded as a digest and a type: %+v", got)
	}
}

// TestSubmitAndNextTaskAreExcluded -- issue #5399's first acceptance, second
// half. They are the session's PROTOCOL, not its work: recording them would
// put two steps in every timeline describing the session's own plumbing, and
// a procedure lifted from that timeline would try to replay them.
//
// IT ASSERTS THE EXCLUSION TWICE, AND THE SECOND HALF IS THE ONE THAT
// MEASURES. Running the negative control on the first half -- deleting the
// isAppSessionTool guard in recordAppSessionToolCall -- left it GREEN, because
// callMCPTool dispatches the back-channel pair and RETURNS before the deferred
// recording is even registered. So the dispatch ORDER is what actually
// excludes them today, and a test that only went through callMCPTool would go
// on passing if that order were ever rearranged. The direct sub-test is what
// holds the guard itself.
func TestSubmitAndNextTaskAreExcluded(t *testing.T) {
	t.Run("through the dispatcher", func(t *testing.T) {
		for _, name := range []string{toolSubmit, toolNextTask} {
			rec := &captureRecorder{}
			callMCPTool(recordingCtx(rec), newFakeEngine(), "assistant", TierAuthoring,
				"v1:worker:appSession:s1", name, map[string]any{"result": map[string]any{"ok": true}})
			if calls := rec.recorded(); len(calls) != 0 {
				t.Errorf("%s was recorded: %+v", name, calls)
			}
		}
	})

	t.Run("at the recorder itself", func(t *testing.T) {
		for _, name := range []string{toolSubmit, toolNextTask} {
			rec := &captureRecorder{}
			recordAppSessionToolCall(recordingCtx(rec), "v1:worker:appSession:s1", name,
				map[string]any{"result": map[string]any{"ok": true}}, textResult("{}"))
			if calls := rec.recorded(); len(calls) != 0 {
				t.Errorf("%s was recorded: %+v", name, calls)
			}
		}
	})

	// The control for both: an ordinary tool through the same direct path IS
	// recorded, so a guard that refused everything would not pass this.
	rec := &captureRecorder{}
	recordAppSessionToolCall(recordingCtx(rec), "v1:worker:appSession:s1", toolRunQuery,
		map[string]any{"name": "librarySearch"}, textResult("{}"))
	if len(rec.recorded()) != 1 {
		t.Fatal("an ordinary tool was not recorded through the direct path, so the two sub-tests above prove nothing")
	}
}

// TestACallWithNoAppSessionRecordsNothing. Every other caller -- a browser, a
// PAT, a service account -- has no session to record into, and inventing one
// would put a step on somebody else's run.
func TestACallWithNoAppSessionRecordsNothing(t *testing.T) {
	rec := &captureRecorder{}
	callMCPTool(recordingCtx(rec), newFakeEngine(), "assistant", TierAuthoring, "", toolRunQuery,
		map[string]any{"name": "librarySearch"})
	if calls := rec.recorded(); len(calls) != 0 {
		t.Fatalf("a browser's tool call was recorded against a session: %+v", calls)
	}
}

// TestANodeWithNoRecorderStillServesTheTool. The recording is added to a live
// path; a node without it must behave exactly as it did before.
func TestANodeWithNoRecorderStillServesTheTool(t *testing.T) {
	res := callMCPTool(context.Background(), newFakeEngine(), "assistant", TierAuthoring,
		"v1:worker:appSession:s1", toolRunQuery, map[string]any{"name": "librarySearch"})
	if res == nil {
		t.Fatal("the tool returned nothing without a recorder wired")
	}
}

// TestAFailedToolCallIsStillRecorded. A recording that only kept the calls
// that worked would make every lifted procedure look more reliable than the
// session it came from.
func TestAFailedToolCallIsStillRecorded(t *testing.T) {
	rec := &captureRecorder{}
	// An unknown meta-tool argument: run_query with no name fails in-band.
	callMCPTool(recordingCtx(rec), newFakeEngine(), "assistant", TierAuthoring,
		"v1:worker:appSession:s1", toolRunQuery, map[string]any{})

	calls := rec.recorded()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls, want the failure recorded too", len(calls))
	}
	if !calls[0].IsError {
		t.Errorf("the failure was recorded as a success: %+v", calls[0])
	}
	if strings.TrimSpace(calls[0].Error) == "" {
		t.Error("a failed call must record why")
	}
}

// TestTheResultTypeNamesTheShape. A shadow replay compares deterministic
// results exactly and varying ones BY INFERRED TYPE (design D15), so the type
// has to distinguish the shapes it claims to.
func TestTheResultTypeNamesTheShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result map[string]any
		want   string
	}{
		{"an object", textResult(`{"rows":[]}`), "object"},
		{"an array", textResult(`[1,2,3]`), "array"},
		{"a sentence", textResult(`nothing matched`), "string"},
		{"nothing at all", textResult(``), "empty"},
		{"no content block", map[string]any{}, "empty"},
	} {
		if got := inferResultType(tc.result); got != tc.want {
			t.Errorf("%s: inferResultType = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestTwoIdenticalResultsDigestAlike. The digest exists so two recordings of
// one call can be compared, which is what a shadow replay does -- a digest
// that varied per call would make every comparison a mismatch.
func TestTwoIdenticalResultsDigestAlike(t *testing.T) {
	a, _ := digestToolResult(textResult(`{"rows":[1]}`))
	b, _ := digestToolResult(textResult(`{"rows":[1]}`))
	c, _ := digestToolResult(textResult(`{"rows":[2]}`))
	if a == "" || a != b {
		t.Errorf("identical results digested as %q and %q", a, b)
	}
	if a == c {
		t.Errorf("different results digested alike as %q", a)
	}
}

// TestASuccessfulCallRecordsNoResultText. The result is a DIGEST and a type
// (design D5); copying the output onto the row would put back in the spine
// exactly what D5 keeps out of it.
func TestASuccessfulCallRecordsNoResultText(t *testing.T) {
	if got := toolResultText(textResult("a page of rows"), false); got != "" {
		t.Errorf("a successful call recorded its output: %q", got)
	}
}
