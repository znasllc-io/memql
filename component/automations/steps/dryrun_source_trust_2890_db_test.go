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
// CallerSuppliedPayload=FALSE onto a caller-chosen payload (run_automation
// binds the MCP caller's `input`; Gate-2 binds an LLM-emitted one). A later
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
	"encoding/json"
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

func dryRunTrustTestEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://memql:memql_local_dev@localhost:5432/memql?sslmode=disable"
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

// TestDryRunCheckpointIsMarkedCallerSupplied is the guard memql#2890 needed.
//
// It drives a real dry-run of a tree automation whose step hits a @serverOnly
// function -- the failure that mints a checkpoint -- and asserts the checkpoint
// records the payload as CALLER-SUPPLIED, so resume.go computes
// SourceTrusted=false and cannot promote it to internal origin.
func TestDryRunCheckpointIsMarkedCallerSupplied(t *testing.T) {
	eng, db := dryRunTrustTestEngine(t)
	ctx := context.Background()

	src, ok := memql.DSLConstructSource(slog.New(slog.NewTextHandler(io.Discard, nil)), "automation", trustProbeAutomation)
	if !ok {
		t.Skipf("automation %q is not in the tree in this build; nothing to assert", trustProbeAutomation)
	}

	// Timestamp the run so the checkpoint lookup below cannot pick up a row
	// left by an earlier run (this DB is shared and accumulates them).
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
			Payload: map[string]any{"node": map[string]any{"id": "v1:identity:user:caller-chosen"}},
		},
		Mode: memql.DryRunModeIsolated,
	})
	if err != nil {
		t.Fatalf("runBundleDryRun: %v", err)
	}

	// Precondition: the run must actually have failed on the @serverOnly gate.
	// Without this the checkpoint assertion below could pass vacuously by
	// finding no checkpoint at all for the wrong reason.
	if report.OK {
		t.Skipf("dry-run of %q no longer fails, so it mints no checkpoint and this guard cannot "+
			"distinguish anything. Point it at an automation that fails, or drop it.", trustProbeAutomation)
	}

	// Find any checkpoint minted since the mark. Time-bounded rather than
	// keyed on an execution id, because BundleDryRunReport does not surface
	// one; the window is this test's own run.
	var raw string
	err = db.NewSelect().Table("MemoryNodes").ColumnExpr("payload::text").
		Where("concept = ?", "v1:memql:checkpoint").
		Where("\"createdAt\" >= ?::timestamptz", mark).
		Order("createdAt DESC").Limit(1).Scan(ctx, &raw)
	if err != nil || strings.TrimSpace(raw) == "" {
		// No checkpoint is a PASS: nothing to resume, nothing to promote. That
		// is the stronger outcome, and the one the sandbox should ideally have.
		t.Logf("dry-run minted no resumable checkpoint -- nothing to promote, which is the stronger outcome")
		return
	}

	var ckpt map[string]any
	if err := json.Unmarshal([]byte(raw), &ckpt); err != nil {
		t.Fatalf("checkpoint payload is not JSON: %v", err)
	}

	got, _ := ckpt["callerSuppliedPayload"].(bool)
	if !got {
		t.Fatalf("dry-run minted a checkpoint with callerSuppliedPayload=%v.\n"+
			"resume.go computes `SourceTrusted = automation.Trusted && !checkpoint.CallerSuppliedPayload`, "+
			"and POST /automations/resume loads this automation from the tree (Trusted=true), so this "+
			"checkpoint re-dispatches a CALLER-CHOSEN payload at INTERNAL origin. That is the replay "+
			"memql#2888 documents, with the dry-run as leg 1 (memql#2890).\n"+
			"The dry-run must drive the run through ExecuteWithClientEvent, not ExecuteWithEvent.\n"+
			"checkpoint: %s", ckpt["callerSuppliedPayload"], raw)
	}
}
