package steps

// dryrun_source_trust_2890_db_test.go -- a dry-run must not mint a resume
// checkpoint that a later resume can promote to internal origin (memql#2890).
//
// # The defect
//
// executeWithEvent stamps TWO fields (component/automations/executor.go:378):
//
//	exec.SourceTrusted         = automation.Trusted && !callerSuppliedPayload
//	exec.CallerSuppliedPayload = callerSuppliedPayload
//
// The second is PERSISTED onto any checkpoint the run mints, and resume.go
// recomputes trust from it:
//
//	exec.SourceTrusted = automation.Trusted && !checkpoint.CallerSuppliedPayload
//
// The dry-run drove the run through ExecuteWithEvent, so it stamped
// CallerSuppliedPayload=FALSE onto a caller-chosen payload -- run_automation
// binds the MCP caller's `input`, and RunInlineAutomation takes the automation
// NAME from caller-submitted source. (The planner's Gate-2 path passes no
// trigger at all; what is caller-influenced there is the SOURCE, not the
// payload, and marking it caller-supplied is right for the same reason.) A later
// POST /automations/resume of that checkpoint loads the automation from the
// tree (Trusted=true) and therefore re-dispatches the caller's payload at
// INTERNAL origin.
//
// That is the replay memql#2888 documents in resume.go's own comment. #2888
// fixed the LIVE leg by moving run_automation to ExecuteWithClientEvent; the
// dry-run leg was left behind, which is what memql#2890 was really reporting.
//
// The vicious part, and why this went unnoticed: saveCheckpointOnFailure fires
// on step failure, so the @serverOnly refusal a preview is SUPPOSED to report
// is precisely the thing that writes the resumable checkpoint. The failing
// preview mints the token.
//
// # What these assert
//
// Checking SourceTrusted alone is NOT sufficient and is how this was nearly
// missed: both paths already resolved SourceTrusted=false, so a test on that
// axis passes while the persisted axis diverges. The assertion has to be on
// what the checkpoint carries.
//
// Postgres-gated: skips cleanly when no DB is reachable.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// trustProbeAutomation is the automation memql#2890 cites. It is read from the
// tree so this exercises the real construct, and it reaches a @serverOnly
// function, which is what makes the run fail and mint a checkpoint.
const trustProbeAutomation = "killSwitchSuspendsRunningPlans"

// callerChosenSentinel is planted in the trigger payload so the checkpoint this
// run mints is positively identifiable among any others on a shared DB.
const callerChosenSentinel = "v1:identity:user:caller-chosen-2890"

func dryRunTrustTestEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "dry-run resume-trust test", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(db)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	return eng, db
}

// memql#2890's guard -- that a dry-run's minted checkpoint is marked
// caller-supplied so resume cannot promote it -- lived here and has been
// REMOVED, not weakened: memql#2932's fix means a dry-run mints no checkpoint
// at all, so that assertion has no subject on this path and would pass
// unconditionally. A test that cannot fail is worse than no test.
//
// The underlying property is still defended in two places: dryrun.go still
// drives the run through ExecuteWithClientEvent (defence in depth, should
// SandboxRun ever be unset), and the checkpoint field mapping itself is
// asserted at the executor level by the memql#2888 tests over
// newCheckpointFromExecution.

// TestDryRunMintsNoCheckpoint is the stronger property memql#2932 asked for,
// now that it holds: a preview writes NOTHING resumable.
//
// SaveCheckpoint went straight to the engine rather than through the sandbox
// step registry, so it was the one write that escaped Gate-2's isolation and
// landed a durable row in the LIVE graph -- contradicting dryrun.go's "zero
// rows land in the live graph". Measured before the fix: one v1:memql:checkpoint
// row per failing dry-run, accumulating without bound.
//
// This supersedes the callerSuppliedPayload assertion above rather than
// replacing it: memql#2890's fix made a minted checkpoint unpromotable, and
// this removes the mint. Both are kept because they fail for different reasons
// -- if checkpointing is ever re-enabled for sandbox runs, THIS test catches
// it, and the one above catches it being promotable.
func TestDryRunMintsNoCheckpoint(t *testing.T) {
	eng, db := dryRunTrustTestEngine(t)
	ctx := context.Background()

	src, ok := memql.DSLConstructSource(slog.New(slog.NewTextHandler(io.Discard, nil)), "automation", trustProbeAutomation)
	if !ok {
		t.Skipf("automation %q is not in the tree in this build", trustProbeAutomation)
	}

	// Time-mark the run. A GLOBAL count would be racy: test/conformance's
	// conf_1727 builds a NON-sandbox executor against this same DB and mints a
	// checkpoint when its automation fails, and `go test ./...` runs those
	// packages concurrently. An unscoped count blames SandboxRun for a row the
	// dry-run did not write. The sibling test above already solved this; this
	// one has to as well.
	var mark string
	if err := db.NewSelect().ColumnExpr("now()::text").Scan(ctx, &mark); err != nil {
		t.Fatalf("clock read: %v", err)
	}

	report, err := runBundleDryRun(ctx, eng, memql.DryRunRequest{
		AutomationName:   trustProbeAutomation,
		AutomationSource: src,
		TriggerEvent: &memql.DryRunTriggerEvent{
			Topic:   "mcp.run." + trustProbeAutomation,
			Kind:    "manual",
			Payload: map[string]any{"node": map[string]any{"id": callerChosenSentinel}},
		},
		Mode: memql.DryRunModeIsolated,
	})
	if err != nil {
		t.Fatalf("runBundleDryRun: %v", err)
	}

	// The run must actually have FAILED -- saveCheckpointOnFailure only fires
	// on step failure, so a passing run would make this vacuous.
	if report.OK || !strings.Contains(strings.ToLower(report.FailureReason), "server-only") {
		t.Fatalf("dry-run did not fail at the @serverOnly gate, so it would not have checkpointed "+
			"even before the fix; this guard is not exercising anything. ok=%v reason=%q",
			report.OK, report.FailureReason)
	}

	// Count only checkpoints minted since the mark that carry THIS run's
	// sentinel payload, so a concurrent package's checkpoint cannot be
	// attributed to the dry-run.
	var mine int
	if err := db.NewSelect().Table("MemoryNodes").ColumnExpr("count(*)").
		Where("concept = ?", "v1:memql:checkpoint").
		Where("\"createdAt\" >= ?::timestamptz", mark).
		Where("payload::text LIKE ?", "%"+callerChosenSentinel+"%").
		Scan(ctx, &mine); err != nil {
		t.Fatalf("count after: %v", err)
	}

	if mine != 0 {
		t.Fatalf("a FAILING dry-run wrote %d checkpoint row(s) carrying its own payload into the "+
			"LIVE graph. A preview has nothing to resume, and the row is a durable resumable token "+
			"naming a real tree automation (memql#2932). ExecutorOptions.SandboxRun must suppress "+
			"saveCheckpointOnFailure.", mine)
	}
}
