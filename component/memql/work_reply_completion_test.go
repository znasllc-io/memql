package memql

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOperationCompletionSourceAttributionAndOutcomeEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, run, step, operation, value string
		allowed                           bool
	}{
		{"verified current effect", "run", "current", "saveFile", `{"fileId":"first","verified":true}`, true},
		{"retained earlier effect", "run", "earlier", "readFile", `{"fileId":"first","sha256":"verified-original"}`, true},
		{"saved failure report", "run", "current", "saveFile", `{ "ok":true,"fileId":"report","data":{"tests":[{"success":false,"error":"expected fixture failure"}]}}`, true},
		{"foreign run", "foreign", "current", "saveFile", `{"saved":true}`, false},
		{"missing step attribution", "run", "", "saveFile", `{"saved":true}`, false},
		{"unused approval", "run", "current", "requestComputerUseScope", `{"approved":true}`, false},
		{"refused command", "run", "current", "workbenchHost", `{"isError":true,"error":"execution_destination_requires_permission"}`, false},
		{"failed command", "run", "current", "workbenchHost", `{"exitCode":1,"stdout":"saved"}`, false},
		{"truncated proof", "run", "current", "workbenchHost", `{"content":[{"type":"text","text":"{\"truncated\":true,\"exitCode\":0,\"stdout\":\"saved\"}"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := map[string]any{"runId": tc.run, "stepKey": tc.step, "operation": tc.operation, "value": workSourcePreviewValue(tc.value)}
			refs := replyCompletionSources([]any{source}, "run")
			c := &WorkReplyCompletion{Kind: "operation", Disposition: "satisfied", SourceRefs: []string{"source-1"}}
			require.Equal(t, tc.allowed, c.Validate(refs) == nil)
			raw, err := json.Marshal(source)
			require.NoError(t, err)
			require.Contains(t, string(raw), tc.operation)
		})
	}
}
