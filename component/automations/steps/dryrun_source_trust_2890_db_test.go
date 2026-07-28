package steps

// dryrun_source_trust_2890_db_test.go -- the two run_automation paths must
// agree on TRUST, and the agreement they must hold is "both untrusted"
// (memql#2890).
//
// # What #2890 reported, and why the obvious fix is wrong
//
// #2890 (filed 2026-07-26T18:06Z) observed that live resolved the automation
// through the tree loader (Trusted=true -> internal origin) while dry-run
// re-compiled the source via CompileSource (Trusted=false -> client origin), so
// a dry-run of killSwitchSuspendsRunningPlans reported a @serverOnly refusal
// the live run would not hit. That was accurate when written.
//
// #2888 landed the same day, ~4h later (9b355bfb), and closed the gap FROM THE
// LIVE SIDE: app/mcp_automation_runner.go now drives the live run through
// ExecuteWithClientEvent, because `input` is caller-supplied. Executor computes
//
//	exec.SourceTrusted = automation.Trusted && !callerSuppliedPayload
//
// so live resolves SourceTrusted=false however trusted the tree source is. Both
// paths are now untrusted, and they agree.
//
// The trap this file exists to close: reading #2890 today and "fixing" the
// dry-run to compile as trusted re-opens the divergence in the INVERSE and
// DANGEROUS direction -- a preview MORE permissive than the run it predicts,
// which is the case #2890 itself calls out as the bad one. A dry-run that
// passes and a live run that then refuses is worse than a noisy preview.
//
// So the assertion is deliberately "the dry-run does NOT get internal origin".
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

// trustProbeAutomation is the automation #2890 cites. Read from the tree rather
// than hand-written, so this exercises the real construct whose dry-run was
// reported as diverging.
const trustProbeAutomation = "killSwitchSuspendsRunningPlans"

func dryRunTrustTestEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		dsn = "postgres://memql:memql_local_dev@localhost:5432/memql?sslmode=disable"
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "dry-run source-trust test", dsn, err)
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
	return eng
}

// dryRunSurface runs one dry-run of the tree automation and flattens everything
// that could carry a refusal, so the assertion does not depend on which field
// the executor chose to record it in.
func dryRunSurface(t *testing.T, eng *memql.MemQLEngine, src string) string {
	t.Helper()
	report, err := runBundleDryRun(context.Background(), eng, memql.DryRunRequest{
		AutomationName:   trustProbeAutomation,
		AutomationSource: src,
		TriggerEvent: &memql.DryRunTriggerEvent{
			Topic:   "mcp.run." + trustProbeAutomation,
			Kind:    "manual",
			Payload: map[string]any{},
		},
		Mode: memql.DryRunModeIsolated,
	})
	if err != nil {
		t.Fatalf("runBundleDryRun: %v", err)
	}
	var b strings.Builder
	b.WriteString(report.FailureReason)
	for _, s := range report.Trace {
		b.WriteString("\n")
		b.WriteString(s.Note)
		b.WriteString(" ")
		b.WriteString(s.Status)
	}
	return b.String()
}

// serverOnlyRefused reports whether a dry-run surface mentions the @serverOnly
// gate. Matched loosely on purpose: the assertion is about WHETHER the gate
// fired, and pinning exact wording would fail on a message reword rather than
// on a trust regression.
func serverOnlyRefused(surface string) bool {
	l := strings.ToLower(surface)
	return strings.Contains(l, "serveronly") || strings.Contains(l, "server-only")
}

// TestDryRunDoesNotCompileTreeSourceAsTrusted is the guard. A dry-run compiles
// its source through CompileSource, which leaves Automation.Trusted false, so
// its steps must run with client origin -- matching the live path, which is
// caller-driven and therefore untrusted too (#2888).
//
// If this fails, someone has made the dry-run trusted. That looks like a fix
// for #2890 and is not one: the live run of the same automation is still
// untrusted, so the preview would now be MORE permissive than the thing it
// previews.
func TestDryRunDoesNotCompileTreeSourceAsTrusted(t *testing.T) {
	eng := dryRunTrustTestEngine(t)

	src, ok := memql.DSLConstructSource(slog.New(slog.NewTextHandler(io.Discard, nil)), "automation", trustProbeAutomation)
	if !ok {
		t.Skipf("automation %q is not in the tree in this build; nothing to assert", trustProbeAutomation)
	}

	surface := dryRunSurface(t, eng, src)
	if !serverOnlyRefused(surface) {
		t.Fatalf("dry-run of %q did NOT hit the @serverOnly gate, so it ran with internal origin. "+
			"The live path runs this same automation through ExecuteWithClientEvent -- caller-supplied "+
			"payload, so SourceTrusted resolves false there (memql#2888) -- which means the preview is "+
			"now more permissive than the run it predicts. That is the inverse of the divergence "+
			"memql#2890 reported, and the dangerous direction.\nsurface: %s", trustProbeAutomation, surface)
	}
}
