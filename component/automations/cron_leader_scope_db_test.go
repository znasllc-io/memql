package automations

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

func TestScopedCronLeaderExcludesReplicasAndFailsOver(t *testing.T) {
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "scoped cron leadership", dsn, err)
	}
	scope := fmt.Sprintf("test-%d", time.Now().UnixNano())
	getter := func() *bun.DB { return db }
	first := NewScopedCronLeader(scope, getter, nil)
	second := NewScopedCronLeader(scope, getter, nil)
	unrelated := NewScopedCronLeader(scope+"-other", getter, nil)
	for _, leader := range []*CronLeader{first, second, unrelated} {
		t.Cleanup(leader.cleanup)
	}
	first.poll(ctx)
	second.poll(ctx)
	unrelated.poll(ctx)
	if !first.IsLeader() || second.IsLeader() || !unrelated.IsLeader() {
		t.Fatal("scoped lease did not exclude its replica independently of other scopes")
	}
	if first.lockKey == cronLeaderLockKey {
		t.Fatal("scoped lease collides with the general cron lease")
	}
	first.cleanup()
	second.poll(ctx)
	if !second.IsLeader() {
		t.Fatal("remaining replica did not take over the released lease")
	}
}
