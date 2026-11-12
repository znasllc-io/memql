//go:build mcp

package app

// mcp_run_automation_dry_run_test.go -- run_automation's dry run asks by name.
//
// The named dry run hands the sandbox the automation's NAME, so what it
// previews is the loader's own copy -- the object the live branch executes --
// under the identity the live branch runs it under: the owner's envelope when
// the session has an owner, and the automation's own actor when it has none.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// dryRunProbeSweep is one of the engine's listed maintenance automations
// (dsl/worker/automations.memql). Only the loader's own copy of it is granted
// its maintenance principal, which is what tells that copy from a compiled
// text of the same name.
const dryRunProbeSweep = "workerModelPullStaleSweep"

// dryRunRunner is a runner over the real loader and an engine with no
// database: the sweep's first read fails there, after the run's actor is
// installed, which is all these tests read.
func dryRunRunner(t *testing.T) *mcpAutomationRunner {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	eng, err := memql.New(nil)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = logger
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	return &mcpAutomationRunner{
		loader: automations.NewLoader(automations.LoaderOptions{Logger: logger, Registry: concept.DefaultRegistry()}),
		engine: eng,
		logger: logger,
	}
}

func dryRunReport(t *testing.T, out map[string]any) memql.BundleDryRunReport {
	t.Helper()
	if dry, _ := out["dryRun"].(bool); !dry {
		t.Fatalf("the result is not a dry run's: %v", out)
	}
	report, ok := out["report"].(memql.BundleDryRunReport)
	if !ok {
		t.Fatalf("the result carries no dry-run report: %v", out)
	}
	return report
}

func TestRunAutomationDryRunRunsTheLoadersCopyByName(t *testing.T) {
	r := dryRunRunner(t)
	out, err := r.RunAutomation(context.Background(), "", dryRunProbeSweep, nil, true)
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	report := dryRunReport(t, out)
	if report.AutomationName != dryRunProbeSweep {
		t.Errorf("the dry run names %q, want %q", report.AutomationName, dryRunProbeSweep)
	}
	if want := "system:maintenance:" + dryRunProbeSweep; report.RanAs != want {
		t.Errorf("the named dry run ran as %q, want %q: with no session owner it previews the loader's own copy under the actor a live run gives it", report.RanAs, want)
	}
	if report.Authority != "" {
		t.Errorf("the run held its automation's principal, and the report says otherwise: %s", report.Authority)
	}
}

func TestRunAutomationDryRunRunsUnderTheOwnersEnvelopeAsTheLiveRunDoes(t *testing.T) {
	r := dryRunRunner(t)
	const owner = "v1:identity:user:zz-mcp-dry-run-owner"
	out, err := r.RunAutomation(context.Background(), owner, dryRunProbeSweep, nil, true)
	if err != nil {
		t.Fatalf("RunAutomation: %v", err)
	}
	report := dryRunReport(t, out)
	if report.RanAs != owner {
		t.Errorf("the named dry run ran as %q, want the session's owner %q, as the live run does", report.RanAs, owner)
	}
	for _, want := range []string{dryRunProbeSweep, owner, "maintenance principal"} {
		if !strings.Contains(report.Authority, want) {
			t.Errorf("the report does not say %q. authority=%q", want, report.Authority)
		}
	}
}
