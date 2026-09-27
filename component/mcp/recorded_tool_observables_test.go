package mcp

import "testing"

// RecordedToolObservables must answer exactly what recordAppSessionToolCall
// writes for the same tool result, because a learned procedure's replay
// (epic memql#5408) compares its own call against the RECORDED values. The
// cases are the ones where the recorder's prefix rule and
// component/work.InferTextType disagree -- which is the whole reason the
// export exists -- plus an error and a result that is not a ToolCallResult.
func TestRecordedToolObservablesMatchWhatTheRecorderWrites(t *testing.T) {
	cases := map[string]string{
		"object":            `{"content":[{"type":"text","text":"{\"a\":1}"}],"isError":false}`,
		"array":             `{"content":[{"type":"text","text":"[1,2]"}],"isError":false}`,
		"a number is text":  `{"content":[{"type":"text","text":"42"}],"isError":false}`,
		"null is text":      `{"content":[{"type":"text","text":"null"}],"isError":false}`,
		"empty":             `{"content":[{"type":"text","text":""}],"isError":false}`,
		"an error":          `{"content":[{"type":"text","text":"not found"}],"isError":true}`,
		"not a tool result": `plain words`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &captureRecorder{}
			recordAppSessionToolCall(recordingCtx(rec), "v1:worker:appSession:s1", "librarySearch", nil, toolResultFromJSON(raw))
			calls := rec.recorded()
			if len(calls) != 1 {
				t.Fatalf("the recorder wrote %d calls, want 1", len(calls))
			}
			isError, resultType := RecordedToolObservables(raw)
			if isError != calls[0].IsError || resultType != calls[0].ResultType {
				t.Fatalf("RecordedToolObservables = (%v, %q), the recorder wrote (%v, %q)",
					isError, resultType, calls[0].IsError, calls[0].ResultType)
			}
		})
	}
}
