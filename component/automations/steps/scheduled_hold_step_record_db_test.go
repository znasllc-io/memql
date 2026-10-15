package steps

// An idle scheduled firing through the real registry writes neither a work
// journal nor the retired per-step record (#6059).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// The retired concept is deliberately registered to catch an accidental writer.
const stepRecordConceptSource = `concept step {
  runId           string
  stepId          string
  stepType        string
  status          string
  sequence        int
  query           string
  resultQuery     string
  itemCount       int
  duration        float
  error           string
  parentStepId    string
  iterationIndex  int
  topic           string
  functionName    string
}
`

// stepRecordConcept builds v1:memql:automation:step from stepRecordConceptSource.
func stepRecordConcept(t *testing.T) *memorynodes.Concept {
	t.Helper()
	file, err := langparser.ParseFile(stepRecordConceptSource)
	require.NoError(t, err)
	for _, def := range file.Definitions {
		if decl, ok := def.(*langparser.ConceptDecl); ok {
			c, err := memorynodes.BuildConceptFromDecl(decl, memorynodes.ConceptMemQLAutomationStep)
			require.NoError(t, err)
			return c
		}
	}
	t.Fatal("the step record source declares no concept")
	return nil
}

func TestIdleScheduledTickThroughTheRealStepRegistryLeavesNoRun_DB(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "idle scheduled tick through the real step registry", dsn, err)
		return
	}
	_, err := memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)

	// The step concept goes on a CLONE of the registry, which only this engine
	// reads: the default registry, shared by every other test in the package,
	// keeps not knowing it.
	registry := memorynodes.CloneDefaultRegistry()
	registry.MergeAll(map[string]*memorynodes.Concept{memorynodes.ConceptMemQLAutomationStep: stepRecordConcept(t)})
	eng, err := memql.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry))
	stepRegistry := NewRegistry()
	eng.SetLogicRunner(automations.NewLogicRunner(eng, stepRegistry, eng.Logger))

	e := automations.NewExecutor(automations.ExecutorOptions{Engine: eng, StepRegistry: stepRegistry})
	t.Cleanup(e.Close)

	// A maintenance-shaped tick: one read, through the real function executor,
	// which records its step after the read returns.
	tick := &automations.Automation{
		Name:     fmt.Sprintf("idleRealRegistryTick%d", time.Now().UnixNano()),
		Schedule: "0 */10 * * * *",
		Trusted:  true,
		Steps: []*automations.Step{{
			ID:       "read",
			Type:     automations.StepTypeFunction,
			Function: &automations.FunctionStepConfig{Name: "staleClusterNodes", Kind: "query"},
		}},
	}
	exec, err := e.Execute(ctx, tick, "schedule")
	require.NoError(t, err)
	require.Equal(t, "completed", exec.Status, "the idle tick itself must succeed: %s", exec.Error)

	records, err := db.NewSelect().
		TableExpr(`"MemoryNodes"`).
		Where("concept = ?", memorynodes.ConceptMemQLAutomationStep).
		Where("payload->>'runId' = ?", exec.ID).
		Count(ctx)
	require.NoError(t, err)
	require.Zero(t, records, "retired step records must never be written")

	// No run: neither through the read resume uses nor in the table itself.
	if _, err := automations.LoadRunJournal(ctx, eng, exec.ID); !errors.Is(err, automations.ErrRunNotFound) {
		t.Fatalf("the idle tick's run: %v, want none -- the step record's insert was heard as the run's write and opened it", err)
	}
	runs, err := db.NewSelect().
		TableExpr(`"MemoryNodes"`).
		Where("concept = ?", "v1:work:run").
		Where("id LIKE ?", "%"+exec.ID).
		Count(ctx)
	require.NoError(t, err)
	require.Zero(t, runs, "the idle tick left %d v1:work:run row(s): createWorkRun was written for a tick that wrote nothing", runs)
}
