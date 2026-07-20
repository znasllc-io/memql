package planner

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

func TestDecideAdmission(t *testing.T) {
	cases := []struct {
		name    string
		running int
		cap     int
		want    bool
	}{
		{"under cap admits", 2, 5, true},
		{"exactly at cap admits", 5, 5, true},
		{"over cap denies", 6, 5, false},
		{"first into a cap of one", 1, 1, true},
		{"second into a cap of one denies", 2, 1, false},
		{"non-positive cap is unlimited", 100, 0, true},
		{"negative cap is unlimited", 100, -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideAdmission(tc.running, tc.cap); got != tc.want {
				t.Fatalf("decideAdmission(%d, %d) = %v, want %v", tc.running, tc.cap, got, tc.want)
			}
		})
	}
}

func TestTryAdmit_NilGetterFailsOpen(t *testing.T) {
	ac := NewAdmissionController(nil, testLogger())
	admitted, err := ac.TryAdmit(context.Background(), "acct", "plan", 1, func(context.Context) error {
		t.Fatal("demote must not be called when failing open")
		return nil
	})
	if err != nil || !admitted {
		t.Fatalf("nil getter must fail open: admitted=%v err=%v", admitted, err)
	}
}

func TestTryAdmit_NilDBFailsOpen(t *testing.T) {
	ac := NewAdmissionController(func() *bun.DB { return nil }, testLogger())
	admitted, err := ac.TryAdmit(context.Background(), "acct", "plan", 1, nil)
	if err != nil || !admitted {
		t.Fatalf("nil db must fail open: admitted=%v err=%v", admitted, err)
	}
}

func TestAccountLockKey_StableAndDistinct(t *testing.T) {
	if accountLockKey("acct-a") != accountLockKey("acct-a") {
		t.Fatal("lock key must be stable for the same account")
	}
	if accountLockKey("acct-a") == accountLockKey("acct-b") {
		t.Fatal("different accounts should (almost always) hash to different keys")
	}
}

// openAdmissionTestDB connects to Postgres for the atomic-admission integration
// test, skipping when none is reachable so it never blocks CI. DSN comes from
// MEMQL_DATABASE_DSN, else the dev-compose default.
func openAdmissionTestDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"
	}
	connector := pgdriver.NewConnector(pgdriver.WithDSN(dsn))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "DB-gated test for the admission integration test", dsn, err)
	}
	return db
}

// TestTryAdmit_AtomicCapUnderConcurrency is the core acceptance check for #904:
// with a finite cap and more dispatches than slots, EXACTLY `cap` Plans stay
// running and the rest are parked -- and concurrent dispatch never exceeds the
// cap. It seeds N already-running Plans for one account (simulating N Run
// clicks), then fires N concurrent TryAdmit calls whose demote callback parks
// the over-cap Plan; the per-account advisory lock must serialize them so each
// demote is visible to the next admission.
func TestTryAdmit_AtomicCapUnderConcurrency(t *testing.T) {
	db := openAdmissionTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const (
		seed = 8
		cap  = 3
	)
	// Unique per-run account so the test never collides with real data or a
	// concurrent run; cleanup removes exactly this account's rows.
	account := fmt.Sprintf("v1:identity:user:test-admission-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:planner:plan' AND payload->>'requestedBy' = $1`,
			account)
	})

	planID := func(i int) string { return fmt.Sprintf("v1:planner:plan:test-%s-%d", account, i) }

	insertVersion := func(c context.Context, id, status, createdAt string) error {
		_, err := db.DB.ExecContext(c,
			`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, concept)
			 VALUES ($1, `+createdAt+`, 'test', '{}', $2::jsonb, 'v1:planner:plan')`,
			id, fmt.Sprintf(`{"status":%q,"requestedBy":%q}`, status, account))
		return err
	}

	// Seed N running Plans, stamped in the past so a later waitingForSlot
	// version wins the DISTINCT ON.
	for i := 0; i < seed; i++ {
		if err := insertVersion(ctx, planID(i), "running", "now() - interval '1 minute'"); err != nil {
			t.Fatalf("seed insert %d: %v", i, err)
		}
	}

	ac := NewAdmissionController(func() *bun.DB { return db }, testLogger())

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted int
	)
	for i := 0; i < seed; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := planID(i)
			demote := func(c context.Context) error {
				return insertVersion(c, id, "waitingForSlot", "now()")
			}
			ok, err := ac.TryAdmit(ctx, account, id, cap, demote)
			if err != nil {
				t.Errorf("TryAdmit(%d) errored: %v", i, err)
				return
			}
			if ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if admitted != cap {
		t.Fatalf("admitted %d Plans, want exactly cap=%d", admitted, cap)
	}

	// And the DB must agree: exactly `cap` Plans are running for this account.
	var running int
	if err := db.DB.QueryRowContext(ctx, countRunningQuery, account).Scan(&running); err != nil {
		t.Fatalf("count running: %v", err)
	}
	if running != cap {
		t.Fatalf("DB shows %d running Plans, want exactly cap=%d (cap exceeded or under-admitted)", running, cap)
	}
}

func TestIsSlotFreeingStatus(t *testing.T) {
	freeing := []string{"succeeded", "failed", "cancelled", "paused", "awaitingFeedback", "needsAgent"}
	for _, s := range freeing {
		if !isSlotFreeingStatus(s) {
			t.Errorf("%q should free a slot", s)
		}
	}
	// running occupies a slot; waitingForSlot is the demote itself (account is
	// at cap at that moment) so it must NOT trigger an admit sweep.
	for _, s := range []string{"running", "waitingForSlot", "queued", "planning", "routing", ""} {
		if isSlotFreeingStatus(s) {
			t.Errorf("%q should NOT be treated as slot-freeing", s)
		}
	}
}

// TestAdmitNext_FIFOFillsFreeSlots is the #905 acceptance check: when slots are
// free, AdmitNext promotes the longest-waiting Plans first, exactly up to the
// cap, and leaves the rest queued. Seeds 1 running + 5 staggered waiting Plans
// (cap=3) and asserts the two OLDEST waiting Plans are promoted (FIFO) and the
// three newest stay parked.
func TestAdmitNext_FIFOFillsFreeSlots(t *testing.T) {
	db := openAdmissionTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const cap = 3
	// Nonce BOTH the account and the plan ids: the count/pick queries do
	// DISTINCT ON (id) across the whole table (plan ids are globally unique in
	// production), so reusing fixed ids across runs would let a prior run's
	// rows shadow this one's. Cleanup is by account.
	nonce := time.Now().UnixNano()
	account := fmt.Sprintf("v1:identity:user:test-admitnext-%d", nonce)
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:planner:plan' AND payload->>'requestedBy' = $1`,
			account)
	})

	insert := func(id, status, createdAtSQL string) {
		t.Helper()
		_, err := db.DB.ExecContext(ctx,
			`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, concept)
			 VALUES ($1, `+createdAtSQL+`, 'test', '{}', $2::jsonb, 'v1:planner:plan')`,
			id, fmt.Sprintf(`{"status":%q,"requestedBy":%q}`, status, account))
		if err != nil {
			t.Fatalf("insert %s/%s: %v", id, status, err)
		}
	}

	// One Plan already running (occupies 1 of 3 slots).
	insert(fmt.Sprintf("v1:planner:plan:run-%d", nonce), "running", "now() - interval '10 minutes'")
	// Five waiting Plans, w0 oldest .. w4 newest (FIFO order = w0,w1,...).
	waitID := func(i int) string { return fmt.Sprintf("v1:planner:plan:wait-%d-%d", nonce, i) }
	for i := 0; i < 5; i++ {
		insert(waitID(i), "waitingForSlot", fmt.Sprintf("now() - interval '%d minutes'", 5-i))
	}

	ac := NewAdmissionController(func() *bun.DB { return db }, testLogger())
	promote := func(c context.Context, planId string) error {
		_, err := db.DB.ExecContext(c,
			`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, concept)
			 VALUES ($1, now(), 'test', '{}', $2::jsonb, 'v1:planner:plan')`,
			planId, fmt.Sprintf(`{"status":"running","requestedBy":%q}`, account))
		return err
	}

	admitted, err := ac.AdmitNext(ctx, account, cap, promote)
	if err != nil {
		t.Fatalf("AdmitNext: %v", err)
	}
	// 3 - 1 already running = 2 free slots.
	if admitted != 2 {
		t.Fatalf("admitted %d, want 2 (cap=3, 1 already running)", admitted)
	}

	statusOf := func(id string) string {
		var s string
		if err := db.DB.QueryRowContext(ctx,
			`SELECT payload->>'status' FROM "MemoryNodes" WHERE id = $1 ORDER BY "createdAt" DESC LIMIT 1`,
			id).Scan(&s); err != nil {
			t.Fatalf("status of %s: %v", id, err)
		}
		return s
	}
	// FIFO: the two oldest waiters now run; the three newest still wait.
	for _, i := range []int{0, 1} {
		if got := statusOf(waitID(i)); got != "running" {
			t.Errorf("oldest waiter %s should be running (FIFO), got %q", waitID(i), got)
		}
	}
	for _, i := range []int{2, 3, 4} {
		if got := statusOf(waitID(i)); got != "waitingForSlot" {
			t.Errorf("newer waiter %s should still wait, got %q", waitID(i), got)
		}
	}

	var running int
	if err := db.DB.QueryRowContext(ctx, countRunningQuery, account).Scan(&running); err != nil {
		t.Fatalf("count running: %v", err)
	}
	if running != cap {
		t.Fatalf("DB shows %d running, want cap=%d", running, cap)
	}
}

// TestAdmitNext_NoQueueNoOp verifies the lock-free fast path: an account with
// no waiting Plans admits nothing and returns cleanly.
func TestAdmitNext_NoQueueNoOp(t *testing.T) {
	db := openAdmissionTestDB(t)
	defer db.Close()
	account := fmt.Sprintf("v1:identity:user:test-noqueue-%d", time.Now().UnixNano())
	ac := NewAdmissionController(func() *bun.DB { return db }, testLogger())
	admitted, err := ac.AdmitNext(context.Background(), account, 5, func(context.Context, string) error {
		t.Fatal("promote must not run when there is no queue")
		return nil
	})
	if err != nil || admitted != 0 {
		t.Fatalf("no-queue AdmitNext: admitted=%d err=%v", admitted, err)
	}
}
