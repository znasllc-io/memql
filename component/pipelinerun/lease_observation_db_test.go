package pipelinerun

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/retention"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// lease_observation_db_test.go -- a pipeline run's lease renewal is a current
// observation (migration 20261108190000_pipeline_run_lease_observations),
// proved on two agent replicas: two engines over one Postgres schema, so
// neither holds the other's result cache or session.
//
// Before the migration every renewal (driverHeartbeatAt, every 30 s) appended
// a whole version of the v1:pipelines:run row, policySnapshot included: about
// three quarters of the 3,831 versions of 30 runs written in a day on
// production (2026-11-06). The lease is still the authority (lease.go): these
// tests hold that a renewal leaves no version, that another replica and its
// recovery list read the observed heartbeat, that the row-version fence still
// refuses a decision made before an observed renewal, that a takeover waits
// for the OBSERVED heartbeat to age, that a business write carries the
// observed heartbeat into its version, that declared retention keeps the
// newest version and the observation beside it, and that the down migration
// folds the observation back into a version.
//
// The schema is private because the switch is cluster-wide: flipping it in the
// shared test schema would change what every other test in the lane counts.

// leaseObservationMigrations are the liveness migrations in rollout order, the
// new one last.
var leaseObservationMigrations = []string{
	"20261013020000_shared_row_schemas.up.sql",
	"20261013050000_liveness.up.sql",
	"20261013060000_reconcile_observations.up.sql",
	"20261106150000_worker_registration_observations.up.sql",
	"20261108190000_pipeline_run_lease_observations.up.sql",
}

// applyLeaseMigration runs one migration file, as the migrator does: the whole
// file in one statement batch.
func applyLeaseMigration(t *testing.T, db *bun.DB, name string) {
	t.Helper()
	raw, err := os.ReadFile("../database/memory-nodes/migrations/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if _, err := db.ExecContext(context.Background(), string(raw)); err != nil {
		t.Fatalf("apply %s: %v", name, err)
	}
}

// leaseObservationReplicas is a private schema with every liveness migration
// applied and pipeline-run observation storage switched on, and two engines
// over it. The pool it answers counts rows; it is replica A's.
func leaseObservationReplicas(t *testing.T) (*bun.DB, [2]*memqlengine.MemQLEngine) {
	t.Helper()
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		dbtest.Unreachable(t, "a pipeline run's lease renewal as a current observation", dbtest.DSN(), err)
		return nil, [2]*memqlengine.MemQLEngine{}
	}
	schema := fmt.Sprintf("lease_observation_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA ?`, bun.Ident(schema)); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA ? CASCADE`, bun.Ident(schema))
		_ = admin.Close()
	})
	open := func() *bun.DB {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
			pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db, other := open(), open()
	for _, table := range []string{"MemoryNodes", "SecretMemoryNodes"} {
		if _, err := db.ExecContext(ctx, `CREATE TABLE ? (LIKE public.? INCLUDING ALL)`, bun.Ident(table), bun.Ident(table)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `SELECT public.create_hypertable(?::regclass,'createdAt',chunk_time_interval=>interval '1 day')`, `"`+table+`"`); err != nil {
			t.Fatal(err)
		}
	}
	// The declared-retention sweep's cursor table.
	if _, err := db.ExecContext(ctx, `CREATE TABLE concept_retention_progress (LIKE public.concept_retention_progress INCLUDING ALL)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range leaseObservationMigrations {
		applyLeaseMigration(t, db, name)
	}
	// Production's state: schema references and this storage both active.
	if _, err := db.ExecContext(ctx, `UPDATE memory_schema_storage SET references_enabled=true`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE memory_pipeline_run_observation_storage SET enabled=true`); err != nil {
		t.Fatal(err)
	}
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	boot := func(db *bun.DB) *memqlengine.MemQLEngine {
		eng, err := memqlengine.New(db)
		if err != nil {
			t.Fatal(err)
		}
		eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if err := eng.Init(concept.DefaultRegistry()); err != nil {
			t.Fatalf("engine init: %v", err)
		}
		return eng
	}
	return db, [2]*memqlengine.MemQLEngine{boot(db), boot(other)}
}

// leaseClock is one replica's clock, moved by the test.
type leaseClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *leaseClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *leaseClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// advance moves the clock and answers the instant as the driver writes it
// (Deps.now: UTC, to the second).
func (c *leaseClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
	return c.at.UTC().Truncate(time.Second)
}

func TestLeaseRenewalsAreObservationsAcrossTwoReplicas(t *testing.T) {
	db, engines := leaseObservationReplicas(t)
	ctx := context.Background()
	gatePool := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = gatePool.Close() })

	p := testPipeline(DeliveryWebhook)
	suffix := id.NewShortId()
	p.ID, p.OwnerUserID = "observed-p-"+suffix, "observed-owner-"+suffix
	run := queuedRun(p, shaA)
	if err := NewDSLStore(engines[0]).CreateRun(ctx, run); err != nil {
		t.Fatalf("open the run: %v", err)
	}
	rowID := RunConcept + ":" + bareID(run.ID)
	nodeA, nodeB := "observed-agent-a-"+suffix, "observed-agent-b-"+suffix
	clockA, clockB := &leaseClock{at: testNow}, &leaseClock{at: testNow}
	replica := func(node string, eng *memqlengine.MemQLEngine, clock *leaseClock) *Integration {
		return New(Deps{
			Store: NewDSLStore(eng), NodeID: node, Now: clock.now,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Gate: watchedGate(t, func(ctx context.Context, key string, fn func(context.Context) error) error {
				return githubconnect.WithGate(ctx, gatePool, key, fn)
			}),
		})
	}
	a, b := replica(nodeA, engines[0], clockA), replica(nodeB, engines[1], clockB)
	storeB := NewDSLStore(engines[1])

	counts := func() (versions, observations int) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id = ?`, rowID).Scan(&versions); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM memory_liveness WHERE id = ?`, rowID).Scan(&observations); err != nil {
			t.Fatal(err)
		}
		return versions, observations
	}
	expect := func(step string, wantVersions, wantObservations int) {
		t.Helper()
		if v, o := counts(); v != wantVersions || o != wantObservations {
			t.Fatalf("%s: the run has %d version(s) and %d current observation(s), want %d and %d", step, v, o, wantVersions, wantObservations)
		}
	}
	newestVersion := func() (at time.Time, payload map[string]any) {
		t.Helper()
		var raw string
		if err := db.QueryRowContext(ctx, `SELECT "createdAt", payload::text FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`, rowID).Scan(&at, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatal(err)
		}
		return at, payload
	}
	heartbeatOf := func(payload map[string]any) time.Time {
		t.Helper()
		at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(payload["driverHeartbeatAt"]))
		if err != nil {
			t.Fatalf("driverHeartbeatAt %v: %v", payload["driverHeartbeatAt"], err)
		}
		return at.UTC()
	}
	readOnB := func(step string) Run {
		t.Helper()
		got, err := storeB.RunByID(memqlengine.ContextWithFreshRead(ctx), run.ID)
		if err != nil || got == nil {
			t.Fatalf("%s: replica B could not read the run: %v", step, err)
		}
		return *got
	}

	// ---- A claims: a new holder and lease token are a business change ----
	holder := claimedTestDriver(t, a, run.ID)
	expect("after the claim", 2, 0)
	claimVersion, claimPayload := newestVersion()
	if !heartbeatOf(claimPayload).Equal(testNow) {
		t.Fatalf("the claim's version carries heartbeat %v, want %v", claimPayload["driverHeartbeatAt"], testNow)
	}

	// ---- ten renewals: no version, one current observation ----
	// Under a held journal's write observer, as a drive started by a scheduled
	// pass may be: a renewal is not a business write and must not open it.
	writes := 0
	observed := common.ContextWithWriteObserver(ctx, func(string, string) { writes++ })
	var lastA time.Time
	for range 10 {
		lastA = clockA.advance(leaseRenewEvery)
		if !holder.renew(observed) {
			t.Fatal("a renewal found nothing to renew")
		}
	}
	expect("after ten renewals", 2, 1)
	if writes != 0 {
		t.Fatalf("ten renewals reported %d business write(s) to the caller's journal, want none", writes)
	}

	// ---- replica B reads the heartbeat A last observed ----
	if got := readOnB("by id"); got.DriverNodeID != nodeA || !got.DriverHeartbeatAt.Equal(lastA) {
		t.Fatalf("replica B reads holder %q at %v, want %q at %v", got.DriverNodeID, got.DriverHeartbeatAt, nodeA, lastA)
	}
	// Recovery's list read decides whether a pass even tries the run.
	unfinished, err := storeB.RunsUnfinished(memqlengine.ContextWithFreshRead(ctx))
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, r := range unfinished {
		if bareID(r.ID) != bareID(run.ID) {
			continue
		}
		listed = true
		if !r.DriverHeartbeatAt.Equal(lastA) {
			t.Fatalf("recovery's list read on replica B says heartbeat %v, want the observed %v", r.DriverHeartbeatAt, lastA)
		}
		if recoverable(r, nodeB, lastA.Add(leaseStaleAfter/2)) {
			t.Fatal("recovery on replica B would try a lease renewed a minute ago")
		}
	}
	if !listed {
		t.Fatal("recovery's list read on replica B does not list the unfinished run")
	}

	// ---- the fence refuses a decision made before an observed renewal ----
	// The claim's version is still the newest BUSINESS version, so a fence
	// blind to observations would let this stale writer commit.
	stale := memqlengine.ContextWithRowVersionFence(ctx, RunConcept, run.ID, claimVersion)
	if err := storeB.UpdateRun(stale, run.OwnerUserID, run.ID, RunPatch{CancelRequested: ptr(true)}); !errors.Is(err, memqlengine.ErrRowVersionChanged) {
		t.Fatalf("a write decided on the claim's version after ten observed renewals: %v, want ErrRowVersionChanged", err)
	}
	expect("after the refused stale write", 2, 1)

	// ---- B waits for the OBSERVED heartbeat to age, then takes over ----
	// By the claim's version alone the lease is six minutes old; by the
	// observation it is one minute old, and B must leave it.
	clockB.set(lastA.Add(leaseStaleAfter / 2))
	if _, ok, err := b.claim(ctx, b.snapshot(), run.ID); err != nil || ok {
		t.Fatalf("replica B claimed a lease renewed %v ago (ok=%v, err=%v)", leaseStaleAfter/2, ok, err)
	}
	clockB.set(lastA.Add(leaseStaleAfter))
	taker := claimedTestDriver(t, b, run.ID)
	expect("after the takeover", 3, 0)
	if got := readOnB("after the takeover"); got.DriverNodeID != nodeB {
		t.Fatalf("after the takeover the run is held by %q, want %q", got.DriverNodeID, nodeB)
	}
	// A finds out at its next renewal's fresh read and writes nothing.
	clockA.advance(leaseRenewEvery)
	if holder.renew(ctx) {
		t.Fatal("the old holder renewed a lease another replica took over")
	}
	expect("after the old holder's renewal", 3, 0)

	// ---- a stage write carries the heartbeat B last observed ----
	var lastB time.Time
	for range 3 {
		lastB = clockB.advance(leaseRenewEvery)
		if !taker.renew(ctx) {
			t.Fatal("the new holder's renewal found nothing to renew")
		}
	}
	expect("after the new holder's renewals", 3, 1)
	if err := taker.writeRun(ctx, RunPatch{Stages: ptr([]StageSummary{{Name: "build", Status: StageRunning, Steps: 1}})}); err != nil {
		t.Fatalf("stage write: %v", err)
	}
	expect("after the stage write", 4, 0)
	_, stagePayload := newestVersion()
	if got := heartbeatOf(stagePayload); !got.Equal(lastB) {
		t.Fatalf("the stage write's version carries heartbeat %v, want the observed %v", got, lastB)
	}
	if stages, _ := stagePayload["stages"].([]any); len(stages) != 1 {
		t.Fatalf("the stage write's version carries stages %v", stagePayload["stages"])
	}

	// ---- declared retention a day on keeps the newest version and the observation ----
	lastB = clockB.advance(leaseRenewEvery)
	if !taker.renew(ctx) {
		t.Fatal("the renewal after the stage write found nothing to renew")
	}
	expect("before the retention sweep", 4, 1)
	runDef, err := concept.Get(RunConcept)
	if err != nil || runDef.Retention == nil || !runDef.Retention.KeepLatest {
		t.Fatalf("%s no longer declares a keep-latest retention (%v): this step measures nothing", RunConcept, err)
	}
	sweep := &retention.Sweeper{
		DB:         db,
		Concepts:   concept.NewRegistry(map[string]*concept.Concept{RunConcept: runDef}),
		Admit:      func(context.Context, concept.MemoryNode) bool { return true },
		Invalidate: func(string) {},
		Now:        func() time.Time { return time.Now().Add(2 * runDef.Retention.TTL) },
	}
	report, err := sweep.Run(ctx, false)
	if err != nil {
		t.Fatalf("retention sweep: %v", err)
	}
	if report.Deleted != 3 {
		t.Fatalf("the sweep deleted %d superseded version(s), want 3: %+v", report.Deleted, report)
	}
	expect("after the retention sweep", 1, 1)
	if got := readOnB("after the retention sweep"); got.DriverNodeID != nodeB || !got.DriverHeartbeatAt.Equal(lastB) {
		t.Fatalf("after retention replica B reads holder %q at %v, want %q at %v", got.DriverNodeID, got.DriverHeartbeatAt, nodeB, lastB)
	}
	lastB = clockB.advance(leaseRenewEvery)
	if !taker.renew(ctx) {
		t.Fatal("the holder could not renew after retention")
	}
	expect("after renewing past retention", 1, 1)

	// ---- the down migration folds the observation back into a version ----
	applyLeaseMigration(t, db, "20261108190000_pipeline_run_lease_observations.down.sql")
	expect("after the down migration", 2, 0)
	if _, folded := newestVersion(); !heartbeatOf(folded).Equal(lastB) {
		t.Fatalf("the folded version carries heartbeat %v, want the observed %v", folded["driverHeartbeatAt"], lastB)
	}
	lastB = clockB.advance(leaseRenewEvery)
	if !taker.renew(ctx) {
		t.Fatal("the holder could not renew after the down migration")
	}
	expect("a renewal after the down migration", 3, 0)
	if got := readOnB("after the down migration"); !got.DriverHeartbeatAt.Equal(lastB) {
		t.Fatalf("after the down migration replica B reads heartbeat %v, want %v", got.DriverHeartbeatAt, lastB)
	}
}
