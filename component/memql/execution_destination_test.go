package memql

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/work"
)

func TestExecutionDestinationVocabulary(t *testing.T) {
	for input, want := range map[string]string{"": "", "auto": "", " workbench ": "workbench", "computer": "computer"} {
		got, err := ParseExecutionDestination(input)
		if err != nil || got != want {
			t.Fatalf("%q: %q %v", input, got, err)
		}
	}
	if _, err := ParseExecutionDestination("another-computer"); err == nil {
		t.Fatal("unknown destination admitted")
	}
}

func TestExecutionDestinationRequiresAnOwnedUnchangedLiveDecision(t *testing.T) {
	now := time.Now()
	subject := map[string]any{"executionDestination": map[string]any{"from": "workbench", "to": "computer"}, "summary": "Needs Docker"}
	fixture := func() map[string]any {
		return map[string]any{"kind": "scopeElevation", "decision": "approved", "runId": "run", "ownerUserId": "alice", "decidedBy": "alice", "expiresAt": now.Add(time.Hour).Format(time.RFC3339Nano), "subject": subject, "artifactHash": work.ArtifactHash(subject)}
	}
	if !executionDestinationReceipt(fixture(), "alice", "run", "workbench", "computer", now) {
		t.Fatal("valid receipt refused")
	}
	for key, value := range map[string]any{"decision": "answered", "decidedBy": "bob", "ownerUserId": "bob", "runId": "other", "artifactHash": "changed", "expiresAt": now.Add(-time.Second).Format(time.RFC3339Nano), "permissionRevokedAt": now.Format(time.RFC3339Nano)} {
		row := fixture()
		row[key] = value
		if executionDestinationReceipt(row, "alice", "run", "workbench", "computer", now) {
			t.Fatalf("admitted changed %s", key)
		}
	}
	if executionDestinationReceipt(fixture(), "alice", "run", "computer", "workbench", now) {
		t.Fatal("permission authorized the reverse move")
	}
}
