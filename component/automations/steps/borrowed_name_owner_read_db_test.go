package steps

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestAnAutomationNotOfTheEnginesTreeReadsNoClusterOwnerRows drives a read
// locked only by `actor.isClusterOwner == true` (recentSendJobs,
// dsl/campaigns/queries.memql) through the real executor, step registry and
// engine, from an automation that carries the name of one of the engine's
// maintenance automations but was not loaded from the engine's own tree.
//
// The maintenance principal is the engine's own automation's. An automation
// from any other source runs as the ordinary system actor whatever it is
// named, so the read answers no rows -- while the same read, made as the
// engine's maintenance principal, answers the row this test wrote.
func TestAnAutomationNotOfTheEnginesTreeReadsNoClusterOwnerRows(t *testing.T) {
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "borrowed-name cluster-owner read test", dsn, err)
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
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	stepReg := NewRegistry()
	eng.SetLogicRunner(automations.NewLogicRunner(eng, stepReg, eng.Logger))

	// One send job, written as the engine writes them.
	const listed = "emailStatisticsRollupSweep"
	principal := auth.MaintenanceActor(auth.AutomationOriginEngine, listed)
	maintenance := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), principal))
	maintenance = auth.ContextWithToken(maintenance, &auth.TokenInfo{Subject: principal.UserId})
	job := fmt.Sprintf("zzborrowedjob%d", time.Now().UnixNano())
	if _, err := eng.Execute(maintenance, fmt.Sprintf(
		"mutation enqueueCampaignSend(campaignId: %s, campaignOwnerUserId: %s, audienceId: %s, templateId: %s)",
		langparser.QuoteString(job), langparser.QuoteString("zz-owner-"+job),
		langparser.QuoteString("zz-audience-"+job), langparser.QuoteString("zz-template-"+job))); err != nil {
		t.Fatalf("seed a send job: %v", err)
	}
	res, err := eng.Execute(maintenance, "query recentSendJobs()")
	if err != nil {
		t.Fatalf("recentSendJobs as the maintenance principal: %v", err)
	}
	if n := extractItemCount(res); n == 0 {
		t.Fatal("recentSendJobs answered no rows to the maintenance principal after a send job was written, so the read below proves nothing")
	}

	// The same name, from a runtime domain's file.
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(
		"use campaigns.queries.{ recentSendJobs }\n\n"+
			"/// A runtime domain's automation.\n"+
			"@trigger(schedule=\"0 0 3 * * *\")\n"+
			"automation "+listed+" {\n"+
			"  jobs := query recentSendJobs()\n"+
			"}\n", "zzborrowedname/automations.memql")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	e := automations.NewExecutor(automations.ExecutorOptions{Engine: eng, StepRegistry: stepReg, Logger: eng.Logger})
	defer e.Close()
	rowsRead := func(a *automations.Automation) int {
		t.Helper()
		exec, err := e.Execute(context.Background(), a, "manual")
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		step := exec.Steps["jobs"]
		if step == nil {
			t.Fatalf("the run recorded no `jobs` step: %+v", exec.Steps)
		}
		if step.Status != "success" {
			t.Fatalf("the read failed rather than answering: %s %s", step.Status, step.Error)
		}
		n, _ := step.Metadata["itemCount"].(int)
		return n
	}
	if n := rowsRead(a); n != 0 {
		t.Fatalf("an automation not of the engine's tree, named %s, read %d send job row(s) through a cluster-owner-only filter: "+
			"it ran as the cluster's maintenance principal", listed, n)
	}

	// The control: the same body, as the engine's tree loader marks the
	// engine's own automation, reads the row -- so the empty answer above is
	// the principal, not the step path.
	a.Source = memqldsl.SourceEngine
	if n := rowsRead(a); n == 0 {
		t.Fatal("the engine's own automation of a listed name read no rows: the maintenance principal is not wired")
	}
}
