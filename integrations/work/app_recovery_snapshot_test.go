package work

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestAppRecoverySnapshotPreservesExactJSON(t *testing.T) {
	newRow := func() map[string]any {
		t.Helper()
		decoder := json.NewDecoder(strings.NewReader(`{
			"id":"run", "ownerUserId":"owner", "goalId":"goal", "status":"running",
			"heartbeatAt":"2026-10-28T12:00:00Z",
			"routing":{"source":"app:*","by":"owner"},
			"rerun":{"requestId":"request","override":{"items":["a","b"],"revision":9007199254740992}}
		}`))
		decoder.UseNumber()
		var row map[string]any
		if err := decoder.Decode(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	want := appRecoverySnapshot(newRow())
	if bytes, err := hex.DecodeString(want); err != nil || len(bytes) != 32 {
		t.Fatalf("snapshot is not a 64-character hex fingerprint: %q", want)
	}
	// Separately read replicas may construct maps in different orders.
	equivalent := newRow()
	equivalent["routing"] = map[string]any{"by": "owner", "source": "app:*"}
	equivalent["input"] = "model content is outside the recovery snapshot"
	if got := appRecoverySnapshot(equivalent); got != want {
		t.Fatal("equivalent persisted state changed its snapshot")
	}
	for name, change := range map[string]func(map[string]any){
		"sequence order": func(row map[string]any) {
			rowMap(rowMap(row, "rerun"), "override")["items"] = []any{"b", "a"}
		},
		"adjacent large integers": func(row map[string]any) {
			rowMap(rowMap(row, "rerun"), "override")["revision"] = json.Number("9007199254740993")
		},
		"owner consent":     func(row map[string]any) { rowMap(row, "routing")["by"] = "another-owner" },
		"real progress":     func(row map[string]any) { row["heartbeatAt"] = "2026-10-28T12:00:01Z" },
		"answer generation": func(row map[string]any) { row["humanResumeId"] = "new-answer" },
	} {
		t.Run(name, func(t *testing.T) {
			row := newRow()
			change(row)
			if got := appRecoverySnapshot(row); got == "" || got == want {
				t.Fatal("changed persisted state did not invalidate its recovery hint")
			}
		})
	}
}

func TestAppRecoverySnapshotRefusesUnencodableState(t *testing.T) {
	if got := appRecoverySnapshot(map[string]any{"routing": make(chan struct{})}); got != "" {
		t.Fatalf("unencodable snapshot = %q, want empty", got)
	}
}
