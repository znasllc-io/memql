package emailrules

// firing_gate_db_test.go -- the firing gate waits only as long as its
// deadline, and only behind what it has to (memql#5431, review).
//
// Each case is a way the database can make a firing wait -- another replica
// holding the rule's lock, a pool with no connection to give, a contended rule
// beside an idle one -- and each asserts the firing comes back inside the bound
// and is recorded, unlocked where it had to be.

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// gatePool opens a pool of its own, capped at maxOpen (0 is unlimited).
func gatePool(t *testing.T, maxOpen int) *bun.DB {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "the email rule firing gate", dsn, err)
		return nil
	}
	db.SetMaxOpenConns(maxOpen)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// lockedBuffer is a log sink the gate's goroutines can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func capturingLogger() (*slog.Logger, *lockedBuffer) {
	sink := &lockedBuffer{}
	return slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})), sink
}

func gateRule(name string) string {
	return "v1:campaigns:emailRule:gate-" + name + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// holdRuleLock takes the rule's lock on a connection of its own and keeps it:
// another replica inside its record, or one that never lets go.
func holdRuleLock(t *testing.T, db *bun.DB, ruleID string) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(?, ?)", firingGateLockClass, firingGateKey(ruleID)); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(?, ?)", firingGateLockClass, firingGateKey(ruleID))
			_ = conn.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// waitersFor counts sessions waiting (not holding) the rule's lock.
func waitersFor(t *testing.T, db *bun.DB, ruleID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM pg_locks
		 WHERE locktype = 'advisory' AND classid = ? AND objid = ? AND objsubid = 2 AND NOT granted`,
		int64(uint32(firingGateLockClass)), int64(uint32(firingGateKey(ruleID)))).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type gateResult struct {
	err      error
	recorded bool
	took     time.Duration
}

// fireThrough runs one record through the gate, in the background.
func fireThrough(gate FiringGate, ruleID string) <-chan gateResult {
	done := make(chan gateResult, 1)
	go func() {
		var r gateResult
		start := time.Now()
		r.err = gate(context.Background(), ruleID, func(context.Context) error { r.recorded = true; return nil })
		r.took = time.Since(start)
		done <- r
	}()
	return done
}

// within waits for a background firing, failing -- rather than hanging the
// suite -- if it has not come back by then.
func within(t *testing.T, done <-chan gateResult, limit time.Duration, what string) gateResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(limit):
		t.Fatalf("%s: the firing has not come back after %s -- a wait with no bound stalls every firing on the node", what, limit)
		return gateResult{}
	}
}

// TestARuleLockHeldElsewhereCostsAFiringNoMoreThanTheDeadline: another replica
// holds the rule's lock and does not let go. The firing waits out the deadline,
// no longer, and is recorded unlocked with the reason logged.
func TestARuleLockHeldElsewhereCostsAFiringNoMoreThanTheDeadline(t *testing.T) {
	db := gatePool(t, 0)
	rule := gateRule("held")
	holdRuleLock(t, db, rule)
	logger, logs := capturingLogger()
	gate := newFiringGateWithin(func() *bun.DB { return db }, logger, time.Second)

	r := within(t, fireThrough(gate, rule), 15*time.Second, "behind a held lock")
	if r.err != nil || !r.recorded {
		t.Fatalf("behind a held lock: recorded=%v err=%v, want the firing recorded unlocked", r.recorded, r.err)
	}
	if r.took > 3*time.Second {
		t.Errorf("behind a held lock the firing took %s; the deadline is 1s", r.took)
	}
	if got := logs.String(); !strings.Contains(got, "recorded without the rule lock") || !strings.Contains(got, "reason=lock") {
		t.Errorf("the unlocked record was not logged with its reason:\n%s", got)
	}
}

// TestAStarvedPoolCostsAFiringNoMoreThanTheDeadline: every connection the lock
// pool has is held elsewhere -- a leader node's leases and a burst. The firing
// waits out the deadline for a connection, no longer, and is recorded unlocked.
func TestAStarvedPoolCostsAFiringNoMoreThanTheDeadline(t *testing.T) {
	db := gatePool(t, 4)
	for i := 0; i < 4; i++ {
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
	}
	logger, logs := capturingLogger()
	gate := newFiringGateWithin(func() *bun.DB { return db }, logger, time.Second)

	r := within(t, fireThrough(gate, gateRule("starved")), 15*time.Second, "on a starved pool")
	if r.err != nil || !r.recorded {
		t.Fatalf("on a starved pool: recorded=%v err=%v, want the firing recorded unlocked", r.recorded, r.err)
	}
	if r.took > 3*time.Second {
		t.Errorf("on a starved pool the firing took %s; the deadline is 1s", r.took)
	}
	if got := logs.String(); !strings.Contains(got, "recorded without the rule lock") || !strings.Contains(got, "no connection") {
		t.Errorf("the unlocked record was not logged with its reason:\n%s", got)
	}
}

// TestAContendedRuleDoesNotHoldUpAnother: one rule's lock is held elsewhere and
// three of its firings arrive at once. A firing of an unrelated rule on the
// same node is not held up by them -- not by a node-wide lock, and not by the
// contended rule's firings taking every slot: they queue on their rule in
// memory, one at a time.
func TestAContendedRuleDoesNotHoldUpAnother(t *testing.T) {
	db := gatePool(t, 0)
	contended, unrelated := gateRule("contended"), gateRule("unrelated")
	release := holdRuleLock(t, db, contended)
	gate := newFiringGateWithin(func() *bun.DB { return db }, quietLogger(), 3*time.Second)

	var queued []<-chan gateResult
	for i := 0; i < 3; i++ {
		queued = append(queued, fireThrough(gate, contended))
	}
	deadline := time.Now().Add(5 * time.Second)
	for waitersFor(t, db, contended) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the contended rule's firing never reached the database")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := waitersFor(t, db, contended); n != 1 {
		t.Errorf("%d sessions wait on the contended rule's lock; its firings on one node should queue in memory, one at the database", n)
	}

	r := within(t, fireThrough(gate, unrelated), 15*time.Second, "an unrelated rule beside a contended one")
	if r.err != nil || !r.recorded {
		t.Fatalf("the unrelated rule: recorded=%v err=%v", r.recorded, r.err)
	}
	// Well under the contended rule's 3s wait, which is what a node-wide lock
	// or a slot hogged by the contended rule would have cost it.
	if r.took > 2*time.Second {
		t.Errorf("the unrelated rule's firing took %s behind a contended rule; it waits on nothing of the contended rule's", r.took)
	}

	// Let the contended rule go: its queue drains, each firing recorded.
	release()
	for i, done := range queued {
		if r := within(t, done, 30*time.Second, "the contended rule's queue"); r.err != nil || !r.recorded {
			t.Errorf("contended firing %d: recorded=%v err=%v", i, r.recorded, r.err)
		}
	}
}
