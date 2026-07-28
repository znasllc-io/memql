package steps

// dryrun_source_trust_2890_db_test.go -- a dry-run of a TREE automation must
// run with the same trust as the live run of that same automation (memql#2890).
//
// The two run_automation paths agreed on WHICH construct to run -- both resolve
// through loader.LoadByName -- and disagreed on whether its steps could reach
// @serverOnly constructs. Live used the tree-loaded Automation (Trusted=true,
// internal origin); dry-run re-compiled the source via CompileSource, which
// leaves Trusted at its false zero value, so steps ran with client origin. A
// dry-run of killSwitchSuspendsRunningPlans therefore reported a @serverOnly
// refusal the live run never hits.
//
// The direction matters. A preview that is WRONGLY pessimistic is not merely
// noisy: it teaches an operator that dry-run refusals are background noise, and
// the next refusal they wave through is a real one.
//
// Trust is provenance, not caution level -- Automation.Trusted documents itself
// as "this automation's SOURCE came from the registered DSL tree rather than
// from a caller". Source fetched from the tree by canonical name HAS tree
// provenance, so it earns tree trust. Caller-submitted source does not, and the
// false zero value keeps it that way.
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
// tree rather than hand-written so this test exercises the real construct whose
// dry-run diverged, not a fixture that merely resembles it.
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

// dryRunReport runs one dry-run of the tree automation at the given trust and
// returns the report plus a flattened view of everything that could carry a
// refusal, so the assertion does not depend on which field the executor chose.
func dryRunReport(t *testing.T, eng *memql.MemQLEngine, src string, trusted bool) (memql.BundleDryRunReport, string) {
	t.Helper()
	report, err := runBundleDryRun(context.Background(), eng, memql.DryRunRequest{
		AutomationName:   trustProbeAutomation,
		AutomationSource: src,
		SourceTrusted:    trusted,
		TriggerEvent: &memql.DryRunTriggerEvent{
			Topic:   "mcp.run." + trustProbeAutomation,
			Kind:    "manual",
			Payload: map[string]any{},
		},
		Mode: memql.DryRunModeIsolated,
	})
	if err != nil {
		t.Fatalf("runBundleDryRun(trusted=%v): %v", trusted, err)
	}
	var b strings.Builder
	b.WriteString(report.FailureReason)
	for _, s := range report.Trace {
		b.WriteString("\n")
		b.WriteString(s.Note)
		b.WriteString(" ")
		b.WriteString(s.Status)
	}
	return report, b.String()
}

// serverOnlyRefused reports whether a dry-run surface mentions the @serverOnly
// gate. The gate's message is matched loosely on purpose: the assertion is
// about WHETHER the gate fired, and pinning its exact wording would make this
// test fail on a message reword rather than on a trust regression.
func serverOnlyRefused(surface string) bool {
	l := strings.ToLower(surface)
	return strings.Contains(l, "serveronly") || strings.Contains(l, "server-only")
}

// TestDryRunOfTreeAutomationRunsTrusted is the parity assertion memql#2890
// asked for: a dry-run of a tree-resolved automation must not report a
// @serverOnly refusal that the live run of the same construct cannot hit.
func TestDryRunOfTreeAutomationRunsTrusted(t *testing.T) {
	eng := dryRunTrustTestEngine(t)

	src, ok := memql.DSLConstructSource(slog.New(slog.NewTextHandler(io.Discard, nil)), "automation", trustProbeAutomation)
	if !ok {
		t.Skipf("automation %q is not in the tree in this build; nothing to assert", trustProbeAutomation)
	}

	// UNTRUSTED is the pre-fix behaviour, asserted first so the test proves the
	// gate is reachable at all on this automation. Without this, the trusted
	// assertion below would pass just as happily against an automation that
	// never touches a @serverOnly construct -- i.e. vacuously.
	_, untrustedSurface := dryRunReport(t, eng, src, false)
	if !serverOnlyRefused(untrustedSurface) {
		t.Skipf("automation %q no longer trips the @serverOnly gate when untrusted, so this "+
			"test can no longer distinguish trusted from untrusted. Point it at an automation "+
			"that does, or drop it. Surface was: %s", trustProbeAutomation, untrustedSurface)
	}

	// TRUSTED: the same source, declared as tree provenance, must not trip it.
	_, trustedSurface := dryRunReport(t, eng, src, true)
	if serverOnlyRefused(trustedSurface) {
		t.Fatalf("dry-run of tree automation %q reported a @serverOnly refusal while SourceTrusted=true. "+
			"The live path loads this same construct through the tree loader (Trusted=true) and does not "+
			"hit the gate, so the preview is predicting a failure that cannot happen (memql#2890).\n"+
			"surface: %s", trustProbeAutomation, trustedSurface)
	}
}

// TestDryRunOfCallerSuppliedSourceStaysUntrusted is the half that must NOT
// change. SourceTrusted defaults false, and the planner's Gate-2 path compiles
// an LLM-emitted bundle without setting it -- so submitted source keeps running
// with client origin. Losing this would hand a caller-authored body internal
// origin, which is the defect Automation.Trusted exists to prevent (memql#2800).
func TestDryRunOfCallerSuppliedSourceStaysUntrusted(t *testing.T) {
	eng := dryRunTrustTestEngine(t)

	src, ok := memql.DSLConstructSource(slog.New(slog.NewTextHandler(io.Discard, nil)), "automation", trustProbeAutomation)
	if !ok {
		t.Skipf("automation %q is not in the tree in this build", trustProbeAutomation)
	}

	// Same source, no provenance declaration -- the zero value. Even though
	// these bytes happen to come from the tree, a caller that does not declare
	// it gets the untrusted treatment, because the field is the declaration.
	_, surface := dryRunReport(t, eng, src, false)
	if !serverOnlyRefused(surface) {
		t.Fatalf("a dry-run with SourceTrusted unset (the zero value) did NOT hit the @serverOnly "+
			"gate. Submitted source must never reach @serverOnly constructs: that is what "+
			"Automation.Trusted's false-by-default exists to guarantee (memql#2800, #2890).\n"+
			"surface: %s", surface)
	}
}
