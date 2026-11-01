package work

import (
	"encoding/json"
	"strings"
	"testing"
)

// Exercise the real shape and owner gates: narrowing in the browser would
// still send the oversized response through the 4 MiB websocket/gRPC bridge.
func TestRunSummaryDBExcludesExecutionPayloads(t *testing.T) {
	a := openActsDB(t)
	document := strings.Repeat("captured document and research\n", 170000)
	runID := a.openRun(t, map[string]any{
		"input":     map[string]any{"document": document, "conversation": map[string]any{"id": "conversation-one"}},
		"variables": map[string]any{"document": document},
	})
	a.write(t, "updateWorkRun", map[string]any{
		"runId": runID, "status": "failed", "errorMessage": "Connection interrupted",
		"humanResumeId":  "v1:work:approval:saved-reporting-answer",
		"classification": map[string]any{"workTitle": "Presidential history"},
		"outcome":        map[string]any{"draft": document},
		"head":           map[string]any{"repair": map[string]any{"version": 1}},
		"stepOrder":      []string{"research", "repair"},
	})
	for _, q := range []string{
		"query workRunSummariesForOwner()",
		"query " + call("workRunSummaryForOwner", map[string]any{"runId": runID}),
	} {
		rows := a.query(t, a.owner, q)
		if len(rows) != 1 {
			t.Fatalf("%s: got %d rows", q, len(rows))
		}
		row := rows[0]
		encoded, err := json.Marshal(row)
		if err != nil || len(encoded) > 4096 {
			t.Fatalf("summary retains execution payload: %d bytes, %v", len(encoded), err)
		}
		for _, key := range []string{"input", "variables", "spine", "outcome", "executionAuthority"} {
			if _, present := row[key]; present {
				t.Errorf("%s exposes %s", q, key)
			}
		}
		if row["workTitle"] != "Presidential history" || row["status"] != "failed" || row["head"] == nil || row["stepOrder"] == nil {
			t.Fatalf("summary lost navigation/recovery fields: %s", encoded)
		}
		if rowMap(row, "conversation")["id"] != "conversation-one" {
			t.Fatalf("summary lost Ask conversation: %s", encoded)
		}
		if row["humanResumeId"] != "v1:work:approval:saved-reporting-answer" {
			t.Fatalf("summary lost the saved answer needed for explicit recovery: %s", encoded)
		}
		if other := a.query(t, a.owner+"-stranger", q); len(other) != 0 {
			t.Fatalf("%s exposed another owner's run", q)
		}
	}
	// Internal replay and existing consumers still receive their full inputs.
	full := a.query(t, a.owner, "query "+call("workRunForOwner", map[string]any{"runId": runID}))
	if len(full) != 1 || rowMap(full[0], "input")["document"] != document {
		t.Fatal("summary change narrowed the full run read")
	}
}
