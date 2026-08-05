package steps

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// TestAccountDeletionSweep_HardDeletesExpiredUser_DBAcceptance is the
// ACCEPTANCE TEST for the condition-evaluator fix tracked in epic #2256, and
// the end-to-end reproduction of the compliance bug #2254.
//
// It drives the REAL accountDeletionSweep cron path through a REAL engine +
// LogicRunner against a Postgres-backed DB: it seeds a v1:identity:user that
// is active and was scheduled for deletion 60 days ago (well past the default
// 30-day cooldown), runs the sweep, and asserts the user was hard-deleted
// (deleteUserHard flips active=false). Post-#2235 the sweep's per-row write
// lives in the accountDeletionSweep AUTOMATION (window -> decide -> forEach
// apply -> deleteUserHard), NOT in the (now-pure) logic, so this drives the
// AUTOMATION via the scheduler -- the real cron path -- rather than calling the
// logic directly.
//
// STATUS: runs whenever a Postgres is reachable (skips cleanly otherwise, so it
// is green-by-skip on CI without a DB). The #2256 condition-evaluator fix has
// landed, so the per-row date-window gate
// `addDuration(deletionScheduledAt, "P{N}D") < timestamp()` now evaluates
// correctly; this is the end-to-end acceptance for the #2254 compliance bug and
// a permanent regression guard. (The migrated forEach + per-row-gate mechanics
// are also unit-covered without a DB by foreach_sweep_argvalue_test.go and the
// condition evaluator's condition_datewindow_test.go.)
//
// Note: authored without a reachable Postgres; the seed payload (createUser +
// updateUser) may need a small adjustment for any additional @required
// user-concept fields when first run against a DB.
func TestAccountDeletionSweep_HardDeletesExpiredUser_DBAcceptance(t *testing.T) {
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "accountDeletionSweep acceptance test", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Build a real engine wired to the DB + the full DSL, exactly like the
	// other *_db_test.go harnesses.
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(db)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	// Wire the LogicRunner against a live step registry, mirroring app bootstrap
	// (app/engine.go) so the decide/window logics dispatch through the real
	// executors.
	stepReg := NewRegistry()
	eng.SetLogicRunner(automations.NewLogicRunner(eng, stepReg, eng.Logger))

	// Build + start a scheduler over the real DSL tree so the migrated
	// accountDeletionSweep AUTOMATION (window -> decide -> forEach apply ->
	// deleteUserHard) runs end to end. Post-#2235 the write lives in the
	// automation's forEach (the logic is pure), so we drive the automation.
	sched, err := automations.NewScheduler(automations.SchedulerOptions{
		Logger:       eng.Logger,
		Loader:       automations.NewLoader(automations.LoaderOptions{Logger: eng.Logger}),
		Engine:       eng,
		EventBus:     events.NewBus(),
		StepRegistry: stepReg,
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	schedCtx, cancelSched := context.WithCancel(context.Background())
	defer cancelSched()
	go sched.Start(schedCtx)
	select {
	case <-sched.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("scheduler did not become ready within 10s")
	}

	ctx := auth.ContextWithToken(context.Background(),
		&auth.TokenInfo{Subject: "system:account-deletion-sweep-acceptance"})

	exec := func(expr string) *memql.ExecuteResult {
		t.Helper()
		res, err := eng.Execute(ctx, expr)
		if err != nil {
			t.Fatalf("execute %q: %v", expr, err)
		}
		return res
	}
	call := func(name string, args map[string]any) *memql.ExecuteResult {
		t.Helper()
		keys := make([]string, 0, len(args))
		for k := range args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(args))
		for _, k := range keys {
			vb, err := json.Marshal(args[k])
			if err != nil {
				t.Fatalf("marshal arg %s for %s: %v", k, name, err)
			}
			parts = append(parts, k+": "+string(vb))
		}
		// Kind-prefixed, named-args call form (#2335).
		return exec("mutation " + name + "(" + strings.Join(parts, ", ") + ")")
	}

	// Seed: an active user scheduled for deletion 60 days ago. The default
	// cooldown is 30 days (the sweep's coalesce fallback), so this user is well
	// past it and MUST be hard-deleted.
	uid := fmt.Sprintf("accttest-%d", os.Getpid())
	created := call("createUser", map[string]any{
		"userId":       uid,
		"displayName":  "Doomed Acceptance User",
		"primaryEmail": uid + "@example.test",
	})
	if created.Bundle == nil || len(created.Bundle.Nodes) == 0 {
		t.Fatalf("createUser wrote no node")
	}
	canonicalID := created.Bundle.Nodes[0].Id

	scheduledAt := time.Now().Add(-60 * 24 * time.Hour).UTC().Format(time.RFC3339)
	// updateUser is @serverOnly as of memql#2991, so this setup call has to
	// carry an internal origin the way its real caller (the identity admin
	// server) does. The sweep itself never calls updateUser -- this is test
	// scaffolding standing in for an admin having scheduled the deletion.
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(ctx), fmt.Sprintf(
		`mutation updateUser(payload: {"deletionScheduledAt": %q}, userId: %q)`,
		scheduledAt, uid)); err != nil {
		t.Fatalf("seed deletionScheduledAt via updateUser: %v", err)
	}

	// Sanity: the user is active before the sweep.
	if got := userActive(t, ctx, db, canonicalID); got != true {
		t.Fatalf("precondition: seeded user active=%v, want true", got)
	}

	// Run the REAL cron path: trigger the accountDeletionSweep AUTOMATION (window
	// + decide + forEach apply). The synthetic event payload is unused by the
	// scheduled sweep but mirrors a manual trigger.
	if _, err := sched.TriggerAutomationWithEvent(ctx, "accountDeletionSweep", &events.Event{
		Topic:   "manual.acceptance.accountDeletionSweep",
		Payload: map[string]any{},
	}); err != nil {
		t.Fatalf("TriggerAutomationWithEvent(accountDeletionSweep): %v", err)
	}

	// ACCEPTANCE: the expired user must be hard-deleted (active=false).
	if got := userActive(t, ctx, db, canonicalID); got != false {
		t.Fatalf("accountDeletionSweep did NOT hard-delete a user 60 days past the 30-day cooldown: active=%v, want false. "+
			"This is the #2254 dead-cron bug; it must pass once the #2256 condition-evaluator fix lands.", got)
	}
}

// userActive reads the latest version of a v1:identity:user row off the
// append-only MemoryNodes table and returns its payload.active.
func userActive(t *testing.T, ctx context.Context, db *bun.DB, id string) bool {
	t.Helper()
	var node concept.MemoryNode
	if err := db.NewSelect().Model(&node).
		Where("concept = ?", "v1:identity:user").
		Where("id = ?", id).
		Order("createdAt DESC").
		Limit(1).
		Scan(ctx); err != nil {
		t.Fatalf("read-back of user %q failed: %v", id, err)
	}
	var p map[string]any
	if err := json.Unmarshal(node.Payload, &p); err != nil {
		t.Fatalf("unmarshal user payload: %v", err)
	}
	active, _ := p["active"].(bool)
	return active
}
