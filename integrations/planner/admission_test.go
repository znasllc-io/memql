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
// MEMORY_NODES_DATABASE_DSN, else the dev-compose default.
func openAdmissionTestDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv("MEMORY_NODES_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"
	}
	connector := pgdriver.NewConnector(pgdriver.WithDSN(dsn))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("no Postgres reachable for the admission integration test: %v", err)
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
