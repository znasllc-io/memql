//go:build clustere2e

package clustere2e

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/work"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// This checks live admission and the persisted source contract through the TLS
// front door. It does not claim successful model/file delivery: those scenarios
// need a configured provider and are separately covered by the DB pipeline tests.
func TestSpineLiveAdmissionPinsAllExecutionPhases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	connections := openConnections(ctx, t, token(t), 2)
	defer connections[0].Close()
	defer connections[1].Close()
	writer := memqlclient.NewQueryClient(connections[0].Dispatcher())
	reader := memqlclient.NewQueryClient(connections[1].Dispatcher())
	if _, err := writer.CreateGoal(ctx, memqlclient.CreateGoalArgs{Statement: "Invalid Spine probe", Spine: "missingSpineE2E"}); err == nil {
		t.Fatal("unknown installed Spine was admitted")
	}
	result, err := writer.CreateGoal(ctx, memqlclient.CreateGoalArgs{
		Statement: "Reply with the word ready. Do not call tools.", RequestedVia: "api",
		Input:    map[string]any{"conversation": []any{map[string]any{"role": "user", "content": "Reply with the word ready. Do not call tools."}}},
		Ceilings: map[string]any{"maxModelCalls": 1, "wallClockMs": 60000, "maxRetries": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := result.Rows()
	if len(rows) != 1 {
		t.Fatalf("missing admission receipt: %v", rows)
	}
	runID, goalID := memqlclient.RowString(rows[0], "runId"), memqlclient.RowString(rows[0], "goalId")
	if runID == "" || goalID == "" {
		t.Fatalf("missing run or goal: %v", rows)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := writer.CancelGoal(cleanup, memqlclient.CancelGoalArgs{GoalId: goalID, Reason: "Spine admission test completed"}); err != nil {
			t.Errorf("cancel probe goal: %v", err)
		}
	}()
	result, err = reader.WorkRunForOwner(ctx, memqlclient.WorkRunForOwnerArgs{RunId: runID})
	if err != nil || len(result.Rows()) != 1 {
		t.Fatalf("read persisted run: %v", err)
	}
	row := result.Rows()[0]
	snapshot, err := workflowhost.SnapshotFromMap(memqlclient.RowObject(row, "spine"))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Contract != work.SpineContract || snapshot.Entry != work.DefaultSpine || len(snapshot.Phases) != 5 {
		t.Fatalf("incomplete shipped Spine: entry=%s phases=%v", snapshot.Entry, snapshot.Phases)
	}
	kinds := map[string]int{}
	for _, definition := range snapshot.Constructs {
		kinds[definition.Kind]++
	}
	if kinds["action"] != 3 || kinds["logic"] < 5 {
		t.Fatalf("lost scoped actions or logic: %v", kinds)
	}
	t.Logf("persisted Spine %s with %d definitions; run status %s", snapshot.Version, len(snapshot.Constructs), memqlclient.RowString(row, "status"))
	// A named capability cannot be invoked merely because its action source is
	// visible in the run's snapshot. It needs the native verified-mismatch scope.
	if _, err := reader.ExecuteNamed(ctx, "agentRecoveryContext", "builtin agentRecoveryContext()"); err == nil {
		t.Fatal("scoped recovery operation escaped its native invocation")
	}
}
