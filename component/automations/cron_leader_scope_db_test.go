package automations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
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

// A real agent has more scheduled leases than its direct pool has slots. All
// leases must become available with one session, while another replica sees
// the same independent locks. A pool increase would hide the startup deadlock.
func TestCronScopesShareOneBoundedSessionAcrossReplicas(t *testing.T) {
	firstDB := openTestDB(t)
	secondDB := openTestDB(t)
	for _, db := range []*bun.DB{firstDB, secondDB} {
		db.SetMaxOpenConns(4)
		t.Cleanup(func() { _ = db.Close() })
	}
	prefix := fmt.Sprintf("shared-scopes-%d", time.Now().UnixNano())
	first := NewScopedCronLeader(prefix, func() *bun.DB { return firstDB }, nil)
	second := NewScopedCronLeader(prefix, func() *bun.DB { return secondDB }, nil)
	t.Cleanup(first.cleanup)
	t.Cleanup(second.cleanup)
	firstGates, secondGates := []func() bool{first.IsLeader}, []func() bool{second.IsLeader}
	for i := range 12 {
		scope := fmt.Sprintf("%s-%d", prefix, i)
		firstGates = append(firstGates, first.ScopedGate(scope))
		secondGates = append(secondGates, second.ScopedGate(scope))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first.poll(ctx)
	first.poll(ctx) // a keepalive must not recursively acquire held locks
	second.poll(ctx)
	assertGates := func(gates []func() bool, want bool) {
		t.Helper()
		for i, gate := range gates {
			if gate() != want {
				t.Fatalf("scope %d leadership = %v, want %v", i, gate(), want)
			}
		}
	}
	assertGates(firstGates, true)
	assertGates(secondGates, false)
	for _, db := range []*bun.DB{firstDB, secondDB} {
		if got := db.Stats().InUse; got != 1 {
			t.Fatalf("scheduled leases consumed %d sessions, want exactly one", got)
		}
		if err := db.PingContext(ctx); err != nil {
			t.Fatalf("scheduled leases starved unrelated direct queries: %v", err)
		}
	}
	first.cleanup()
	assertGates(firstGates, false)
	second.poll(ctx)
	assertGates(secondGates, true)
	first.poll(ctx)
	assertGates(firstGates, false)
	second.cleanup()
	first.poll(ctx)
	assertGates(firstGates, true)
}

func TestCronSessionLossRevokesEveryScope(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	leader := NewScopedCronLeader(fmt.Sprintf("lost-scopes-%d", time.Now().UnixNano()), func() *bun.DB { return db }, nil)
	t.Cleanup(leader.cleanup)
	gate := leader.ScopedGate(fmt.Sprintf("lost-secondary-%d", time.Now().UnixNano()))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	leader.poll(ctx)
	if !leader.IsLeader() || !gate() {
		t.Fatal("test leases were not acquired")
	}
	// Discard the actual driver session, as database/sql does on connection
	// loss. Do not terminate an unrelated backend or affect the shared test DB.
	if err := leader.conn.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
		t.Fatal(err)
	}
	leader.poll(ctx)
	if leader.IsLeader() || gate() {
		t.Fatal("a lost session left a schedule authorized on this replica")
	}
}

func TestCronReadinessDoesNotWaitForeverForAnExhaustedPool(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	borrowed, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer borrowed.Close()
	leader := NewCronLeader(func() *bun.DB { return db }, nil)
	t.Cleanup(leader.cleanup)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- leader.run(ctx, func() { close(ready); cancel() }) }()
	select {
	case <-ready:
	case <-time.After(cronLeaderPollTimeout + time.Second):
		t.Fatal("an unavailable coordination connection blocked service readiness")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if leader.IsLeader() {
		t.Fatal("no connection must leave scheduled work closed")
	}
}
