package memql

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

// ONE ROW, TWO READERS, AND THEY WANT OPPOSITE THINGS FROM A FAILED CALL
// (memql#6414). The degraded check has to see it and the person who lent the
// machine must not be told it was served. Both reads run here over stored rows,
// because a filter and a projection are the DSL's to honour and a fold over
// fixture maps cannot show that either one does.
func TestAFailedCallReachesTheDegradedCheckAndNotTheLedger(t *testing.T) {
	ctx := context.Background()
	reachable, err := dbtest.EnsureSchema(ctx)
	require.NoError(t, err)
	if !reachable {
		dbtest.Unreachable(t, "sharing ledger", dbtest.DSN(), nil)
		return
	}
	_, err = LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	engine, err := New(db)
	require.NoError(t, err)
	require.NoError(t, engine.Init(memorynodes.DefaultRegistry()))

	machine := id.NewShortId()
	surface := FleetReferencePrefix + machine
	caller := "v1:identity:user:" + id.NewShortId()
	writer := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, caller))
	record := func(onSurface, outcome string) {
		call, err := langparser.RenderCall("recordRouterCall", map[string]any{
			"callId": id.NewShortId(), "requestId": id.NewShortId(), "userId": caller,
			"vendor": "local", "model": "m", "providerName": "fleet:m", "totalDurationMs": 1200, "outcome": outcome,
			"executionSurface": onSurface,
		})
		require.NoError(t, err)
		_, err = engine.Execute(writer, "mutation "+call)
		require.NoError(t, err)
	}
	for _, outcome := range []string{"ok", "ok", "error", "error", "error", "cancelled"} {
		record(surface, outcome)
	}
	record(FleetReferencePrefix+id.NewShortId(), "ok")

	now := time.Now().UTC()
	call, err := langparser.RenderCall(LedgerQuery, map[string]any{
		"surface": surface,
		"since":   now.Add(-time.Hour).Format(time.RFC3339),
		"until":   now.Add(time.Hour).Format(time.RFC3339),
	})
	require.NoError(t, err)
	res, err := engine.Execute(ContextWithFreshRead(auth.ContextWithInternalOrigin(ctx)), call)
	require.NoError(t, err)
	require.Len(t, modelPullRows(res.OutputPayload()), 2, "the ledger is told what the machine served: its two answers")

	health, err := ReadFleetHealth(ContextWithFreshRead(ctx), engine, now.Add(time.Minute))
	require.NoError(t, err)
	got := health.For(machine, "m")
	require.Equal(t, 5, got.Samples, "the check weighs every call but the cancelled one")
	require.Equal(t, 3, got.Failures)
	calls, _, _ := health.Performance(machine)
	require.Equal(t, 2, calls, "fleetCapacity's figures count the answers")
}
