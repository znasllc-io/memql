package automations

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

func TestStrictClusterClaimRefusesStorageFailure(t *testing.T) {
	closed := sql.OpenDB(pgdriver.NewConnector())
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		db   *bun.DB
	}{
		{"missing database", nil},
		{"closed connection pool", bun.NewDB(closed, pgdialect.New())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := NewClusterExecutionGuard(func() *bun.DB { return tc.db }, nil)
			guard.unguardedMax = 1
			strict := guard.StrictClaimer()
			for range 2 {
				if strict.ClaimWithTTL(context.Background(), "work.run.dispatch", "run", 4*time.Minute) {
					t.Fatal("work execution admitted without a persisted claim")
				}
			}
			if guard.ClaimErrors() != 2 || guard.Claimed() != 0 {
				t.Fatalf("claim errors=%d claimed=%d", guard.ClaimErrors(), guard.Claimed())
			}
			// Work's refusal must neither change nor consume the scheduler's
			// separate budget for unguarded execution.
			if !guard.Claim(context.Background(), "scheduler", "first") {
				t.Fatal("strict work claims changed scheduler outage behavior")
			}
			if guard.Claim(context.Background(), "scheduler", "second") {
				t.Fatal("scheduler no longer enforces its unguarded-execution budget")
			}
		})
	}
}

func TestStrictClusterClaimDBAllowsOneReplica(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	name := fmt.Sprintf("test_strict_guard_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(), `DELETE FROM automation_execution_claims WHERE automation_name = $1`, name)
	})
	first := NewClusterExecutionGuard(func() *bun.DB { return db }, nil)
	second := NewClusterExecutionGuard(func() *bun.DB { return db }, nil)
	first.nodeId, second.nodeId = "agent-one", "agent-two"
	guards := []*ClusterExecutionGuard{first, second}
	claimRace := func() int64 {
		var winners atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for n := range 16 {
			wg.Go(func() {
				<-start
				if guards[n%2].StrictClaimer().ClaimWithTTL(context.Background(), name, "same-run", time.Minute) {
					winners.Add(1)
				}
			})
		}
		close(start)
		wg.Wait()
		return winners.Load()
	}
	if got := claimRace(); got != 1 {
		t.Fatalf("first execution admitted on %d replicas, want one", got)
	}
	if got := claimRace(); got != 0 {
		t.Fatalf("fresh lease admitted %d duplicate executions", got)
	}
	if _, err := db.DB.ExecContext(context.Background(), `UPDATE automation_execution_claims SET claimed_at = now() - interval '2 minutes', expires_at = now() - interval '1 minute' WHERE automation_name = $1`, name); err != nil {
		t.Fatal(err)
	}
	if got := claimRace(); got != 1 {
		t.Fatalf("expired lease admitted %d recoveries, want one", got)
	}
	if first.ClaimErrors()+second.ClaimErrors() != 0 {
		t.Fatal("healthy claims encountered storage errors")
	}
}
